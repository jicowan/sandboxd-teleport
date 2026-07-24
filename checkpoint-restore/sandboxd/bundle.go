package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

	// DNS (/etc/resolv.conf) is written directly into the rootfs by the CALLER, not
	// here: on /run by prepareRootfsContainerd (which must run on the same mount-ns
	// OS thread that set the rootfs up), and on /restore by an explicit
	// writeResolvIntoRootfs after the rootfs is rebuilt. Writing it here too was a
	// redundant second write of the same file to the same path (bundle/rootfs).
	uid, gid := parseUser(ic.User)
	spec := ociSpec(args, env, firstNonEmpty(ic.WorkingDir, "/"), uid, gid, netnsPath)
	b, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(bundle, "config.json"), b, 0o644)
}

// parseUser resolves an image's USER (imageConfig.User) into a numeric uid/gid for
// the OCI spec's process.user. It honors the NUMERIC forms the OCI config commonly
// carries — "" / "0" / "1000" / "1000:1000" / "1000:mygroup" (numeric uid, best-
// effort numeric gid) — so an image that declares `USER 1000` no longer silently
// runs as root inside the sandbox.
//
// It deliberately does NOT resolve a NAMED user/group (e.g. "nginx"): that needs an
// /etc/passwd + /etc/group lookup in the image rootfs, with checkpoint/restore edge
// cases, for a low‑severity item (the workload is already gVisor‑isolated — root in
// the guest is not host root). A named/unparseable user falls back to root (0,0),
// which is the prior behavior, and logs it once so the limitation is visible.
// (docs: GitHub issue "imageConfig.User ignored".)
func parseUser(user string) (uid, gid int) {
	if user == "" {
		return 0, 0 // no USER declared → root, as before
	}
	u, g, _ := strings.Cut(user, ":")
	n, err := strconv.Atoi(u)
	if err != nil {
		// named user (e.g. "nginx") — resolving it needs /etc/passwd; fall back to root.
		log.Printf("WARN: image USER %q is not numeric; running the sandbox as root (named-user resolution is not supported)", user)
		return 0, 0
	}
	uid = n
	if g != "" {
		if gn, gerr := strconv.Atoi(g); gerr == nil {
			gid = gn
		} else {
			gid = uid // named group → default gid to uid (best effort; no /etc/group lookup)
		}
	} else {
		gid = uid // "uid" with no group → gid = uid (OCI/Docker default)
	}
	return uid, gid
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
