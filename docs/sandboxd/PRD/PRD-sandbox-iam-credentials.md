# PRD — per‑session IAM credentials for sandboxes

Status: **Implemented + verified live** (2026‑07‑08, operator `v16`, worker `v51`).
Related: [architecture-sandboxd.md](../architecture-sandboxd.md),
[PRD-arbitrary-image-sessions.md](PRD-arbitrary-image-sessions.md) (same authz class).

> **As built (differences from the design, all found via live testing):**
> - **Vendor address = `169.254.170.2`, not the interior gateway `169.254.17.1`.**
>   AWS SDKs allow‑list which host `AWS_CONTAINER_CREDENTIALS_FULL_URI` may point at
>   (only loopback / `169.254.170.2` / `169.254.170.23`); a link‑local like
>   `169.254.17.1` is rejected by botocore. We use **`169.254.170.2`** (the ECS
>   task‑role address) — **not** `169.254.170.23`, because that's the EKS Pod
>   Identity agent address the *worker's own* SDK uses to load its identity;
>   pinning `.23` locally hijacked the worker's credential source (observed:
>   `AssumeRole` → `dial 169.254.170.23:80: connection refused`).
> - **Reachability:** `169.254.170.2/32` is added to the **host veth `sbx0`** in
>   `setupSandboxNet` (and pinned on `lo` at boot as a bind anchor), so a packet the
>   sandbox sends to it via its default gateway is delivered on‑link — cross‑
>   interface delivery to a `lo`‑only address failed (read timeout).
> - **Vendor's own identity:** with EKS Pod Identity the worker pod has ONE
>   identity (the checkpoint SA), so the vendor's `sts:AssumeRole` runs as that
>   identity — **the worker checkpoint role carries the `sts:AssumeRole`
>   permission** on the session role (a separate "vendor" Pod Identity isn't
>   achievable — one association per SA). The target role's trust policy names the
>   worker role as principal; scoped by `sts` session tag `sandbox-session=<sid>`.
> - **Token:** `AWS_CONTAINER_AUTHORIZATION_TOKEN` (plain env), value =
>   `HMAC(fleet-key, sid)`; fleet key injected from Secret `sandboxd-cred-token`
>   via operator flag `--cred-token-secret`. Enabled per pool by
>   `SandboxTemplate.spec.iam.roleArn`.
> - **Verified live:** boto3 in the sandbox → `assumed-role/sandbox-session-demo/<sid>`
>   (session role, not the worker/node identity); role permission usable (listed
>   S3 buckets); node IMDS blocked (401); **survived teleport** to a different
>   worker (identity re-established automatically).
> - **Rename DONE (2026‑07‑08):** the checkpoint identity was migrated
>   `ckpt-spike` → `sandboxd-worker` (SA) and `aio-checkpoint-spike-role` →
>   `sandboxd-worker-checkpoint` (role) via the staged §5.4a procedure — new
>   SA/role/association created, `--worker-sa` flipped, workers rolled, S3 +
>   AssumeRole verified under the new identity, old SA/role/association deleted.
>   The S3 **bucket** name is unchanged (cosmetic). Live workers now run as
>   `sandboxd-worker`.
> - **Still open:** a real subject→role **authorization gate** (front door / CEL) —
>   today any session in a pool with `iam.roleArn` set gets that role; there is no
>   per‑subject entitlement check yet.

## 1. Summary

Let a sandboxed workload **assume an AWS IAM role** — get temporary AWS
credentials scoped to *its session*, auto‑refreshed, surviving teleport — without
inheriting the worker's own identity and without baking secrets into the
checkpoint. The mechanism: the worker exposes a **container‑credentials HTTP
endpoint on the interior gateway** that the AWS SDK inside the sandbox fetches
from (and re‑fetches on expiry); the endpoint vends `sts:AssumeRole` output for the
role the session is authorized to use.

This is the AWS‑native "container credentials provider" pattern
(`AWS_CONTAINER_CREDENTIALS_FULL_URI`), applied at the sandbox boundary — the
sandbox equivalent of EKS Pod Identity / IRSA, which don't reach a nested gVisor
container.

## 2. Problem

A sandboxed workload today has **no AWS identity**, and the normal Kubernetes
paths don't reach it:

- **The sandbox is not a pod.** EKS Pod Identity / IRSA inject a projected token +
  AWS env into the *pod*; the kubelet knows nothing about the nested gVisor
  container, so none of that reaches the sandbox.
