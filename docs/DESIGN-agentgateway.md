# Design — agentgateway front door (current/production)

Supersedes the earlier AgentCore Gateway design (that evaluation is archived at
`delete_me/docs/POSTMORTEM-agentcore-vs-agentgateway.md`). Verified end-to-end on the
live cluster: a `tools/call` through agentgateway executed shell in a sandbox as the
authenticated user, HTTP 200 in ~0.6s.

## Architecture

```
Claude Code
  │  Streamable HTTP MCP + native OAuth (browser PKCE against Keycloak)
  ▼
agentgateway (standalone, in-cluster; public via ALB Ingress — TODO)
  │  - mcpAuthentication: validate the Keycloak user JWT (iss/aud/JWKS),
  │    serve /.well-known/oauth-protected-resource for client discovery
  │  - backendAuth: passthrough → forward the SAME user JWT to the broker
  │  - MCP session affinity (issues mcp-session-id, routes by it)
  ▼
broker (Streamable HTTP MCP server)
  │  - re-validate the user JWT as a resource server (JWKS, iss/aud/azp)
  │  - enforce sandbox-users group
  │  - answer initialize locally (instant); claim sandbox on first tool call
  │  - forward tools/list, tools/call with X-Sandbox-* headers
  ▼
sandbox-router (ClusterIP, no sidecar) → resolves the sandbox's headless
  Service → AIO pod :8080/mcp
```

Identity propagation is `backendAuth: passthrough` — no OBO, no token
exchange, no consent. The broker sees the real end user
(`preferred_username`, `groups`).

## Sandbox keying — per-user, multi-sandbox

Requirement: each user gets their own sandbox; a user may need more than one.

Model (label every SandboxClaim with both):
- `aio-sandbox.broker/principal` = the JWT principal (e.g. `jicowan`)
- `aio-sandbox.broker/session-id` = the MCP session id

Resolution on a tool call:
- One MCP **session** ↔ one sandbox. Same session id → reuse the same
  sandbox (verified: two calls on one session hit the same pod).
- A **new** MCP session for the same user → a new sandbox. Since a user can
  open multiple MCP sessions, this naturally yields **N sandboxes per user**
  (the multi-sandbox case) while isolating users from each other (per-user
  case, because the principal label scopes them).
- Per-user quota (cap N) is a future policy check: list claims by
  `principal` label, reject/recycle past a limit. Deliberately not enforced
  yet.

This is correct *now* because identity is real per-user (passthrough). Under
the earlier AgentCore CLIENT_CREDENTIALS model every caller shared one
principal, so principal-keying would have collapsed all users into one
sandbox — that constraint is gone.

## Operational fixes that were required

- **CoreDNS black-hole pod (root cause of all intermittent failures)**: one
  CoreDNS replica (`10.0.3.139` on node `ip-10-0-3-26`) was a network
  black hole — `Running`/`Ready` but answering `0/10` direct queries, while
  the other replicas answered `10/10`. The kube-dns Service load-balanced
  across all endpoints, so ~25% of every pod's DNS queries silently timed out
  (no CoreDNS error logged — the packets never arrived). This produced every
  "Temporary failure in name resolution" / 504-waiting-for-sandbox symptom seen
  across the whole project (it degraded the AgentCore attempts too). Fix:
  `kubectl delete pod` the bad replica; it rescheduled healthy. Resolution went
  to 30/30 and the broker→router→sandbox path to 5/5.
  - Diagnosis technique: dig each CoreDNS pod IP directly (`@<podIP>`), not
    just the Service VIP, to find a single bad endpoint.
  - **Durable hardening (recommended, not yet done): NodeLocal DNSCache** — a
    per-node DNS cache that talks to CoreDNS over TCP, eliminating both the
    conntrack-UDP race and single-bad-endpoint fan-out. Best production fix for
    EKS DNS flakiness.
- **CoreDNS**: scaled 2→4 replicas (across subnets) — helped spread load but
  did NOT fix the black-hole pod above (the real cause).
- **ndots:3 + FQDN**: pods use `dnsConfig` ndots:3 and the broker/agentgateway
  target hosts use a trailing dot (`...svc.cluster.local.`) to avoid
  search-domain query amplification (ndots:5 → 5 lookups per name).
- **Broker initialize is local**: the broker answers MCP `initialize` itself
  (instant) instead of forwarding to the AIO hub (which can take >20s), so
  front-proxy connect timeouts don't fire on the handshake. Sandbox is claimed
  lazily on the first real tool call.
- **JWKS resilience**: broker caches JWKS (lifespan 1h) + retries transient
  fetch failures, and warms the cache at startup.
- **Stale-claim hygiene**: a claim pointing at a wedged/abused sandbox makes
  the router 504. Repeated failed tests leaked such claims. Reaping by TTL is
  the backstop; consider a readiness re-check before reusing a claim.

## Still TODO for production

1. **Public endpoint**: agentgateway is ClusterIP-only. Claude Code (on a
   laptop) needs a public, TLS-terminated endpoint — internet-facing ALB
   Ingress + ACM cert for `agentgateway.example.com` + Route53 alias
   (mirror the pattern previously used for the router/broker).
2. **Align audience/resource**: confirm `mcpAuthentication.audiences` and
   `resourceMetadata.resource` match what Claude Code's OAuth discovery
   expects for the public hostname.
3. **Register in Claude Code**: `claude mcp add --transport http aio-sandbox
   https://agentgateway.example.com/mcp --client-id aio-sandbox-client`,
   then verify the native browser OAuth flow end-to-end from the client.
4. **Reap stale claims** proactively (readiness check on reuse, or a sweeper).
5. **Per-user quota** policy if/when needed.
6. Productionize agentgateway (replicas, resources; consider the Gateway-API/
   kgateway install instead of the standalone single Deployment).
