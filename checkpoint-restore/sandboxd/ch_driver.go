package main

// chDriver is the Cloud Hypervisor microVM implementation of runtimeDriver
// (Phase 1b of docs/sandboxd/PRD/PRD-microvm-runtime-cloud-hypervisor.md). It
// drives a per-sandbox cloud-hypervisor VMM over its REST api-socket (the ch
// package, ported from Agent Substrate's cmd/ateom-microvm/internal/ch) and — in
// later slices — a virtiofsd rootfs + the kata-agent in-guest.
//
// STATUS (this slice): the CH REST client (ch/) is ported and unit-tested, and
// this driver satisfies the runtimeDriver interface so SANDBOXD_RUNTIME=microvm
// constructs a real engine and reports runtime="microvm". The boot/checkpoint/
// restore verbs are NOT yet wired — they need the virtiofs rootfs assembly +
// kata-agent + microvm networking port (next slices) — and return a clear
// not-implemented error rather than pretending to work. gVisor is unaffected.
//
// The KVM + CH + virtiofs-rootfs prerequisites are LIVE-VALIDATED on a nested-virt
// c7i node (see [[sandboxd-microvm-nested-virt]] / examples/microvm/): /dev/kvm
// present, CH boots a guest, virtiofsd serves a container rootfs as guest /.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/jicowan/aio-sandbox/sandboxd/ateomnet"
	"github.com/jicowan/aio-sandbox/sandboxd/ch"
	"github.com/jicowan/aio-sandbox/sandboxd/kata"
	"github.com/vishvananda/netns"
)

// runOnce execs a command and returns trimmed stdout (best-effort; errors bubble).
func runOnce(bin string, args ...string) (string, error) {
	var o, e bytes.Buffer
	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = &o, &e
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %v: %v (%s)", bin, args, err, bytes.TrimSpace(e.Bytes()))
	}
	return string(bytes.TrimSpace(o.Bytes())), nil
}

// chVM tracks one running microVM sandbox on this worker.
type chVM struct {
	id        string
	apiSocket string            // cloud-hypervisor REST api-socket for this sandbox
	chCmd     *exec.Cmd         // the cloud-hypervisor VMM process we own
	vfsdCmd   *exec.Cmd         // the virtiofsd process serving the rootfs RO lower
	agent     *kata.AgentClient // open kata-agent ttrpc client (closed on delete)

	// restoreSourceDir/snapshotSelfContained drive the suspend/resume merge chain
	// (see microvm_checkpoint.go). A cold-booted VM has neither set. A restored VM
	// records where CH is demand-paging from (restoreSourceDir) and whether that
	// restore was eager (self-contained: the next snapshot is already complete, no
	// merge) or OnDemand (the next snapshot is a sparse delta to overlay onto the
	// source). CH v53 prefaults OnDemand, so restores are eager today; the field
	// keeps the merge path correct for a future non-prefaulting CH.
	restoreSourceDir      string
	snapshotSelfContained bool

	// baseID is the FROZEN base id the guest's virtio-fs find-paths are pinned to
	// (see chBaseIDFile). A cold boot sets it to the sandbox's own id; a restore
	// carries the base id forward from the snapshot, so a fork's later checkpoint
	// still records the golden base — the guest never re-pins find-paths on restore.
	baseID string

	// stopOOM signals the per-VM OOM-watcher goroutine to exit (closed on delete).
	stopOOM chan struct{}
}

