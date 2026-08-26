package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jicowan/aio-sandbox/sandboxd/ateomnet"
)

// runscDriver drives the pinned runsc binary against a per-sandboxd state root.
// Flags encode everything the spikes proved necessary:
//
//	--network=sandbox (checkpointable netstack), -overlay2=root:dir=<per-sandbox>
//	(writable upper outside the rootfs; see base/overlayDir), --directfs=false.
type runscDriver struct {
	bin      string // path to the pinned runsc binary
	root     string // runsc state -root (owned by this worker)
	debugDir string // where sentry/gofer --debug-log files land
	network  string // runsc --network mode: "host" | "sandbox" | "none"
	ver      string // pinned runsc version, resolved ONCE at startup (immutable binary)
	// streamConsole streams the nested workload's own stdout/stderr to sandboxd
	// stdout (→ kubectl logs). OFF by default (SANDBOXD_STREAM_CONSOLE=1): the
	// workload console is UNTRUSTED, attacker-controlled output — see
	// tailConsoleToStdout for the sanitize/cap mitigations and why this is opt-in.
	streamConsole bool
}

func newRunsc(bin, root string) *runscDriver {
	os.MkdirAll(root, 0o755)
	d := &runscDriver{
		bin:           bin,
		root:          root,
		debugDir:      filepath.Join(filepath.Dir(root), "gvisor-logs"),
		network:       envOr("SANDBOXD_NETWORK", "sandbox"),
		streamConsole: os.Getenv("SANDBOXD_STREAM_CONSOLE") == "1",
	}
	os.MkdirAll(d.debugDir, 0o755)
	// Resolve the pinned runsc version ONCE (the binary is immutable for the
	// process lifetime). Doing it here — instead of forking `runsc --version` on
	// every /run, /restore (3×), and /version request — both removes per-request
	// process spawns and lets us surface a failure loudly: a silently-empty version
	// would make the restore version-guard (main.go) treat a genuine runsc mismatch
	// as "no version recorded" and restore anyway (a teleport-safety hole).
	d.ver = d.resolveVersion()
	if d.ver == "" {
		log.Printf("WARN: `runsc --version` returned no version; checkpoint/restore version-guard is DISABLED (mismatched restores will not be refused). Check the pinned runsc binary at %s", bin)
	}
	return d
}

// overlayDir returns the per-sandbox host directory that backs runsc's writable
// overlay upper. See base(): we use --overlay2=root:dir=<this> instead of
// root:self so runsc does NOT write .gvisor.filestore INTO the (containerd
// overlay) rootfs — which caused the intermittent "filestore file ... no such
// file or directory" race (the file is created by the runsc parent into the
// gofer's mount-ns copy of the rootfs, which may not have the snapshot mount
// propagated yet). A stable plain dir sidesteps that entirely.
func (r *runscDriver) overlayDir(id string) string {
	return filepath.Join(filepath.Dir(r.root), "overlays", id)
}

func (r *runscDriver) base(id string) []string {
	// -overlay2=root:dir=<per-sandbox host dir>: writable upper in a plain dir
	//   OUTSIDE the rootfs (see overlayDir). MUST be identical across create/
	//   checkpoint/restore/state/delete for a given container.
	// --network: sandbox netstack (checkpointable) via the veth/interior-netns.
	// --directfs=false: directfs needs /proc/self/uid_map (absent nested).
	// Every per-container op passes a non-empty id, so the overlay upper is always
	// a per-sandbox host dir (root:dir=); there is no root:self path.
	od := r.overlayDir(id)
	os.MkdirAll(od, 0o755)
	flags := []string{
		"-root", r.root,
		"--network=" + r.network,
		"-overlay2=root:dir=" + od,
		"--directfs=false",
		"--log", r.debugDir + "/runsc.log",
	}
	// --debug is EXTREMELY verbose (per-syscall trace) — floods stdout + overhead.
	// Off by default; enable via SANDBOXD_DEBUG=1 when diagnosing.
	if os.Getenv("SANDBOXD_DEBUG") == "1" {
		flags = append(flags, "--debug", "--debug-log", r.debugDir+"/")
	}
	return flags
}

// runTimeout bounds a runsc call so a wedged/half-dead sandbox can't hang the
// HTTP handler forever. On timeout the process (and its group) is killed.
const runTimeout = 30 * time.Second

