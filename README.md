# aio-sandbox

Run per-session [AIO Agent Sandboxes](https://github.com/agent-infra/sandbox)
on Kubernetes (via the [agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox)
framework) and expose them to **Claude Code** — or any MCP client — as a remote
MCP server, with **no SDK, kubeconfig, or proxy on the client**. The client just
authenticates with OAuth and calls tools.

```
Claude Code ──HTTPS MCP + OAuth──▶ agentgateway ──▶ broker ──▶ sandbox-router ──▶ AIO pod
              (browser login,        (validate JWT,   (claim a    (route to the
               Keycloak)              passthrough)     sandbox)     sandbox pod)
```

Per-user identity flows all the way to the broker (agentgateway forwards the
user's Keycloak JWT unchanged), so each user gets their own sandbox and access
is gated by Keycloak group membership.

See **[ARCHITECTURE.md](./ARCHITECTURE.md)** for the full design, auth/authz
flow, and swim-lane diagram. See
**[docs/POSTMORTEM-agentcore-vs-agentgateway.md](./docs/POSTMORTEM-agentcore-vs-agentgateway.md)**
for why this uses agentgateway rather than AWS Bedrock AgentCore Gateway.

## Components

| Component | What it is |
|---|---|
| **agentgateway** | OSS MCP gateway (standalone). Public front door. Validates the inbound Keycloak JWT, serves OAuth discovery metadata, forwards the user's token to the broker (`backendAuth: passthrough`), manages MCP sessions. |
| **broker** (`broker/broker_mcp.py`) | In-cluster MCP server. Re-validates the JWT as a resource server, enforces the `sandbox-users` group, claims/reuses a sandbox per MCP session, forwards `tools/*` to the router. Holds the only RBAC to create `SandboxClaim`s. |
| **sandbox-router** | Routes MCP requests to the addressed sandbox pod's headless Service (upstream agent-sandbox component; runs ClusterIP-only, no sidecar). |
| **Keycloak** | OIDC provider (`sandbox` realm). Public client `aio-sandbox-client` for user login; `sandbox-users` group gates access. |
| **AIO sandbox pod** | The actual sandbox (bash, code, browser, files…) — its MCP hub is what tools pass through to. Provisioned from a `SandboxTemplate` + warm pool. |

## Prerequisites

- EKS cluster with the AWS Load Balancer Controller, an `agent-sandbox`
  controller, and a `SandboxTemplate` + `SandboxWarmPool` (`aio-sandbox-template` /
  `aio-sandbox-warmpool`).
- Keycloak reachable at `keycloak.jicomusic.com` with the `sandbox` realm
  (public `aio-sandbox-client`, `sandbox-users` group, the scope mappers in
  `docs/` — `sub`, `preferred_username`, `groups`, `aud=sandbox-router`).
- Route 53 zone for the public hostname + an ACM cert for
  `agentgateway.jicomusic.com`.
- `kubectl`, `docker buildx`, `aws` CLI, and `claude` (Claude Code).

## Get it running

### 1. Build & push the broker image

```bash
cd broker
docker buildx build --platform linux/amd64 \
  -t <your-registry>/aio-sandbox-broker:0.3.1 --push .
# update deploy/20-broker.yaml image: if you use a different registry/tag
```

### 2. Deploy the in-cluster pieces

```bash
cd deploy
kubectl apply -f 00-serviceaccount-rbac.yaml   # broker SA + RBAC (SandboxClaim CRUD)
kubectl apply -f 10-router-clusterip.yaml      # router: ClusterIP-only, no sidecar
kubectl apply -f 20-broker.yaml                # broker (Deployment + Service)
kubectl apply -f 30-agentgateway.yaml          # agentgateway (ConfigMap + Deployment + Service)
```

### 3. Expose agentgateway publicly

```bash
# Ensure an ACM cert for agentgateway.jicomusic.com exists; put its ARN in
# deploy/40-agentgateway-ingress.yaml, then:
kubectl apply -f 40-agentgateway-ingress.yaml

# Point Route 53 at the provisioned ALB:
ALB=$(kubectl get ingress -n default agentgateway \
  -o jsonpath='{.status.loadBalancer.ingress[0].hostname}')
aws route53 change-resource-record-sets --hosted-zone-id <ZONE_ID> \
  --change-batch "{\"Changes\":[{\"Action\":\"UPSERT\",\"ResourceRecordSet\":{
    \"Name\":\"agentgateway.jicomusic.com\",\"Type\":\"A\",
    \"AliasTarget\":{\"HostedZoneId\":\"Z1H1FL5HABSF5\",\"DNSName\":\"${ALB}\",
      \"EvaluateTargetHealth\":false}}}]}"
```

Verify the OAuth discovery doc points at Keycloak:

```bash
curl -s https://agentgateway.jicomusic.com/.well-known/oauth-protected-resource/mcp | jq
# authorization_servers should be ["https://keycloak.jicomusic.com/realms/sandbox"]
```

### 4. Register in Claude Code & authenticate

```bash
claude mcp add aio-sandbox \
  --scope user --transport http \
  --client-id aio-sandbox-client \
  https://agentgateway.jicomusic.com/mcp
```

Start a **new** Claude Code session, run `/mcp` → **aio-sandbox** →
**Authenticate**. Log in to Keycloak as a member of `sandbox-users`. Then ask
Claude to do something in a sandbox (e.g. *"run `uname -a` in a sandbox"*).

> Keycloak anonymous Dynamic Client Registration is disabled, so the static
> `--client-id aio-sandbox-client` is required (it has the localhost redirect
> URIs + PKCE that Claude Code's auth-code flow needs).

## Repo layout

```
broker/         broker_mcp.py (the MCP broker) + Dockerfile; broker.py is the
                Stage-1 REST fallback (not used by the agentgateway design)
deploy/         k8s manifests, applied in numeric order
proxy/          legacy local stdio↔HTTP proxy (pre-agentgateway fallback)
docs/           DESIGN-agentgateway.md, POSTMORTEM, ADRs, and historical
                Stage-1/Stage-2 notes
ARCHITECTURE.md the design + auth/authz swim-lane
```

## Operational notes

- **DNS**: EKS CoreDNS reliability matters here (the broker resolves the router
  and Keycloak JWKS). If you see intermittent `Temporary failure in name
  resolution` / 504s, check for a black-hole CoreDNS pod (dig each pod IP, not
  just the Service VIP) and consider NodeLocal DNSCache. See
  `docs/DESIGN-agentgateway.md`.
- **Karpenter**: broker, agentgateway, and sandbox pods carry
  `karpenter.sh/do-not-disrupt` so consolidation doesn't drain them.
- **Tool-level authz** (gate individual tools by `sub`/`scope`/`groups`) is a
  planned enhancement via agentgateway's `mcpAuthorization` CEL policy.
