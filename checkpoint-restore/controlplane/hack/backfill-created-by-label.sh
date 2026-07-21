#!/usr/bin/env bash
# One-time migration: stamp sandboxd.io/created-by=operator on Session CRs that
# predate the ownership label (PRD-session-garbage-collection §10 Q7).
#
# Sessions the operator lazily creates from a broker pool hint (resume_glue.go
# planFor) now carry this label so GC may DELETE them when dead; CRs created before
# that change carry no label and would be treated as user-declared (tombstone-only,
# never deleted). This backfill labels pre-existing operator-created sessions so the
# GC's operator-owned reap path applies to them.
#
# SAFETY: this asserts the listed Sessions were operator-created. In the single test
# env every Session was broker/operator-created (none hand-declared), so it labels
# all of them. If you ever run a mixed fleet, pass an explicit allowlist instead of
# --all, or exclude any you authored by hand — a labeled CR becomes GC-deletable.
#
# Idempotent (re-labeling is a no-op). Requires: kubectl (context set to target).
#
# Usage:
#   ./backfill-created-by-label.sh [namespace]            # label ALL sessions (test env)
#   ./backfill-created-by-label.sh [namespace] sid1 sid2  # label only the named sessions
set -euo pipefail

NS="${1:-default}"
shift || true

echo "context: $(kubectl config current-context)"

if [ "$#" -gt 0 ]; then
  sids="$*"
  echo "labeling ${#} named session(s) in ns=$NS as created-by=operator"
else
  sids=$(kubectl get sessions -n "$NS" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')
  echo "labeling ALL sessions in ns=$NS as created-by=operator"
fi
[ -z "$sids" ] && { echo "no sessions found"; exit 0; }

count=0
for sid in $sids; do
  existing=$(kubectl get session "$sid" -n "$NS" \
    -o jsonpath='{.metadata.labels.sandboxd\.io/created-by}' 2>/dev/null || true)
  if [ "$existing" = "operator" ]; then
    echo "  skip $sid: already labeled"
    continue
  fi
  kubectl label session "$sid" -n "$NS" sandboxd.io/created-by=operator --overwrite >/dev/null
  echo "  labeled $sid"
  count=$((count+1))
done
echo "done: $count session(s) labeled created-by=operator"
