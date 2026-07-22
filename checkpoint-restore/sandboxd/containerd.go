package main

// Reuse the NODE's containerd image cache instead of pulling+flattening images
// ourselves. sandboxd connects to the node containerd over the mounted socket,
// Pull+Unpack (once, node-level, shared across all worker pods, survives worker
// restart), then Prepare a per-sandbox overlay snapshot and mount it as the
// bundle rootfs. runsc -overlay2=root:self layers on top, so the workload's
// writes go to runsc's own filestore and the shared snapshot lower stays clean.
//
// Replaces the go-containerregistry pull + 8.9GB flatten + hostPath cache: no
// parallel copy, no walk/hardlink; containerd already has the layers unpacked in
// its overlayfs snapshot store and handles pull retry/resume/auth/dedup.

import (
	"context"
	"fmt"
	"os"
	"syscall"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/image-spec/identity"
)

const (
	containerdSock = "/run/containerd/containerd.sock"
	containerdNS   = "k8s.io" // share the same namespace/store kubelet uses
	snapshotter    = "overlayfs"
)

func cdClient() (*containerd.Client, context.Context, error) {
	cl, err := containerd.New(containerdSock)
	if err != nil {
		return nil, nil, fmt.Errorf("containerd dial: %w", err)
	}
	ctx := namespaces.WithNamespace(context.Background(), containerdNS)
	return cl, ctx, nil
}

// containerdAvailable reports whether the node containerd socket is reachable.
func containerdAvailable() bool {
	if _, err := os.Stat(containerdSock); err != nil {
		return false
	}
	cl, err := containerd.New(containerdSock)
	if err != nil {
		return false
	}
	defer cl.Close()
	ctx, cancel := context.WithTimeout(namespaces.WithNamespace(context.Background(), containerdNS), 3*time.Second)
	defer cancel()
	_, err = cl.Version(ctx)
	return err == nil
}

