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
	"github.com/containerd/containerd/v2/pkg/namespaces"
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

	img, err := cl.Pull(ctx, ref,
		containerd.WithPullUnpack,
		containerd.WithPullSnapshotter(snapshotter),
	)
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
	// Prepare + mount, with retries: right after a FRESH WithPullUnpack the
	// snapshot/overlay can briefly present an incomplete tree (the first-ever run
	// of a just-pulled image fails, a retry succeeds). Retry until the rootfs is
	// actually populated so runsc's gofer doesn't hit "filestore file ... no such
	// file or directory".
	var lastErr error
	for attempt := 1; attempt <= 5; attempt++ {
		syscall.Unmount(destRootfs, syscall.MNT_DETACH)
		sn.Remove(ctx, snapKey)
		mounts, err := sn.Prepare(ctx, snapKey, parent)
		if err != nil {
			lastErr = fmt.Errorf("snapshot prepare: %w", err)
			time.Sleep(time.Duration(attempt) * 300 * time.Millisecond)
			continue
		}
		if err := mount.All(mounts, destRootfs); err != nil {
			lastErr = fmt.Errorf("mount snapshot: %w", err)
			sn.Remove(ctx, snapKey)
			time.Sleep(time.Duration(attempt) * 300 * time.Millisecond)
			continue
		}
		// rshared so the mount PROPAGATES into runsc's gofer mount namespace.
		syscall.Mount("", destRootfs, "", syscall.MS_SHARED|syscall.MS_REC, "")
		// confirm the rootfs is actually usable (populated). A well-formed image
		// rootfs always has several top-level dirs (bin, etc, usr, ...).
		if ents, _ := os.ReadDir(destRootfs); len(ents) >= 3 {
			return ic, nil
		}
		lastErr = fmt.Errorf("rootfs mount empty after prepare (parent=%s)", parent)
		time.Sleep(time.Duration(attempt) * 300 * time.Millisecond)
	}
	syscall.Unmount(destRootfs, syscall.MNT_DETACH)
	sn.Remove(ctx, snapKey)
	return nil, fmt.Errorf("prepare rootfs (after retries): %w", lastErr)
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
