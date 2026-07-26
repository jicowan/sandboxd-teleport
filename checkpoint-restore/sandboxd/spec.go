package main

// ociSpec builds a minimal OCI runtime spec (as a generic map so we don't pull
// the full runtime-spec module) that runsc accepts, with the corrections proven
// necessary in the spikes: writable root, standard namespaces/mounts, no
// host-specific bind mounts.
//
// netnsPath: if non-empty, the sandbox JOINS this network namespace (the interior
// netns with the veth) — this is the checkpointable "sandbox" netstack path. If
// empty, no network namespace is declared (host-net inherits the pod netns).
//
// memLimitBytes: if >0, sets linux.resources.memory.limit so this sandbox's own
// cgroup OOM-kills a runaway guest BEFORE it can drive the worker POD cgroup to the
// edge (where the kernel could pick the sandboxd agent as the victim). 0 = omit =
// today's behavior (sandbox bounded only by the pod cgroup). See
// docs/sandboxd/PRD/PRD-worker-memory-reserve.md.
func ociSpec(args, env []string, cwd string, uid, gid int, netnsPath string, memLimitBytes int64) map[string]any {
	namespaces := []map[string]any{
		{"type": "pid"},
		{"type": "ipc"},
		{"type": "uts"},
		{"type": "mount"},
	}
	if netnsPath != "" {
		namespaces = append(namespaces, map[string]any{"type": "network", "path": netnsPath})
	}
	mounts := append([]map[string]any{}, defaultMounts...)
	linux := map[string]any{
		"namespaces": namespaces,
	}
	if memLimitBytes > 0 {
		linux["resources"] = map[string]any{
			"memory": map[string]any{"limit": memLimitBytes},
		}
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
		"mounts":   mounts,
		"linux":    linux,
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
	// /tmp world-writable (mode 1777) — AIO's python-server runs as user gem(1000)
	// and does os.makedirs('/tmp/aio-sandbox'); without a writable /tmp it fails
	// with PermissionError and crash-loops. Real runtimes give containers a
	// writable /tmp; our minimal spec must too.
	{"destination": "/tmp", "type": "tmpfs", "source": "tmpfs",
		"options": []string{"nosuid", "nodev", "mode=1777", "size=1g"}},
	{"destination": "/dev/mqueue", "type": "mqueue", "source": "mqueue",
		"options": []string{"nosuid", "noexec", "nodev"}},
	{"destination": "/sys", "type": "sysfs", "source": "sysfs",
		"options": []string{"nosuid", "noexec", "nodev", "ro"}},
}