// run executes a per-container runsc subcommand. id is the container id (used to
// select its overlay dir); it must match the id in args.
func (r *runscDriver) run(id string, args ...string) (string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	full := append(r.base(id), args...)
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
func (r *runscDriver) runDetached(id, logPath string, args ...string) error {
	full := append(r.base(id), args...)
	cmd := exec.Command(r.bin, full...)

	// stdin is always /dev/null (the workload has no interactive input).
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer devnull.Close()
	cmd.Stdin = devnull

	// stdout/stderr: by default /dev/null (the workload console is discarded;
	// only gVisor --debug-log FILES are surfaced). When console streaming is on,
	// point them at a per-sandbox console FILE (never a pipe — a detached sandbox
	// keeps these fds open, so a pipe would block cmd.Run() forever; a real *os.File
	// fd does not, exactly like /dev/null). A background tailer then relays the
	// file to sandboxd stdout. logPath is the console file for this run.
	cmd.Stdout, cmd.Stderr = devnull, devnull
	if r.streamConsole && logPath != "" {
		console, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer console.Close()
		cmd.Stdout, cmd.Stderr = console, console
		go r.tailConsoleToStdout(id, logPath)
	}

	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Run(); err != nil {
		// on failure, the useful detail is in the --debug-log; include a tail
		return fmt.Errorf("%v; gvisor logs: %s", err, r.tailGvisorLogs(4096))
	}
	return nil
}

// sanitizeConsole strips C0 control bytes and DEL (0x7f) from a console line,
// EXCEPT tab (\t) and carriage return (\r) which are harmless. Newlines are
// already consumed as line delimiters upstream. This defeats terminal escape
// sequences (ESC=0x1b drives cursor moves, color, title-set, clear-screen) a
// malicious workload could otherwise inject into `kubectl logs`. Bytes >= 0x80
// are passed through untouched so legitimate UTF-8 multibyte text survives.
func sanitizeConsole(b []byte) string {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c < 0x20 && c != '\t' && c != '\r' {
			continue // C0 control (incl. ESC 0x1b) — drop
		}
		if c == 0x7f {
			continue // DEL — drop
		}
		out = append(out, c)
	}
	return string(out)
}

