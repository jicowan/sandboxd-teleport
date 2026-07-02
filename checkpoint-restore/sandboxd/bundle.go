package main

import (
	"encoding/json"
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
	Ref        string // fully-qualified image reference (for teleport identity)
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

	uid, gid := 0, 0
	spec := ociSpec(args, env, firstNonEmpty(ic.WorkingDir, "/"), uid, gid, netnsPath)
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
