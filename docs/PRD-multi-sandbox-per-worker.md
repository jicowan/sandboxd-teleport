# PRD (stub) — per-sandbox resource limits → multiple sandboxes per worker

Status: **Idea / not scheduled.** Captured 2026-07-22 while building generic pools
(see [PRD-arbitrary-image-sessions.md §13](./PRD-arbitrary-image-sessions.md)). This
is a placeholder to hold the idea; it is **not** a decision-ready spec yet.

## The idea

Today sandboxd runs **exactly one nested gVisor sandbox per worker pod** (the
worker-vs-sandbox model): kube-scheduler places the worker pod using
`SandboxTemplate.spec.resources`, and the single sandbox inside effectively gets the
whole worker's CPU/mem. The `/run` API carries **no** per-sandbox resource limits, and
`resources` is a *pool/worker-shape* property (it sizes the pod, and is the scheduler's
bin-packing input) — not a per-workload knob.

If we added **per-sandbox cgroup limits**, one (larger) worker could host **multiple**
independent sandboxes, each capped — raising density (more sessions per pod) the way
Agent Substrate multiplexes actors onto workers. This is intriguing precisely because
the current 1:1 model leaves a worker's spare capacity unused when its sandbox is small
(e.g. a tiny redis on a 500m/1Gi worker).

## Why it's a real feature (not a tweak)

It touches several load-bearing assumptions and must be designed carefully:

- **Worker API:** `RunRequest`/`RestoreRequest` gain resource fields; the worker passes
  cgroup/`runsc` limits (`--cpu-max`, memory cgroup, etc.) per sandbox.
- **Where the limit lives:** at that point a per-sandbox limit *is* a per-workload
  property with no placement implication, so it would belong on the **`AppTemplate`**
  (unlike pod `resources`, which stays on `SandboxTemplate`). This is the clean
  distinction: pod-shape = SandboxTemplate; per-sandbox cap = AppTemplate.
- **Assignment model:** `ClaimIdleWorker` assumes one sandbox per worker (a worker is
  `idle` or `busy`). Multi-tenancy per worker breaks that — the KV model needs
  per-worker capacity accounting (remaining CPU/mem, N slots), and the WarmPool
  `minIdle`/replica math changes from "count workers" to "count/size free slots."
- **Isolation & noisy-neighbor:** multiple tenants' sandboxes on one privileged worker
  pod — gVisor is still the boundary, but blast radius, fairness, and the
  checkpoint-on-terminate / teleport story (does one sandbox's checkpoint move while
  others keep running?) all need thought.
- **Routing:** unchanged in spirit (still session-keyed), but multiple sandboxes now
  share a worker pod IP → the existing per-sandbox interior-IP/DNAT scheme must
  distinguish N sandboxes on one worker (it already uses a stable interior IP per
  sandbox; verify it generalizes to N).
- **Scale-in / reclaim / GC:** a worker isn't free until *all* its sandboxes are gone;
  pod-deletion-cost and the worker-binding reclaim sweep become per-slot.

## Open questions (for when this is picked up)

1. Density target — how many sandboxes per worker, and how are worker sizes chosen?
2. Overcommit policy — requests vs limits per sandbox; do we oversubscribe CPU?
3. Does the KV assignment table move to a slot/capacity model, or a second index?
4. How does teleport/checkpoint interact with co-tenant sandboxes on the same worker?
5. Relationship to Substrate's multiplexing — is this the same bet, and worth it on EKS?

## Not now

Deferred. The generic-pool work (AppTemplate + `Session.appRef`) ships the
one-sandbox-per-worker model first; this PRD is the natural follow-on if per-worker
density becomes a goal.
