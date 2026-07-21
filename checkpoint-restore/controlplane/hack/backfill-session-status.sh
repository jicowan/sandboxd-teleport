#!/usr/bin/env bash
# One-time backfill: mirror SUSPENDED sessions' state from Valkey into their
# Session CR .status (etcd), closing the migration gap for sessions that existed
# before the durable-assignment-state feature (PRD-durable-assignment-state).
#
# Only Suspended sessions are backfilled: they hold a real S3 snapshot and no live
# worker dependency, so they are the only ones that genuinely need durability. A
# Running session that loses the cache simply re-resumes.
#
# Idempotent. Requires: kubectl (context set to the target cluster), a pod that can
# reach Valkey via redis-cli (default: pod "toolbox" in ns "default"), python3.
#
# Usage: ./backfill-session-status.sh [namespace] [valkey-host] [redis-pod]
set -euo pipefail

NS="${1:-default}"
VALKEY_HOST="${2:-valkey.sandboxd-controlplane-system}"
REDIS_POD="${3:-toolbox}"

echo "context: $(kubectl config current-context)"
echo "backfilling SUSPENDED sessions in ns=$NS from valkey=$VALKEY_HOST (via pod $REDIS_POD)"

# Pull every session:* entry from Valkey as one JSON line per value.
keys=$(kubectl exec "$REDIS_POD" -n "$NS" -- redis-cli -h "$VALKEY_HOST" --scan --pattern 'session:*')
[ -z "$keys" ] && { echo "no session keys found"; exit 0; }

count=0
for k in $keys; do
  val=$(kubectl exec "$REDIS_POD" -n "$NS" -- redis-cli -h "$VALKEY_HOST" GET "$k")
  [ -z "$val" ] && continue

  # Build the status merge-patch from the KV entry, ONLY for Suspended sessions.
  patch=$(printf '%s' "$val" | python3 -c '
import sys, json, datetime
d = json.load(sys.stdin)
if d.get("state") != "Suspended":
    sys.exit(3)  # skip non-suspended
st = {"phase": "Suspended"}
for kv, sk in (("pool","pool"),("workerPod","workerPod"),("workerPodIP","workerPodIP"),
               ("image","image"),("snapshotURI","snapshotURI"),("iamRoleArn","iamRoleArn")):
    if d.get(kv):
        st[sk] = d[kv]
if d.get("ports"):
    st["ports"] = [{"container": p["container"], "host": p.get("host", 0)} for p in d["ports"]]
h = d.get("health")
if h:
    hh = {}
    for f in ("restartPolicy","probe","probePort","probePath"):
        if h.get(f) not in (None, "", 0):
            hh[f] = h[f]
    if hh:
        st["health"] = hh
la = d.get("lastActiveAt")
if la:
    st["lastActiveAt"] = datetime.datetime.fromtimestamp(la/1000.0, datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
print(json.dumps({"status": st}))
') || { continue; }   # python exit 3 (non-suspended) -> skip

  sid=$(printf '%s' "$val" | python3 -c 'import sys,json;print(json.load(sys.stdin)["sid"])')
  if ! kubectl get session "$sid" -n "$NS" >/dev/null 2>&1; then
    echo "  skip $sid: no Session CR"
    continue
  fi
  kubectl patch session "$sid" -n "$NS" --subresource=status --type=merge -p "$patch" >/dev/null
  echo "  backfilled $sid (Suspended)"
  count=$((count+1))
done
echo "done: $count suspended session(s) backfilled"
