# PRD — sparse-aware checkpoint transfer to S3

Status: **Proposed.** Written 2026-08-12. Grounded in a read of the live worker's S3
path (`checkpoint-restore/sandboxd/s3.go` — `uploadDir`/`downloadPrefix`/`downloadOne`),
the checkpoint/suspend/restore handlers (`checkpoint-restore/sandboxd/main.go`), the
microVM driver's snapshot files (`checkpoint-restore/sandboxd/microvm_checkpoint.go`), and
Agent Substrate's prior art `cmd/atelet/internal/ategcs/sparsezstd.go` (a working
sparse-extent + zstd object format). The measurements below are from the live microVM
teleport validation on 2026-08-12, not estimates.

Related: [PRD-microvm-runtime-cloud-hypervisor.md §8 Q5](./PRD-microvm-runtime-cloud-hypervisor.md)
(this splits out that open question), [[sandboxd-microvm-nested-virt]] (the teleport
validation that surfaced the cost), [PRD-on-demand-suspend.md](./PRD-on-demand-suspend.md)
(suspend is on the latency-sensitive path), [PRD-checkpoint-on-terminate.md](./PRD-checkpoint-on-terminate.md).

## 1. The problem

A checkpoint's memory image is a **sparse file**: most of a guest's RAM is unallocated
or zero, so the on-disk footprint is a fraction of the logical size. On the microVM
teleport validation a redis sandbox's `memory-ranges` was **2.0 GiB logical / ~154 MiB
resident** (`du --apparent-size` vs `du`). gVisor's `pages.img` is sparse the same way.

The worker's S3 transfer is **hole-blind**. `s3Store.uploadDir` opens each file and hands
the whole thing to the AWS `manager.Uploader`, which reads it start to end — **holes and
all** — so the 2 GiB logical image uploads as a **dense 2 GiB** S3 object. Restore's
`downloadOne` then pulls all 2 GiB back and writes it dense to local disk, and CH's eager
restore reads the entire 2 GiB. Measured impact on the validation:

- **Upload**: ~9 s for a redis suspend (dominated by shipping 2 GiB of mostly-zero bytes).
- **Download**: ~10 s before restore can even begin.
- **Restore read**: the dense image is why restore needed a 10-minute budget instead of
  the 120 s cold-boot budget (`restoreTimeout` in `microvm_checkpoint.go`).
- **S3 cost + storage**: every snapshot is billed and stored at full guest-RAM size
  (2 GiB here; larger guests scale linearly), not working-set size.

This is the single biggest cost on the microVM suspend/restore path, and it grows with
guest RAM. It also taxes gVisor, just less visibly (smaller sandboxes today). The
`compress` flag exists on the checkpoint contract but is a **no-op for microVM** (CH has
no compression knob) and for gVisor only compresses runsc's own output; neither makes the
S3 transfer hole-aware.

## 2. Goal & non-goals

**Goal.** Ship and restore checkpoint memory images at **working-set size, not logical
size** — read/transfer/store only the populated extents — transparently to the
checkpoint/restore/suspend handlers and to both runtimes. Target: the redis case's ~2 GiB
S3 object becomes ~150 MiB (or less, with compression), and upload/download/restore-read
times drop proportionally.

**Non-goals.**
- Changing the snapshot *semantics* (what CH/runsc write) — this is purely the transfer +
  at-rest encoding of files that already exist on disk.
- Cross-runtime portability of snapshots (still hard-refused by the restore guard).
- Incremental/diff snapshots across suspends (that is the microVM PRD's OnDemand
  delta-merge, a separate mechanism; this PRD ships whatever a single snapshot produced).
- Deduplication across sandboxes / content-addressed storage.

## 3. Why this is tractable

- **The files are already sparse on disk.** `SEEK_DATA`/`SEEK_HOLE` (Linux, `lseek`)
  enumerate the populated extents of a file in O(extents), so the writer never reads a
  hole. No cooperation from CH/runsc is needed.
- **The seam is one file.** Only `s3.go`'s `uploadDir`/`downloadOne` touch the wire; the
  handlers call them and are otherwise runtime-neutral. A sparse encoder/decoder drops in
  behind those two functions.
- **Substrate hands us a proven format.** `cmd/atelet/internal/ategcs/sparsezstd.go`
  implements exactly this: a self-describing `magic[8] | version:u32 | zstd(totalSize |
  (off,len,data)* | -1)` stream that walks `SEEK_DATA`/`SEEK_HOLE` and feeds only the real
  extents to zstd — "scan the resident set, not the logical image." It is Apache-2.0 and
  lift-and-adaptable, the same way the CH driver was.

## 4. Design (sketch)

### 4.1 A sparse object codec behind the S3 seam

Introduce a codec used by `uploadDir`/`downloadOne` for memory images (and any file that
is sparse). Adapting substrate's `sparsezstd`:

- **Upload** (`writeSparseZstd(dst, src *os.File)`): write `magic + version` in the clear,
  then a single zstd stream of `logicalSize` followed by `(offset, len, data)` frames for
  each populated extent found via `SEEK_DATA`/`SEEK_HOLE`, terminated by a `-1` sentinel.
  The `dst` is streamed to S3 (the uploader sees a normal `io.Reader`, so multipart still
  works). Holes are never read or compressed.
- **Download** (`readSparseZstd(dst *os.File, src)`): read the magic to dispatch, then
  `ftruncate` `dst` to `logicalSize` (creating a sparse file) and `WriteAt` each extent at
  its offset. Unwritten ranges stay holes, so the restored local file is sparse again —
  which also keeps CH's eager restore reading only the extents.

**Format dispatch, not a flag.** The 8-byte magic lets the reader tell a sparse-zstd
object from a plain byte stream, so the encoding is self-describing on the object itself —
no separate metadata, and old plain objects still restore (see §5 compat).

### 4.2 Which files get the codec

Memory images are the win: `clh-memory-ranges` (microVM) and `pages.img` (gVisor). The
small metadata files (`config.json`, `state.json`, runsc's `checkpoint.img`) are tiny and
can ship plain or through the same codec harmlessly. Simplest correct rule: **apply the
codec to every checkpoint file**; a dense small file just yields a ~1:1 sparse-zstd object.
Whether to keep an object-name suffix (e.g. `.spz`) or rely solely on the magic is an open
question (§7).

### 4.3 Compression is on the same pass

Feeding only extents to zstd compresses the working set for free — memory images are
compressible (zeroed pages that *are* resident, text, etc.). This subsumes the current
`compress` flag for the memory image: the flag can select the zstd level (or off), and the
existing gVisor `compress` behavior is unaffected.

## 5. Compatibility & correctness

- **Backward read compat.** Existing snapshots in S3 are dense/plain. The magic-dispatch
  reader must fall back to a plain streaming copy when the magic is absent, so already-
  stored snapshots keep restoring. (Alternatively, gate the new format behind a version and
  only write it going forward — decide in §7.)
- **Restore guard unchanged.** The `{runtime, engineVersion}` guard is orthogonal; a sparse
  object of a microVM snapshot is still refused on a gVisor worker.
- **Integrity.** Keep whatever checksum S3 already provides per object; consider a logical-
  size assertion on download (truncate target == header logicalSize) so a truncated
  transfer fails loudly instead of silently restoring a short image.
- **The `du` trap.** `sizeBytes` reported by `/checkpoint` today is `dirSize` (logical).
  After this change, report both logical and transferred bytes so the win is visible and
  monitorable.

## 6. Phasing

- **Phase 1 — codec + S3 seam.** Port `sparsezstd` into the worker (Apache-2.0 provenance),
  wire it behind `uploadDir`/`downloadOne` with magic-dispatch read fallback. Unit-test the
  round-trip on a hand-built sparse file (holes preserved, bytes identical). This alone
  delivers the win for both runtimes.
- **Phase 2 — live validation + telemetry.** Re-run the microVM teleport cycle; confirm the
  redis snapshot S3 object drops from ~2 GiB to ~working-set, and upload/download/restore
  times drop. Surface logical-vs-transferred bytes in the checkpoint/suspend responses and
  logs. Revisit `restoreTimeout` (a working-set-sized restore may fit the normal budget).
- **Phase 3 (optional) — tuning.** zstd level as a per-pool/template knob; parallel extent
  compression if the single-stream encode becomes the bottleneck; drop the interim
  `restoreTimeout` bump once sparse restore is the default.

## 7. Open questions

1. **Object naming vs magic-only.** Rely solely on the in-band magic (cleanest, one code
   path) or also suffix the S3 key (`.spz`) so the format is visible in `aws s3 ls`?
2. **Migrate old snapshots or read-compat forever?** Keep the plain-stream fallback
   indefinitely (simple, a little dead code) or add a one-time re-pack + cutover?
3. **Apply to metadata files too, or memory-only?** Uniform (all files through the codec)
   is simplest; memory-only avoids touching the tiny files at all. Any downside to uniform?
4. **zstd level default.** Fastest (`best-speed`, matches the current gVisor default) to
   keep suspend latency low, or a higher level since suspend is off the request hot path?
5. **Does the sparse download interact with CH OnDemand restore-dir lifetime**
   (microVM PRD §8 Q4)? The restored local file must stay sparse *and* present for the VM's
   lifetime under OnDemand; confirm `WriteAt`-into-truncated-file yields a file CH can
   demand-page from.

## 8. Recommendation

Do Phase 1 — it is a contained, one-file change with a proven reference implementation and
a large, measured payoff (≈13× smaller S3 objects on the validated case, plus faster
suspend/restore), and it benefits gVisor as well as microVM. It also removes the main
reason restore needed a 10-minute timeout. Phases 2–3 follow naturally once it lands.
