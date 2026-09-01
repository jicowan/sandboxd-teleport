//go:build linux

// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

// Host-side rootfs overlay uppers for the microVM runtime (phase 3 of
// PRD-microvm-rootfs-upper-on-host-disk). Ported from Agent Substrate c1339e5f
// (Apache-2.0, Google LLC) and adapted to sandboxd's paths.
//
// Each container's rootfs is assembled ON THE HOST — the stock kata arrangement:
// overlay(lower = the OCI image bundle, upper/work = this per-sandbox directory),
// merged by the host kernel and served to the guest over the one kataShared
// virtio-fs share (see kata.StageMergedRootfs). Rootfs writes cost HOST DISK, not
// guest RAM. The host kernel owns all overlay mechanics, so deletion metadata is the
// canonical kind: whiteouts as 0:0 char devices and opaque markers as
// trusted.overlay.* xattrs in the upper — no special mount options, no guest xattr
// passthrough.
//
// The directory is on disk (SANDBOXD_WORK, /work) NOT tmpfs — the whole point is to
// keep the upper off guest RAM. It is created pristine at cold boot, re-materialized
// from the snapshot at restore, and removed at teardown (after CleanupSandboxState
// has dropped the overlay mounts that use it).
//
// Snapshots: the upper does not ride in guest memory, so a checkpoint ships it as a
// tar (rootfsUpperTarFile), taken while the guest is PAUSED (virtiofsd is
// write-through, so a paused guest's completed writes are already in the upper).
// Restore is SELF-DESCRIBING — the tar's presence is what says the snapshot carries a
// host-merged rootfs — which is also what keeps LEGACY tmpfs-upper snapshots (taken
// before this change) restorable: no tar => the bare image is staged and the guest's
// own in-memory upper takes over.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jicowan/aio-sandbox/sandboxd/tarutil"
)

// rootfsUpperTarFile is the snapshot file holding the tar of the sandbox's rootfs
// upper. Its entries are <cid>/fs/... and <cid>/work/... relative to rootfsUpperDir —
// the same layout kata.UpperWorkDirs mounts — so extraction restores exactly the tree
// the merged overlay (and the guest's find-paths) expects. It rides the checkpoint's
// top-level file set, so the S3 uploadDir picks it up automatically.
const rootfsUpperTarFile = "rootfs-upper.tar"

// rootfsUpperDir is the HOST directory backing a sandbox's rootfs overlay upper: one
// <cid>/{fs,work} subtree per container (see kata.UpperWorkDirs).
//
// It MUST be on a NON-overlay filesystem: overlayfs rejects an upperdir that sits on
// another overlayfs, and the worker container's own rootfs (including /work) IS an
// overlay (containerd's overlay snapshotter). So this points at SANDBOXD_ROOTFS_UPPER
// (default /var/lib/sandboxd/rootfs-upper), where the operator mounts an emptyDir —
// node-disk-backed ext4/xfs, a real fs that overlayfs accepts as an upper AND that
// keeps the writes off guest RAM (the whole point). NOT /run (tmpfs = RAM) and NOT
// /work (overlay). If no emptyDir is mounted there the overlay mount fails loudly at
// cold boot (StageMergedRootfs) rather than silently degrading.
func rootfsUpperDir(id string) string {
	return filepath.Join(envOr("SANDBOXD_ROOTFS_UPPER", "/var/lib/sandboxd/rootfs-upper"), id)
}

// resetRootfsUpperDir gives a cold boot a pristine upper directory (a cold boot must
// start from the bare image). The per-container fs/work subdirs are created by
// kata.StageMergedRootfs.
func resetRootfsUpperDir(id string) error {
	dir := rootfsUpperDir(id)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("clearing rootfs upper dir %q: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating rootfs upper dir %q: %w", dir, err)
	}
	return nil
}

// removeRootfsUpperDir drops the sandbox's upper on teardown. Best-effort: called
// after the overlay mounts that use it are already gone (CleanupSandboxState).
func removeRootfsUpperDir(id string) { _ = os.RemoveAll(rootfsUpperDir(id)) }

// dirExists reports whether path exists (used to tell a host-merged sandbox — which
// has an upper dir — from a legacy guest-tmpfs-upper one).
func dirExists(path string) bool { _, err := os.Stat(path); return err == nil }

// snapshotHasRootfsUpper reports whether a downloaded snapshot carries a host-merged
// rootfs upper (i.e. whether restore must untar it + stage the merged overlay) vs a
// LEGACY tmpfs-upper snapshot (whose share presents the bare image and whose upper
// rides in the restored guest memory). This is the self-describing restore switch.
func snapshotHasRootfsUpper(snapshotDir string) bool {
	_, err := os.Stat(filepath.Join(snapshotDir, rootfsUpperTarFile))
	return err == nil
}

// tarRootfsUpper archives the sandbox's rootfs upper (dir) into the checkpoint dir as
// rootfsUpperTarFile. The caller MUST have paused the guest first: virtiofsd is
// write-through, so a completed guest write has reached the host overlay's upper by
// then, but a running guest could still add more after the walk.
//
// The overlay workdir (<cid>/work, sibling of the <cid>/fs upper — see
// kata.UpperWorkDirs) is EXCLUDED: with index=off pinned on the merged mount a restored
// workdir is inert (overlayfs wipes and rebuilds it at mount, and staging recreates it),
// so archiving it is dead weight and can capture copy-up temp files in flight at pause.
func tarRootfsUpper(ctx context.Context, dir, checkpointDir string) error {
	skipWorkdir := func(rel string) bool {
		parts := strings.Split(rel, "/")
		return len(parts) == 2 && parts[1] == "work" // <cid>/work
	}
	if err := tarutil.CreateFiltered(ctx, filepath.Join(checkpointDir, rootfsUpperTarFile), dir, skipWorkdir); err != nil {
		return fmt.Errorf("archiving rootfs upper from %q: %w", dir, err)
	}
	return nil
}

// untarRootfsUpper restores the rootfs upper from a snapshot into the sandbox's host
// directory. It must run BEFORE the merged overlay is mounted (the mount consumes
// these contents). The directory is recreated from scratch: stale contents from a
// previous activation would corrupt the overlay state the guest's find-paths re-opens.
func untarRootfsUpper(dir, snapshotDir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("clearing rootfs upper dir %q: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating rootfs upper dir %q: %w", dir, err)
	}
	if err := tarutil.Extract(filepath.Join(snapshotDir, rootfsUpperTarFile), dir); err != nil {
		return fmt.Errorf("restoring rootfs upper into %q: %w", dir, err)
	}
	return nil
}
