# PRD — broker: let a Claude client run different sandboxes (multi-app)

Status: **Proposed / not started.** Drafted 2026-07-22, to revisit. Grounded in the
shipped generic-pool + AppTemplate model (see
[PRD-arbitrary-image-sessions.md §13](./PRD-arbitrary-image-sessions.md) and
[[sandboxd-generic-pools]]) and the live `broker/broker_sandboxd.py`.

## 1. Problem

Today an MCP client (Claude Code) that connects through the broker always gets **one
durable sandbox running one app**. Two hard constraints in the broker cause this:

- **Session id is derived from the principal only.** `_sid_for(principal, ...)` (slot 0)
  produces one stable `sess-<user>-<hash>` per user; every reconnect resumes it.
- **The app is a broker-wide constant.** `_session_headers()` sends a fixed
  `SANDBOXD_POOL` + `SANDBOXD_APP` (env) on every request. The `X-Session-App` hint is
  also only honored on **lazy Session creation** — once a session exists, its `appRef`
  is fixed, so changing the env can't re-image it.

So a user cannot (a) choose *which* app to run, nor (b) run *several different* apps
concurrently. We want "a Claude client can run different sandboxes."

## 2. Goal / non-goals

**Goal:** an authorized user can launch and concurrently use **multiple distinct
sandboxes**, each running a different admin-curated **AppTemplate**, through the normal
OAuth front door — with each app as its own durable, teleporting session.

**Non-goals:**
- **Not** arbitrary caller-supplied images (that's the BYOC governance track,
  PRD-arbitrary-image-sessions §5.1–5.5). Apps here are admin-authored AppTemplates.
- **Not** any control-plane / router / operator change — the generic-pool machinery
  already runs any `appRef` session. This is a **broker-only** change (+ optional
  agentgateway route).
- **Not** changing Keycloak's schema (reuse groups for entitlement).

## 3. What already works (so this is smaller than it looks)

- The control plane runs any number of independent `appRef` sessions on a generic pool;
  each is routed by its own `X-Session-ID`, teleports, idle-suspends, and GCs normally.
- The broker already sends `X-Session-App`; the operator lazy-creates a Session with
  `Spec.AppRef` from it (shipped, live-verified).
- `_authenticate()` already extracts the principal **and groups** from the JWT.

The gap is entirely: **(1) how the client picks an app, (2) folding the app into the
session id so multiple apps don't collide, (3) per-app quota, (4) an entitlement ACL.**

## 4. The design decisions

### 4.1 How does the client select the app? (the crux)

MCP tools **cannot** bootstrap this — a tool runs *inside* an already-running sandbox,
so you'd need a session before you could call a tool to create one (circular). The app
must be declared **at/before session establishment**. Options:

- **(A) Per-app MCP endpoint (recommended).** The broker serves a path per app
  (`/mcp/<app>`) or a hostname per app; it maps path → AppTemplate. The user registers
  each app as a separate MCP server in Claude Code
  (`claude mcp add aio .../mcp/aio`, `claude mcp add devbox .../mcp/devbox`). Clean,
  MCP-native, supports many concurrent apps, and the app arrives exactly when the
  session is established. agentgateway needs one route per app (or a wildcard path
  route) but no auth change.
- **(B) App from Keycloak group.** Map a group → app (`sandbox-app-aio → aio-app`).
  Smallest change; fully admin-governed; but a user gets exactly **one** app (their
  group's), not several concurrently. Good if the requirement is really "different
  users get different apps," not "one user runs several."
- **(C) `initialize` parameter / custom header.** The broker reads an app id from an
  `initialize` param or a header. Rejected: Claude Code doesn't expose a way to set a
  custom per-server field the broker can rely on, and it muddies the MCP handshake.

**Leaning: (A)** for true multi-app-per-user; **(B)** is a cheap stepping stone if only
per-user differentiation is needed. Decide before coding.

### 4.2 Session id must fold in the app

`_sid_for(principal, app, ...)` so each app gets a distinct durable session
(`sess-<user>-<app>-<hash>`) that doesn't clobber another app's. Must stay within the
sandbox-id charset `^[a-z0-9][a-z0-9-]{0,62}$` (sanitize the app name, keep the hash).

### 4.3 Per-app request headers

`_session_headers(sid, app)` sends the resolved `X-Session-App` (and pool) for *that*
app, replacing the single env constant. (A pool could also be per-app if apps need
different capacity classes — default: one generic pool for all apps.)

### 4.4 Entitlement — which apps may a subject launch?

Without a gate, any user could name any AppTemplate. Reuse the group model: a
`subject → allowed apps` ACL (e.g. group `sandbox-app-<name>` grants app `<name>`, or a
single `sandbox-power`-style group grants all). The broker rejects an app the caller
isn't entitled to. This is the "template-reference ACL" from
PRD-arbitrary-image-sessions §13.7 made concrete — small, since apps are curated.

### 4.5 Quota

The per-user cap (`MAX_SESSIONS_PER_USER`) becomes per-user **across apps** (or
per-user-per-app). Decide which; the broker is the enforcement point.

## 5. Proposed broker changes (Level B / option A)

1. **Routing:** register `/mcp/{app}` (or map Host); resolve `{app}` → AppTemplate name
   (+ validate it's in the allowed set / exists).
2. **`_sid_for(principal, app, mcp_session_id)`** → `sess-<user>-<app>-<hash>`.
3. **`_session_headers(sid, app)`** → per-app `X-Session-App`.
4. **Entitlement check** in `_authenticate`/handler: caller's groups must allow `app`.
5. **Quota** per the §4.5 decision.
6. **Config:** an app registry env/ConfigMap (`app id → {appTemplate, pool, required
   group}`), instead of the single `SANDBOXD_APP`.

No agentgateway auth change; add a route per app (or a wildcard `/mcp/*`). **Zero**
control-plane/router/operator/Keycloak-schema change.

## 6. Rollout

- **M1:** config-driven app registry + per-app endpoint + app-aware `_sid_for` +
  per-app headers (no entitlement yet; internal). Prove two apps run concurrently for
  one user.
- **M2:** entitlement ACL (group → allowed apps) + per-app/aggregate quota + audit.
- **M3 (optional):** self-service discovery (list the apps a user may launch).

## 7. Open questions

1. Selection mechanism: per-app endpoint (A) vs group-mapped single app (B)?
2. One generic pool for all apps, or per-app pools for distinct capacity classes?
3. Quota: per-user total across apps, or per-user-per-app?
4. Entitlement: one group per app, or tiers (a "power" group that grants all)?
5. Does Claude Code cleanly handle N registered MCP servers pointing at the same
   gateway host with different paths? (verify during M1)

## 8. Effort

Broker-only, ~medium: routing + sid + headers + ACL + config. The control plane already
does the hard part (independent `appRef` sessions, routing, teleport, GC). This is the
governed multi-app front door on top of the shipped generic-pool model.
