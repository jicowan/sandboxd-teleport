# microVM e2e: per-sandbox ingress/egress bandwidth limits

End-to-end proof that `SandboxTemplate.spec.network.{egressMbps,ingressMbps}` actually
throttles a microVM sandbox's traffic, in **both** directions, and that the cap
**survives teleport** (suspend → restore onto another worker).

## What it exercises

```
SandboxTemplate.spec.network ──▶ operator (toTemplateSpec → resume.TemplateSpec)
                              ──▶ /run + /restore request (per session, like ports/iam)
                              ──▶ worker: bandwidthFromMbps → ateomnet.ApplyBandwidth
                              ──▶ tc on ateom0 (host veth peer, in the worker netns):
                                     egress cap = ingress qdisc + ifb (ateom-bwifb) + TBF
                                     ingress cap = root TBF on ateom0
```

Directions are from the **sandbox's** point of view: *egress* = sandbox → outside
(upload), *ingress* = outside → sandbox (download). The shaper sits on `ateom0` in the
worker pod's netns — outside the guest, so the workload can't tamper with it, and it is
re-applied verbatim on restore because host-side tc state is not part of the checkpoint.

## Pieces

| File | Role |
|------|------|
| `10-netbw-server.yaml` | plain pod: TCP sink (:5001, egress target), HTTP source (`/download`, ingress target), result collector (`/report`, `/results`). |
| `20-netbw-pool.yaml` | **capped** microVM `SandboxTemplate` (`network: {egressMbps:50, ingressMbps:50}`) + `WarmPool`. The sandbox workload is the bandwidth client (inline python — nothing to build). |
| `30-netbw-session.yaml` | poolRef-only `Session`; warmed via the router. |

The client loops forever: blast 64 MiB at the sink (egress), drain 64 MiB from the
source (ingress), print `NETBW {...}`, and POST the numbers to the server so results are
readable from the stable server pod no matter which worker the sandbox lands on.

## Prerequisites

- The sandboxd control plane is up (operator, router, Valkey) in
  `sandboxd-controlplane-system`, and a nested-virt node labeled `sandbox=microvm` with
  the matching taint exists (see `examples/microvm/00..05`).
- A **freshly built** microVM **worker** image carrying the bandwidth code
  (`ateomnet.ApplyBandwidth`) and a **operator** image carrying the `spec.network`
  plumbing. Build/push both (watch the ECR **region** — a shell `AWS_REGION=us-east-2`
  pushes to the wrong registry), roll the operator, and put the worker image tag into
  `20-netbw-pool.yaml`'s `<WORKER_IMAGE>`.
- An in-cluster shell to reach the router + server ClusterIPs:
  ```
  kubectl run toolbox --image=curlimages/curl -n default --command -- sleep infinity
  kubectl exec -it toolbox -n default -- sh
  ```
  `R=http://sandboxd-router.sandboxd-controlplane-system:8080` in the commands below.

## Run

### 1. Server, then wire its pod IP into the pool

```sh
kubectl apply -f 10-netbw-server.yaml
kubectl rollout status deploy/netbw-server -n default

SRV=$(kubectl get pod -n default -l app=netbw-server -o jsonpath='{.items[0].status.podIP}')
echo "server pod IP = $SRV"

# fill the two placeholders in the pool manifest
sed -e "s#SERVER_IP_HERE#$SRV#" \
    -e "s#<WORKER_IMAGE>#820537372947.dkr.ecr.us-west-2.amazonaws.com/sandboxd-microvm:netbw#" \
    20-netbw-pool.yaml | kubectl apply -f -

kubectl apply -f 30-netbw-session.yaml
kubectl get warmpool netbw -n default -w   # wait for an idle worker (READY/IDLE ≥ 1)
```

### 2. Warm the session (starts the sandbox → the client begins reporting)

```sh
kubectl exec toolbox -n default -- sh -c '
R=http://sandboxd-router.sandboxd-controlplane-system:8080
curl -s -o /dev/null -w "warm:%{http_code}\n" -X POST \
  -H "X-Session-ID: netbw-1" -H "X-Session-Pool: netbw" $R/_warm'
```

### 3. Read the measured rates and assert the cap

```sh
# From the toolbox, read the collector (newest last):
kubectl exec toolbox -n default -- sh -c \
  'curl -s http://netbw-server.default:5002/results' ; echo
# …or from the server's own logs:
kubectl logs deploy/netbw-server -n default | grep REPORT | tail -3
# …streamConsole is on, so the worker shows it too:
kubectl logs <netbw-worker-pod> -n default | grep NETBW | tail -3
```

