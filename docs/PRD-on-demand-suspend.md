# PRD — on‑demand session suspend (declarative, edge‑triggered)

Status: **Implemented + verified live** (2026‑07‑20, operator `v29`). As built: 2
additive Session fields (`spec.suspendRequest` + `status.lastSuspendHandled`), an
exported `Suspender.SuspendNow` over the existing `suspendOne`, and a new
`SessionReconciler` (the first controller to watch Session). Live‑verified: patch
`suspendRequest` → Suspended + fresh snapshot + watermark advances; a marker survives
checkpoint/restore; a request resumes it and it STAYS Running (stale token doesn't
re‑suspend — the level‑vs‑edge guard); same token is a no‑op; a new token suspends
again. On‑demand checkpoints are GC'd identically to any suspended‑session snapshot.
Commits `73d225a` (PRD) + `021ae1e` (impl). Related:
[PRD-snapshot-fork.md](PRD-snapshot-fork.md) (the primary consumer),
[PRD-durable-assignment-state.md](PRD-durable-assignment-state.md),
[architecture-sandboxd.md](sandboxd/architecture-sandboxd.md),
[admin-guide-crds.md](sandboxd/admin-guide-crds.md).

## 1. Summary

Add a way to **checkpoint‑and‑suspend a running session on demand** — a deliberate
"save my state now, the sandbox may stop running" operation — expressed
**declaratively on the `Session` CR** and reconciled by the operator. Today suspend
only happens two ways: the **idle sweeper** (inactivity timeout) and
**checkpoint‑on‑terminate** (worker pod death). Neither is callable on request, so
there is no way for a client (or the example broker) to say "checkpoint this session
now."

The trigger is a **one‑shot, edge‑triggered request** carried by a new spec field
(`spec.suspendRequest`) with a matching status watermark (`status.lastSuspendHandled`),
**not** a level‑triggered desired‑state. This is deliberate (see §3): resume is
demand‑driven (a request to the router reactively restore‑on‑connects a suspended
session), so a standing `desiredState: Suspended` would fight reactive resume and
re‑suspend a session the user is actively using. Suspend is the *one* control signal
the demand‑driven model cannot express on its own; this adds exactly that, nothing
more.

