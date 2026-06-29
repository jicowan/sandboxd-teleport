# Deploying the AIO Sandbox Broker (Stage 1)

End-to-end runbook for the broker + broker-aware proxy. Assumes the
cluster-side infrastructure from the `agent-sandbox` bundle's `auth/` is
already applied (Keycloak `sandbox` realm, ALB ingress class, oauth2-proxy
sidecar pattern, `oauth2-proxy-cookie` secret in `default`).

Reference values used below (replace for your environment):

| Thing | Value |
|---|---|
| Cluster | `EKSClusterStack-cluster`, `us-west-2`, account `820537372947` |
| Hosted zone | `jicomusic.com` → `Z11NQ8WVIQM93N` |
| ALB hosted-zone id (us-west-2) | `Z1H1FL5HABSF5` (constant) |
| Broker host | `broker.jicomusic.com` |
| Keycloak realm | `https://keycloak.jicomusic.com/realms/sandbox` |
| Images | `jicowan/aio-sandbox-broker`, `jicowan/aio-sandbox-proxy` |

## 0. Prerequisites already in place

- `aio-sandbox-template` SandboxTemplate + `aio-sandbox-warmpool` SandboxWarmPool.
- Keycloak `sandbox` realm with the `sandbox-router-cli` public device-flow
  client and the `sandbox` client scope.
- The router behind its own ALB + oauth2-proxy sidecar.

## 1. Keycloak: claims the broker depends on

The broker derives the caller principal and the authorization group from the
access token. The minimal device-flow access token does **not** carry these
by default — add two protocol mappers to the `sandbox` client scope plus a
group and membership. These are captured in the realm manifest
(`agent-sandbox/.../auth/00-keycloak-sandbox-realm.yaml`) but the Keycloak
operator does not reliably push realm *updates*, so apply via the admin API
(or UI) against the running realm.

Mappers on the `sandbox` client scope:
- `sandbox-audience` (`oidc-audience-mapper`) → `aud: sandbox-router`
- `sandbox-groups` (`oidc-group-membership-mapper`) → `groups: [...]`
- `sandbox-username` (`oidc-usermodel-property-mapper`) → `preferred_username`

Group + membership:
- realm group `sandbox-users`
- add each authorized user to `/sandbox-users`

Verify a freshly issued token has all three:

```bash
# device flow → token, then decode the payload
python3 - <<'PY'
import json, base64
at = open("/tmp/at.txt").read().strip()
p = at.split(".")[1]; p += "="*(-len(p)%4)
c = json.loads(base64.urlsafe_b64decode(p))
print("preferred_username:", c.get("preferred_username"))
print("groups:", c.get("groups"))
print("aud:", c.get("aud"))
PY
```

You must see `preferred_username`, `groups: ['sandbox-users']`, and
`aud: sandbox-router`. **Tokens issued before the mappers were added won't
have them** — re-run the login.

## 2. Build and push the broker image

Multi-arch is **required** — the EKS nodes are `linux/amd64`; a single-arch
arm64 push (the default on Apple Silicon) fails to pull with
`no match for platform in manifest`.

```bash
cd broker
docker buildx build --platform linux/amd64,linux/arm64 \
  -t jicowan/aio-sandbox-broker:latest \
  -t jicowan/aio-sandbox-broker:0.1.1 \
  --push .
```

## 3. ACM cert + Route 53 for the broker host

```bash
CERT_ARN=$(aws acm request-certificate --region us-west-2 \
  --domain-name broker.jicomusic.com --validation-method DNS \
  --tags Key=Name,Value=aio-sandbox-broker \
  --query CertificateArn --output text)

read -r VN VV < <(aws acm describe-certificate --region us-west-2 \
  --certificate-arn "$CERT_ARN" \
  --query 'Certificate.DomainValidationOptions[0].ResourceRecord.[Name,Value]' \
  --output text)

aws route53 change-resource-record-sets --hosted-zone-id Z11NQ8WVIQM93N \
  --change-batch "{\"Changes\":[{\"Action\":\"UPSERT\",\"ResourceRecordSet\":{
    \"Name\":\"${VN}\",\"Type\":\"CNAME\",\"TTL\":300,
    \"ResourceRecords\":[{\"Value\":\"${VV}\"}]}}]}"

aws acm wait certificate-validated --region us-west-2 --certificate-arn "$CERT_ARN"
echo "$CERT_ARN"   # paste into deploy/20-ingress.yaml
```

## 4. Deploy the broker

```bash
cd deploy
# RBAC: the ServiceAccount that holds the ONLY SandboxClaim CRUD permission
kubectl apply -f 00-serviceaccount-rbac.yaml
# Deployment (broker + oauth2-proxy sidecar) + Service
kubectl apply -f 10-deployment.yaml
kubectl rollout status deploy/aio-sandbox-broker -n default

# Ingress (after pasting CERT_ARN into 20-ingress.yaml)
kubectl apply -f 20-ingress.yaml
```

Then the R53 alias for the broker host:

```bash
ALB=$(kubectl get ingress -n default aio-sandbox-broker \
  -o jsonpath='{.status.loadBalancer.ingress[0].hostname}')
aws route53 change-resource-record-sets --hosted-zone-id Z11NQ8WVIQM93N \
  --change-batch "{\"Changes\":[{\"Action\":\"UPSERT\",\"ResourceRecordSet\":{
    \"Name\":\"broker.jicomusic.com\",\"Type\":\"A\",
    \"AliasTarget\":{\"HostedZoneId\":\"Z1H1FL5HABSF5\",\"DNSName\":\"${ALB}\",
      \"EvaluateTargetHealth\":false}}}]}"
```

Wait for DNS + target health:

```bash
dig +short @8.8.8.8 broker.jicomusic.com
TG=$(aws elbv2 describe-target-groups --region us-west-2 \
  --query 'TargetGroups[?contains(TargetGroupName,`aiosandb`)].TargetGroupArn' \
  --output text)
aws elbv2 describe-target-health --region us-west-2 --target-group-arn "$TG" \
  --query 'TargetHealthDescriptions[].TargetHealth.State' --output text
```

## 5. Verify the broker

```bash
AT=$(cat /tmp/at.txt)   # an in-group token with the claims from step 1

# POST → 201, returns the sandbox identity
curl -s -X POST -H "Authorization: Bearer $AT" \
  https://broker.jicomusic.com/sessions | python3 -m json.tool
# {session_id, sandbox_id, namespace, container_port, expires_at, principal}

SID=sandbox-claim-XXXXXXXX   # session_id from above
curl -s -H "Authorization: Bearer $AT" \
  https://broker.jicomusic.com/sessions/$SID         # GET → 200
curl -s -o /dev/null -w '%{http_code}\n' -X DELETE \
  -H "Authorization: Bearer $AT" \
  https://broker.jicomusic.com/sessions/$SID         # DELETE → 204
```

A `401 No authenticated identity` means the token lacks
`preferred_username` (step 1 not applied, or a stale token). A `403` means
the caller isn't in `sandbox-users`.

## 6. Build and push the broker-aware proxy

```bash
cd proxy
docker buildx build --platform linux/amd64,linux/arm64 \
  -t jicowan/aio-sandbox-proxy:latest \
  -t jicowan/aio-sandbox-proxy:0.2.0 \
  --push .
```

## 7. Log in (one-time, per token lifetime)

```bash
docker run -it --rm \
  --user "$(id -u):$(id -g)" \
  -v "$HOME/.config/aio-sandbox:/config" \
  -e AIO_OIDC_ISSUER=https://keycloak.jicomusic.com/realms/sandbox \
  -e AIO_OIDC_CLIENT_ID=sandbox-router-cli \
  jicowan/aio-sandbox-proxy:latest login
```

Open the printed device URL, log in as a `sandbox-users` member, approve.
Caches a refresh token at `~/.config/aio-sandbox/oidc.json`.

## 8. Register the MCP server (broker-aware, user scope)

`--scope user` so it applies from any directory. No `claim.py`, no
`current.json`, no SDK on the client — the proxy claims via the broker.

```bash
claude mcp remove aio-sandbox -s user 2>/dev/null
claude mcp add aio-sandbox \
  --scope user \
  --env AIO_BROKER_URL=https://broker.jicomusic.com \
  --env AIO_ROUTER_URL=https://sandbox-router.jicomusic.com \
  --env AIO_OIDC_ISSUER=https://keycloak.jicomusic.com/realms/sandbox \
  --env AIO_OIDC_CLIENT_ID=sandbox-router-cli \
  -- docker run -i --rm \
       --user "$(id -u):$(id -g)" \
       -v "$HOME/.config/aio-sandbox:/config" \
       -e AIO_BROKER_URL -e AIO_ROUTER_URL -e AIO_OIDC_ISSUER -e AIO_OIDC_CLIENT_ID \
       jicowan/aio-sandbox-proxy:latest
```

Restart Claude Code so it spawns a fresh MCP child with the new config.

Note: `claude mcp get aio-sandbox` may show `tools fetch failed · timed out`.
That health probe has a short window; the first real tool call performs the
broker claim (which waits for the sandbox to be ready) and succeeds. It's not
an error in the wiring.

## How the request flows (Stage 1)

```
Claude Code → docker(proxy) → POST /sessions ┐
                                             ├→ ALB → oauth2-proxy (verify JWT,
                                             │        enforce sandbox-users)
                                             │     → broker (principal from JWT,
                                             │        SDK + ServiceAccount creates
                                             │        a warmpool-adopted claim)
                              ← {sandbox_id} ┘
            → MCP tools/call (X-Sandbox-* + Bearer)
                 → ALB → oauth2-proxy → sandbox-router → AIO pod :8080/mcp
```

## Gotchas hit during the first deploy (so you don't re-debug them)

1. **Single-arch image → `ImagePullBackOff` (`no match for platform`).**
   Always `docker buildx --platform linux/amd64,linux/arm64`. A plain
   `docker build` on Apple Silicon produces arm64-only.
2. **`--allowed-groups` is not a flag.** oauth2-proxy uses `--allowed-group`
   (singular, repeatable). The wrong name crash-loops the sidecar with a
   help dump.
3. **`401 No authenticated identity` despite a valid token.** In
   `--skip-jwt-bearer-tokens` mode oauth2-proxy does not synthesize
   `X-Auth-Request-*` headers, so the broker reads identity from the
   forwarded bearer token directly (`--pass-authorization-header=true`). For
   that to work the token must carry `preferred_username` — add the
   `oidc-usermodel-property-mapper` (step 1).
4. **Keycloak realm-import updates don't take.** The operator imports a realm
   on create but won't reliably push later edits. Apply mappers/groups via
   the admin REST API or UI against the running realm; keep the YAML as the
   source of record.
5. **`502` right after a rollout.** ALB targets briefly `draining`/`initial`
   during a deploy. Wait for `healthy` before testing.
6. **Stale token after adding claims.** Mappers only affect newly issued
   tokens. Re-run the login after any claim/group change.

## Rollback

```bash
kubectl delete ingress -n default aio-sandbox-broker
kubectl delete -f deploy/10-deployment.yaml
kubectl delete -f deploy/00-serviceaccount-rbac.yaml
# Point the MCP server back at the pre-broker proxy if needed (see the
# agent-sandbox bundle's auth/README.md for the router-only registration).
```