// prepareRootfsContainerd pulls (if needed) + unpacks ref via the node
// containerd, prepares a per-sandbox snapshot keyed by snapKey (the sandbox id),
// mounts it at destRootfs, and returns the image run config.
func prepareRootfsContainerd(ref, destRootfs, snapKey string) (*imageConfig, error) {
	cl, ctx, err := cdClient()
	if err != nil {
		return nil, err
	}
	defer cl.Close()

	// Authenticate the pull for private registries. containerd's default resolver
	// is anonymous (fine for public ghcr/docker-hub images, and for cache hits on
	// images the kubelet already pulled), but a cache MISS on a private ECR image
	// 401s. Pass a resolver whose Hosts authenticate ECR endpoints with a token
	// fetched via the worker's AWS identity (Pod Identity); non-ECR hosts stay
	// anonymous. See ecrauth.go.
	pullOpts := []containerd.RemoteOpt{
		containerd.WithPullUnpack,
		containerd.WithPullSnapshotter(snapshotter),
	}
	if isECRHost(registryHost(ref)) {
		pullOpts = append(pullOpts, containerd.WithResolver(
			docker.NewResolver(docker.ResolverOptions{Hosts: ecrRegistryHosts(ctx)}),
		))
	}
	img, err := cl.Pull(ctx, ref, pullOpts...)
	if err != nil {
		return nil, fmt.Errorf("containerd pull %q: %w", ref, err)
	}

	ic, err := imageConfigFromContainerd(ctx, ref, img)
	if err != nil {
		return nil, err
	}

	diffIDs, err := img.RootFS(ctx)
	if err != nil {
		return nil, fmt.Errorf("image rootfs: %w", err)
	}
	parent := identity.ChainID(diffIDs).String()

	sn := cl.SnapshotService(snapshotter)
	if err := os.MkdirAll(destRootfs, 0o755); err != nil {
		return nil, err
	}
	// Prepare + mount the per-sandbox overlay snapshot. No retry wrapper: the
	// "first-ever run of a just-pulled image fails, a retry succeeds" symptom we
	// used to retry against was actually the mount being invisible to a later,
	// netns-switched OS thread (setupSandboxNet's runtime.LockOSThread +
	// netns.Set). The fix is to do the writable ops that need the mount HERE, on
	// the same OS thread that ran mount.All, before any netns work — not to retry.
	// Verified: cold uncached images now run first-attempt (memcached/nats/mongo/
	// httpd/rabbitmq + a cold teleport all passed 5/5).
	// Clear any stale snapshot under this key before Prepare. A prior sandbox with
	// the SAME sandbox id may have run on this worker (fork sid reuse / reset+retry
	// under churn) and left its overlay snapshot behind — Prepare then fails
	// "already exists". Unmount the rootfs first (Remove fails while the snapshot is
	// still mounted), then Remove.
	syscall.Unmount(destRootfs, syscall.MNT_DETACH)
	sn.Remove(ctx, snapKey)
	mounts, err := sn.Prepare(ctx, snapKey, parent)
	if err != nil && errdefs.IsAlreadyExists(err) {
		// The pre-Prepare Remove didn't take (snapshot still referenced/mounted).
		// Force a clean removal and retry once — this is the churn-triggered
		// sid-reuse collision, recoverable without failing the run.
		syscall.Unmount(destRootfs, syscall.MNT_DETACH)
		if rerr := sn.Remove(ctx, snapKey); rerr != nil {
			return nil, fmt.Errorf("snapshot prepare: %w (stale snapshot remove failed: %v)", err, rerr)
		}
		mounts, err = sn.Prepare(ctx, snapKey, parent)
	}
	if err != nil {
		return nil, fmt.Errorf("snapshot prepare: %w", err)
	}
	if err := mount.All(mounts, destRootfs); err != nil {
		sn.Remove(ctx, snapKey)
		return nil, fmt.Errorf("mount snapshot: %w", err)
	}
	// rshared so the mount PROPAGATES into runsc's gofer mount namespace.
	syscall.Mount("", destRootfs, "", syscall.MS_SHARED|syscall.MS_REC, "")
	// Writability probe: assert the rootfs is actually usable (not just listable)
	// before runsc's gofer tries to create its .gvisor.filestore file there. Kept
	// as a cheap correctness check; the retry loop it used to gate is gone.
	probe := destRootfs + "/.sbxd-probe"
	f, perr := os.Create(probe)
	if perr != nil {
		syscall.Unmount(destRootfs, syscall.MNT_DETACH)
		sn.Remove(ctx, snapKey)
		return nil, fmt.Errorf("rootfs not writable after prepare: %w", perr)
	}
	f.Close()
	os.Remove(probe)
	// Write /etc/resolv.conf HERE, on the SAME OS thread / mount namespace that
	// just ran mount.All. Doing it later in the handler failed with "rootfs 0
	// entries": setupSandboxNet's netns thread-switching left that code on a
	// thread whose mount-ns copy didn't include this mount. Best-effort; a
	// DNS-less sandbox is non-fatal.
	if werr := writeResolvIntoRootfs(destRootfs); werr != nil {
		fmt.Fprintf(os.Stderr, "[prepare] WARN resolv.conf: %v\n", werr)
	}
	return ic, nil
}

// imageConfigFromContainerd reads the OCI run config (entrypoint/cmd/env/workdir)
// + digest from a containerd image.
func imageConfigFromContainerd(ctx context.Context, ref string, img containerd.Image) (*imageConfig, error) {
	spec, err := img.Spec(ctx)
	if err != nil {
		return nil, fmt.Errorf("image spec: %w", err)
	}
	c := spec.Config
	wd := c.WorkingDir
	if wd == "" {
		wd = "/"
	}
	return &imageConfig{
		Entrypoint: c.Entrypoint,
		Cmd:        c.Cmd,
		Env:        c.Env,
		WorkingDir: wd,
		User:       c.User,
		Ref:        ref,
		Digest:     img.Target().Digest.String(),
	}, nil
}

// teardownRootfsContainerd unmounts destRootfs and removes the sandbox snapshot.
func teardownRootfsContainerd(destRootfs, snapKey string) {
	mount.Unmount(destRootfs, 0)
	cl, ctx, err := cdClient()
	if err != nil {
		return
	}
	defer cl.Close()
	cl.SnapshotService(snapshotter).Remove(ctx, snapKey)
}
