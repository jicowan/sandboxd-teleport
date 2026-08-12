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
}

// chDriver implements runtimeDriver for Cloud Hypervisor microVMs.
type chDriver struct {
	root      string // per-worker state root (VM dirs, sockets, snapshots)
	chBin     string // cloud-hypervisor binary path
	virtiofsd string // virtiofsd binary path
	kernel    string // guest kernel (virtiofs-enabled, e.g. kata vmlinux.container)
	ver       string // resolved cloud-hypervisor version (recorded in snapshots)
	network   string // data-path mode; microVM path is "sandbox" (see microvm_net.go)

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

// delete tears down the microVM sandbox: close the agent, kill the VMM +
// virtiofsd, clean per-sandbox host state, and free the slot. Best-effort and
// idempotent.
func (d *chDriver) delete(id string) error {
	d.mu.Lock()
	vm := d.vms[id]
	delete(d.vms, id)
	d.mu.Unlock()
	if vm != nil {
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
	os.RemoveAll(d.vmDir(id))
	return nil
}

func (d *chDriver) runtimeName() string { return "microvm" }
func (d *chDriver) version() string     { return d.ver }
func (d *chDriver) networkMode() string { return d.network }

// streamLogsToStdout / recentLogs: CH/virtiofsd log surfacing lands with the boot
// path (later slice). No-ops for now so the interface is satisfied.
func (d *chDriver) streamLogsToStdout()         {}
func (d *chDriver) recentLogs(max int64) string { return "" }

// chDriver satisfies runtimeDriver.
var _ runtimeDriver = (*chDriver)(nil)
