package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// runscDriver drives the pinned runsc binary against a per-sandboxd state root.
// Flags encode everything the spikes proved necessary:
//   --network=none, -overlay2=root:self (atomic mem+fs), no -direct on overlay.
type runscDriver struct {
	bin      string // path to the pinned runsc binary
	root     string // runsc state -root (owned by this worker)
	debugDir string // where sentry/gofer --debug-log files land
	network  string // runsc --network mode: "host" | "sandbox" | "none"
}

func newRunsc(bin, root string) *runscDriver {
	os.MkdirAll(root, 0o755)
	d := &runscDriver{
		bin:      bin,
		root:     root,
		debugDir: filepath.Join(filepath.Dir(root), "gvisor-logs"),
		network:  envOr("SANDBOXD_NETWORK", "sandbox"),
	}
	os.MkdirAll(d.debugDir, 0o755)
	return d
}

func (r *runscDriver) base() []string {
	// --network=host: the sandbox shares the WORKER POD's netns, so a server the
	//   workload binds on 0.0.0.0:PORT is reachable at the worker pod IP:PORT (the
	//   pod has a routable CNI IP). One sandbox per worker => no port collisions.
	//   This is the MVP data path; --network=sandbox + a proxy is the more-isolated
	//   evolution (see NETWORKING-LIFECYCLE.md).
	// --debug/--debug-log: capture the nested sentry/gofer logs (surfaced via /logs
	//   and streamed to stdout).
	// --directfs=false: directfs needs /proc/self/uid_map (absent nested).
	flags := []string{
		"-root", r.root,
		"--network=" + r.network,
		"-overlay2=root:self",
		"--directfs=false",
		"--log", r.debugDir + "/runsc.log",
	}
	// --debug is EXTREMELY verbose (per-syscall trace) — it floods stdout and adds
	// real overhead. Off by default; enable via SANDBOXD_DEBUG=1 when diagnosing.
	if os.Getenv("SANDBOXD_DEBUG") == "1" {
		flags = append(flags, "--debug", "--debug-log", r.debugDir+"/")
	}
	return flags
}

// runTimeout bounds a runsc call so a wedged/half-dead sandbox can't hang the
// HTTP handler forever. On timeout the process (and its group) is killed.
const runTimeout = 30 * time.Second

func (r *runscDriver) run(args ...string) (string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	full := append(r.base(), args...)
	cmd := exec.CommandContext(ctx, r.bin, full...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	// Detach the child from our process group so a backgrounded sandbox (from
	// `run -detach`) doesn't keep our stdio pipes open and block cmd.Run().
	// Cancel kills the whole group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return out.String(), errb.String(), fmt.Errorf("runsc %v timed out after %s", args, runTimeout)
	}
	return out.String(), errb.String(), err
}

// runDetached execs runsc with stdio pointed at FILES (not pipe-buffers). A
// detached sandbox keeps its stdio open in the background; if we gave it buffer
// pipes, cmd.Run() would block forever waiting for EOF. Files avoid that.
// runDetached launches a runsc command whose sandbox DETACHES and keeps running.
// CRITICAL: the detached sandbox inherits our stdout/stderr fds; if those are a
// pipe/buffer/MultiWriter, cmd.Run() blocks forever waiting for the (never-
// closing) sandbox to close them. So we point the detached process's stdio at
// /dev/null and rely on runsc --debug-log FILES (surfaced via /logs) for the
// nested container's own logs. cmd.Wait then returns once the foreground runsc
// forks the detached child.
func (r *runscDriver) runDetached(logPath string, args ...string) error {
	full := append(r.base(), args...)
	cmd := exec.Command(r.bin, full...)
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer devnull.Close()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, devnull, devnull
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Run(); err != nil {
		// on failure, the useful detail is in the --debug-log; include a tail
		return fmt.Errorf("%v; gvisor logs: %s", err, r.tailGvisorLogs(4096))
	}
	return nil
}

// tailToStdout streams NEW content from the gVisor debug-log files to stdout, so
// the nested sentry/gofer logs appear in `kubectl logs` on the parent. It runs in
// a background goroutine, decoupled from the exec'd runsc processes (so it can't
// cause the fd-inheritance blocking that forced detached runs to use /dev/null).
func (r *runscDriver) tailToStdout() {
	offsets := map[string]int64{}
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for range t.C {
		entries, _ := os.ReadDir(r.debugDir)
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			p := filepath.Join(r.debugDir, e.Name())
			fi, err := os.Stat(p)
			if err != nil {
				continue
			}
			off := offsets[e.Name()]
			if fi.Size() <= off {
				continue
			}
			f, err := os.Open(p)
			if err != nil {
				continue
			}
			f.Seek(off, 0)
			buf := make([]byte, fi.Size()-off)
			n, _ := f.Read(buf)
			f.Close()
			offsets[e.Name()] = fi.Size()
			if n > 0 {
				fmt.Fprintf(os.Stdout, "[gvisor %s] %s", e.Name(), string(buf[:n]))
			}
		}
	}
}

