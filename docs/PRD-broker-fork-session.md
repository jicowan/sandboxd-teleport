# PRD — example broker: `fork_session` (agent‑driven sandbox forking)

Status: **Proposed** (decision‑ready; grounded in `broker/broker_sandboxd.py` and the
shipped control plane). **Scope: reference / example, not core product** — see §1.
Depends on: [PRD-on-demand-suspend.md](PRD-on-demand-suspend.md) (the suspend trigger
this composes) and [PRD-snapshot-fork.md](PRD-snapshot-fork.md) (the ForkSet/BaseSnapshot
primitives). Related: [architecture-broker.md](sandboxd/architecture-broker.md).

## 1. Scope: this is an EXAMPLE, not the product

The sandboxd **operator + router + agent** are the generic product; the **broker in
this repo is a reference example** of a front door. The product theory: customers bring
(or build) their **own identity broker** against the sandboxd primitives, and *how*
forking is exposed to end users is the **customer's** decision. The **product API for
forking is the CRDs** (`ForkSet`, `BaseSnapshot`) + on‑demand suspend
(`Session.spec.suspendRequest`) + RBAC + admission — reached through the Kubernetes
apiserver, which already provides authentication/authorization/audit.

Therefore this PRD builds `fork_session` **to demonstrate one wiring**, not to be the
canonical interface. It is deliberately kept as thin composition over the CRDs so a
customer can read it as a worked example. **No new operator endpoints** (no `/fork`);
everything the broker does is a Kubernetes CR write via a scoped ServiceAccount.

## 2. The flow it implements

The target user experience:

1. A user asks their agent to **fork their sandbox**, specifying **N**.
2. The current sandbox is **checkpointed** (it may stop running).
3. The broker creates a **ForkSet** from that fresh checkpoint.
4. The agent gets back the **N fork session ids**.
5. The user issues commands against **individual forks by id**.

Steps 2–4 are the broker composing existing primitives; step 5 is per‑call routing
(increment 2). Multi‑tenancy is **out of scope** — single namespace, multiple users,
same tenant.

## 3. Background — the broker today

`broker/broker_sandboxd.py` is a near‑transparent MCP reverse proxy behind
Keycloak/agentgateway:
- Validates the user JWT (`_authenticate`: iss/aud/azp/group), derives **one durable
  session id per principal** (`_sid_for` → `sess-<principal>-<hash>`), and forwards MCP
  to the router with `X-Session-ID` + `X-Session-Pool`.
- Answers `initialize`/`notifications`/`ping` locally; **forwards everything else**
  (`tools/list`, `tools/call`, …) untouched — it advertises **no tools of its own**.
- **Has no Kubernetes client** (deliberate: docstring "No SandboxClaim / no k8s client").
- Per‑principal quota exists only conceptually (`MAX_SESSIONS_PER_USER`).

So forking needs: a k8s client (to write CRs), two new MCP tools, and — for step 5 — a
way to address a specific fork over the one MCP connection.

## 4. Design

### 4.1 Kubernetes client + scoped RBAC
The broker gains an in‑cluster Kubernetes client and a dedicated **ServiceAccount** +
**namespaced Role** granting only:
- `create, get, list, delete` on `forksets` and `basesnapshots`,
- `get, patch` on `sessions` (to write `spec.suspendRequest`),

in the broker's **own namespace only** — nothing else, no ClusterRole. A fully
compromised broker can then only create forks / request suspends in that one namespace,
bounded further by CEL admission on `ForkSet` (absolute `count` cap). This crosses the
broker's current "no k8s client" line deliberately; it is the price of the broker being
a front door that provisions.

### 4.2 `fork_session` tool (increment 1 — fire‑and‑return)
Advertised by **injecting it into the `tools/list` response** (the broker intercepts
that response and adds `fork_session` to the returned tools — an extension of the
existing `_rewrite_init_version` response‑rewriting). A `tools/call` with
`name == "fork_session"` is **handled locally** (not forwarded); all other tool calls
forward as today.

Arguments: `{ count: int }` (v1 forks the caller's *current* session; `from` is
implicitly "current" — no source menu needed, no discovery primitive).

Handler steps (all CR writes; poll for completion):
1. **Authz** (§4.4) — eligibility + per‑principal fork quota. Reject early.
2. **Checkpoint the caller's session**: patch the caller's `Session.spec.suspendRequest`
   with a fresh opaque token; poll `status.lastSuspendHandled == token &&
   status.snapshotURI != ""` (the durable‑snapshot signal from
   [PRD-on-demand-suspend.md]).
3. **Promote**: create `BaseSnapshot{sourceSessionRef: <caller sid>}`; poll
   `status.ready == true` (promotes the now‑Suspended session — works today).
4. **Fork**: create `ForkSet{baseRef: <base>, count: N, pool: <caller's pool>}`; poll
   `status.forks` (the N child session ids).
5. **Return** the ids as the MCP tool result (JSON: `{ forks: [...], base: <name> }`).

The caller's own session **resumes on its next request** (normal reactive
restore‑on‑connect) — the suspend was a one‑shot checkpoint, not a standing state.

**Increment 1 stops here**: the agent receives the ids but drives forks out‑of‑band (or
via increment 2). This lets us validate fork creation + authz end‑to‑end before touching
per‑call routing.

### 4.3 `target` routing (increment 2 — drive a fork by id)
So the user/agent can issue commands against a specific fork over its single MCP
connection:
- **Advertise** an optional `target` parameter by injecting it into the input schema of
  the forwarded tools in `tools/list` (so the model knows it can set it).
