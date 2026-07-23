# How to: filter sandbox tools per user, and configure quotas

This guide covers the two access-control knobs an operator turns:

1. **Tool filtering** — which MCP tools a user may see and call, per app
   (enforced at **agentgateway** via each route's `mcpAuthorization`).
2. **The create gate & quotas** — who may open which app (the **broker**'s per‑app
   group gate), and how much runs at once (**pool capacity**; plus the apiserver
   fork‑count cap).

Both are driven by the user's Keycloak identity in the JWT that agentgateway
passes through unchanged. Nothing here requires a client-side change.

---

## Part 1 — Filter which tools a user can use

### Where it lives

`deploy/30-agentgateway.yaml`, in a route's `policies.mcpAuthorization.rules` block.
agentgateway evaluates the rules against the **passed-through user JWT** on every
`tools/call`, and **also filters `tools/list`** — so a tool a user may not call is
also invisible to them (their client never even lists it).

> **Per app.** The gateway has one route per app (`aio-route` at `/aio/mcp`,
> `everything-route` at `/everything/mcp`, …), and `mcpAuthorization.rules` is set
> **on each route independently**. So tool filtering is per‑app: the tiered AIO
> ruleset below lives on `aio-route`; a different app (e.g. `everything`, whose tools
> aren't the AIO set) has its own rules on its own route — commonly a single
> group gate like `'"sandbox-power" in jwt.groups'`. Edit the rules on the route for
> the app you're tuning. (The app you're on is decided by which endpoint the client
> connected to — see [howto-add-an-app.md](howto-add-an-app.md).)

### How the rules work

- `rules` is a list of [CEL](https://github.com/google/cel-spec) expressions.
- They are **OR'd**: a tool call is allowed if **any** rule evaluates true.
- If no rule matches, the tool is denied (and filtered from `tools/list`).

Variables you can reference:

| Variable | Meaning | Example |
|---|---|---|
| `mcp.tool.name` | The bare tool name as published by the AIO hub | `"sandbox_execute_bash"` |
| `jwt.<claim>` | Any claim in the user's access token | `jwt.groups`, `jwt.sub`, `jwt.preferred_username` |
| `has(jwt.x)` | Test a claim exists before reading it | `has(jwt.groups)` |

> The `groups` claim is emitted by the `sandbox-groups` mapper on the `sandbox`
> client scope, as **bare names** (`full.path=false`) — so you match
> `"sandbox-power" in jwt.groups`, not `"/sandbox-power"`.

### The shipped policy (group-based tiers)

```yaml
mcpAuthorization:
  rules:
  # Power users: any tool (incl. shell + code execution).
  - '"sandbox-power" in jwt.groups'
  # Standard users: web browsing.
  - 'mcp.tool.name.startsWith("browser_")'
  # Standard users: non-exec sandbox tools.
  - 'mcp.tool.name == "sandbox_file_operations"'
  - 'mcp.tool.name == "sandbox_str_replace_editor"'
  - 'mcp.tool.name == "sandbox_convert_to_markdown"'
  - 'mcp.tool.name == "sandbox_get_context"'
  - 'mcp.tool.name == "sandbox_get_packages"'
  - 'mcp.tool.name == "sandbox_load_skill"'
  # sandbox_execute_bash / sandbox_execute_code are intentionally absent,
  # so only sandbox-power may see or call them.
```

Result, verified live:

| Tier (group) | tools/list | `sandbox_execute_bash` |
|---|---|---|
| `sandbox-users` (standard) | 30 tools, no exec | `-32602 Unknown tool` (denied) |
| `sandbox-power` | 32 tools, exec visible | runs (HTTP 200) |

### Recipes

**Restrict a tool to one user (by subject):**
```yaml
- 'jwt.sub == "7613ff43-..." && mcp.tool.name == "sandbox_execute_bash"'
```

**Gate the whole browser family behind a group:**
```yaml
- 'mcp.tool.name.startsWith("browser_") && "sandbox-browser" in jwt.groups'
- 'mcp.tool.name.startsWith("sandbox_")'   # sandbox_* for everyone
```

**Read-only tier (only non-mutating tools), everyone else full:**
```yaml
- '!("sandbox-readonly" in jwt.groups)'                 # full access
- 'mcp.tool.name == "sandbox_get_context"'              # readonly allowances
- 'mcp.tool.name == "sandbox_get_packages"'
- 'mcp.tool.name == "sandbox_convert_to_markdown"'
```

**Require an OAuth scope instead of a group:**
```yaml
- 'mcp.tool.name.startsWith("browser_") && "sandbox:browser" in jwt.scope.split(" ")'
```

### Apply changes

The policy is in a ConfigMap; agentgateway loads it at startup, so restart it
after editing:

```bash
kubectl apply -f deploy/30-agentgateway.yaml
kubectl rollout restart deployment/agentgateway -n default
kubectl rollout status   deployment/agentgateway -n default
```

> A malformed CEL rule makes agentgateway fail to load the config — check
> `kubectl logs -n default -l app=agentgateway` for a parse error after rollout.

### Set up the Keycloak groups

```bash
# Authenticate to the Keycloak admin API (master realm), then:
# Create the tier group
POST /admin/realms/sandbox/groups            {"name":"sandbox-power"}
# Add a user to it
PUT  /admin/realms/sandbox/users/<userId>/groups/<groupId>
```

Make sure the `sandbox` client scope keeps its **group-membership mapper**
(`sandbox-groups`, claim `groups`, `Add to access token = ON`,
`Full group path = OFF`) so the groups reach the access token agentgateway reads.

---

## Part 2 — The create gate and per-user sessions

The sandboxd reference broker (`broker/broker_sandboxd.py`) is the only place
sessions originate — every request goes through it, so its checks can't be bypassed
by a client opening more MCP connections. (There is **no** `SandboxClaim` /
per‑session pod here; sandboxd sessions are portable and principal‑derived — see
[architecture-broker.md](architecture-broker.md).)

### a) Who may get a sandbox at all (the create gate)

The broker rejects (`403`) any request whose JWT `groups` claim does not contain
`AIO_REQUIRED_GROUP` (the baseline gate). In **multi‑app** mode, each app in
`SANDBOXD_APPS` also carries its own required `group`, enforced per app — so an app
can require a higher tier than the baseline (e.g. `everything` requires
`sandbox-power`). See [howto-add-an-app.md](howto-add-an-app.md).

```yaml
# checkpoint-restore/controlplane/deploy/aio/broker-sandboxd.yaml
- { name: AIO_REQUIRED_GROUP, value: "sandbox-users" }   # baseline
```

To require a distinct "creator" role, point this at a different group and grant it
only to users who may provision sandboxes.

### b) How much a user can consume (the actual quota levers)

There is **no per‑user session counter** to set — and you don't need one. A user's
session id is **derived from their identity + the app** (`sess-<principal>[-<app>]-<hash>`),
so one user maps to **exactly one durable session per app**, reused across reconnects.
A user can't multiply sessions by opening more MCP connections. So "how many sessions
per user" is bounded structurally: **(# apps they're entitled to)**.

That makes the real quota levers these three — use them, not a session count:

1. **Entitlement (who can open which app) — the primary lever.** Each app in
   `SANDBOXD_APPS` has a required `group`; a caller without it gets `403`. To limit a
   user to fewer/cheaper apps, don't put them in the pricier apps' groups. This is how
   you cap *what* a user can run. (See (a) and
   [howto-add-an-app.md](howto-add-an-app.md).)

2. **Pool capacity (how many run at once) — the fleet‑wide cap.** Concurrency is bounded
   by the pool, not per user: a `WarmPool`'s `replicas` sets the ceiling on
   simultaneously‑*running* sessions, and `minIdle` keeps warm headroom. When every
   worker is busy, a new session gets **`503 Retry-After`** (idle ones checkpoint to S3
   and free their worker, so the ceiling is on *active*, not *total*, sessions). Size
   `replicas`/`minIdle` to your expected concurrency + budget:

   ```yaml
   # checkpoint-restore/controlplane/deploy/aio/generic-pool.yaml
   spec: { templateRef: { name: aio-generic }, replicas: 8, minIdle: 2 }
   ```

3. **Fork fan‑out cap (blast radius) — apiserver‑enforced.** A `ForkSet.count` is hard‑
   capped at **256** by the CRD schema (`kubebuilder:validation:Maximum`), so one fan‑out
   can't spawn unbounded sessions regardless of caller. (Per‑subject fork quota is a
   caller/front‑door concern, not enforced here — see
   [PRD-snapshot-fork.md](../PRD-snapshot-fork.md).)

> **`SANDBOXD_MAX_SESSIONS_PER_USER` is currently read but NOT enforced** (each
> principal already maps to one durable session per app, so there's nothing to count).
> It's a placeholder for a future multiple‑named‑sessions‑per‑user feature — don't rely
> on it as a quota today; use the three levers above.

### Tenant isolation (built in, nothing to configure)

The session id is derived from the caller's principal (+ app), so a user's requests
only ever resolve to *their own* session — sessions are never shared across users, and
a different app gets a distinct session id that can't collide. The router dispatches
each request to the one worker holding that session via `X-Session-ID`.

### Apply changes

```bash
# Broker gate / app registry (env-only — no new image; re-apply + roll):
kubectl apply -f checkpoint-restore/controlplane/deploy/aio/broker-sandboxd.yaml
kubectl rollout status deployment/aio-sandbox-broker-sandboxd -n default

# Pool capacity (replicas / minIdle): the operator reconciles the WarmPool:
kubectl apply -f checkpoint-restore/controlplane/deploy/aio/generic-pool.yaml
```

(Group membership changes take effect on the user's next token — no redeploy.)

### Inspect / operate

sandboxd sessions aren't `SandboxClaim` objects — they live in the Valkey assignment
table (authoritative) and are mirrored to `Session` CRs. Inspect/operate on those:

```bash
# A user's sessions (ids are sess-<principal>[-<app>]-<hash>):
kubectl get sessions -n default | grep <user>

# A session's phase + worker:
kubectl get session -n default sess-<user>-<app>-<hash> \
  -o custom-columns=PHASE:.status.phase,WORKER:.status.workerPod

# End a session manually: delete the CR (the operator/GC also reaps its Valkey entry
# and S3 snapshot; TTL-after-suspend is the automatic backstop). Deleting the CR is
# the durable action — the router won't resurrect a session with no record.
kubectl delete session -n default sess-<user>-<app>-<hash>
```

---

## Quick reference

| Goal | Knob | File |
|---|---|---|
| Hide/deny a tool for a tier (per app) | that app route's `mcpAuthorization.rules` (CEL on `jwt.groups` / `mcp.tool.name`) | `deploy/30-agentgateway.yaml` |
| Grant a user the exec tools | add to `sandbox-power` group in Keycloak | Keycloak admin |
| Require membership to get any sandbox | `AIO_REQUIRED_GROUP` (baseline gate) | `checkpoint-restore/controlplane/deploy/aio/broker-sandboxd.yaml` |
| Gate a specific app to a tier (limit what a user can run) | the app's `group` in `SANDBOXD_APPS` | `…/broker-sandboxd.yaml` |
| Cap concurrent running sessions (fleet‑wide) | `WarmPool` `replicas` / `minIdle` (→ `503` when full) | `…/generic-pool.yaml` |
| Bound a single fan‑out | `ForkSet.count` ≤ 256 (apiserver‑enforced) | CRD schema |

See [architecture-broker.md](architecture-broker.md) for how these fit into the
end-to-end auth/authz flow.
