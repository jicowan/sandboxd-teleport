package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// runscDriver drives the pinned runsc binary against a per-sandboxd state root.
// Flags encode everything the spikes proved necessary:
//   --network=none, -overlay2=root:self (atomic mem+fs), no -direct on overlay.
type runscDriver struct {
	bin      string // path to the pinned runsc binary
	root     string // runsc state -root (owned by this worker)
	debugDir string // where sentry/gofer --debug-log files land
}

func newRunsc(bin, root string) *runscDriver {
	os.MkdirAll(root, 0o755)
	d := &runscDriver{bin: bin, root: root, debugDir: filepath.Join(filepath.Dir(root), "gvisor-logs")}
	os.MkdirAll(d.debugDir, 0o755)
	return d
}

func (r *runscDriver) base() []string {
	// --debug + --debug-log captures the SENTRY/GOFER logs (the nested gVisor
	// container's own output) into files we can surface in the parent.
	return []string{
		"-root", r.root,
		"--network=none",
		"-overlay2=root:self",
		"--debug",
		"--debug-log", r.debugDir + "/",
		"--log", r.debugDir + "/runsc.log",
	}
}

func (r *runscDriver) run(args ...string) (string, string, error) {
	full := append(r.base(), args...)
	cmd := exec.Command(r.bin, full...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	// Detach the child from our process group so a backgrounded sandbox (from
	// `run -detach`) doesn't keep our stdio pipes open and block cmd.Run().
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err := cmd.Run()
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

// Restore: create then restore (the proven sequence), detached, no -direct.
// The restore is -detach (backgrounds the sandbox) so it MUST use runDetached
// (file stdio) or cmd.Run() blocks on inherited pipes — same bug as createStart.
func (r *runscDriver) restore(id, bundle, imageDir string) error {
	pid := filepath.Join(bundle, id+".pid")
	if _, se, err := r.run("create", "-bundle", bundle, "-pid-file", pid, id); err != nil {
		return fmt.Errorf("restore/create: %v: %s", err, se)
	}
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

func (r *runscDriver) delete(id string) error {
	_, se, err := r.run("delete", "-force", id)
	if err != nil {
		return fmt.Errorf("delete: %v: %s", err, se)
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