// chDriver implements runtimeDriver for Cloud Hypervisor microVMs.
type chDriver struct {
	root      string // per-worker state root (VM dirs, sockets, snapshots)
	chBin     string // cloud-hypervisor binary path
	virtiofsd string // virtiofsd binary path
	kernel    string // guest kernel (virtiofs-enabled, e.g. kata vmlinux.container)
	ver       string // resolved cloud-hypervisor version (recorded in snapshots)
	network   string // data-path mode; microVM path is "sandbox" (see microvm_net.go)

	// streamConsole relays each VM's serial console to worker stdout (kubectl logs)
	// when set (SANDBOXD_STREAM_CONSOLE=1, via streamLogsToStdout). Opt-in per pool.
	streamConsole bool

	// interiorNetNS is the persistent per-worker netns that hosts each sandbox's
	// veth peer (the actor eth0); the tap that CH's virtio-net attaches to is
	// cross-connected to that eth0 (see microvm_net.go). Created once at newCH.
	interiorNetNS netns.NsHandle

	mu  sync.Mutex
	vms map[string]*chVM
}

// newCH constructs the Cloud Hypervisor driver. Binaries/kernel come from the
// worker image (bundled like runsc is today) via SANDBOXD_CH_* env, defaulting to
// the conventional install paths.
func newCH(root string) *chDriver {
	os.MkdirAll(root, 0o755)
	d := &chDriver{
		root:      root,
		chBin:     envOr("SANDBOXD_CH_BIN", "/usr/local/bin/cloud-hypervisor"),
		virtiofsd: envOr("SANDBOXD_VIRTIOFSD_BIN", "/usr/local/bin/virtiofsd"),
		kernel:    envOr("SANDBOXD_CH_KERNEL", "/usr/local/share/kata/vmlinux.container"),
		network:   envOr("SANDBOXD_NETWORK", "sandbox"),
		vms:       map[string]*chVM{},
	}
	d.ver = d.resolveVersion()
	// Create the persistent interior netns for the sandbox veth peers (idempotent
	// by name). Best-effort at construction: a failure only disables the sandbox
	// data path (createStart will error clearly), it must not crash a worker whose
	// SANDBOXD_RUNTIME=microvm but which is only, say, answering /healthz.
	if ns, err := ateomnet.CreateNetNSWithoutSwitching(microvmInteriorNetNSName); err != nil {
		log.Printf("WARN: microvm interior netns setup failed (no sandbox networking): %v", err)
	} else {
		d.interiorNetNS = ns
	}
	return d
}

// microvmInteriorNetNSName is the persistent per-worker interior netns for microVM
// sandbox veth peers (one worker hosts one sandbox today, matching gVisor's
// single interior netns).
const microvmInteriorNetNSName = "sbx-microvm"

// resolveVersion runs `cloud-hypervisor --version` once (best-effort). An empty
// result disables the checkpoint/restore engine-version guard, same as runsc.
func (d *chDriver) resolveVersion() string {
	out, err := runOnce(d.chBin, "--version")
	if err != nil {
		// The binary may be absent in a non-microVM worker image; don't crash the
		// process here (newRuntimeDriver only selects CH when SANDBOXD_RUNTIME=microvm).
		return ""
	}
	return out
}

// vmDir is the per-sandbox state directory (VM sockets, snapshot staging).
func (d *chDriver) vmDir(id string) string { return filepath.Join(d.root, "vm", id) }