**Primary motivation:** the "fork my sandbox" flow ([PRD-snapshot-fork.md], and the
example broker's `fork_session`): a user asks their agent to fork their current
sandbox → the sandbox is checkpointed (may stop running) → the broker promotes that
checkpoint to a `BaseSnapshot` → forks N sessions from it. That flow needs a way to
checkpoint the caller's live session on demand, and needs a **reliable completion
signal** ("the snapshot is durable, safe to promote"). The watermark provides it.

## 2. Goals / non‑goals

### Goals
- A client that can write a `Session` CR can request an **immediate checkpoint +
  suspend** of that session (checkpoint → S3, mark `Suspended`, free the worker).
- The request is **edge‑triggered and idempotent** — performed once per distinct
  request, safe across operator restarts / reconcile replays / client retries.
- A **reliable, race‑free completion signal**: the requester can tell when the
  snapshot is durably in S3 (so the fork flow can promote it) — distinct from "the
  session happens to be Suspended for some other reason."
- **Generic** — this is a session‑lifecycle primitive ("save/checkpoint on demand"),
  not fork logic. Forking merely consumes it. No fork‑specific coupling in the
  operator.
- **Broker touches only CRs** — the trigger is a CR write, consistent with the
  "CRDs are the API, no bespoke operator endpoints" model (the operator's only HTTP
  endpoint stays `/resume`; the broker never calls it).

### Non‑goals
- **Not** a level‑triggered lifecycle state machine (`spec.desiredState`) — that
  would fight reactive resume + the idle sweeper (§3). We add one explicit override,
  not a full declarative lifecycle.
- **Not** making `/_warm` declarative — resume is already demand‑driven; `/_warm` is
  a pre‑warm *optimization* on the reactive path, not session control. Left as‑is.
- **Not** a new operator HTTP endpoint (no `/_suspend`, no `/fork`). The trigger is
  a `Session` CR field.
- **Not** "pin this session running / never idle‑suspend" — that's a separate
  feature (a standing desired‑state), explicitly out of scope.
- **Not** changing the router or worker. The worker's `/checkpoint` + suspend
  mechanics already exist; this only adds a new *trigger* into the existing operator
  suspend path.

## 3. Why edge‑triggered, not level‑triggered (the crux)

Resume in sandboxd is **demand‑driven**: when a request reaches the router for a
Suspended (or Absent) session, the router reactively resumes it (restore‑on‑connect
via the operator `/resume`). Traffic *is* the intent to run; `/_warm` just front‑runs
that cold start.

A level‑triggered `spec.desiredState: Suspended` therefore breaks:

```
broker sets desiredState=Suspended → operator suspends
  → user sends a request → router REACTIVELY RESUMES it (correct — they're using it)
  → but spec still says Suspended → controller RE-SUSPENDS the live session  ✗ fight
```

An **edge‑triggered one‑shot request** avoids this: the operator suspends exactly
once when a new request value appears, records that it handled that value, and then
normal reactive resume takes over with no standing state to conflict with. If the
user immediately sends traffic, the session resumes and stays running — the suspend
request was a point‑in‑time event, already satisfied.

## 4. API design

Two additive fields on the existing `Session` CRD (`core.sandboxd.io/v1alpha1`).

### `spec.suspendRequest` (string, optional)
An **opaque token** set by the requester to ask for an immediate checkpoint+suspend.
Any change to a new, non‑empty value the operator hasn't handled triggers one suspend.
Opaque (not a counter) so the client needs no read‑modify‑write — it writes a value it
already has (a uuid, a timestamp, the fork request id). Empty/unset = no request.

### `status.lastSuspendHandled` (string, optional)
The **watermark**: the `suspendRequest` value the operator most recently completed —
set **only after the checkpoint is durably in S3 and the session is marked Suspended.**
The operator (sole status writer) advances it. `spec.suspendRequest != status.lastSuspendHandled`
⇒ a suspend is pending/in‑flight; equal ⇒ done.

```yaml
apiVersion: core.sandboxd.io/v1alpha1
kind: Session
metadata: { name: sess-alice, namespace: default }
spec:
  poolRef: { name: aio-pool }
  suspendRequest: "req-7f3a"        # requester writes an opaque token
status:
  phase: Suspended
  snapshotURI: sandboxes/sess-alice/snap-…
  lastSuspendHandled: "req-7f3a"    # operator: this request is complete (snapshot durable)
```

**Completion check (what the fork flow polls):** `status.lastSuspendHandled ==
spec.suspendRequest` **and** `status.snapshotURI != ""`. This is unambiguous and does
not rely on `status.phase` (which the idle sweeper / reactive resume also drive).

Optionally surface a condition (`type: SuspendRequest`, `Reason: Completed|InProgress|Failed`)
for human/`kubectl` visibility; the watermark is the machine signal.

## 5. Behavior / reconcile

A **new lightweight `SessionReconciler`** (there is none today — Sessions are only
lazy‑created + status‑mirrored; nothing watches them). It watches `Session` and, per
reconcile:

1. If `spec.suspendRequest == "" ` or `== status.lastSuspendHandled` → nothing to do.
2. Else a suspend is requested and not yet handled:
   - Read the KV entry (`assign.GetSession`). If the session is **not Running** (already
     Suspended/Absent/Suspending) → treat the request as satisfied: set
     `status.lastSuspendHandled = spec.suspendRequest` and return (idempotent — nothing
     to checkpoint; if already Suspended with a snapshot, the request's intent already
     holds).
   - If **Running** → invoke the operator's existing suspend path (see §6) with
     `action = suspend` (checkpoint → S3 → Suspended → free worker).
   - On success (snapshot durable, entry Suspended) → set
     `status.lastSuspendHandled = spec.suspendRequest`.
   - On failure → leave the watermark unchanged, surface a `Failed` condition, requeue
     (idempotent retry — the request value hasn't been marked handled).

**Idempotency + safety:**
- Edge‑triggered on `spec != status` → each distinct request value causes at most one
  suspend; replays/restarts re‑evaluate the same comparison and no‑op once handled.
- Uses the same **CAS‑on‑version** KV writes as every other suspend, so it can't
  split‑brain with a concurrent reactive resume: if a resume wins the race and the
  session is Running again, `suspendOne`'s `Suspending` CAS still applies to the
  current entry; if the user is actively driving it, the intended one‑shot has already
  fired once (acceptable — they asked to checkpoint at that moment).
- **Reactive resume is untouched** — after the watermark advances, a later request
  resumes the session normally; there is no standing state to re‑trigger suspend.

## 6. Implementation notes (grounded in current code)

- **Reuse the existing suspend mechanics — do not reimplement.** `resume/suspend.go`
  `Suspender.suspendOne(ctx, entry, "suspend")` already does exactly checkpoint → S3 →
  mark Suspended → free worker, with CAS + mirror + pool‑notify. It is currently
  **private** and reachable only via `SweepOnce` / `SuspendForTerminate`. Add a thin
  **exported entry point** (e.g. `Suspender.SuspendNow(ctx, sid) error` that loads the
  entry and calls `suspendOne(..., "suspend")`), so the new reconciler drives the same
  path the sweeper does. No behavior change to the sweeper.
- **New `SessionReconciler`** in `internal/controller` (watches `core.sandboxd.io/Session`),
  wired in `cmd/main.go` alongside the others. It needs the cached client (status
  writes) + the KV client + the `Suspender` entry point. Leader‑gated like the other
  reconcilers.
- **CRD change:** add `spec.suspendRequest` + `status.lastSuspendHandled` to
  `api/v1alpha1/session_types.go`; `make generate manifests`; apply the updated CRD.
  Both additive/optional → backward compatible.
- **No worker, router, or `/resume` change.**

## 7. Consumer: the example broker `fork_session`

The broker's fork orchestration (all CR writes, no endpoints):
1. Patch the caller's `Session.spec.suspendRequest` with a fresh token.
2. Poll `status.lastSuspendHandled == token && status.snapshotURI != ""` (the durable
   completion signal — the reason the watermark exists).
3. Create `BaseSnapshot{sourceSessionRef: caller‑sid}` → wait `Ready` (promotes the
   now‑Suspended session's snapshot — works with today's Suspended‑source promote).
4. Create `ForkSet{baseRef, count: N}` → wait `.status.forks` → return the ids.

The caller's own session resumes on its next request (normal reactive restore), or is
itself treated as one of the forks — a broker‑level choice, out of scope here.

## 8. Authorization

Writing `spec.suspendRequest` is an ordinary `Session` update → gated by **RBAC** on
`sessions` (the broker's scoped Role already needs `patch sessions` for this flow). The
operator does **not** authenticate an end user — it trusts whoever RBAC let write the
CR (the same trust model as every other CR‑driven action). Per‑user policy (who may
suspend/fork, how often) is the **caller's** responsibility (the example broker), not
the operator's — consistent with the ForkSet authz split (operator = absolute
guardrails via RBAC/CEL; caller = per‑subject policy).

## 9. Testing / acceptance

1. **Trigger:** patch `spec.suspendRequest` on a Running session → within a reconcile
   it is checkpointed, `status.phase == Suspended`, `status.snapshotURI` set, and
   `status.lastSuspendHandled == spec.suspendRequest`. (envtest + a live check.)
2. **Idempotent:** re‑reconcile / operator restart with the same `suspendRequest` value
   → no second suspend (watermark already equal). A *new* value → one more suspend.
3. **No fight with reactive resume:** after suspend completes, send a request → the
   session resumes and **stays Running** (the stale `suspendRequest` value is already
   handled, so it does not re‑suspend). This is the level‑vs‑edge regression guard.
4. **Already‑not‑Running:** `suspendRequest` on an already‑Suspended/Absent session →
   watermark advances, no error, no spurious work.
5. **Failure path:** checkpoint failure → watermark unchanged, `Failed` condition,
   requeue; a later successful reconcile completes it.
6. **Fork end‑to‑end (consumer):** broker `fork_session` → suspend‑request →
   BaseSnapshot promote → ForkSet → N ids; the caller's session resumes on next use.

## 10. Effort estimate

Small–medium, operator‑only:
- 2 additive CRD fields + generate/manifests.
- 1 exported `Suspender.SuspendNow` (thin wrapper over existing `suspendOne`).
- 1 new lightweight `SessionReconciler` (watch + compare + call + watermark) + main.go
  wiring + envtest.
No worker/router/`/resume`/broker‑protocol changes. The broker `fork_session` consumer
is separate work ([PRD-snapshot-fork.md] follow‑ups).

## 11. Open questions

| # | Question | Leaning |
|---|----------|---------|
| Q1 | `suspendRequest` opaque token vs monotonic int? | **Opaque token** — no client read‑modify‑write; equality on a string the client already has (uuid/ts/fork‑req‑id). |
| Q2 | Also expose a `SuspendRequest` status condition? | Yes, for `kubectl`/human visibility; the watermark stays the machine signal. |
| Q3 | Should the request also support `action: reset` (discard, no checkpoint)? | Not in v1 — suspend‑with‑checkpoint is the fork need; add `reset` later if a "discard now" use case appears. |
| Q4 | Does the caller's own session become one of the forks, or just resume? | Broker‑level decision (out of scope); default = it resumes on next request. |
| Q5 | Debounce rapid distinct `suspendRequest` values? | Not needed — each is one‑shot and cheap‑guarded; the CAS + not‑Running check absorb races. |

## 12. Status

**Proposed — nothing built.** The suspend *mechanics* already exist
(`Suspender.suspendOne`); this adds a declarative, edge‑triggered *trigger* (2 CRD
fields + a small SessionReconciler + an exported wrapper) so suspend is callable on
demand — the missing primitive the "fork my sandbox" flow needs. See
[[sandboxd-forkset-followups]] (memory) for the cross‑session tracker.
