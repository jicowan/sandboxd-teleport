//go:build linux

// microVM checkpoint/restore: chDriver.checkpoint and chDriver.restore — the
// suspend/resume half of the Cloud Hypervisor runtime. checkpoint pauses the
// guest and drives the CH REST api-socket to write a snapshot into imageDir;
// restore relaunches a bare VMM from that snapshot (with fresh fd-backed net
// devices) and resumes the guest.
//
// PROVENANCE: the sequence and helper bodies (snapshot pause/snapshot/teardown,
// restoreFullScope, rewriteSnapshotSocketPaths, restoreMemMode) are ported from
// Agent Substrate's cmd/ateom-microvm (checkpoint.go/restore.go, Apache-2.0,
// Copyright 2026 Google LLC) and adapted to sandboxd's model: ONE container per
// sandbox, no durable-dir volumes, and a FLAT imageDir (the /checkpoint + /restore
// handlers in main.go upload/download top-level files only, and seed their own
// OCI config.json), so the CH snapshot files are namespaced (clh-*) to coexist
// with the handler's config.json and are staged flat rather than in a subdir.
//
// Licensed under the Apache License, Version 2.0.

package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jicowan/aio-sandbox/sandboxd/ateomnet"
	"github.com/jicowan/aio-sandbox/sandboxd/ch"
	"github.com/jicowan/aio-sandbox/sandboxd/kata"
)

// entropyReseedBytes is how many fresh random bytes we inject into the guest CRNG on
// restore. 32 bytes (256 bits) is plenty to reseed — the kernel mixes it into the
// pool; the point is uniqueness per clone, not volume.
const entropyReseedBytes = 32

// reseedGuestEntropy reads fresh host entropy and injects it into the restored
// guest's CRNG via the kata-agent, forking each clone's randomness apart (see the
// VMGenID note at the call site).
func reseedGuestEntropy(ctx context.Context, ac *kata.AgentClient) error {
	seed := make([]byte, entropyReseedBytes)
	if _, err := rand.Read(seed); err != nil {
		return fmt.Errorf("reading host entropy: %w", err)
	}
	return ac.ReseedRandomDev(ctx, seed)
}

// chSnapshotFiles are the CH snapshot artifacts, namespaced with a clh- prefix so
// they sit flat in imageDir alongside the /checkpoint handler's own config.json
// (the OCI spec) without colliding with CH's config.json (the VM config). CH
// writes config.json/state.json/memory-ranges into the destination dir; we stage
// it into a private subdir, then move those three out under the clh- names.
const (
	chStageSubdir      = "clh-snap"      // private staging dir under imageDir for CH's raw output
	chConfigFile       = "clh-config.json"
	chStateFile        = "clh-state.json"
	chMemoryRangesFile = "clh-memory-ranges"
	// chBaseIDFile records the FROZEN base id — the sandbox id whose paths the guest's
	// virtio-fs find-paths are pinned to (SharedDir(baseID)/<baseID>/rootfs, frozen at
	// snapshot time). For a cold-booted sandbox this is its own id; a FORK restored
	// under a NEW id must reconstruct the RO lower at this frozen path, not the new id,
	// or virtiofsd find-paths can't reopen the guest's inodes (vm.restore 500). It
	// propagates across a fork lineage (a fork's own checkpoint keeps the golden base
	// id). This is the sandboxd analog of substrate's baseIDFile.
	chBaseIDFile = "clh-baseid"
)

// checkpointTimeout bounds pause+snapshot+teardown so a wedged VMM can't hang
// /checkpoint or /suspend forever.
const checkpointTimeout = 120 * time.Second

// restoreTimeout bounds the whole restore. It is much larger than the cold-boot
// bootTimeout because an eager (Copy) vm.restore reads the ENTIRE memory image
// off disk — and S3 does not preserve sparseness, so a snapshot downloads DENSE
// (a 2GiB guest = 2GiB read) even when its working set was small. CH's restore of
// a multi-GiB image legitimately runs past the 120s boot budget; capping it there
// kills a restore that would otherwise have succeeded.
const restoreTimeout = 10 * time.Minute

