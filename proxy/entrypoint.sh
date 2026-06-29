#!/bin/sh
# Dispatch between the long-lived MCP proxy (default) and the interactive
# device-flow login. Both scripts read AIO_STATE_DIR (default /config in the
# image), so mount the host's ~/.config/aio-sandbox directory there.
set -e

case "${1:-serve}" in
  serve)
    exec python3 -u /app/aio_proxy.py
    ;;
  login)
    shift
    exec python3 -u /app/aio_login.py "$@"
    ;;
  *)
    echo "Usage: $(basename "$0") {serve|login}" >&2
    exit 64
    ;;
esac