**PASS** (cap = 50 Mbit/s): both `egress_mbps` and `ingress_mbps` land in **~40–55**
(TBF settles just under the configured line rate). **FAIL** if either is near the
uncapped in-VPC rate (hundreds–thousands of Mbit/s — see the control below).

### 3b. Host-side cross-check (optional, deterministic)

Confirms the operator threaded the exact numbers through to tc — no traffic needed. If
the worker image has `iproute2`:

```sh
W=<netbw-worker-pod>
kubectl exec $W -n default -- tc -s qdisc show dev ateom0        # root TBF = ingress cap
kubectl exec $W -n default -- tc -s qdisc show dev ateom-bwifb   # ifb TBF  = egress cap
# rate ≈ 50Mbit on each (bytes/s under the hood: 50e6/8 = 6.25 MB/s).
```

### 4. Teleport: the cap must survive suspend → restore

```sh
# Edge-triggered on-demand suspend: checkpoint -> S3, free the worker.
kubectl patch session netbw-1 -n default --type merge \
  -p '{"spec":{"suspendRequest":"netbw-teleport-1"}}'

# Wait until Suspended AND a snapshotURI + watermark are recorded:
kubectl get session netbw-1 -n default \
  -o jsonpath='{.status.phase}{"  snap="}{.status.snapshotURI}{"\n"}' ; echo

# Restore by warming again — lands on a (typically different) idle worker:
kubectl exec toolbox -n default -- sh -c '
R=http://sandboxd-router.sandboxd-controlplane-system:8080
curl -s -o /dev/null -w "resume:%{http_code}\n" -X POST \
  -H "X-Session-ID: netbw-1" -H "X-Session-Pool: netbw" $R/_warm'

# New reports should keep landing in ~40–55 Mbit/s on the NEW worker:
kubectl exec toolbox -n default -- sh -c \
  'curl -s http://netbw-server.default:5002/results' ; echo
```

**PASS:** post-restore `egress_mbps`/`ingress_mbps` still ~40–55 (the destination worker
re-applied the cap from the `/restore` request, sourced from the SessionEntry recorded at
bind). Confirm the worker actually changed with
`kubectl get session netbw-1 -n default -o jsonpath='{.status.workerPod}'` before/after.

### 5. Uncapped control (shows the delta)

```sh
# Re-apply the pool with the caps removed, recreate the session, warm, read /results:
sed -e "s#SERVER_IP_HERE#$SRV#" \
    -e "s#<WORKER_IMAGE>#820537372947.dkr.ecr.us-west-2.amazonaws.com/sandboxd-microvm:netbw#" \
    -e "/^  network:/,+2d" \
    20-netbw-pool.yaml | kubectl apply -f -
kubectl delete session netbw-1 -n default --ignore-not-found
kubectl apply -f 30-netbw-session.yaml
# warm as in step 2; /results should now show hundreds–thousands of Mbit/s.
```

## Cleanup

```sh
kubectl delete session netbw-1 -n default --ignore-not-found
kubectl delete -f 20-netbw-pool.yaml --ignore-not-found
kubectl delete -f 10-netbw-server.yaml --ignore-not-found
kubectl delete pod toolbox -n default --ignore-not-found
```

## Notes

- **Reachability:** the client uses the server **pod IP** (VPC-routable under the AWS VPC
  CNI); the sandbox's egress SNATs through the worker pod, so a flat pod IP avoids any
  question of kube-proxy running in the sandbox netns.
- **Idle action is `suspend`** (not `reset`) so the idle sweep produces a teleport rather
  than discarding the client; the client also keeps traffic flowing, so bump
  `idle.timeoutSeconds` if you want it to sit longer before auto-suspend.
- **gVisor parity:** the same `spec.network` caps a gVisor pool too — the driver applies
  the identical tc mechanism on gVisor's host veth `sbx0` (vs microVM's `ateom0`). To run
  the gVisor variant, point a pool at the gVisor worker image with `runtime: gvisor` and
  gVisor-node scheduling (`sandbox: gvisor`); everything else in this harness is unchanged.
  Verified live: egress/ingress both ~48 Mbit/s, and `tc qdisc show dev sbx0` shows the
  same 50Mbit TBF + ifb redirect as `ateom0`.