- **Route per call**: on a `tools/call`, if `params.arguments.target` is present **and is
  one of THIS principal's fork ids** (validated — you can't target another user's
  session), forward with `X-Session-ID: <target>` instead of the principal's default sid,
  and **strip `target` from the arguments** before forwarding (the sandbox workload
  doesn't understand it). Absent/invalid `target` → default session (current behavior).

This is the "agent reuses the returned ids to route to a specific fork" capability. It's
LLM‑reliability‑dependent (the model must set `target`) — acceptable for interactive
steering; not a hard guarantee.

### 4.4 Authorization (example‑grade)
Before creating anything, the broker checks:
- **Eligibility** — principal is in a fork‑entitled group (e.g. `sandbox-fork` /
  `sandbox-power`); else the tool is filtered out of `tools/list` and/or the call is
  refused.
- **Per‑principal fan‑out quota** — count this principal's existing live fork children
  (label‑select `sandboxd.io/forkset` owned by this subject) and cap
  (`count ≤ MAX_FORKS_PER_REQUEST`, live forks `≤ MAX_CONCURRENT_FORKS_PER_SUBJECT`);
  env‑configured, mirroring `MAX_SESSIONS_PER_USER`.

**This is EXAMPLE code.** A real customer broker owns its own per‑user policy tied to
its identity model. The **operator's** real, bypass‑proof guardrails are RBAC (§4.1) +
CEL admission on `ForkSet` (absolute caps) — those hold regardless of what any broker
does. The broker never asserts trust the operator relies on for absolute safety.

## 5. What this deliberately does NOT do
- **No `from: <base>` / `from: image` menu** or `list_fork_sources` discovery — v1 forks
  "current" only (the sole source needing no discovery). Base/image forks remain possible
  by writing a `ForkSet` directly; they're just not broker sugar.
- **No operator/router/worker/`/resume` change** — consumes on‑demand‑suspend + the
  ForkSet CRDs only.
- **No multi‑tenancy** — single namespace (the broker's own).
- **No claim of being the product interface** — see §1.

## 6. Dependencies / sequencing
1. **[PRD-on-demand-suspend.md]** must land first (the broker cannot checkpoint the
   caller's live session without `spec.suspendRequest`). Hard dependency.
2. **[PRD-snapshot-fork.md]** primitives (`ForkSet`, `BaseSnapshot`) — already
   implemented + live.
3. Then broker **increment 1** (k8s client + RBAC + `fork_session` fire‑and‑return),
   then **increment 2** (`target` routing).

## 7. Testing / acceptance
1. **RBAC scoping:** the broker SA can create/list/delete forksets+basesnapshots and
   patch sessions in its namespace, and **nothing else** (negative test: cannot touch
   pods/secrets/other namespaces).
2. **`fork_session` advertised:** `tools/list` includes `fork_session` for an eligible
   principal; absent for an ineligible one.
3. **Fire‑and‑return (incr 1):** call `fork_session{count:3}` on a live session → caller
   is checkpointed (Suspended + snapshot), a BaseSnapshot promotes, a ForkSet yields 3
   ids, the tool returns them; the caller's session resumes on its next request.
4. **Quota:** a call exceeding `MAX_FORKS_PER_REQUEST`, or a principal over the
   concurrent cap, is refused with a clear error.
5. **`target` routing (incr 2):** a `tools/call` with `target: <fork‑id>` reaches that
   fork's worker (verify via a per‑fork marker); `target` is stripped before forwarding;
   an invalid/foreign `target` is rejected or falls back to the default session.
6. **End‑to‑end (the §2 flow):** user asks agent to fork N → ids returned → agent drives
   each fork by id and observes divergent, isolated state.

## 8. Effort estimate
Medium, broker‑only (after the on‑demand‑suspend dependency lands):
- k8s client dep + in‑cluster config + SA/Role/RoleBinding manifests.
- `tools/list` response injection + `tools/call` local handling for `fork_session`.
- The orchestration (patch suspend → poll → BaseSnapshot → poll → ForkSet → poll →
  return), reusing the broker's existing async httpx/poll style.
- Example authz (env‑config quotas + group check).
- Increment 2: per‑call `target` routing + schema injection + arg stripping.

## 9. Open questions
| # | Question | Leaning |
|---|----------|---------|
| Q1 | Should the caller's own session become one of the N forks, or just resume? | Just resume (simplest, least surprising); the N forks are new sessions. Revisit if "replace me with a fork" is wanted. |
| Q2 | `fork_session` synchronous (block until forks Ready) or return ids immediately? | Return ids as soon as `ForkSet.status.forks` is populated; don't block on all N reaching Running (they warm in the background; `activation` controls eagerness). |
| Q3 | Cleanup — who deletes the ForkSet/forks when the user is done? | v1: a `close_fork`/`unfork` tool + idle GC on the forks (idleAction=reset). Out of scope for the first cut beyond noting it. |
| Q4 | Increment 1 and 2 in one release or staged? | Staged — ship fire‑and‑return, validate, then add routing (per the ForkSet build rhythm). |

## 10. Status
**Proposed — nothing built.** Blocked on [PRD-on-demand-suspend.md]. This is the
reference‑example front‑door layer over the (implemented) ForkSet/BaseSnapshot
primitives; the product contract remains the CRDs. See [[sandboxd-forkset-followups]]
(memory) for the cross‑session tracker.
