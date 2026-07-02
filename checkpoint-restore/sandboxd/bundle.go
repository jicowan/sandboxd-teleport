package main

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// imageConfig is the subset of the OCI image config sandboxd needs to build a
// runsc-compatible OCI runtime spec: how the image expects to run.
type imageConfig struct {
	Entrypoint []string
	Cmd        []string
	Env        []string
	WorkingDir string
	User       string
	Ref        string // fully-qualified image reference (for teleport identity)
	Digest     string // image manifest digest (recorded in the snapshot manifest)
}

// pullAndFlatten pulls an OCI image from any registry (no daemon, no host
// containerd) and flattens its layers into destRootfs. Returns the image's run
// config so the caller can build a matching OCI spec. This is the library
// equivalent of `crane export` + `crane config`.
func pullAndFlatten(ref, destRootfs string) (*imageConfig, error) {
	r, err := name.ParseReference(ref)
	if err != nil {
		return nil, fmt.Errorf("parse ref %q: %w", ref, err)
	}
	// Anonymous auth for public images (Docker Hub, gcr, etc.). Private registries
	// (ECR) will need creds wired later; on distroless there's no docker config to
	// read, so the keychain path could stall — anonymous is the correct default here.
	img, err := remote.Image(r, remote.WithAuthFromKeychain(authn.DefaultKeychain), remote.WithContext(context.Background()))
	if err != nil {
		return nil, fmt.Errorf("pull %q: %w", ref, err)
	}
	if err := os.MkdirAll(destRootfs, 0o755); err != nil {
		return nil, err
	}
	// Flatten: mutate.Extract gives a single tar stream of the merged filesystem.
	if err := extractImage(img, destRootfs); err != nil {
		return nil, fmt.Errorf("extract rootfs: %w", err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("config file: %w", err)
	}
	dig, err := img.Digest()
	if err != nil {
		return nil, err
	}
	c := cf.Config
	return &imageConfig{
		Entrypoint: c.Entrypoint,
		Cmd:        c.Cmd,
		Env:        c.Env,
		WorkingDir: firstNonEmpty(c.WorkingDir, "/"),
		User:       c.User,
		Ref:        ref,
		Digest:     dig.String(),
	}, nil
}

// extractImage writes the flattened image filesystem into dest.
func extractImage(img v1.Image, dest string) error {
	rc := mutateExtract(img)
	defer rc.Close()
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dest, filepath.Clean("/"+hdr.Name))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		case tar.TypeSymlink:
			os.MkdirAll(filepath.Dir(target), 0o755)
			os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		case tar.TypeLink:
			os.MkdirAll(filepath.Dir(target), 0o755)
			os.Remove(target)
			if err := os.Link(filepath.Join(dest, hdr.Linkname), target); err != nil {
				// best-effort; some hardlinks may reference not-yet-seen files
			}
		}
	}
}

// writeOCISpec generates an OCI runtime spec (config.json) for the bundle,
// derived from a `runsc spec` baseline and patched with the image's run config.
// Key correctness bits learned the hard way:
//   - root.readonly MUST be false (runsc spec defaults true -> workload can't write)
//   - process.args = entrypoint+cmd (or user override)
//   - essential mounts for localhost/DNS/shm
func writeOCISpec(bundle string, ic *imageConfig, cmdOverride, envOverride []string) error {
	args := append(append([]string{}, ic.Entrypoint...), ic.Cmd...)
	if len(cmdOverride) > 0 {
		args = cmdOverride
	}
	if len(args) == 0 {
		args = []string{"/bin/sh"}
	}
	env := ic.Env
	if len(env) == 0 {
		env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	}
	env = append(env, envOverride...)

	uid, gid := 0, 0
	spec := ociSpec(args, env, firstNonEmpty(ic.WorkingDir, "/"), uid, gid)
	b, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(bundle, "config.json"), b, 0o644)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
