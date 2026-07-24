# ForkSet example — reinforcement-learning parallel rollouts

The other example (`checkpoint-restore/controlplane/deploy/aio/`) is **on-demand**:
a user sends a request through an MCP client, a session is created/resumed, the
request is processed, a response comes back. This example is the **other shape** —
**fan-out for reinforcement learning**, driven by a **`ForkSet`**.

In RL, a *rollout* is "a complete sequence of interactions between an agent and its
environment for a single training episode" — observe → act → reward → transition
until the episode ends. Training "commonly runs hundreds to tens of thousands of
parallel environments," each of which must be **isolated** (a failed rollout must not
corrupt its neighbors) and **reset cleanly** between episodes. A common optimization
is to **"snapshot that state at checkpoints, allowing the environment to fork from a
known point rather than rebuilding from scratch."**
(<https://northflank.com/blog/reinforcement-learning-agents-in-secure-sandboxes>;
the branch-from-a-common-state idea also underpins SWE-agent-style env fan-out,
[arXiv:2602.11210](https://arxiv.org/html/2602.11210v3).)

That maps 1:1 onto sandboxd's `ForkSet` (see
[docs/sandboxd/PRD/PRD-snapshot-fork.md](../../docs/sandboxd/PRD/PRD-snapshot-fork.md)):

| RL concept | sandboxd |
|---|---|
| Environment | a **Session** (nested gVisor sandbox running your env server) |
| Per-episode state, isolated | each fork = own session id → own worker → own netns/IP → own S3 checkpoint lineage |
| Fork from a golden checkpoint | **snapshot fork** — `BaseSnapshot` + `ForkSet{baseRef}` |
| Fresh env per rollout | **app fork** — `ForkSet{appRef}` (independent cold-start, no base) |
| Ephemeral vs persistent env | `lifecycle.idleAction: reset` vs `suspend` |
| Reset to a baseline | the env's own `POST /reset`, or reset-on-idle |

## No broker for RL

Unlike the on-demand example, the RL trainer talks **directly to the router**, not
through the broker. The broker derives one durable session id **per user** — that's
right for interactive callers but wrong for fan-out, where one trainer drives *N*
distinct env sessions at once. So the trainer holds the N fork session ids (from
`ForkSet.status.forks`) and addresses each by **`X-Session-ID`** straight to the
router, which resolves each id to its worker independently. The router is a plain
1:1 session→worker proxy — no fan-out/broadcast in the data plane; parallelism is the
trainer's job (open N streams). See [PRD §5.7](../../docs/sandboxd/PRD/PRD-snapshot-fork.md).

The env here also speaks **plain HTTP** (`/step`, `/state`, `/reset`), not MCP — the
router is protocol-agnostic and forwards any path to the workload port, so RL needs
neither the broker nor MCP.

## Files

| File | What |
|------|------|
| `00-rl-env-apptemplate.yaml` | The RL **environment** as an `AppTemplate` — a tiny stock-image HTTP env server (`/step`, `/reset`, `/state`), nothing to build. Swap for your real env. |
| `10-rl-pool.yaml` | A **generic pool** (`rl-generic` SandboxTemplate + `rl-pool` WarmPool) to place forks on. |
| `20-forkset-app.yaml` | **App fork:** `ForkSet{appRef, count:8}` → 8 **independent** env cold-starts. The cheap path (no base). |
| `30-golden-session.yaml` | **Snapshot fork, step 1:** the golden source `Session`. |
| `40-basesnapshot.yaml` | **Snapshot fork, step 2:** promote the suspended golden session to a `BaseSnapshot`. |
| `50-forkset-snapshot.yaml` | **Snapshot fork, step 3:** `ForkSet{baseRef, count:8}` → 8 forks of the **identical** golden state. |

## Prerequisites

- The sandboxd control plane is up (operator, router, Valkey) — see
  [`../../checkpoint-restore/controlplane/deploy/smoke/controlplane.yaml`](../../checkpoint-restore/controlplane/deploy/smoke/controlplane.yaml)
  and [docs/sandboxd/install-guide-sandboxd.md](../../docs/sandboxd/install-guide-sandboxd.md).
- gVisor worker nodes labeled `sandbox=gvisor` (+ the matching taint toleration).
- Set `workerImage` in `10-rl-pool.yaml` to your pushed sandboxd worker image (or rely
  on the operator's `--worker-image` default).
- An in-cluster shell to reach the router's ClusterIP (examples use a `toolbox` pod;
  no ingress/ALB needed — this is all cluster-internal):
  ```sh
  kubectl run toolbox --image=curlimages/curl -n default --command -- sleep infinity
  ```

`ROUTER=http://sandboxd-router.sandboxd-controlplane-system:8080` in the commands below.

---

## Path A — app fork (independent trials, no golden state)

Use when the common start is just "freshly booted," or you *want* independently
initialized trials ("ephemeral environments, created fresh for each rollout and
discarded after the episode").

```sh
kubectl apply -f 00-rl-env-apptemplate.yaml
kubectl apply -f 10-rl-pool.yaml
kubectl apply -f 20-forkset-app.yaml

# The ForkSet reaches Ready immediately — with activation: Lazy the N children are
# CREATED (born Absent), not yet materialized. Read back their ids:
kubectl get fork rl-trials -n default -o wide
kubectl get fork rl-trials -n default -o jsonpath='{.status.forks}{"\n"}'
# -> ["sess-fork-rl-trials-0", … "sess-fork-rl-trials-7"]
```

Each fork cold-starts **on first contact** (it doesn't hold a worker until you drive
it). Address a fork directly through the router by its `X-Session-ID`:

```sh
# First request materializes the fork (cold start ~a few seconds), then it responds.
# Observe fork 0, step it, observe fork 3 — each is an independent environment:
kubectl exec toolbox -n default -- sh -c '
R=http://sandboxd-router.sandboxd-controlplane-system:8080
curl -s -H "X-Session-ID: sess-fork-rl-trials-0" $R/state
curl -s -H "X-Session-ID: sess-fork-rl-trials-0" -H "Content-Type: application/json" -d "{\"a\":5}" $R/step
curl -s -H "X-Session-ID: sess-fork-rl-trials-3" $R/state'
```

Each fork returns a **different `boot`** id and its own independent `step`/`sum` —
confirming they were initialized independently (no shared frozen state). Mutating
fork 0 never affects fork 3.

Because the forks are Lazy and `idleAction: reset`, only the forks you actually drive
hold a worker, and each frees its worker (no snapshot) after `idleTimeoutSeconds` of
inactivity — so a small pool serves far more trials than it has workers (temporal
oversubscription). Watch `kubectl get warmpool rl-pool` `BUSY` rise as you drive forks
and fall as they idle out.

Tear down the batch:

```sh
kubectl delete fork rl-trials -n default
```

---

## Path B — snapshot fork (branch from a golden state)

Use when the common state is **expensive to reach** (loaded model, primed cache, repo
+ deps installed, an agent N steps into a task) or the branch point *is* a specific
reached state.

### 1. Create the golden session and drive it to a golden state

```sh
kubectl apply -f 00-rl-env-apptemplate.yaml   # if not already applied
kubectl apply -f 10-rl-pool.yaml              # if not already applied
kubectl apply -f 30-golden-session.yaml

# A declaratively-created Session doesn't auto-start — warm it via the router, then
# drive it to the golden state (here: accumulate sum=42). Address it by its id.
kubectl exec toolbox -n default -- sh -c '
R=http://sandboxd-router.sandboxd-controlplane-system:8080
curl -s -o /dev/null -w "warm:%{http_code}\n" -X POST \
  -H "X-Session-ID: rl-golden" -H "X-Session-Pool: rl-pool" -H "X-Session-App: rl-env" $R/_warm
curl -s -H "X-Session-ID: rl-golden" -H "Content-Type: application/json" -d "{\"a\":42}" $R/step
curl -s -H "X-Session-ID: rl-golden" $R/state'      # -> sum:42, note the boot id
```

### 2. Checkpoint the golden state (on-demand suspend) and promote a BaseSnapshot

```sh
# Edge-triggered, one-shot suspend: set an opaque token. The operator checkpoints ->
# S3, suspends the session, frees the worker, then records the watermark.
kubectl patch session rl-golden -n default --type merge \
  -p '{"spec":{"suspendRequest":"golden-v1"}}'

# Wait until the checkpoint is durable BEFORE promoting (watermark caught up + snapshot):
kubectl get session rl-golden -n default \
  -o jsonpath='{.status.lastSuspendHandled} {.status.snapshotURI}{"\n"}'
# -> "golden-v1 sandboxes/rl-golden/snap-…"   (both set = safe to promote)

# Promote: S3 server-side copy to a fork-stable bases/rl-golden-v1/ prefix.
kubectl apply -f 40-basesnapshot.yaml
kubectl get basesnap rl-golden-v1 -n default        # wait for READY=true
```

### 3. Fan out N forks from the golden base

```sh
kubectl apply -f 50-forkset-snapshot.yaml
kubectl get fork rl-rollouts -n default -o wide
kubectl get fork rl-rollouts -n default -o jsonpath='{.status.forks}{"\n"}'
```

With `activation: Lazy` the 8 forks are created **Suspended** (each pre-seeded with the
base's snapshot); none holds a worker yet. A fork **restores from the golden base on
first contact**. Drive one to see it restore the identical state, then diverge it:

```sh
kubectl exec toolbox -n default -- sh -c '
R=http://sandboxd-router.sandboxd-controlplane-system:8080
# First request to fork 0 restores it from the base (~seconds). It comes up at the
# GOLDEN state — sum=42, SAME boot id as the base (identical frozen RAM+FS):
curl -s -H "X-Session-ID: sess-fork-rl-rollouts-0" $R/state
# Drive fork 0 and fork 1 differently — the branch:
curl -s -H "X-Session-ID: sess-fork-rl-rollouts-0" -H "Content-Type: application/json" -d "{\"a\":1}"   $R/step
curl -s -H "X-Session-ID: sess-fork-rl-rollouts-1" -H "Content-Type: application/json" -d "{\"a\":100}" $R/step
# fork 0 -> sum=43, fork 1 -> sum=142: same golden start, independent futures.'
```

`kubectl get basesnap rl-golden-v1 -o wide` shows `REFS` dropping as forks finish
their first restore (a fork needs the base only for that first restore). The base is
retained while `PINNED=true`.

### 4. Checkpoint each rollout when its episode completes (temporal oversubscription)

This is the point of `activation: Lazy` + `idleAction: suspend`: a rollout runs its
episode, then **checkpoints and frees its worker** so the next fork can materialize on
it — so you need only a handful of workers to serve all N rollouts, not one per fork.
Two ways to free a finished rollout:

```sh
# (a) Automatic: after idleTimeoutSeconds (60s here) of no requests, the fork idle-
#     suspends — checkpoint -> its own sandboxes/<forkId>/ lineage, worker freed.
#     Watch BUSY fall as rollouts idle out:
kubectl get warmpool rl-pool -n default -w   # Ctrl-C when BUSY drops

# (b) Explicit "episode done, checkpoint now" — the harness sets a suspendRequest per
#     fork the moment its episode ends (no waiting for the idle timer):
kubectl patch session sess-fork-rl-rollouts-0 -n default --type merge \
  -p '{"spec":{"suspendRequest":"ep-done-0"}}'
kubectl get session sess-fork-rl-rollouts-0 -n default \
  -o jsonpath='{.status.phase} {.status.snapshotURI}{"\n"}'   # -> Suspended sandboxes/…/snap-…
```

A suspended rollout **teleport-resumes** on its next request (its own lineage, not the
base) — so the trainer can revisit a rollout later. And a rollout that reached an
interesting state can be **promoted to a new `BaseSnapshot`** and forked again (nested
fork trees). Because only actively-driven rollouts hold workers, a pool of, say, 4
workers can cycle through all 8 (or 800) rollouts.

### Cleanup

```sh
kubectl delete fork rl-rollouts -n default
kubectl delete basesnap rl-golden-v1 -n default    # finalizer reclaims bases/rl-golden-v1/
kubectl delete session rl-golden -n default
```

> **Note (see [Findings](#findings--control-plane-issues-surfaced-by-this-example) below):**
> deleting a `ForkSet` today removes the child `Session` CRs but does **not** yet
> release their Valkey worker bindings, so workers can stay `busy` until manually
> cleared. Prefer letting rollouts idle-suspend (freeing workers cleanly) over bulk CR
> deletion while a fix lands.

---

## Scaling notes

- **Temporal oversubscription is the model.** With `activation: Lazy` +
  checkpoint-when-done (`idleAction: suspend`/`reset`, or explicit `suspendRequest`),
  only *actively-driven* rollouts hold workers. A small pool serves many more rollouts
  than it has workers — run an episode, checkpoint (free the worker), the next fork
  materializes on it. This mirrors how RL fleets run hundreds–tens of thousands of envs
  on far fewer workers.
- **`Eager` needs ~N workers at once** and issues N simultaneous restores/cold-starts.
  Prefer `Lazy` for large fan-outs; reserve `Eager` for small batches you want pre-warm.
- **`count` is capped at 256 per ForkSet** (apiserver-enforced blast-radius guard).
  Real RL runs use many ForkSets / larger pools; a per-subject aggregate quota is a
  front-door concern, not enforced here (PRD §6).
- **Capacity / backpressure:** a fork with no idle worker gets `503 Retry-After` until
  one frees; size `replicas`/`minIdle` to your expected *concurrent* (not total) rollouts.
- **Determinism caveat (snapshot forks only):** N copies restored from one checkpoint
  share anything the workload froze in as "unique" (a nonce, an open outbound
  connection). Quiesce external I/O before checkpointing a base. App forks are exempt —
  each boots independently. See PRD §7.
- **Where fork state lives is a workload concern:** a fork restores whatever the env
  persisted where the checkpoint captures it (process RAM + rootfs). Put episode state
  somewhere the checkpoint sees it (this example keeps it in process memory).

## Findings — control-plane issues surfaced by this example

Running this example live surfaced two control-plane gaps (being fixed; tracked in
[docs/sandboxd/PRD/PRD-snapshot-fork.md](../../docs/sandboxd/PRD/PRD-snapshot-fork.md)):

1. **CR deletion doesn't release the KV worker binding.** Deleting a `ForkSet` (or a
   child `Session` CR) removes the CRs but leaves the authoritative Valkey
   `session:<sid>` entry `Running` and its `worker:<pod>` `busy`, so workers aren't
   freed and the pool doesn't scale back down. Root cause: the Session controller has
   no delete-time finalizer, and the self-healing sweeps key on the KV entry (which the
   delete leaves intact), not on CR existence. Workaround until fixed: let rollouts
   idle-suspend/reset (which *does* release the worker) instead of bulk-deleting CRs.
2. **Burst-restore worker double-booking.** A large simultaneous `Eager` fan-out could
   bind two forks to the same worker (one wedges in `Resuming`), because the
   pick-idle-then-claim step isn't atomic across concurrent resumes. `activation: Lazy`
   (staggered materialization) avoids the burst and is the recommended pattern above.
