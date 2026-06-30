# How to: filter sandbox tools per user, and configure quotas

This guide covers the two access-control knobs an operator turns:

1. **Tool filtering** — which MCP tools a user may see and call
   (enforced at **agentgateway** via `mcpAuthorization`).
2. **Quotas & the create gate** — who may get a sandbox at all, and how many
   (enforced at the **broker**).

Both are driven by the user's Keycloak identity in the JWT that agentgateway
passes through unchanged. Nothing here requires a client-side change.

---

## Part 1 — Filter which tools a user can use

### Where it lives

`deploy/30-agentgateway.yaml`, in the route's `policies.mcpAuthorization.rules`
block. agentgateway evaluates the rules against the **passed-through user JWT**
on every `tools/call`, and **also filters `tools/list`** — so a tool a user may
not call is also invisible to them (their client never even lists it).

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

## Part 2 — The create gate and per-user quota

These are enforced in the **broker** (`broker/broker_mcp.py`) — the only
component with RBAC to create `SandboxClaim`s, so they can't be bypassed by a
client opening more MCP sessions.

### a) Who may get a sandbox at all (the create gate)

The broker rejects (`403`) any request whose JWT `groups` claim does not contain
`AIO_REQUIRED_GROUP`:

```yaml
# deploy/20-broker.yaml
- { name: AIO_REQUIRED_GROUP, value: "sandbox-users" }
```

To require a distinct "creator" role, point this at a different group (e.g.
`sandbox-creators`) and grant it only to users who may provision sandboxes.

### b) How many sandboxes per user (the quota)

Before claiming a **new** sandbox, the broker counts the caller's existing
claims (by the `aio-sandbox.broker/principal` label) and returns **`429`** once
the count reaches the cap:

```yaml
# deploy/20-broker.yaml
- { name: AIO_MAX_SANDBOXES_PER_USER, value: "3" }   # 0 disables the cap
```

- One MCP session ⇒ one sandbox (reused across calls in that session).
- A new session for the same user ⇒ another sandbox — until the cap.
- Reusing an existing session never counts against the cap (no new claim).

Verified live: a user's 4th concurrent claim is refused with `429`
(`Sandbox quota reached: 3/3`).

### Tenant isolation (built in, nothing to configure)

Each claim is labelled with the **principal** and the **session id**, and the
session→claim lookup is scoped by **both** — so a session id can never resolve
to another user's sandbox, and sandboxes are never shared across users. The
router dispatches each request to one addressed pod via `X-Sandbox-ID`.

### Apply changes

```bash
kubectl apply -f deploy/20-broker.yaml
kubectl rollout status deployment/aio-sandbox-broker -n default
```

(Env-only changes don't need a new image; just re-apply and let the Deployment
roll.)

### Inspect / operate

```bash
# Who holds what
kubectl get sandboxclaims -n default \
  -L aio-sandbox.broker/principal,aio-sandbox.broker/session-id

# Count for one principal (compare against the cap)
kubectl get sandboxclaims -n default \
  -l aio-sandbox.broker/principal=<user> --no-headers | wc -l

# Free a user's sandboxes manually (TTL is the automatic backstop)
kubectl delete sandboxclaims -n default -l aio-sandbox.broker/principal=<user>
```

---

## Quick reference

| Goal | Knob | File |
|---|---|---|
| Hide/deny a tool for a tier | `mcpAuthorization.rules` (CEL on `jwt.groups` / `mcp.tool.name`) | `deploy/30-agentgateway.yaml` |
| Grant a user the exec tools | add to `sandbox-power` group in Keycloak | Keycloak admin |
| Require membership to get any sandbox | `AIO_REQUIRED_GROUP` | `deploy/20-broker.yaml` |
| Cap sandboxes per user | `AIO_MAX_SANDBOXES_PER_USER` | `deploy/20-broker.yaml` |

See [ARCHITECTURE.md](../ARCHITECTURE.md) for how these fit into the end-to-end
auth/authz flow.