// chLog opens (truncating) a cloud-hypervisor log file under the sandbox's VMDir
// so the VMM's own stdout/stderr — its warnings and the reason it exits — are
// captured for diagnostics instead of discarded. Best-effort: a nil writer just
// means CH output goes nowhere, exactly as before, so a failure here never blocks
// a boot/restore. The file lives under kata.VMDir(id) alongside serial.log.
func chLog(id string) *os.File {
	f, err := os.OpenFile(filepath.Join(kata.VMDir(id), "clh.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil
	}
	return f
}

// --- runtimeDriver interface (createStart is in microvm_boot.go,
// checkpoint/restore are in microvm_checkpoint.go) ---

// state returns the CH VM state mapped to sandboxd's vocabulary. An absent sandbox
// → "stopped" (matches how the supervisor treats a gone sandbox). A tracked one is
// queried via the CH api-socket: Running/Paused → "running", else "stopped".
func (d *chDriver) state(id string) (string, error) {
	d.mu.Lock()
	vm, ok := d.vms[id]
	d.mu.Unlock()
	if !ok {
		return "stopped", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := ch.NewClient(vm.apiSocket).State(ctx)
	if err != nil {
		return "stopped", nil // VMM gone/unreachable → treat as stopped
	}
	switch st {
	case "Running", "Paused":
		return "running", nil
	default:
		return "stopped", nil
	}
}

// watchOOM runs the per-VM OOM-event watcher: it loops on the kata-agent's blocking
// GetOOMEvent and logs + counts each guest-cgroup OOM. This is observability parity
// with the gVisor path (which scans the HOST cgroup in supervisor.checkOne) — a
// microVM workload OOMs INSIDE the guest, invisible to the host cgroup, so the only
// way to see it is to ask the in-guest agent. The goroutine exits when the agent
// connection closes (VM torn down) or delete() closes stop. Best-effort diagnostics:
// it never affects sandbox lifecycle, matching the gVisor OOM scan.
func (d *chDriver) watchOOM(id string, agent *kata.AgentClient, stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		cid, err := agent.GetOOMEvent(context.Background())
		if err != nil {
			// Agent gone (VM torn down) or ctx done → end the watcher. Not logged as an
			// error: a normal teardown closes the connection, which lands here.
			return
		}
		log.Printf("microvm %s: guest OOM-kill in container %q (workload exceeded guest memory)", id, cid)
		metrics.inc("sandbox_oom_kills")
	}
}

// delete tears down the microVM sandbox: close the agent, kill the VMM +
// virtiofsd, clean per-sandbox host state, and free the slot. Best-effort and
// idempotent.
func (d *chDriver) delete(id string) error {
	d.mu.Lock()
	vm := d.vms[id]
	delete(d.vms, id)
	d.mu.Unlock()
	if vm != nil {
		if vm.stopOOM != nil {
			close(vm.stopOOM) // stop the OOM-watcher goroutine
		}
		if vm.agent != nil {
			_ = vm.agent.Close()
		}
		if vm.chCmd != nil && vm.chCmd.Process != nil {
			_ = vm.chCmd.Process.Kill()
			_, _ = vm.chCmd.Process.Wait()
		}
		if vm.vfsdCmd != nil && vm.vfsdCmd.Process != nil {
			_ = vm.vfsdCmd.Process.Kill()
			_, _ = vm.vfsdCmd.Process.Wait()
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	kata.CleanupSandboxState(ctx, id)
	cancel()
	// Host-merged rootfs teardown: CleanupSandboxState's sweep of SharedDir(id) already
	// unmounts a cold boot's merged overlay (id==baseID); a fork's merge lives under
	// baseID, so drop that explicitly. Then remove the host upper dir on /work —
	// CleanupSandboxState only touches /run, and the upper is deliberately on disk.
	if vm != nil && vm.baseID != "" && vm.baseID != id {
		kata.UnmountMergedRootfs(vm.baseID, vm.baseID)
	}
	removeRootfsUpperDir(id)
	// Bandwidth shaping's IFB device outlives the veth (CleanupActorNetwork only drops
	// ateom0 + its qdiscs), so clear it explicitly. Best-effort/idempotent.
	ateomnet.ClearBandwidth()
	os.RemoveAll(d.vmDir(id))
	return nil
}

// buildsOwnNetwork: the microVM driver constructs its own veth + interior netns
// (via ateomnet) inside createStart/restore, and installs the inbound DNAT +
// credential-vendor routing itself (setupInboundPorts), so the handler must NOT
// call setupSandboxNet (which would build a conflicting gVisor veth/netns).
func (d *chDriver) buildsOwnNetwork() bool { return true }

func (d *chDriver) runtimeName() string { return "microvm" }
func (d *chDriver) version() string     { return d.ver }
func (d *chDriver) networkMode() string { return d.network }

// streamLogsToStdout arms per-workload log streaming when SANDBOXD_STREAM_CONSOLE=1
// (opt-in per pool, like gVisor — the workload output is attacker-controlled and
// multi-tenant over a worker's lifetime). NOTE the distinction from the VM's serial
// console (kernel + kata-agent, captured to VMDir/serial.log): what we forward here
// is the WORKLOAD CONTAINER's own stdout/stderr, pulled from the kata-agent via
// ReadStdout/ReadStderr for exec id <id>_ovl (see forwardWorkloadLogs) — the actual
// process output a user expects in `kubectl logs`, not the guest boot console. The
// forwarder goroutines are started per VM at boot/restore; this only records the flag.
func (d *chDriver) streamLogsToStdout() { d.streamConsole = true }

// recentLogs returns the tail of the (single) running VM's serial console for the
// /logs endpoint, up to maxBytes. One sandbox per worker today, so return the first.
// This is the VM console (a coarse fallback); the live workload stdout/stderr is
// streamed separately via forwardWorkloadLogs.
func (d *chDriver) recentLogs(maxBytes int64) string {
	d.mu.Lock()
	var id string
	for k := range d.vms {
		id = k
		break
	}
	d.mu.Unlock()
	if id == "" {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(kata.VMDir(id), "serial.log"))
	if err != nil {
		return ""
	}
	if int64(len(b)) > maxBytes {
		b = b[int64(len(b))-maxBytes:]
	}
	return string(b)
}

// overlayWorkloadExecID is the container/exec id of the workload container a LEGACY
// (guest-overlay) microVM sandbox runs — id+"_ovl". Host-merged sandboxes run the
// workload as id itself (StartRootfsContainer). kata sets ExecId==ContainerId.
func overlayWorkloadExecID(id string) string { return id + "_ovl" }

// forwardWorkloadLogs relays the workload container's stdout AND stderr to the
// worker's stdout as prefixed, sanitized, byte-capped lines, by pumping the
// kata-agent's ReadStdout/ReadStderr for the workload's exec id (via NewStdioReader).
// execID is the workload container id: `id` for host-merged sandboxes, `id+"_ovl"`
// for legacy guest-overlay ones. Started per VM at boot/restore only when
// streamConsole is set; each stream's goroutine ends when the agent signals
// container-exit / the connection closes (StreamReader returns io.EOF), so they never
// outlive the VM. ac is the open agent client tracked on the chVM.
func (d *chDriver) forwardWorkloadLogs(id, execID string, ac *kata.AgentClient) {
	for _, stderr := range []bool{false, true} {
		go d.pumpStream(id, kata.NewStdioReader(context.Background(), ac, execID, execID, stderr), stderr)
	}
}

// pumpStream reads a workload stdio stream to EOF, emitting complete sanitized lines
// to worker stdout under a byte cap (mirrors runscDriver.tailConsoleToStdout).
func (d *chDriver) pumpStream(id string, r io.Reader, stderr bool) {
	stream := "stdout"
	if stderr {
		stream = "stderr"
	}
	prefix := "[sandbox " + id + " " + stream + "] "
	maxBytes := envInt64("SANDBOXD_STREAM_CONSOLE_MAX_BYTES", 8<<20)

	var relayed int64
	buf := make([]byte, 0, 4096)
	rd := make([]byte, 4096)
	for {
		n, err := r.Read(rd)
		if n > 0 {
			buf = append(buf, rd[:n]...)
			for {
				i := bytes.IndexByte(buf, '\n')
				if i < 0 {
					break
				}
				line := buf[:i]
				buf = buf[i+1:]
				clean := sanitizeConsole(line)
				if relayed+int64(len(clean)) > maxBytes {
					fmt.Fprintf(os.Stdout, "%s<truncated: exceeded %d bytes>\n", prefix, maxBytes)
					return
				}
				relayed += int64(len(clean))
				fmt.Fprintf(os.Stdout, "%s%s\n", prefix, clean)
			}
		}
		if err != nil { // io.EOF on container exit / connection close
			return
		}
	}
}

// chDriver satisfies runtimeDriver.
var _ runtimeDriver = (*chDriver)(nil)