// tailConsoleToStdout relays a per-sandbox console file to sandboxd stdout as
// `[sandbox <id>] …` lines so the nested workload's own output shows up in
// `kubectl logs` on the worker pod. It runs in a background goroutine, fully
// decoupled from the exec'd runsc process.
//
// The console is UNTRUSTED (attacker-controlled). Mitigations here:
//   - unambiguous per-line prefix `[sandbox <id>]` that is NOT sandboxd's
//     structured-log schema, so a workload can't forge sandboxd log lines;
//   - control-char/ANSI stripping (see controlChars) against terminal escapes;
//   - a byte cap (SANDBOXD_STREAM_CONSOLE_MAX_BYTES, default 8 MiB) so a
//     log-bomb workload (Chrome is chatty) can't run the node out of log
//     space/cost — past the cap we stop relaying and print one truncation note.
//
// The multi-tenant concern (a worker hosts many tenants' sessions over its
// lifetime, so this shared log plane mixes tenants under one pod identity) is
// inherent and cannot be fixed here — it's why this is opt-in and the
// session-scoped /logs API stays the production path.
func (r *runscDriver) tailConsoleToStdout(id, path string) {
	prefix := "[sandbox " + id + "] "
	maxBytes := envInt64("SANDBOXD_STREAM_CONSOLE_MAX_BYTES", 8<<20)

	var f *os.File
	// The console file is created by runDetached just before the child starts;
	// wait briefly for it to appear.
	for i := 0; i < 25; i++ {
		if fh, err := os.Open(path); err == nil {
			f = fh
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if f == nil {
		return
	}
	defer f.Close()

	var relayed int64
	var truncated bool
	buf := make([]byte, 0, 4096)
	rd := make([]byte, 4096)
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()
	for range t.C {
		for {
			n, err := f.Read(rd)
			if n > 0 && !truncated {
				buf = append(buf, rd[:n]...)
				// emit complete lines; hold any partial trailing line for next read
				for {
					i := bytes.IndexByte(buf, '\n')
					if i < 0 {
						break
					}
					line := buf[:i]
					buf = buf[i+1:]
					clean := sanitizeConsole(line)
					if relayed+int64(len(clean)) > maxBytes {
						truncated = true
						fmt.Fprintf(os.Stdout, "%s<console truncated: exceeded %d bytes>\n", prefix, maxBytes)
						break
					}
					relayed += int64(len(clean))
					fmt.Fprintf(os.Stdout, "%s%s\n", prefix, clean)
				}
			}
			if err == io.EOF || n == 0 {
				break
			}
			if err != nil {
				return
			}
		}
		// Exit once the sandbox is gone: its console file was unlinked (by
		// delete/cleanupArtifacts) and we've drained what we held open. Without
		// this the per-run goroutine would spin forever after teardown.
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return
		}
	}
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
// createStart ignores ports: the gVisor network (veth + DNAT) is built by the
// handler's setupSandboxNet BEFORE this call, so runsc joins the ready interior
// netns via the spec's netnsPath. (buildsOwnNetwork() returns false.)
func (r *runscDriver) createStart(id, bundle string, _ []portMap, _ ateomnet.BandwidthConfig) error {
	pid := filepath.Join(bundle, id+".pid")
	log := filepath.Join(bundle, id+".run.log")
	return r.runDetached(id, log, "run", "-bundle", bundle, "-pid-file", pid, "-detach", id)
}

// Checkpoint the sandbox to imageDir (single atomic mem+fs image via overlay).
// When compress is true, the pages file is compressed (flate-best-speed): ~6.5x
// smaller on disk/S3 at the cost of foreground (eager) page-load on restore
// (compressed images can't use restore -background). Also drops
// committed zero-filled pages (cheap, helps sparse/fresh memory).
func (r *runscDriver) checkpoint(id, imageDir string, leaveRunning, compress bool) error {
	os.MkdirAll(imageDir, 0o755)
	args := []string{"checkpoint", "-image-path", imageDir}
	if leaveRunning {
		args = append(args, "--leave-running")
	}
	if compress {
		args = append(args, "-compression=flate-best-speed", "-exclude-committed-zero-pages")
	}
	args = append(args, id)
	if _, se, err := r.run(id, args...); err != nil {
		return fmt.Errorf("checkpoint: %v: %s", err, se)
	}
	return nil
}

// Restore: `runsc restore` alone establishes the sandbox AND restores into it in
// one step (like `run` does for a fresh start). A separate preceding `create`
// boots a DIFFERENT sandbox that waits for `start` — its watchdog logs
// "Watchdog.Start() not called within 30s" and the container hangs at "created"
// because the restore never targets it. So: NO separate create; restore -detach.
func (r *runscDriver) restore(id, bundle, imageDir string, _ []portMap, _ ateomnet.BandwidthConfig) error {
	pid := filepath.Join(bundle, id+".pid")
	log := filepath.Join(bundle, id+".restore.log")
	return r.runDetached(id, log, "restore", "-bundle", bundle, "-image-path", imageDir,
		"-pid-file", pid, "-detach", id)
}

// buildsOwnNetwork: gVisor's veth/DNAT is built by the handler's setupSandboxNet
// before createStart/restore, so the handler owns networking here.
func (r *runscDriver) buildsOwnNetwork() bool { return false }

// State returns the container status ("running", "stopped", ...).
func (r *runscDriver) state(id string) (string, error) {
	out, se, err := r.run(id, "state", id)
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
	r.run(id, "state", id) // best-effort state sync before delete (substrate pattern)
	_, se, err := r.run(id, "delete", "-force", id)
	os.RemoveAll(r.overlayDir(id)) // drop the per-sandbox overlay upper dir
	if err != nil {
		return fmt.Errorf("delete: %v: %s", err, se)
	}
	if d := time.Since(t0); d > time.Second {
		fmt.Fprintf(os.Stderr, "[runsc] delete %s took %s\n", id, d)
	}
	return nil
}

// version returns the pinned runsc version string (recorded in snapshot manifests
// so restores can refuse a version mismatch). It is resolved once at startup and
// cached (the binary is immutable for the process lifetime).
func (r *runscDriver) version() string { return r.ver }

// runtimeName identifies this engine family for the cross-runtime restore guard.
func (r *runscDriver) runtimeName() string { return "gvisor" }

// networkMode reports the runsc --network mode ("host"|"sandbox"|"none"); only
// "sandbox" (netstack) supports the checkpointable veth/interior-netns data path.
func (r *runscDriver) networkMode() string { return r.network }

// streamLogsToStdout / recentLogs satisfy the runtimeDriver interface with the
// gVisor-specific implementations (sentry/gofer debug logs).
func (r *runscDriver) streamLogsToStdout()         { r.tailToStdout() }
func (r *runscDriver) recentLogs(max int64) string { return r.tailGvisorLogs(max) }

// runscDriver satisfies runtimeDriver.
var _ runtimeDriver = (*runscDriver)(nil)

// resolveVersion execs `runsc --version` once. On error it logs and returns "" —
// the caller (newRunsc) warns that the version-guard is disabled rather than
// silently persisting an empty version.
func (r *runscDriver) resolveVersion() string {
	cmd := exec.Command(r.bin, "--version")
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	if err := cmd.Run(); err != nil {
		log.Printf("WARN: runsc --version failed: %v (stderr: %s)", err, strings.TrimSpace(e.String()))
		return ""
	}
	return o.String()
}
