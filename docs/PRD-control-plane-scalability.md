# PRD — control‑plane scalability under session churn

Status: **Proposed** (not scheduled — analysis + plan; nothing built). Decision‑ready.
Grounded in the shipped code on the `checkpoint-restore` branch. Related:
[architecture-sandboxd.md](sandboxd/architecture-sandboxd.md),
[PRD-durable-assignment-state.md](PRD-durable-assignment-state.md).

## 1. Summary

Characterize and then remove the control‑plane's scaling limits under a busy
cluster (many sessions, high create/update/suspend/resume churn, many pools). The
current design has **two O(N)‑on‑a‑timer costs** (the session sweepers) and, as of
the durability work, **one write amplifier** (the etcd status mirror). None hurts at
today's scale (dozens–hundreds of sessions); all three are what bite in a genuinely
busy cluster. This PRD proposes fixes ordered cheapest‑first and gates each on real
load — build nothing until measured.

## 2. Where the pressure lands (measured from code)

### 2.1 Router (data plane) — fine

Per MCP request: O(1) Valkey ops — `GetSession` + `GetWorker` on the fast path, one
`StampActive` write. No scans. Stateless, horizontally scalable. Grows with
**request rate**, not session count. **Not a bottleneck.**

### 2.2 Valkey — full‑table scans on a fixed timer (designed‑in O(N))

Three loops scan the whole keyspace on a timer regardless of how much actually
needs work:

| Loop | Interval | Cost per pass | Notes |
|------|----------|---------------|-------|
| **Suspend sweeper** (`ListSessions`) | **15s** | `SCAN session:*` + 1 `GET` per key | scans **all** N sessions every pass |
| **Checkpoint sweeper** (`ListSessions`) | **15s** | same full scan | **scans even when the feature is off** (opt‑in `checkpointIntervalSeconds`, usually 0) — pure waste |
| **Worker prune** (`ListWorkerPods` + `GET` each) | 30s | scan all `worker:*` | proportional to worker count |
| **WarmPool status** (`CountWorkers` / `PoolWorkers`) | per reconcile | scans **all** `worker:*` **per pool** | O(pools × workers); reconcile is nudged on **every** claim/release |

Impact: with **N sessions**, the two session sweepers alone do ≈ **2·N GETs / 15s**
of bookkeeping — ~1.3K GET/s at N=10K, ~13K GET/s at N=100K — most of it finding
nothing to do. Valkey can serve it, but it's the textbook "polling cost tied to
table size, not to work" pattern. The per‑pool `CountWorkers` compounds with pool
count and with claim/release churn (each nudge → a reconcile → a full worker scan).

### 2.3 Operator — CPU/GC from the scans + the new etcd writes

- The sweepers **unmarshal every session's JSON** each pass (CPU + GC pressure
  proportional to N, every 15s).
- **New as of durability:** every authoritative session transition now also writes
  `Session.status` to the **apiserver/etcd** (`SessionMirror`). Before, Valkey
  absorbed all session writes and etcd held only the rarely‑touched spec.

### 2.4 etcd / kube‑apiserver — the newest and most cluster‑impacting sensitivity

etcd is Raft‑replicated, fsyncs per write, and is **shared by the whole cluster's
control plane**. High session churn now translates into apiserver write QPS and etcd
write load. `lastActiveAt` is already throttled (mirrored on transitions, not per
router stamp) to avoid the worst, but a burst of creates/suspends/resumes is now an
etcd write burst — a scaling dimension that didn't exist before this week.

## 3. Order in which it breaks

1. **etcd/apiserver write pressure** from the status mirror under churn (shared
   infra — affects more than just us).
2. **Session‑sweeper scan cost** — O(N) every 15s.
3. **`CountWorkers` per‑pool** reconcile cost with many pools + high churn.

The operator (leader) is the single write serializer, so today this is **vertical**
until the loops become incremental.

## 4. Goals / non‑goals

### Goals
- Make the periodic work **proportional to work due**, not to table size.
- Bound the **etcd write rate** from durability mirroring under churn.
- Remove the **per‑pool worker scan** from the hot reconcile path.
- Keep the correctness properties intact (CAS single‑writer, durable recovery,
  event‑driven pool status).
- Every change **measurable + gated on real load** — no speculative rewrites.

### Non‑goals
- Not multi‑operator write sharding (that's the tabled multi‑namespace work; it's the
  horizontal escape hatch, out of scope here).
- Not changing the data‑plane router (it already scales).
- Not premature: nothing ships until metrics show a limit approaching.

## 5. Proposed changes (cheapest first)

### 5.1 Stop the checkpoint sweeper scanning when the feature is off (trivial)

The checkpoint sweeper full‑scans every 15s even though `checkpointIntervalSeconds`
is opt‑in and 0 by default. Skip the sweep entirely when no template enables it (or
only start the loop if some pool has it set). Removes one full N‑scan / 15s outright.

