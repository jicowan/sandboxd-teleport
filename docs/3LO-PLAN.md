# Plan — per-user identity via 3LO + static schema

## Goal

Make the broker receive the **end user's** identity (not the gateway's service
identity), so it can key sandboxes per user and enforce the `sandbox-users`
group at the broker. This is the "finest authz" tier deferred from Stage 1/2.

## Why this is the only viable path (see STAGE2.md map)

Per-user identity through AgentCore Gateway requires the gateway-target's
outbound grant to carry the user. Empirically:

- OBO (`TOKEN_EXCHANGE`) **fails** with a live-introspection MCP target — no
  user exists at creation-time `tools/list`, so there's no subject token.
- Static `mcpToolSchema` removes creation-time introspection, but the API
  **only allows it with `AUTHORIZATION_CODE` (3LO)**, not OBO.
- 3LO mints a per-user token at invoke time → the broker gets user identity.

So: **static schema + AUTHORIZATION_CODE (3LO)** is the route.

## Hard constraints discovered (gates before any code)

1. **MCP protocol version**: 3LO requires the gateway configured for MCP
   `2025-11-25` or later. Our gateway `aio-sandbox-gateway-qgj2drbxed` is
   `2025-03-26` → must recreate (gateway protocolConfiguration version is set
   at create; verify it's not updatable before recreating).
2. **Static tool schema**: live `tools/list` passthrough is lost. The 27/31
   AIO tools must be declared as `mcpToolSchema.inlinePayload` and re-synced
   whenever the AIO image's tool set changes.
3. **Interactive consent**: the authorization-code flow prompts the user to
   consent (browser redirect). This is per the 3LO grant; acceptable for an
   interactive Claude Code user, awkward for headless/automated callers.
4. **Schema hygiene**: tool schemas must have no `$ref`/`$defs` (Gateway
   rejects them). The harvested AIO schema was already clean.

## What changes vs. today

| Aspect | Today (CLIENT_CREDENTIALS) | 3LO target |
|---|---|---|
| Gateway | MCP 2025-03-26 | recreate at MCP 2025-11-25 |
| Target discovery | live introspection | static `mcpToolSchema` |
| Outbound grant | CLIENT_CREDENTIALS | AUTHORIZATION_CODE |
| Cred provider | `sandbox-gateway-outbound` (M2M) | new authcode-mode provider |
| Broker sees | gateway service account | end-user `preferred_username` + `groups` |
| Tool transparency | automatic | manual schema, re-sync on image change |
| Consent UX | none | one-time browser consent per user |

## Build steps (verify-first, non-destructive until cutover)

1. **Confirm the version gate.** Check whether an existing gateway's
   `protocolConfiguration.mcp.supportedVersions` can be updated to
   `2025-11-25`; if not, plan a parallel gateway (don't disturb the working
   one).
2. **Stand up a parallel 3LO gateway** `aio-sandbox-gateway-3lo` at MCP
   `2025-11-25`, same `CUSTOM_JWT` inbound authorizer (sandbox realm,
   `aio-sandbox-client`).
3. **Create an AUTHORIZATION_CODE credential provider** for the sandbox realm.
   Keycloak `aio-sandbox-client` already has standardFlow + localhost
   redirects + PKCE; confirm the provider's redirect/callback wiring against
   AgentCore's callback URL (the provider create returns a `callbackUrl` that
   must be registered as an allowed redirect URI on the Keycloak client).
4. **Harvest the tool schema** (already done once): introspect the live
   CLIENT_CREDENTIALS target's `tools/list`, strip the `aio-sandbox-broker___`
   prefix and `_meta`, shape into `ToolDefinition[]`
   (`name`/`description`/`inputSchema`), JSON-encode as a string. Save it as a
   versioned artifact in the repo (e.g. `deploy/aio-tool-schema.json`) so the
   static schema is reproducible.
5. **Create the static-schema 3LO target** on the new gateway:
   `mcpServer.endpoint = https://broker.example.com`,
   `mcpServer.mcpToolSchema.inlinePayload = <json string>`,
   outbound `grantType: AUTHORIZATION_CODE`, the authcode provider ARN,
   private endpoint VPC config.
6. **Re-enable broker per-user logic.** The broker already reads
   `preferred_username` + `groups` and has the `sandbox-users` gate
   (`AIO_REQUIRED_GROUP`); set it back to `sandbox-users`. Revisit the
   session/claim keying: with real user identity, key on the principal again
   (the earlier per-principal churn was a *shared* M2M principal; distinct
   users won't collide — but re-test, since the churn root cause was the
   resolve-then-create race, not the key choice alone).
7. **Verify (the decisive test):** drive `initialize` → `tools/call` through
   the 3LO gateway with a real user token; confirm the broker logs the **end
   user's** `preferred_username`/`sub` (not the gateway SA), and that two
   different users get two different sandboxes.
8. **Cut over** Claude Code's `claude mcp add --transport http` to the new
   gateway URL; retire the CLIENT_CREDENTIALS gateway/target if desired (or
   keep both: 3LO for interactive users, CLIENT_CREDENTIALS for headless).

## Open questions to resolve during the build

- Does Claude Code's remote-MCP flow handle the **gateway's** 3LO consent
  redirect transparently, or does the per-tool 3LO consent surface awkwardly
  in the CLI? (The inbound user OAuth to the gateway already works; 3LO is the
  *outbound* leg — confirm whether the user sees a second consent.)
- Is `protocolConfiguration` updatable on an existing gateway, or is a
  recreate mandatory? (Determines step 1 vs. parallel-gateway.)
- Schema drift: when the AIO image adds/removes tools, the static schema goes
  stale silently. Need a re-harvest/CI step or a periodic diff check.
- Whether to run **two gateways** (3LO interactive + CLIENT_CREDENTIALS
  headless) long-term, or standardize on one.

## Recommendation

Treat this as a deliberate, separate workstream, not a bolt-on. The shipped
CLIENT_CREDENTIALS path works for single-user/interactive use today. Pursue
3LO when multi-user attribution or per-user policy becomes a real requirement
— the cost is a parallel gateway, a maintained static schema, and consent UX,
in exchange for genuine per-user identity at the broker.
