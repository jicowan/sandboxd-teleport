# AIO on sandboxd — reproducible deploy

Runs the AIO sandbox image (`ghcr.io/agent-infra/sandbox`) as a **nested gVisor
sandbox** under the sandboxd control plane, fronted by the sandboxd broker so a
Claude MCP client teleports its own durable session.

Full path:

```
Claude --MCP+OAuth--> agentgateway --(passthrough JWT)--> broker_sandboxd
   --X-Session-ID + X-Session-Pool--> sandboxd router
   --resume--> operator --> sandboxd worker (nested gVisor AIO) --> MCP tools
```

## Prerequisites

- The control plane is deployed (see `../smoke/controlplane.yaml`): operator,
  router, Valkey in `sandboxd-controlplane-system`.
- gVisor worker nodes labeled `sandbox=gvisor` (+ the matching taint toleration).
- The images referenced below are pushed to ECR.

## Apply

```sh
# 1. AIO pool (SandboxTemplate + WarmPool) in the control-plane's session namespace
kubectl apply -f aio-pool.yaml

# 2. The sandboxd broker, deployed side-by-side with any existing broker
kubectl apply -f broker-sandboxd.yaml
```

`aio-pool.yaml` notes:
- image `ghcr.io/agent-infra/sandbox:latest`, port 8080, health `GET /v1/health`.
- `idle.timeoutSeconds: 600` (checkpoint→S3 after 10m idle; teleport-resumes on
  reconnect).
- `replicas: 4, minIdle: 2` — warm headroom (AIO cold start is ~40-45s: image
  pull + Chrome boot).
- scheduling pins to gVisor nodes and spreads across nodes (`minDomains: 2`).

## Cut traffic over to this broker

The public front door is an ALB Ingress (`broker.jicomusic.com`) → Service
`aio-sandbox-broker-svc` → broker pods (by label). Flip the Service selector:

```sh
# cut over to the sandboxd broker
kubectl patch svc aio-sandbox-broker-svc -n default --type=merge \
  -p '{"spec":{"selector":{"app":"aio-sandbox-broker-sandboxd"}}}'

# ROLLBACK to the original agent-sandbox broker
kubectl patch svc aio-sandbox-broker-svc -n default --type=merge \
  -p '{"spec":{"selector":{"app":"aio-sandbox-broker"}}}'
```

The Claude client needs **no config change** — same endpoint/OAuth.

## Notes / gotchas learned

- **First tool call is slow (~45s)** while AIO cold-starts, unless the session is
  pre-warmed. The broker warms on MCP `initialize`; still, a client with a short
  per-request timeout may need one retry on a truly cold session.
- **Capacity**: a new session needs an idle worker. `minIdle` keeps headroom; if
  the pool is saturated a new session gets `503 Retry-After`.
- The resume/warm-up deadline defaults to 90s (`SANDBOXD_RESUME_DEADLINE_SECONDS`)
  — must exceed AIO cold start.
