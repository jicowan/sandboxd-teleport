# PRD — sparse-aware checkpoint transfer to S3

Status: **IMPLEMENTED (Phase 1) + live-validated 2026-08-12.** On branch
`feat/sparse-checkpoint-s3`. See §6 for per-phase status and §5.1 for the live results
(microVM 43× smaller; gVisor ~1× — the data is incompressible, benchmarked). Written
2026-08-12. Grounded in a read of the live worker's S3
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

## 5.1 Implementation + live results (2026-08-12)

Implemented as `checkpoint-restore/sandboxd/sparse_linux.go` (`writeSparseZstd` /
`readSparseZstd` / `hasSparseMagic`, magic `SBXDSPRS`, zstd `SpeedFastest` +
`GOMAXPROCS` concurrency), wired behind `s3.go`'s `uploadOne`/`downloadOne`:

- **Upload** streams the codec through an `io.Pipe` whose reader is the multipart
  upload `Body`, so `manager.Uploader` still buffers into 5 MiB parts and uploads them
  **concurrently** — compression runs in the pipe-writer goroutine alongside the parallel
  part uploads. No loss of upload parallelism.
- **Download** does the concurrent ranged-GET into a `.dl` temp file (keeps download
  parallelism; the compressed object is small), then decodes locally with **magic
  dispatch**: sparse-zstd → `readSparseZstd`; magic absent → a dense pre-codec object,
  renamed into place (backward compatible — snapshots already in S3 still restore).

**Live-validated on both runtimes** (redis on microVM, the AIO agent sandbox on gVisor);
both round-tripped losslessly (state teleported exactly, ~4.4 s restore):

| Runtime | Image file | Dense (before) | Sparse+zstd (after) | Reduction |
|---|---|---|---|---|
| microVM (redis) | `clh-memory-ranges` | 2048 MiB | **47.9 MiB** | **~43×** |
| gVisor (AIO) | `checkpoint.img` | 163.6 MiB | 161.2 MiB | ~1× (none) |

**Why the two runtimes differ — and why no zstd feature closes the gap.** The microVM
`clh-memory-ranges` is a sparse mmap of the full guest address space: mostly holes, so
`SEEK_DATA`/`SEEK_HOLE` drops almost everything → 43×. gVisor's `checkpoint.img` is
already **densely packed** by runsc (no holes to drop) AND its content is
near-incompressible. Benchmarked directly on the real 163.6 MiB AIO `checkpoint.img`:

| codec / setting | ratio |
|---|---|
| gzip -1 (32 KiB window) | 1.02× |
| zstd -1 (≈ our `SpeedFastest`) | 1.005× |
| zstd `--long=27 -1` (large window / long-distance matching) | 1.019× |
| zstd `--long=31 -19` (max window + high level) | 1.019× |
| zstd -19 (high level) | 1.036× |

The question "is there a zstd feature that works better on dense files?" was tested: the
feature for far-apart redundancy is **long-distance matching (large window)**, but it
gains ~1.9% here — confirming there is no distant redundancy to find. This is an
**entropy** ceiling (JS heaps, JIT code, already-compressed assets, TLS state), not a
window-size or level-tuning problem; even `-19` (≈10× the CPU of `-1`) buys only ~3%.

**Decision: keep `SpeedFastest` as the single, always-on setting.** It delivers 43× where
the data is sparse/compressible (microVM, the case that mattered) and a cheap ~1× pass
where it isn't (gVisor), at negligible CPU on the suspend hot path. A higher level or
long-distance matching is not worth an order of magnitude more CPU for a few percent, and
helps neither case. We always compress (no per-file skip): the codec is a no-op-fast pass
on incompressible data, and uniformity keeps one code path.

## 6. Phasing

- **Phase 1 — codec + S3 seam (DONE 2026-08-12).** Ported `sparsezstd` into the worker
  (`sparse_linux.go`, Apache-2.0 provenance), wired behind `s3.go`'s `uploadOne`/
  `downloadOne` with magic-dispatch read fallback; upload keeps multipart concurrency via
  an `io.Pipe`, download keeps ranged-GET concurrency via a temp file + local decode.
  Round-trip unit test (holes preserved, bytes identical, decoded file stays sparse) passes
  on linux. Live-validated on microVM + gVisor (see §5.1). The big win is microVM-specific.
- **Phase 2 — telemetry + timeout revisit (NOT DONE).** Still to do: surface
  logical-vs-transferred bytes in the checkpoint/suspend responses + logs so the win is
  observable/monitorable; revisit the microVM `restoreTimeout` (a working-set-sized restore
  — the microVM case is now ~48 MiB, not 2 GiB — likely fits a far smaller budget than the
  10-min interim). Live functional validation is already done (§5.1).
- **Phase 3 (optional) — tuning.** DEPRIORITIZED by §5.1: a zstd level knob / long-distance
  matching was benchmarked and doesn't pay off (entropy ceiling on dense data; sparse data
  is already tiny at `SpeedFastest`). Parallel extent compression only if the single-stream
  encode ever becomes the bottleneck (it is not — S3 dominates). Keep `SpeedFastest`.

## 7. Open questions

1. **Object naming vs magic-only.** DECIDED: magic-only (in-band `SBXDSPRS` dispatch, one
   code path, no key suffix). Kept the plain-object fallback for backward compat.
2. **Migrate old snapshots or read-compat forever?** OPEN, but low-stakes: the reader falls
   back to a plain rename for dense pre-codec objects, so read-compat is free and
   indefinite. A one-time re-pack is unnecessary; leave the fallback in.
3. **Apply to metadata files too, or memory-only?** DECIDED: uniform — every file goes
   through the codec. It's a cheap ~1× pass on the tiny metadata + incompressible images
   (see §5.1) and keeps a single code path. No downside observed.
4. **zstd level default.** DECIDED: `SpeedFastest`, always on. Benchmarked (§5.1) — higher
   levels / long-distance matching don't pay off on either the sparse (already tiny) or the
   dense/incompressible case, and cost far more CPU on the suspend hot path.
5. **Sparse download vs CH OnDemand restore-dir lifetime** (microVM PRD §8 Q4). Not
   exercised: CH v53 prefaults OnDemand, so restores are eager `Copy` and don't demand-page
   from the staged file for the VM's lifetime — so a sparse-decoded local file is read fully
   at restore and the interaction doesn't arise today. Re-check if a non-prefaulting CH
   makes OnDemand usable (the decoded file is a normal sparse file with real extents at
   their offsets, which CH can demand-page from, but this is untested).

## 8. Recommendation

DONE (Phase 1) — a contained change with a proven reference implementation and a large,
measured payoff **for the microVM runtime**: a 2 GiB `clh-memory-ranges` → ~48 MiB (~43×),
lossless teleport, ~4.4 s restore (§5.1). This directly removes the main reason microVM
restore needed a 10-minute timeout.

Correction to the original estimate: the win is **not** universal. gVisor's `checkpoint.img`
is already dense and near-incompressible, so it stays ~1× — no regression (still correct,
still fast to encode), but no benefit. The value is proportional to sparseness, and only the
microVM memory image is sparse. We keep the codec always-on for both anyway (uniform, one
path, negligible cost on dense data). Remaining work is Phase 2 telemetry + the microVM
`restoreTimeout` revisit; Phase 3 tuning is deprioritized (benchmarked as not worthwhile).
