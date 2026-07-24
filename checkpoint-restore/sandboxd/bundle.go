package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// imageConfig is the subset of the OCI image config sandboxd needs to build a
// runsc-compatible OCI runtime spec: how the image expects to run. Populated
// from the node containerd image (see containerd.go).
type imageConfig struct {
	Entrypoint []string
	Cmd        []string
	Env        []string
	WorkingDir string
	User       string
	Digest     string // image manifest digest (recorded in the snapshot manifest)
}

// writeOCISpec generates an OCI runtime spec (config.json) for the bundle,
// derived from a `runsc spec` baseline and patched with the image's run config.
// Key correctness bits learned the hard way:
//   - root.readonly MUST be false (runsc spec defaults true -> workload can't write)
//   - process.args = entrypoint+cmd (or user override)
//   - a network namespace path is injected iff networking is set up
func writeOCISpec(bundle string, ic *imageConfig, cmdOverride, envOverride []string, netnsPath string) error {
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

	// DNS: when networked, write /etc/resolv.conf directly INTO the rootfs. We do
	// NOT bind-mount it via the OCI spec: runsc requires a bind mount's TARGET file
	// to pre-exist in the rootfs, and /etc/resolv.conf usually doesn't, so the bind
	// fails ("open .../rootfs/etc/resolv.conf: no such file or directory"). The
	// rootfs is the writable containerd overlay and /etc exists in real images, so
	// a direct write is the robust path. bundle/rootfs is passed in as rootfsDir.
	if netnsPath != "" {
		if err := writeResolvIntoRootfs(filepath.Join(bundle, "rootfs")); err != nil {
			return fmt.Errorf("resolv.conf: %w", err)
		}
	}

	uid, gid := 0, 0
	spec := ociSpec(args, env, firstNonEmpty(ic.WorkingDir, "/"), uid, gid, netnsPath)
	b, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(bundle, "config.json"), b, 0o644)
}

// writeResolvIntoRootfs writes /etc/resolv.conf directly into the (writable
// containerd overlay) rootfs. On restore the rootfs is rebuilt fresh from the
// image, so this is re-applied there too. Copies the worker pod's resolver
// (kube-dns) so in-cluster + external names resolve; egress is masqueraded.
func writeResolvIntoRootfs(rootfsDir string) error {
	etc := filepath.Join(rootfsDir, "etc")
	if err := os.MkdirAll(etc, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", etc, err)
	}
	data, err := os.ReadFile("/etc/resolv.conf")
	if err != nil || len(data) == 0 {
		data = []byte("nameserver 8.8.8.8\noptions ndots:1\n")
	}
	target := filepath.Join(etc, "resolv.conf")
	os.Remove(target) // break any hardlink / stale symlink
	if err := os.WriteFile(target, data, 0o644); err != nil {
		// diagnostic: is /etc a symlink? does rootfs look mounted?
		fi, statErr := os.Lstat(etc)
		mode := "missing"
		if statErr == nil {
			mode = fi.Mode().String()
		}
		rents, _ := os.ReadDir(rootfsDir)
		return fmt.Errorf("write %s: %w (etc mode=%s, rootfs has %d entries)", target, err, mode, len(rents))
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
