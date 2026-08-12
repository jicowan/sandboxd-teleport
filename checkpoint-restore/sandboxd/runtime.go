package main

import (
	"log"
	"path/filepath"
)

// runtimeDriver is the sandbox-runtime seam. The worker's HTTP handlers, the
// supervisor, and state reconciliation talk ONLY to this interface — never to a
// concrete engine — so a second runtime (Cloud Hypervisor microVMs; see
// docs/sandboxd/PRD/PRD-microvm-runtime-cloud-hypervisor.md) drops in as a second
// implementation without touching the control paths. runscDriver is the gVisor
// implementation today.
//
// The interface is intentionally NARROW: only the verbs the control paths need
// cross it. Engine-specific mechanics (runsc's overlay/bundle flags, gVisor
// debug-log formats, CH's api-socket, etc.) stay INSIDE the implementation.
//
// It has unexported methods because every implementation lives in this package
// (the worker is a single flat package main); this also "seals" the interface so
// only same-package engines can satisfy it.
type runtimeDriver interface {
	// createStart boots a fresh workload from a prepared OCI bundle, detached.
	createStart(id, bundle string) error
	// checkpoint writes an atomic snapshot of the sandbox into imageDir. When
	// leaveRunning is set the sandbox keeps running (periodic checkpoint); compress
	// trades restore speed for on-disk/S3 size.
	checkpoint(id, imageDir string, leaveRunning, compress bool) error
	// restore establishes AND resumes the sandbox from imageDir in one step.
	restore(id, bundle, imageDir string) error
	// state returns the container status ("running"|"stopped"|...).
	state(id string) (string, error)
	// delete tears the sandbox down fast and robustly (frees the worker slot).
	delete(id string) error

	// runtimeName identifies the engine family ("gvisor"|"microvm"). It is recorded
	// in snapshot metadata and enforced by the restore guard: a snapshot from a
	// different runtime can NEVER be restored here (a checkpoint is not portable
	// across runtimes), independent of engine version.
	runtimeName() string
	// version is the pinned engine version string (e.g. runsc's), recorded in
	// snapshot metadata so a restore can refuse an incompatible engine build.
	version() string
	// networkMode reports the data-path mode ("host"|"sandbox"|"none"). Only
	// "sandbox" supports the checkpointable veth/interior-netns path.
	networkMode() string

	// streamLogsToStdout runs a background goroutine that relays the engine's own
	// diagnostic logs to the worker's stdout (→ kubectl logs). Started once at boot.
	streamLogsToStdout()
	// recentLogs returns up to maxBytes of the engine's recent diagnostic logs, for
	// the /logs endpoint.
	recentLogs(maxBytes int64) string
}

// newRuntimeDriver selects the sandbox runtime from SANDBOXD_RUNTIME (default
// "gvisor"). The runtime is a per-worker (thus per-pool) property: a worker image
// carries exactly one engine, and a pool is single-runtime by construction because
// a session only ever teleports within its own pool. microVM is not yet wired
// (Phase 1) — selecting it fails loudly rather than silently degrading.
func newRuntimeDriver(runscBin, root string) runtimeDriver {
	switch rt := envOr("SANDBOXD_RUNTIME", "gvisor"); rt {
	case "gvisor":
		return newRunsc(runscBin, root)
	case "microvm":
		// Cloud Hypervisor microVM engine (Phase 1b). The CH REST client + node
		// KVM/CH/virtiofs prerequisites are validated; boot/checkpoint/restore verbs
		// land in later slices (chDriver returns a clear not-implemented error until
		// then). Uses the same state root as gVisor would.
		return newCH(root)
	default:
		log.Fatalf("unknown SANDBOXD_RUNTIME=%q (want gvisor|microvm)", rt)
		return nil
	}
}

// runtimeStateRoot is the driver's state-root path under the worker workdir. Kept
// here so the selection site in main.go stays a one-liner.
func runtimeStateRoot(work string) string { return filepath.Join(work, "rt") }

// restoreGuardVerdict is the result of checkRestoreCompat: ok, or the reason a
// restore must be refused (mapped to a 409 by the handler).
type restoreGuardVerdict struct {
	OK   bool
	Kind string // "" | "runtime mismatch" | "engine version mismatch"
	Want string
	Have string
}

// checkRestoreCompat decides whether a snapshot may be restored on THIS worker.
// It generalizes the old runsc-only version check to a {runtime, version} pair:
//
//  1. A CROSS-RUNTIME restore is refused unconditionally (a checkpoint produced by
//     one engine is not loadable by another), independent of version — this is what
//     makes it impossible to restore e.g. a gVisor snapshot on a microVM worker.
//  2. Within the same runtime, an engine-VERSION mismatch is refused (gVisor
//     hard-errors on a mismatched restore; CH snapshots are also version-pinned).
//
// wantEngineVer is the caller's engineVersion; wantRunscVer is the back-compat
// alias older callers/snapshots send (used only when engineVersion is empty). An
// empty wantRuntime is treated as compatible (older callers omit it) so this stays
// backward compatible; an empty resolved version disables the version check (as
// today, when `runsc --version` returned nothing).
func checkRestoreCompat(wantRuntime, wantEngineVer, wantRunscVer, haveRuntime, haveVer string) restoreGuardVerdict {
	if wantRuntime != "" && wantRuntime != haveRuntime {
		return restoreGuardVerdict{Kind: "runtime mismatch", Want: wantRuntime, Have: haveRuntime}
	}
	wantVer := wantEngineVer
	if wantVer == "" {
		wantVer = wantRunscVer
	}
	if wantVer != "" && wantVer != haveVer {
		return restoreGuardVerdict{Kind: "engine version mismatch", Want: wantVer, Have: haveVer}
	}
	return restoreGuardVerdict{OK: true}
}
