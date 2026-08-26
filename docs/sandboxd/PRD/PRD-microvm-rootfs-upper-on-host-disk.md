# PRD (eval) — microVM container rootfs upper on host disk (off guest memory)

Status: **Evaluation / not yet decided.** Written 2026-08-13. Prompted by a review of
Agent Substrate commit **`c1339e5f`** (`microvm: write container rootfs to host disk
instead of guest memory`, #846) — the largest microVM change upstream has made since our
port point (`c538b68b`). This document evaluates whether/how to adopt it in sandboxd; it
is not yet an approved feature.

Related: [PRD-microvm-runtime-cloud-hypervisor.md](./PRD-microvm-runtime-cloud-hypervisor.md)
(the runtime we'd modify), [PRD-sparse-checkpoint-s3-transfer.md](./PRD-sparse-checkpoint-s3-transfer.md)
(our checkpoint codec, which this interacts with), and the (proposed)
volume-bridge/durable-dir work (substrate builds this on the same virtiofsd share).

## 1. Problem (does sandboxd have it too?)

In sandboxd's microVM runtime the container's writable overlay **upper is a guest
tmpfs** (see `buildVMConfig`: "the writable upper is a guest tmpfs"). Consequences,
inherited directly from the pre-`c1339e5f` substrate design we ported:

1. **Every written byte consumes guest RAM 1:1.** The upper lives in the guest's tmpfs,
   which is backed by guest RAM (the CH memfd). A write-heavy workload's files sit in the
   same memory the guest runs in.
2. **A hard write cap ≈ tmpfs size.** tmpfs defaults to ~20% of guest RAM; substrate
   measured **~264 MiB effective on a 2 GiB guest** once `/run`'s other tmpfs occupants
   are counted. Past that, writes fail **`ENOSPC`** — write-heavy actors **crash**,
   everyone else loses density (you must oversize guest RAM just to hold scratch files).
3. **It bloats the checkpoint.** Because the upper is in guest RAM, it rides inside the
   **memory snapshot** — so scratch files inflate the thing we ship to S3 on every
   suspend, working *against* our sparse-checkpoint codec.

sandboxd has all three — **CONFIRMED live on our build 2026-08-26** (standalone microVM
sandbox on the current worker image, guest sized ~3.6 GiB via a 4 GiB `limits.memory`):

- **Write cliff:** the writable rootfs is `overlay` on a **guest-RAM-backed tmpfs**
  (`df /` → `overlay 691M`, ~19% of guest RAM); writing 16 MiB blocks to `/big` failed
  with **`ENOSPC` (errno 28) at 672 MiB**. Write-heavy workloads crash.
- **Checkpoint bloat (incompressible):** writing **304 MiB of `os.urandom`** to the upper
  then `/suspend` inflated the S3 transfer from **~42 MiB (idle) → 362 MiB** — the scratch
  rides the memory snapshot ~1:1.
- **Nuance our sparse codec adds (substrate lacks it):** the *same* 304 MiB written as
  **zeros** transferred only **42 MiB** — our sparse-extent + zstd codec crushes
  compressible scratch, so it MASKS the S3-bloat for zero/compressible data. But the
  write-cliff and the guest-RAM/density cost are unaffected (zeros still occupy tmpfs
  pages in guest RAM and still count against the ENOSPC cap), and *incompressible* scratch
  (the realistic case: databases, compiled artifacts, media) bloats S3 directly as shown.

Conclusion: the problem is real and worth fixing; the codec only softens one of the three
symptoms, and only for compressible data. Gate cleared — proceed to §5.

## 2. What substrate's `c1339e5f` changes

The rootfs overlay is **assembled on the host** instead of in the guest — the conventional
arrangement for VM-isolated containers:

- **lower** = the read-only OCI image bundle (as today);
- **upper/work** = **per-actor host directories**, merged by the **host** kernel;
- the merged tree is served to the guest over the **single existing virtio-fs share**
  (`cache=auto`), and the guest runs the container **directly on the merged directory** —
  it never mounts an overlay, needs no overlay mount options, and **no second virtiofsd**
  is added.
- The merged mount pins **`metacopy=off,index=off`** so every copy-up is a full data copy
  — the upper never holds file-handle references to lower inodes, which would go stale
  when restore rebuilds the lower from the image.

Snapshot/restore integration:

- **FULL checkpoint:** archive the upper as **`rootfs-upper.tar`**, captured while the
  guest is paused (the share is write-through, so the tar is coherent) and **concurrently
  with the CH memory snapshot** — the paused window costs the *slowest* artifact, not the
  sum.
- **Restore:** re-materialize the upper in the background (overlapped with bundle prep),
  re-mount the merged trees at the frozen find-paths locations, then start the share.
- **Self-describing restore:** the tar's presence routes it — **old (memory-upper)
  snapshots still restore unchanged** (bare-image bind at `cache=always`, upper rides in
  restored guest memory), and their re-checkpoints stay correct. So it's **backward
  compatible** with snapshots taken by our current implementation.
- Requires `tarutil` to round-trip **overlay deletion metadata**: whiteout device nodes
  (`mknodat`) and `user.*`/`trusted.overlay.*` xattrs (PAX `SCHILY.xattr`), or deleted
  files silently reappear after resume. Extraction is contained to the root (a crafted
  archive cannot escape).

Files touched upstream: `checkpoint.go`, `durable.go`, `internal/kata/overlay_linux.go`
(heavy), `internal/kata/{kata,restore}.go`, and new `internal/tarutil/{tarutil,fifo_linux}.go`.

## 3. Why it's attractive for sandboxd

- **Removes the ~264 MiB write cliff** — write-heavy sandboxes stop crashing with
  `ENOSPC`; scratch space is bounded by host disk, not guest RAM.
- **Shrinks the memory checkpoint** — the upper leaves guest RAM, so the snapshot we ship
  to S3 no longer carries scratch files. Compounds with our sparse codec (smaller *and*
  sparser memory image).
- **Lets us run smaller guests** — pairs with the `acpi=off` + `#38` sizing work: less
  RAM needed just to hold scratch → higher density.
- **Unifies with durable volumes / the volume-bridge PRD** — it's the same "serve host
  dirs through the one kataShared virtiofsd" mechanism substrate then reused for
  durable-dir volumes (`236a02bf`) and read-only OCI-image mounts (`0741459b`). Adopting
  this lays the groundwork for those.

## 4. Why it's non-trivial (the eval risks)

- **It's the biggest surface-area change** of any post-port substrate commit — it rewrites
  the checkpoint path and the overlay assembly, and adds a `tarutil` with security-sensitive
  extraction (whiteouts, xattrs, path-escape defense). Not a cherry-pick.
- **It interacts with our divergences.** sandboxd added the **sparse-extent + zstd
  checkpoint codec** (substrate ships to GCS via `atelet`; we ship to S3 via our own
  `s3.go`/`sparse_linux.go`). The new `rootfs-upper.tar` artifact must flow through *our*
  upload/download path and coexist with the sparse memory image — the port has to be
  re-plumbed onto our S3 layer, not lifted whole.
- **Restore timing / correctness must be re-validated** across the full teleport matrix we
  just exercised (C/R, multi-checkpoint, forkset, IAM, ports) — the upper is now a
  separate artifact re-materialized on a possibly-different worker.
- **Host disk pressure moves to the worker.** Scratch now consumes the worker pod's
  ephemeral storage / the node disk instead of guest RAM — so it wants an ephemeral-storage
  limit on the worker pod (and interacts with the memory-reserve accounting). New failure
  mode to bound (node disk full) replacing the old one (guest tmpfs full).
- **`virtio-fs` write-through semantics + `cache=auto`** change the guest's filesystem
  behavior vs. a local tmpfs overlay — needs correctness testing for workloads sensitive to
  fsync/rename/mmap semantics over virtio-fs.

## 5. Proposed approach (if greenlit)

1. **Confirm the problem on our build** (§1): reproduce the `ENOSPC` cliff + measure
   checkpoint bloat. Gate the whole effort on this.
2. **Port `tarutil` first**, with its privileged round-trip unit tests (whiteouts, xattrs,
   extraction-escape rejection) — it's the security-sensitive, self-contained core.
3. **Port the host-side overlay assembly** (`overlay_linux.go`) — merged lower+upper served
   over the existing kataShared virtiofsd, `metacopy=off,index=off`.
4. **Re-plumb checkpoint/restore onto our S3 + sparse codec**: add `rootfs-upper.tar` as a
   second artifact, uploaded concurrently with the (sparse) memory image; restore
   re-materializes it. Preserve the **self-describing** back-compat (old memory-upper
   snapshots still restore).
5. **Add a worker ephemeral-storage limit** + account for the upper in sizing docs.
6. **Re-run the full live e2e matrix** including a write-heavy workload and multi-cycle
   suspend/resume with a byte-identical large payload (substrate's own test shape).

## 6. Open questions

- Do we adopt substrate's exact "one merged share, guest runs directly on it" model, or
  keep our current in-guest overlay and only move the *upper* backing to a host-served
  dir? (Substrate's model is cleaner and drops the guest-side overlay mount entirely —
  lean toward adopting it wholesale to stay mergeable with their durable-volume follow-ups.)
- Interaction with the **`acpi=off` segment-0** virtio-fs placement we shipped — the upper
  rides the *same* kataShared share, so it inherits segment 0 automatically; confirm no new
  device is introduced that would re-raise the MCFG/segment issue.
- Ephemeral-storage sizing: derive a worker `ephemeral-storage` limit from the template, or
  leave it operator-set? (Parallels the `#38` limits discussion.)

## 7. Recommendation

**Worth doing, but as its own tracked effort after confirming §1** — it fixes a real
write-capacity cliff we inherited and shrinks the checkpoint, and it's the substrate
change with the most downstream leverage (durable volumes, OCI-image mounts build on it).
But it is the opposite of a quick win: it rewrites the checkpoint path that our sparse
codec also lives in, so it must be re-plumbed onto our S3 layer and re-validated end to
end. Sequence it **after** the low-risk substrate pulls (agent-poll, merge-unlink) and
**alongside** the volume-bridge design, since they share the host-served-virtiofsd
mechanism.