- **IMDS / the Pod Identity agent are unreachable** from the interior netns: the
  sandbox has only a masquerade route out (interior IP `169.254.17.2` → worker pod
  IP), no route to the node's `169.254.169.254` or the Pod Identity agent at
  `169.254.170.23`. (Good — those are the *node/worker* identities, which the
  untrusted sandbox must not reach.)
- **The worker's own identity is `ckpt-spike`** (S3 checkpoint read/write). The
  sandbox must **never** inherit it — that's the platform's credential, and workers
  are fungible/multi‑tenant over their lifetime (teleport), so a worker‑level role
  can't express a per‑session role anyway.

So there's no supported way for a workload to call AWS as itself. The naive
workaround — assume a role upstream and pass static creds via `/run` env — breaks
on expiry (role sessions are ≤1h chained / ≤12h direct) and on teleport (a restore
replays *stale* creds baked into the process environment). We want a durable,
refreshing, per‑session credential path.

## 3. What already exists (the seams to build on)

- **Interior gateway is reachable.** The sandbox's default route is the host veth
  end `169.254.17.1` (in the worker pod's netns), and the nftables FORWARD policy is
  `accept`. So an HTTP endpoint the worker listens on at `169.254.17.1:<port>` is
  reachable from inside the sandbox — the natural, netns‑local home for a credential
  endpoint (no exposure outside the worker).
- **Env flows to the sandbox.** `SandboxTemplate.spec.env` / `Session.spec.env` →
  resume → `/run` → the OCI spec's `process.env`. So
  `AWS_CONTAINER_CREDENTIALS_FULL_URI` (and a token env, below) can be injected the
  same way, per pool or per session.
- **Session identity exists.** `Session.spec.subject` is the JWT‑derived principal
  the router matches on (O4). That's the authz key for "which session may assume
  which role."
- **The AWS SDK supports this natively.** With `AWS_CONTAINER_CREDENTIALS_FULL_URI`
  set (plus an authorization token), every AWS SDK fetches credentials from that URI
  and **auto‑refreshes** before expiry — no app code change in the workload.

## 4. Goals / non‑goals

### Goals

1. A workload in an authorized session can obtain temporary AWS credentials for a
   specific IAM role, via the standard AWS SDK, with **no workload code change**.
2. Credentials are **short‑lived and auto‑refreshed** — no static secrets, no
   expiry surprises.
3. Credentials **survive teleport**: after restore on a new worker, the SDK keeps
   working (the endpoint is rebuilt at the same interior gateway).
4. The sandbox **cannot** reach or assume the worker's checkpoint identity
   (`sandboxd-worker`, §5.4a), IMDS, or the Pod Identity agent.
5. **Authorization**: which session (subject) may assume which role is an explicit,
   auditable policy decision — off by default (a session gets no AWS identity unless
   configured).

### Non‑goals

- **Not** giving every sandbox AWS access by default. Opt‑in per pool/session.
- **Not** a general secrets‑distribution system — scope is AWS role credentials.
- **Not** cross‑cloud (GCP/Azure) — AWS only for v1.
- **Not** preserving a specific credential *value* across teleport — the SDK
  re‑fetches; we preserve the *ability to fetch*, not the cached token.

## 5. Proposed design

### 5.1 Credential endpoint on the interior gateway

The worker (sandboxd) runs a small HTTP **credential vendor** bound to the interior
gateway address `169.254.17.1` on a dedicated port (e.g. `:8091`), reachable only
from that worker's sandbox netns — never from the pod network or outside.

- On `GET /creds` (path TBD), it returns the standard container‑credentials JSON:
  `{ "AccessKeyId", "SecretAccessKey", "Token", "Expiration" }`.
- It obtains that by calling `sts:AssumeRole` for the **session's authorized role**
  (see 5.3), caching the result and refreshing before `Expiration`.
- It requires the SDK to present the token from
  `AWS_CONTAINER_CREDENTIALS_FULL_URI`'s companion auth (see 5.2), so a compromised
  *other* sandbox can't fetch (defense in depth; the netns is already per‑worker).

### 5.2 Env injected into the sandbox

Set on the sandbox process (via template/session `env`):

```
AWS_CONTAINER_CREDENTIALS_FULL_URI = http://169.254.17.1:8091/creds
AWS_CONTAINER_AUTHORIZATION_TOKEN  = <per-session opaque token>   # SDK sends as Authorization header
AWS_REGION                          = <region>                    # optional, convenience
```

The SDK's container‑credentials provider handles fetch + refresh. No workload
change. (`AWS_CONTAINER_CREDENTIALS_FULL_URI` — not the RELATIVE_URI form — is
required because the endpoint is a link‑local IP, not the ECS `169.254.170.2` host.)

### 5.3 Who may assume which role (authorization)

The role a session gets is decided by the **control plane**, from the session's
identity — not by the sandbox asking for an arbitrary role:

- A pool/session declares an **assumable role ARN** (new optional field, e.g.
  `SandboxTemplate.spec.iam.roleArn` and/or `Session.spec.iam.roleArn`), off by
  default.
- An **authorization policy** gates it: which `subject` (JWT principal / group) may
  use which role. Enforced at the front door and/or the control plane, mirroring the
  arbitrary‑image entitlement model (O6a). A session with no configured role gets no
  AWS identity.
- The vendor assumes **only** the session's authorized role — the sandbox can't
  request a different one.

### 5.4a Rename the worker's checkpoint identity (bundled cleanup) — DONE 2026‑07‑08

This work introduces a *second* worker identity (the credential vendor), so it's
the right moment to fix the misleading name of the *first* one. The worker's S3
checkpoint identity is currently the ServiceAccount **`ckpt-spike`** (a leftover
from the throwaway‑spike era) bound via Pod Identity to IAM role
**`aio-checkpoint-spike-role`**. Rename both to a coherent `sandboxd-worker-*`
family so the two worker identities read clearly:

| Purpose | Old | New |
|---------|-----|-----|
| Worker checkpoint S3 identity (SA) | `ckpt-spike` | `sandboxd-worker` |
| …its IAM role | `aio-checkpoint-spike-role` | `sandboxd-worker-checkpoint` |
| Credential‑vendor identity (new, §5.4) | — | `sandboxd-worker-vendor` |

- **The S3 bucket is NOT renamed** (`aio-checkpoint-spike-...` stays — renaming a
  bucket means copying/abandoning existing snapshots, not worth it; the name is
  cosmetic).
- **Staged, safe migration** (the SA/role are live and load‑bearing — they're what
  lets workers read/write checkpoints):
  1. Create the new SA `sandboxd-worker` + role `sandboxd-worker-checkpoint`
     (same S3 policy, scoped to the existing bucket) + a new Pod Identity
     association.
  2. Flip the operator's `--worker-sa` (and `worker-deploy.yaml`) to the new SA;
     roll workers.
  3. Verify checkpoint/restore end‑to‑end on the new identity.
  4. Delete the old association + SA + role.
- Update repo references (grep‑clean: `--worker-sa` in `deploy/smoke/controlplane.yaml`,
  `worker-deploy.yaml`, and docs/runbooks) and memory.

Coupling note: keeping this in *this* PRD avoids doing worker‑identity surgery
twice — the vendor identity (§5.4) and the renamed checkpoint identity land
together as the `sandboxd-worker-*` family.

### 5.4 Where the vendor gets *its* credentials to call AssumeRole

The vendor itself needs an identity permitted to `sts:AssumeRole` the target roles.
Two options (decide in design):

- **(a) A dedicated worker "vendor" identity** (`sandboxd-worker-vendor`) via Pod
  Identity — separate from the checkpoint identity `sandboxd-worker` (§5.4a), with a
  trust policy allowing it to assume only the allow‑listed session roles. Keeps the
  S3 identity and the assume‑role identity distinct.
- **(b) The control plane vends** — the operator (not the worker) calls
  `sts:AssumeRole` and hands the worker the resulting short‑lived creds to serve.
  Keeps AWS‑assume authority off the worker entirely, at the cost of an operator↔
  worker credential handoff. Heavier; keep as alternative.

Leaning (a): the target roles' trust policies allow the vendor identity to assume
them, scoped by `sts:AssumeRole` conditions (e.g. session tag = subject).

### 5.5 Teleport interaction

Nothing about the credentials is checkpointed. On restore:

- The worker rebuilds the veth/netns and the credential endpoint at the same
  interior gateway `169.254.17.1:8091` (same as it rebuilds the DNAT today).
- The sandbox's env (`AWS_CONTAINER_CREDENTIALS_FULL_URI`) is unchanged — it's part
  of the OCI spec that travels with the session.
- The SDK's next fetch hits the new worker's vendor, which assumes the session's
  role fresh. So AWS access resumes automatically, no stale token.

This is exactly why option 2 beats baking creds into env: the *ability to fetch*
teleports; a cached token would not.

## 6. Security considerations

- **Isolation preserved.** The vendor listens only on the per‑worker interior
  gateway; it's not on the pod network. gVisor remains the boundary.
- **Least privilege / blast radius.** The sandbox gets *only* its session's role,
  never the worker checkpoint identity `sandboxd-worker` (S3) and never the node
  role. The vendor's assume authority is
  scoped to allow‑listed roles.
- **Confused‑deputy / tag conditions.** Use `sts:AssumeRole` with a session tag =
  session subject, and role trust policies that require it, so a role can be scoped
  to specific principals — not "anything the vendor can assume."
- **Untrusted workload.** The sandbox runs arbitrary code (esp. with BYOC). The
  auth token (5.2) + per‑worker netns limit fetch to the intended sandbox; the role
  policy limits what those creds can do. Log every AssumeRole (subject, role,
  session).
- **Pairs with P1.5.** mTLS/NetworkPolicy hardening and this share the "establish a
  real per‑session identity" theme; the credential endpoint should be covered by the
  same network lockdown.
- **Auditability.** Every vend is attributable to a session + subject + role.

## 7. Failure modes

- **AssumeRole fails** (policy/trust misconfig): the vendor returns an error; the
  SDK surfaces a credentials error to the workload. No fallback to any other
  identity (never silently use the worker checkpoint identity `sandboxd-worker`).
- **Vendor down / unreachable**: workload's AWS calls fail closed. Health‑check the
  vendor as part of worker readiness if AWS access is required for a pool.
- **Role/session TTL vs. long sessions**: the SDK refreshes before expiry; the
  vendor re‑assumes as needed. A session outliving a max role duration still works
  because each fetch is a fresh AssumeRole.

## 8. Testing / acceptance

1. **Unit:** vendor returns well‑formed container‑creds JSON; refreshes before
   `Expiration`; rejects requests without the session auth token.
2. **Integration:** a sandbox with the env set can `aws sts get-caller-identity`
   and see the **session role** (not the worker `sandboxd-worker` identity, not the
   node role).
3. **Teleport:** suspend→restore a session mid‑use; the workload's AWS calls keep
   working after resume on a different worker.
4. **Authz:** a session without a configured/authorized role gets **no** AWS
   identity; a session cannot obtain a role it isn't authorized for.
5. **Isolation:** the sandbox cannot reach IMDS (`169.254.169.254`) or the Pod
   Identity agent (`169.254.170.23`).

Acceptance: an authorized session's workload transparently assumes its role via the
standard SDK, refreshes automatically, survives teleport, and can reach no identity
other than its own.

## 9. Effort estimate

Medium–large. New pieces: the worker credential‑vendor HTTP service (bind to the
interior gateway, AssumeRole + cache/refresh, auth‑token check); CRD fields
(`iam.roleArn`) + env injection through resume/`/run`; the vendor's own AWS
identity + trust policies; the subject→role authorization policy; teleport rebuild
of the endpoint; and tests. Touches worker, operator, CRDs, and IAM. Likely 2–3
PRs. No change to the resume/checkpoint core.

## 10. Open questions

| # | Question | Leaning |
|---|----------|---------|
| Q1 | Vendor gets its creds via a dedicated worker Pod Identity (a) or operator‑vended (b)? | (a) dedicated worker "vendor" identity (`sandboxd-worker-vendor`), distinct from the checkpoint identity; scoped trust policies. |
| Q7 | Do the `ckpt-spike` → `sandboxd-worker` rename first (standalone) or as part of this PRD? | As part of this PRD (§5.4a) — avoids doing worker‑identity surgery twice. Bucket name unchanged. |
| Q2 | Role config granularity — per pool (`SandboxTemplate`), per session (`Session`), or both? | Both; session overrides template. Off by default. |
| Q3 | Authorization home — front door (broker/agentgateway) vs. control‑plane admission (CEL/webhook) vs. both? | Control plane is authoritative (can't be bypassed); front door may pre‑check. Mirror BYOC. |
| Q4 | Auth token for the endpoint — per‑session static vs. rotating? | Per‑session token injected in env; rotate on refresh if cheap. |
| Q5 | Scope creds by `sts:AssumeRole` session tags (subject) for confused‑deputy safety? | Yes — tag = subject, require it in role trust policy. |
| Q6 | One shared vendor per worker serving its single sandbox, or per‑session process? | One per worker (one sandbox per worker today); revisit if multi‑sandbox workers ever exist. |

## 11. Interim workaround (available today, documented honestly)

For a **short‑lived** task that needs AWS now, the control plane / broker can
`sts:AssumeRole` upstream and pass `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` /
`AWS_SESSION_TOKEN` into the sandbox via `SandboxTemplate.spec.env` or
`Session.spec.env`. **Caveats:** the creds expire (≤1h chained) and a teleport
restore replays the *stale* baked‑in creds — so this is only for tasks shorter than
the credential TTL and not expected to teleport. This PRD exists to replace that
stopgap with the refreshing, teleport‑safe endpoint above.
