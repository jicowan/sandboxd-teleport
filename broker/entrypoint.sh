#!/bin/sh
# Select the broker app. BROKER_APP=mcp (Stage 2, default) or rest (Stage 1).
set -e

case "${BROKER_APP:-mcp}" in
  mcp)
    exec uvicorn broker_mcp:app --host 0.0.0.0 --port 8080
    ;;
  rest)
    exec uvicorn broker:app --host 0.0.0.0 --port 8080
    ;;
  *)
    echo "BROKER_APP must be 'mcp' or 'rest', got: ${BROKER_APP}" >&2
    exit 64
    ;;
esac
