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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
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
	apiSocket string // cloud-hypervisor REST api-socket for this sandbox
	// (later slices: virtiofsd cmd, kata-agent conn, tap fds, snapshot dir, ...)
}

// chDriver implements runtimeDriver for Cloud Hypervisor microVMs.
type chDriver struct {
	root      string // per-worker state root (VM dirs, sockets, snapshots)
	chBin     string // cloud-hypervisor binary path
	virtiofsd string // virtiofsd binary path
	kernel    string // guest kernel (virtiofs-enabled, e.g. kata vmlinux.container)
	ver       string // resolved cloud-hypervisor version (recorded in snapshots)
	network   string // data-path mode; microVM path is "sandbox" (see microvm_net.go, later)

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
	return d
}

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

// errNotWired is returned by verbs whose port lands in a later Phase 1b slice.
func errNotWired(verb string) error {
	return fmt.Errorf("microvm %s not yet implemented (Phase 1b: needs virtiofs rootfs + kata-agent + microvm networking port); the CH REST client + node KVM/CH/virtiofs prerequisites are validated — see PRD-microvm-runtime-cloud-hypervisor.md §6", verb)
}

// --- runtimeDriver interface ---

func (d *chDriver) createStart(id, bundle string) error { return errNotWired("createStart") }

func (d *chDriver) checkpoint(id, imageDir string, leaveRunning, compress bool) error {
	return errNotWired("checkpoint")
}

func (d *chDriver) restore(id, bundle, imageDir string) error { return errNotWired("restore") }

// state returns the CH VM state mapped to sandboxd's vocabulary. Unknown/absent
// sandbox → "stopped" (matches how the supervisor treats a gone sandbox).
func (d *chDriver) state(id string) (string, error) {
	d.mu.Lock()
	_, ok := d.vms[id]
	d.mu.Unlock()
	if !ok {
		return "stopped", nil
	}
	// Later slice: query ch.Client.State() and map Running->running, Paused->paused.
	return "stopped", nil
}

// delete tears down the microVM + its VMM/virtiofsd and frees the slot. With no
// VMs wired yet this is a no-op cleanup of any per-sandbox dir.
func (d *chDriver) delete(id string) error {
	d.mu.Lock()
	delete(d.vms, id)
	d.mu.Unlock()
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