// tailGvisorLogs returns the last maxBytes of the sentry/gofer debug logs — the
// nested gVisor container's own logs — for surfacing via the /logs endpoint.
func (r *runscDriver) tailGvisorLogs(maxBytes int64) string {
	entries, _ := os.ReadDir(r.debugDir)
	var out []byte
	for _, e := range entries {
		p := filepath.Join(r.debugDir, e.Name())
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if int64(len(b)) > maxBytes {
			b = b[int64(len(b))-maxBytes:]
		}
		out = append(out, []byte("==== "+e.Name()+" ====\n")...)
		out = append(out, b...)
		out = append(out, '\n')
	}
	return string(out)
}

// Create + Start the sandbox from a prepared bundle, DETACHED.
// `runsc start` runs the container in the FOREGROUND and blocks until it exits —
// fatal for a long-running workload. `runsc run -detach` does create+start and
// returns, leaving the sandbox running in the background.
func (r *runscDriver) createStart(id, bundle string) error {
	pid := filepath.Join(bundle, id+".pid")
	log := filepath.Join(bundle, id+".run.log")
	if err := r.runDetached(log, "run", "-bundle", bundle, "-pid-file", pid, "-detach", id); err != nil {
		return fmt.Errorf("run -detach: %w", err)
	}
	return nil
}

// Checkpoint the sandbox to imageDir (single atomic mem+fs image via overlay).
func (r *runscDriver) checkpoint(id, imageDir string, leaveRunning bool) error {
	os.MkdirAll(imageDir, 0o755)
	args := []string{"checkpoint", "-image-path", imageDir}
	if leaveRunning {
		args = append(args, "--leave-running")
	}
	args = append(args, id)
	if _, se, err := r.run(args...); err != nil {
		return fmt.Errorf("checkpoint: %v: %s", err, se)
	}
	return nil
}

// Restore: `runsc restore` alone establishes the sandbox AND restores into it in
// one step (like `run` does for a fresh start). A separate preceding `create`
// boots a DIFFERENT sandbox that waits for `start` — its watchdog logs
// "Watchdog.Start() not called within 30s" and the container hangs at "created"
// because the restore never targets it. So: NO separate create; restore -detach.
func (r *runscDriver) restore(id, bundle, imageDir string) error {
	pid := filepath.Join(bundle, id+".pid")
	log := filepath.Join(bundle, id+".restore.log")
	if err := r.runDetached(log, "restore", "-bundle", bundle, "-image-path", imageDir,
		"-pid-file", pid, "-detach", id); err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	return nil
}

// State returns the container status ("running", "stopped", ...).
func (r *runscDriver) state(id string) (string, error) {
	out, se, err := r.run("state", id)
	if err != nil {
		return "", fmt.Errorf("state: %v: %s", err, se)
	}
	var st struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		return "", err
	}
	return st.Status, nil
}

// delete tears a sandbox down fast and robustly.
//
// The naive `runsc kill` + `delete -force` was slow (~5s) and error-prone: runsc
// kill often reports "sandbox is not running" while the host runsc-gofer/
// runsc-sandbox processes are STILL ALIVE in the container's cgroup, so
// `delete -force` then spins ~5s retrying "removing cgroup: device or resource
// busy" and finally FATALs. Fix: kill the whole cgroup directly via cgroup v2
// `cgroup.kill` (reliably SIGKILLs every process in it, incl. the gofer/sentry),
// wait briefly for the cgroup to drain, THEN delete.
// delete mirrors substrate's teardown (cmd/ateom-gvisor cleanupContainersAfterCheckpoint):
// call `runsc state` FIRST to let runsc sync its internal state with the kernel
// ("Without this, runsc delete occasionally throws an error" / cgroup busy), THEN
// `runsc delete -force`. The real fix for the cgroup-busy stalls is the child
// reaper (see reapChildren): sandboxd is PID 1 in its container, so it must reap
// the gofer/sentry's exited children or their zombies pin the cgroup.
func (r *runscDriver) delete(id string) error {
	t0 := time.Now()
	r.run("state", id) // best-effort state sync before delete (substrate pattern)
	_, se, err := r.run("delete", "-force", id)
	if err != nil {
		return fmt.Errorf("delete: %v: %s", err, se)
	}
	if d := time.Since(t0); d > time.Second {
		fmt.Fprintf(os.Stderr, "[runsc] delete %s took %s\n", id, d)
	}
	return nil
}

// version returns the pinned runsc version string (recorded in snapshot manifests
// so restores can refuse a version mismatch).
func (r *runscDriver) version() string {
	out, _, _ := func() (string, string, error) {
		cmd := exec.Command(r.bin, "--version")
		var o, e bytes.Buffer
		cmd.Stdout, cmd.Stderr = &o, &e
		err := cmd.Run()
		return o.String(), e.String(), err
	}()
	return out
}