### 5.2 Raise + stagger sweeper intervals (trivial, config)

15s full scans are aggressive for idle detection. Make the intervals configurable
(they're already flag‑plumbed) and default higher (e.g. 30–60s), and stagger the two
session sweepers so they don't scan in lockstep. Pure config; buys headroom while the
indexed approach (5.3) is built.

### 5.3 Index suspend candidates instead of scanning (the real fix)

Replace the O(N) suspend scan with an O(due) lookup:
- Maintain a Redis **sorted set** `suspend:due` scored by each Running session's
  suspend deadline (`lastActiveAt + idleTimeout`), updated when the router stamps
  activity / on state transitions.
- The sweeper does `ZRANGEBYSCORE suspend:due -inf now` — reads **only sessions
  actually due**, not the whole table. Same pattern for periodic checkpoints
  (`checkpoint:due`).
- Turns the dominant O(N)/15s cost into O(due), which is what "busy cluster" should
  cost. Requires the router's `StampActive` to also update the ZSET (still O(1)).

### 5.4 Coalesce / prioritize the etcd status mirror (bounds 2.4) — DONE (operator v19)

**Implemented:** the mirror now fires only on durability‑critical transitions —
`Suspended` (idle‑suspend + checkpoint‑on‑terminate) and periodic‑checkpoint
advances, plus delete‑on‑reset. Resuming/Running/Suspending are skipped, so
**resume does zero etcd writes**. Recovery is provably equivalent (a wiped Running
session falls back to its last snapshot regardless). Debounce/batch (below) was not
needed once Running was dropped. Verified live.

- **Only mirror durability‑critical transitions.** `Suspended` (+ its `snapshotURI`)
  is the one that *must* survive a Valkey wipe; the intermediate `Resuming`→`Running`
  flips are reconstructible (a lost Running session just re‑resumes). Mirroring only
  Suspended (and Absent/delete) cuts mirror writes to a fraction of transitions.
- Optionally **batch/debounce** mirrors under burst (coalesce rapid transitions of
  the same session into one status write).
- Net: keeps the durability guarantee (Suspended is what needs it) while removing
  most of the new etcd write pressure.

### 5.5 EndpointSlice‑based worker discovery (removes the per‑pool worker scan)

Already noted as a future item in the TDD. Watch a per‑pool headless Service's
EndpointSlices instead of scanning `worker:*` per pool for status: readiness is
precomputed, membership is a watch event, and `CountWorkers` becomes a cache read
rather than a full scan per reconcile. Removes 2.2's per‑pool cost and the
churn‑driven rescan.

### 5.6 (Later) horizontal sharding

If a single leader operator's write‑serialization becomes the ceiling after 5.1–5.5,
the escape hatch is sharding sessions across namespaces/operators (the tabled
multi‑namespace work). Called out for completeness; not this PRD.

## 6. Observability first (prerequisite for gating)

Before building 5.3–5.5, add the metrics that tell us *when* to: sweeper pass
duration + sessions‑scanned‑vs‑acted, Valkey op rate, operator reconcile rate +
`CountWorkers` cost, and etcd/apiserver write QPS attributable to the mirror. Gate
each fix on its metric crossing a threshold under real load. (Some gauges exist —
pool workers; the rest are new.)

## 7. Effort / sequencing

- **Now, trivial:** 5.1 (skip checkpoint scan when off) + 5.2 (interval config) —
  small, safe, immediate headroom.
- **When metrics warrant:** 5.4 (mirror coalescing — cheap, high‑value for etcd) →
  5.3 (suspend indexing — the structural fix) → 5.5 (EndpointSlice discovery —
  larger).
- **Only if still limited:** 5.6 (sharding).

## 8. Acceptance

Under a synthetic busy‑cluster load (target N and churn TBD from §6 metrics):
sweeper cost is proportional to sessions *due* not *total*; etcd write QPS from the
mirror stays within an agreed budget; per‑pool reconcile cost doesn't grow with
worker count; no regression in idle‑suspend timeliness, durable recovery, or pool
status freshness.

## 9. Open questions

| # | Question | Leaning |
|---|----------|---------|
| Q1 | Do 5.1 + 5.2 now, or bundle everything behind metrics? | Do 5.1/5.2 now (trivial, obvious waste); gate the rest on §6 metrics. |
| Q2 | Mirror only `Suspended`, or all transitions? | Only durability‑critical (Suspended/Absent) + debounce; Running is reconstructible. Confirm no consumer needs live Running in etcd. |
| Q3 | Suspend index: ZSET in Valkey, or derive from the (future) durable store? | ZSET in Valkey — it's the hot path; keep it where the sweeper already reads. |
| Q4 | Target scale to design for (N sessions, churn/s, pools)? | Set from §6 once we can measure; don't guess a number into the design. |
| Q5 | EndpointSlice discovery now or later? | Later — larger change; the scans aren't the first limit (etcd + O(N) sweeps are). |