// checkpoint pauses the running microVM and writes a portable snapshot into
// imageDir: the CH snapshot (VM config + device state + guest memory) namespaced
// as clh-*. The guest's container rootfs is overlay(virtio-fs RO lower +
// guest-tmpfs upper), so rootfs writes live in guest RAM and are captured by the
// memory image; the RO lower is reconstructed from the OCI image on restore, so
// nothing rootfs-related ships.
//
// leaveRunning keeps the guest running after the snapshot (periodic checkpoint);
// otherwise the VMM is torn down and the worker slot freed by the caller's delete.
// compress is accepted for interface parity but not honored: CH memory-ranges are
// already sparse, and CH has no snapshot-compression knob.
func (d *chDriver) checkpoint(id, imageDir string, leaveRunning, compress bool) (retErr error) {
	d.mu.Lock()
	vm := d.vms[id]
	d.mu.Unlock()
	if vm == nil {
		return fmt.Errorf("microvm checkpoint: unknown sandbox %q", id)
	}
	ctx, cancel := context.WithTimeout(context.Background(), checkpointTimeout)
	defer cancel()

	client := ch.NewClient(vm.apiSocket)
	if _, err := client.WaitReady(ctx, 10*time.Second); err != nil {
		return fmt.Errorf("microvm checkpoint: CH api-socket not ready: %w", err)
	}

	// Pause the guest to quiesce it, then snapshot. On any failure after the pause,
	// resume so a failed checkpoint leaves the sandbox running (matches gVisor,
	// where a failed checkpoint is non-destructive), unless we're tearing down.
	if err := client.Pause(ctx); err != nil {
		return fmt.Errorf("microvm checkpoint: pause: %w", err)
	}
	resumeOnErr := true
	defer func() {
		if retErr != nil && resumeOnErr {
			rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			_ = client.Resume(rctx)
			rcancel()
		}
	}()

	// CH writes config.json/state.json/memory-ranges into a dir it owns; stage into
	// a private subdir so its config.json can't clobber the handler's OCI config.json.
	stage := filepath.Join(imageDir, chStageSubdir)
	if err := os.RemoveAll(stage); err != nil {
		return fmt.Errorf("microvm checkpoint: clearing stage dir: %w", err)
	}
	if err := client.Snapshot(ctx, stage); err != nil {
		return fmt.Errorf("microvm checkpoint: snapshot: %w", err)
	}

	// Complete an OnDemand delta by overlaying it onto the source it was demand-paged
	// from (see chVM.restoreSourceDir). CH v53 prefaults OnDemand so restores are
	// eager (self-contained) and this is skipped — but keep it correct for a future
	// non-prefaulting CH where restores use OnDemand and snapshots are sparse deltas.
	if vm.restoreSourceDir != "" && !vm.snapshotSelfContained {
		base := filepath.Join(vm.restoreSourceDir, chMemoryRangesFile)
		delta := filepath.Join(stage, "memory-ranges")
		if err := ch.MergeDeltaIntoBase(ctx, base, delta); err != nil {
			return fmt.Errorf("microvm checkpoint: merge OnDemand delta: %w", err)
		}
	}

	// Move CH's three files out of the stage dir into imageDir under clh- names so
	// they upload flat (the handler's S3 uploadDir only walks top-level files).
	for _, m := range [][2]string{
		{"config.json", chConfigFile},
		{"state.json", chStateFile},
		{"memory-ranges", chMemoryRangesFile},
	} {
		if err := os.Rename(filepath.Join(stage, m[0]), filepath.Join(imageDir, m[1])); err != nil {
			return fmt.Errorf("microvm checkpoint: staging %s: %w", m[0], err)
		}
	}
	_ = os.RemoveAll(stage)

	// Record the frozen base id so a fork restored under a NEW id reconstructs the RO
	// lower at the path the guest's find-paths expect (see chBaseIDFile). Falls back
	// to the sandbox's own id if unknown (e.g. a checkpoint after an operator restart
	// lost in-memory state — the common cold-boot case where baseID == id anyway).
	baseID := vm.baseID
	if baseID == "" {
		baseID = id
	}
	if err := os.WriteFile(filepath.Join(imageDir, chBaseIDFile), []byte(baseID), 0o644); err != nil {
		return fmt.Errorf("microvm checkpoint: writing base id: %w", err)
	}

	if leaveRunning {
		resumeOnErr = false // resume unconditionally below
		if err := client.Resume(ctx); err != nil {
			return fmt.Errorf("microvm checkpoint: resume (leaveRunning): %w", err)
		}
	}
	// When !leaveRunning we leave the guest paused: the caller (/checkpoint) does
	// not tear the sandbox down, but /suspend calls delete() next, and a paused
	// guest is torn down the same way. gVisor's checkpoint likewise stops it.
	return nil
}

