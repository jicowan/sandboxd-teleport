package main

import (
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"io"
)

// mutateExtract returns a reader over the flattened image filesystem tar.
func mutateExtract(img v1.Image) io.ReadCloser { return mutate.Extract(img) }

// ociSpec builds a minimal OCI runtime spec (as a generic map so we don't pull
// the full runtime-spec module) that runsc accepts, with the corrections proven
// necessary in the spikes: writable root, standard namespaces/mounts, no
// host-specific bind mounts.
//
// netnsPath: if non-empty, the sandbox JOINS this network namespace (the interior
// netns with the veth) — this is the checkpointable "sandbox" netstack path. If
// empty, no network namespace is declared (host-net inherits the pod netns).
func ociSpec(args, env []string, cwd string, uid, gid int, netnsPath string) map[string]any {
	namespaces := []map[string]any{
		{"type": "pid"},
		{"type": "ipc"},
		{"type": "uts"},
		{"type": "mount"},
	}
	if netnsPath != "" {
		namespaces = append(namespaces, map[string]any{"type": "network", "path": netnsPath})
	}
	return map[string]any{
		"ociVersion": "1.2.0",
		"process": map[string]any{
			"terminal": false,
			"user":     map[string]any{"uid": uid, "gid": gid},
			"args":     args,
			"env":      env,
			"cwd":      cwd,
			"capabilities": map[string]any{
				"bounding":  defaultCaps,
				"effective": defaultCaps,
				"permitted": defaultCaps,
			},
		},
		// CRITICAL: readonly must be false or the workload can't write its rootfs
		// overlay (runsc spec defaults this true -> silent state loss).
		"root":     map[string]any{"path": "rootfs", "readonly": false},
		"hostname": "sandbox",
		"mounts":   defaultMounts,
		"linux": map[string]any{
			"namespaces": namespaces,
		},
	}
}

var defaultCaps = []string{
	"CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_FSETID", "CAP_FOWNER", "CAP_MKNOD",
	"CAP_NET_RAW", "CAP_SETGID", "CAP_SETUID", "CAP_SETFCAP", "CAP_SETPCAP",
	"CAP_NET_BIND_SERVICE", "CAP_SYS_CHROOT", "CAP_KILL", "CAP_AUDIT_WRITE",
}

var defaultMounts = []map[string]any{
	{"destination": "/proc", "type": "proc", "source": "proc"},
	{"destination": "/dev", "type": "tmpfs", "source": "tmpfs",
		"options": []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
	{"destination": "/dev/pts", "type": "devpts", "source": "devpts",
		"options": []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620"}},
	{"destination": "/dev/shm", "type": "tmpfs", "source": "shm",
		"options": []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"}},
	{"destination": "/dev/mqueue", "type": "mqueue", "source": "mqueue",
		"options": []string{"nosuid", "noexec", "nodev"}},
	{"destination": "/sys", "type": "sysfs", "source": "sysfs",
		"options": []string{"nosuid", "noexec", "nodev", "ro"}},
}