// restore relaunches a microVM from the snapshot in imageDir and resumes it. The
// bundle's rootfs was re-prepared by the /restore handler (prepareRootfsContainerd);
// we reconstruct the virtio-fs RO lower from it, repoint the snapshot's per-VMDir
// socket paths at this sandbox, rebuild the tap (the snapshot's virtio-net is
// fd-backed → fresh net_fds), relaunch CH with --restore, and resume. Guest RAM —
// process state, the tmpfs rootfs upper (so rootfs writes persist), and the frozen
// network config — comes back from the memory image.
func (d *chDriver) restore(id, bundle, imageDir string, ports []portMap) (retErr error) {
	if d.interiorNetNS == 0 {
		return fmt.Errorf("microvm restore: interior netns unavailable (networking disabled)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), restoreTimeout)
	defer cancel()

	rootfs := filepath.Join(bundle, "rootfs")

	// Read the FROZEN base id (see chBaseIDFile). The guest's virtio-fs find-paths are
	// pinned to SharedDir(baseID)/<baseID>/rootfs; for a same-id resume baseID == id,
	// but a FORK restores under a new id and MUST reconstruct the RO lower at the
	// golden base's path or find-paths can't reopen the guest's inodes. Fall back to
	// id (a pre-baseid snapshot, or a same-id resume).
	baseID := id
	if b, rerr := os.ReadFile(filepath.Join(imageDir, chBaseIDFile)); rerr == nil {
		if v := strings.TrimSpace(string(b)); v != "" {
			baseID = v
		}
	}

	// The snapshot was staged flat under clh- names; CH's vm.restore wants a dir
	// whose files are named exactly config.json/state.json/memory-ranges. Rebuild
	// that layout in a private restore dir (hardlink where possible — same fs — so
	// the large memory-ranges isn't copied; CH demand-pages/reads from it).
	restoreDir := filepath.Join(imageDir, "clh-restore")
	if err := os.RemoveAll(restoreDir); err != nil {
		return fmt.Errorf("microvm restore: clearing restore dir: %w", err)
	}
	if err := os.MkdirAll(restoreDir, 0o755); err != nil {
		return fmt.Errorf("microvm restore: restore dir: %w", err)
	}
	for _, m := range [][2]string{
		{chConfigFile, "config.json"},
		{chStateFile, "state.json"},
		{chMemoryRangesFile, "memory-ranges"},
	} {
		src := filepath.Join(imageDir, m[0])
		dst := filepath.Join(restoreDir, m[1])
		if err := os.Link(src, dst); err != nil {
			// Fall back to copy across a filesystem boundary (EXDEV) or if the file is
			// missing (a non-microVM snapshot would fail the restore guard upstream).
			if err := copyFile(src, dst); err != nil {
				return fmt.Errorf("microvm restore: staging %s: %w", m[0], err)
			}
		}
	}

	// Repoint the snapshot's per-VMDir socket/console paths at THIS sandbox's VMDir
	// (the golden/source pod's paths are stale here). The kernel + /dev/vda kata
	// image are static content-addressed files with identical paths on every node.
	if err := rewriteSnapshotSocketPaths(restoreDir, id); err != nil {
		return fmt.Errorf("microvm restore: rewrite socket paths: %w", err)
	}

	// Host networking: rebuild the per-sandbox veth into the interior netns (the guest
	// attaches to it via the tap). The microVM driver owns its networking, so — unlike
	// gVisor, where the handler's setupSandboxNet runs first — we also (re)install the
	// inbound DNAT + cred-vendor routing here, on the SAME worker so the restored
	// sandbox is reachable at the same podIP:hostPort. Clean up on failure.
	if err := ateomnet.SetupActorNetwork(ctx, ateomnet.NetworkConfig{
		InteriorNetNS:      d.interiorNetNS,
		HostVethHWAddr:     hostVethHWAddr,
		SweepInteriorLinks: true,
	}); err != nil {
		return fmt.Errorf("microvm restore: network setup: %w", err)
	}
	defer func() {
		if retErr != nil {
			cctx, ccancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
			_ = ateomnet.CleanupActorNetwork(cctx, d.interiorNetNS)
			ccancel()
		}
	}()
	if err := d.setupInboundPorts(ports); err != nil {
		return fmt.Errorf("microvm restore: inbound ports: %w", err)
	}

	// Clean stale per-sandbox state + create the VM runtime dir for the sockets CH
	// will reopen (vsock, serial, fs) at the paths rewritten above.
	kata.CleanupSandboxState(ctx, id)
	if err := os.MkdirAll(kata.VMDir(id), 0o700); err != nil {
		return fmt.Errorf("microvm restore: vm dir: %w", err)
	}

	// Reconstruct the overlay RO lower at the FROZEN base path (SharedDir(baseID)/
	// <baseID>/rootfs) — find-paths re-opens the guest's inodes by that exact path, so
	// a fork under a new id must still lay the lower where the golden guest froze it.
	// The writable upper is a guest tmpfs restored from the memory image. The virtiofsd
	// SOCKET stays per-VMDir(id) (matches rewriteSnapshotSocketPaths), but the SHARED
	// DIR it serves is the base's, so the guest's find-paths resolve.
	if err := kata.ReconstructSharedDirFromImage(ctx, rootfs, baseID, baseID); err != nil {
		return fmt.Errorf("microvm restore: stage rootfs: %w", err)
	}
	vfsdLog, _ := os.OpenFile(filepath.Join(kata.VMDir(id), "virtiofsd.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	vfsdCmd, err := kata.StartVirtiofsd(ctx, kata.VirtiofsdOptions{
		Binary:     d.virtiofsd,
		SocketPath: kata.VirtiofsdSocketPath(id),
		SharedDir:  kata.SharedDir(baseID),
		Log:        vfsdLog,
	})
	if err != nil {
		return fmt.Errorf("microvm restore: virtiofsd: %w", err)
	}
	defer func() {
		if retErr != nil && vfsdCmd.Process != nil {
			_ = vfsdCmd.Process.Kill()
			_, _ = vfsdCmd.Process.Wait()
		}
	}()

	// Rebuild the tap in the interior netns for each net device the snapshot has (one
	// today), and collect the fresh fds CH must adopt on restore.
	netDevs, err := ch.SnapshotNetDevices(restoreDir)
	if err != nil {
		return fmt.Errorf("microvm restore: read snapshot net devices: %w", err)
	}
	var restoredNets []ch.RestoredNet
	var tapFiles []*os.File
	defer func() {
		for _, f := range tapFiles {
			_ = f.Close() // CH dups adopted fds; ours always close.
		}
	}()
	for i, nd := range netDevs {
		files, terr := d.setupRestoreTap(ctx, fmt.Sprintf("tap%d_kata", i), nd.QueuePairs)
		if terr != nil {
			return fmt.Errorf("microvm restore: build tap for %s: %w", nd.ID, terr)
		}
		tapFiles = append(tapFiles, files...)
		rn := ch.RestoredNet{ID: nd.ID}
		for _, f := range files {
			rn.FDs = append(rn.FDs, int(f.Fd()))
		}
		restoredNets = append(restoredNets, rn)
	}

	// Launch a bare VMM and restore into it with the tap fds attached (SCM_RIGHTS).
	// Capture CH stdout/stderr to clh.log — vm.restore is where a bad snapshot or a
	// path mismatch surfaces, and CH reports it there, not over the api-socket.
	apiSocket := filepath.Join(kata.VMDir(id), "clh-api.sock")
	launchOpts := ch.LaunchVMMOptions{Binary: d.chBin, APISocket: apiSocket}
	if lf := chLog(id); lf != nil {
		launchOpts.Stdout, launchOpts.Stderr = lf, lf
	}
	chCmd, client, err := ch.LaunchVMM(ctx, launchOpts)
	if err != nil {
		return fmt.Errorf("microvm restore: launch VMM: %w", err)
	}
	defer func() {
		if retErr != nil && chCmd.Process != nil {
			_ = chCmd.Process.Kill()
			_, _ = chCmd.Process.Wait()
		}
	}()

	// Guest RAM restore mode depends on the VMM (see ch.PrefaultsUnconditionally):
	// OnDemand keeps a small idle footprint but is unusable on a prefaulting CH
	// (v53), so we fall back to eager there. Eager restores are self-contained (the
	// next snapshot stands alone; no merge base to keep).
	memMode := ch.MemRestoreEager
	if !client.Info().PrefaultsUnconditionally() {
		memMode = ch.MemRestoreOnDemand
	}
	if err := client.RestoreWithNetFDs(ctx, restoreDir, restoredNets, memMode); err != nil {
		return fmt.Errorf("microvm restore: vm.restore: %w", err)
	}
	if err := client.Resume(ctx); err != nil {
		return fmt.Errorf("microvm restore: resume: %w", err)
	}

	// An eager restore has read the whole snapshot into guest memory and nothing
	// merges against it, so the staged memory image is dead weight — drop it (keep
	// the small config/state files; the sandbox is torn down whole later).
	selfContained := memMode == ch.MemRestoreEager
	if selfContained {
		_ = os.Remove(filepath.Join(restoreDir, "memory-ranges"))
	}

	// Re-attach the kata-agent client (the restored guest's agent is alive) so
	// delete() can close it and log surfacing can resume, AND so we can apply the two
	// post-restore guest fixups below. Best-effort — a failed dial must not fail an
	// already-running restore.
	var ac *kata.AgentClient
	if a, derr := kata.DialAgentRetry(ctx, kata.VsockSocketPath(id), 15*time.Second); derr == nil {
		ac = a
		// Post-restore guest fixups (see PRD §4.5). Both are best-effort: the sandbox
		// is already Running, so a failure here is logged, not fatal.
		//
		// 1) Entropy reseed (VMGenID analog). The guest resumes with the CRNG state
		//    frozen at snapshot time; N clones from one base snapshot would generate
		//    IDENTICAL randomness. Inject fresh host entropy so each clone diverges.
		if err := reseedGuestEntropy(ctx, ac); err != nil {
			log.Printf("[microvm restore %s] WARN: entropy reseed failed (clones may share randomness): %v", id, err)
		}
		// 2) Clock-fixup. The guest wall clock is frozen at snapshot time; correct it
		//    to the host's current time so TLS/token/log timestamps are sane on resume.
		now := time.Now()
		if err := ac.SetGuestDateTime(ctx, now.Unix(), int64(now.Nanosecond()/1000)); err != nil {
			log.Printf("[microvm restore %s] WARN: guest clock fixup failed (guest clock stale until NTP): %v", id, err)
		}
	}

	var stopOOM chan struct{}
	if ac != nil {
		stopOOM = make(chan struct{})
	}
	d.mu.Lock()
	d.vms[id] = &chVM{
		id: id, apiSocket: apiSocket, chCmd: chCmd, vfsdCmd: vfsdCmd, agent: ac,
		restoreSourceDir: restoreDir, snapshotSelfContained: selfContained,
		baseID:  baseID, // propagate the golden base across a fork lineage
		stopOOM: stopOOM,
	}
	d.mu.Unlock()
	if ac != nil {
		go d.watchOOM(id, ac, stopOOM) // observability parity: surface guest OOM-kills
		if d.streamConsole {
			d.forwardWorkloadLogs(id, ac) // relay restored WORKLOAD stdout/stderr → kubectl logs
		}
	}
	return nil
}

// rewriteSnapshotSocketPaths repoints the snapshot config.json's per-VMDir paths
// (hybrid-vsock socket, File serial console, virtio-fs socket) from the source
// sandbox's VMDir to id's, so the sockets/files we create are the ones CH reopens.
// The kernel and /dev/vda kata image are content-addressed static files with
// identical paths on every node, so they need no rewrite. Ported from substrate,
// trimmed to the single kataShared fs share (no durable-dir volumes here).
func rewriteSnapshotSocketPaths(snapshotDir, id string) error {
	cfgPath := filepath.Join(snapshotDir, "config.json")
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		return err
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		return fmt.Errorf("parsing %q: %w", cfgPath, err)
	}
	if vsock, ok := cfg["vsock"].(map[string]any); ok {
		vsock["socket"] = kata.VsockSocketPath(id)
	}
	if serial, ok := cfg["serial"].(map[string]any); ok {
		if mode, _ := serial["mode"].(string); mode == "File" {
			serial["file"] = filepath.Join(kata.VMDir(id), "serial.log")
		}
	}
	if fss, ok := cfg["fs"].([]any); ok {
		for _, f := range fss {
			fm, ok := f.(map[string]any)
			if !ok {
				return fmt.Errorf("snapshot config %q has a malformed fs device", cfgPath)
			}
			// Single share in sandboxd's one-container model: the overlay RO lower.
			fm["socket"] = kata.VirtiofsdSocketPath(id)
		}
	}
	out, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(cfgPath, out, 0o600)
}

// copyFile copies src to dst (used as the cross-filesystem fallback when hardlink
// staging fails). Not on the hot path — the staging files share imageDir's fs.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := out.ReadFrom(in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
