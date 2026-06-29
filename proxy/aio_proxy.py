"""Transparent stdio<->Streamable HTTP MCP proxy for an AIO Sandbox.

Architecture (two reachability modes):

  Local-dev path:
    Claude Code --stdio MCP--> this proxy --HTTP /mcp-->
      kubectl port-forward --> sandbox-router --> AIO pod :8080/mcp

  LB path (AIO_ROUTER_URL set):
    Claude Code --stdio MCP--> this proxy --HTTPS /mcp-->
      ALB (TLS) --> oauth2-proxy sidecar --> sandbox-router --> AIO pod

The AIO image's built-in MCP Hub is a Streamable HTTP MCP server at
`/mcp` on port 8080. We connect to it directly (rather than translating
to the `/v1/mcp/*` REST shim), so this proxy is transparent: tool names,
schemas, and results come straight from the in-pod hub.

The previous REST-shim implementation only enumerated sub-servers via
`GET /v1/mcp/servers`, which missed the "official" sandbox tools
(`sandbox_execute_bash`, `sandbox_file_operations`, `sandbox_execute_code`,
`sandbox_convert_to_markdown`, etc.) that are mounted as top-level tools
on `/mcp` rather than as a sub-server. Speaking MCP directly returns all
of them in one `tools/list`.

Lifecycle:
  - Read ~/.config/aio-sandbox/current.json (written by the
    aio-sandbox skill's claim.py).
  - If AIO_ROUTER_URL is set, talk to that URL directly (HTTPS to the ALB)
    and add an OIDC Bearer token via the device-authorization flow against
    the Keycloak `sandbox` realm.
  - Otherwise, open a kubectl port-forward to the sandbox-router and
    connect to the in-pod hub through it.
  - Either way, send X-Sandbox-* headers so the router targets the right pod.
  - Run an stdio MCP server on this end. `tools/list` and `tools/call`
    from Claude Code are forwarded to the upstream session unchanged.
  - If `current.json` is missing or expired, advertise a single stub
    tool whose call returns a `no_active_sandbox` error the skill knows
    how to recover from.
"""

import asyncio
import atexit
import json
import os
import socket
import subprocess
import time
from collections.abc import AsyncIterator
from contextlib import AsyncExitStack, asynccontextmanager
from pathlib import Path
from typing import Any, Optional

import httpx
from mcp import ClientSession
try:
    from mcp.client.streamable_http import streamable_http_client
    _NEW_STREAMABLE_API = True
except ImportError:  # older mcp: kwarg-based client without http_client param
    from mcp.client.streamable_http import streamablehttp_client as streamable_http_client
    _NEW_STREAMABLE_API = False
from mcp.server import Server
from mcp.server.stdio import stdio_server
import mcp.types as types

STATE_DIR = Path(os.environ.get(
    "AIO_STATE_DIR",
    str(Path.home() / ".config" / "aio-sandbox"),
))
STATE_FILE = STATE_DIR / "current.json"

ROUTER_NAMESPACE = os.environ.get("AIO_ROUTER_NAMESPACE", "default")
ROUTER_SVC = "svc/sandbox-router-svc"
ROUTER_PORT = 8080
AIO_CONTAINER_PORT = 8080

# When set, the proxy talks to the router via this URL (typically an HTTPS
# ALB in front of an oauth2-proxy sidecar) and skips kubectl port-forward.
ROUTER_URL = os.environ.get("AIO_ROUTER_URL", "").strip() or None

# OIDC config (only used when ROUTER_URL is set). Device flow against Keycloak.
OIDC_ISSUER = os.environ.get("AIO_OIDC_ISSUER", "").strip() or None
OIDC_CLIENT_ID = os.environ.get("AIO_OIDC_CLIENT_ID", "").strip() or None
OIDC_SCOPES = os.environ.get(
    "AIO_OIDC_SCOPES", "openid sandbox offline_access"
).strip()
OIDC_CACHE_FILE = STATE_DIR / "oidc.json"

# Broker base URL (Stage 1). When set, the proxy claims a sandbox by calling
# the broker's POST /sessions on first tool use, instead of reading a
# current.json written by the skill's claim.py. The broker holds the only
# SandboxClaim RBAC; this client just needs a valid in-group JWT.
BROKER_URL = os.environ.get("AIO_BROKER_URL", "").strip() or None


# ---- kubectl port-forward to the sandbox-router (lazy + cached) ----

_tunnel_proc: Optional[subprocess.Popen] = None
_tunnel_url: Optional[str] = None


def _free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def _ensure_tunnel() -> str:
    global _tunnel_proc, _tunnel_url
    if _tunnel_proc and _tunnel_proc.poll() is None and _tunnel_url:
        return _tunnel_url
    local_port = _free_port()
    _tunnel_proc = subprocess.Popen(
        [
            "kubectl", "port-forward", ROUTER_SVC,
            f"{local_port}:{ROUTER_PORT}",
            "-n", ROUTER_NAMESPACE,
        ],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    deadline = time.monotonic() + 15
    while time.monotonic() < deadline:
        if _tunnel_proc.poll() is not None:
            _, stderr = _tunnel_proc.communicate()
            raise RuntimeError(
                f"kubectl port-forward to {ROUTER_SVC} exited: "
                f"{stderr.decode(errors='replace')}"
            )
        try:
            with socket.create_connection(("127.0.0.1", local_port), timeout=0.2):
                _tunnel_url = f"http://127.0.0.1:{local_port}"
                time.sleep(0.5)
                return _tunnel_url
        except (socket.timeout, ConnectionRefusedError, OSError):
            time.sleep(0.3)
    raise TimeoutError("Failed to establish tunnel to sandbox-router.")


@atexit.register
def _stop_tunnel() -> None:
    if _tunnel_proc and _tunnel_proc.poll() is None:
        _tunnel_proc.terminate()
        try:
            _tunnel_proc.wait(timeout=3)
        except subprocess.TimeoutExpired:
            _tunnel_proc.kill()


# ---- state file helpers ----

def _read_state() -> Optional[dict]:
    if not STATE_FILE.exists():
        return None
    try:
        return json.loads(STATE_FILE.read_text())
    except json.JSONDecodeError:
        return None


def _state_is_expired(state: dict) -> bool:
    expires_at = state.get("expires_at")
    return expires_at is not None and time.time() >= expires_at


def _no_sandbox_payload() -> dict:
    return {
        "error": "no_active_sandbox",
        "hint": (
            "No active sandbox in state file. Invoke the aio-sandbox "
            "skill's claim.py to create one, then retry."
        ),
    }


# ---- OIDC device flow (used when ROUTER_URL is set) ----

_oidc_cache: dict = {}


def _load_oidc_cache() -> dict:
    global _oidc_cache
    if _oidc_cache:
        return _oidc_cache
    if OIDC_CACHE_FILE.exists():
        try:
            _oidc_cache = json.loads(OIDC_CACHE_FILE.read_text())
        except json.JSONDecodeError:
            _oidc_cache = {}
    return _oidc_cache


def _save_oidc_cache() -> None:
    OIDC_CACHE_FILE.parent.mkdir(parents=True, exist_ok=True)
    OIDC_CACHE_FILE.write_text(json.dumps(_oidc_cache))
    try:
        os.chmod(OIDC_CACHE_FILE, 0o600)
    except OSError:
        pass


async def _oidc_device_login(http: httpx.AsyncClient) -> None:
    """Run the OAuth2 device authorization grant and cache the result."""
    r = await http.post(
        f"{OIDC_ISSUER}/protocol/openid-connect/auth/device",
        data={"client_id": OIDC_CLIENT_ID, "scope": OIDC_SCOPES},
    )
    r.raise_for_status()
    d = r.json()
    interval = int(d.get("interval", 5))
    deadline = time.monotonic() + int(d.get("expires_in", 600))
    # stderr so we don't pollute the stdio MCP channel.
    import sys
    print(
        f"\n[aio-sandbox] Open {d.get('verification_uri_complete', d['verification_uri'])}"
        f" and approve (code: {d['user_code']}).\n",
        file=sys.stderr,
        flush=True,
    )
    while time.monotonic() < deadline:
        await asyncio.sleep(interval)
        tr = await http.post(
            f"{OIDC_ISSUER}/protocol/openid-connect/token",
            data={
                "client_id": OIDC_CLIENT_ID,
                "grant_type": "urn:ietf:params:oauth:grant-type:device_code",
                "device_code": d["device_code"],
            },
        )
        if tr.status_code == 200:
            body = tr.json()
            _oidc_cache.update({
                "access_token": body["access_token"],
                "refresh_token": body.get("refresh_token"),
                "exp": time.time() + int(body["expires_in"]),
            })
            _save_oidc_cache()
            return
        err = tr.json().get("error", "")
        if err == "slow_down":
            interval += 5
        elif err not in ("authorization_pending",):
            raise RuntimeError(f"OIDC device flow failed: {tr.text}")
    raise TimeoutError("OIDC device flow timed out before user approval.")


async def _oidc_refresh(http: httpx.AsyncClient) -> bool:
    """Try to refresh the access token. Returns True on success."""
    rt = _oidc_cache.get("refresh_token")
    if not rt:
        return False
    r = await http.post(
        f"{OIDC_ISSUER}/protocol/openid-connect/token",
        data={
            "client_id": OIDC_CLIENT_ID,
            "grant_type": "refresh_token",
            "refresh_token": rt,
        },
    )
    if r.status_code != 200:
        return False
    body = r.json()
    _oidc_cache.update({
        "access_token": body["access_token"],
        "refresh_token": body.get("refresh_token", rt),
        "exp": time.time() + int(body["expires_in"]),
    })
    _save_oidc_cache()
    return True


class OIDCLoginRequired(RuntimeError):
    """Raised when no usable OIDC token is cached and we can't get one within
    the MCP call's time budget. The user must run aio_login.py once."""


async def _oidc_access_token() -> str:
    if not (OIDC_ISSUER and OIDC_CLIENT_ID):
        raise RuntimeError(
            "AIO_ROUTER_URL is set but AIO_OIDC_ISSUER / AIO_OIDC_CLIENT_ID "
            "are not. Configure both, or unset AIO_ROUTER_URL for the local "
            "port-forward path."
        )
    _load_oidc_cache()
    now = time.time()
    if _oidc_cache.get("access_token") and _oidc_cache.get("exp", 0) - 60 > now:
        return _oidc_cache["access_token"]
    async with httpx.AsyncClient(timeout=httpx.Timeout(30.0)) as http:
        if _oidc_cache.get("refresh_token") and await _oidc_refresh(http):
            return _oidc_cache["access_token"]
    # No usable token. The device flow takes minutes (user has to open a
    # browser), so we don't try to run it inside an MCP call. Surface a
    # clear error instead.
    raise OIDCLoginRequired(
        "No cached OIDC token. Run `python3 aio_login.py` once with "
        "AIO_OIDC_ISSUER and AIO_OIDC_CLIENT_ID set, approve in browser, "
        "then retry."
    )


# ---- broker session (Stage 1: claim a sandbox via POST /sessions) ----

# One sandbox per proxy process (i.e. per Claude Code session). The claim is
# made lazily on first tool use and cached here for the process lifetime.
_broker_session: Optional[dict] = None


class BrokerError(RuntimeError):
    """The broker rejected the claim (e.g. 403 not in sandbox-users, or 5xx)."""


async def _broker_claim() -> dict:
    """Claim a sandbox from the broker, or return the cached claim.

    Returns a dict with sandbox_id / namespace / container_port for the
    X-Sandbox-* headers. Sends the same OIDC bearer the router uses; the
    broker's oauth2-proxy sidecar enforces group membership."""
    global _broker_session
    if _broker_session is not None:
        return _broker_session
    token = await _oidc_access_token()
    async with httpx.AsyncClient(timeout=httpx.Timeout(190.0)) as http:
        r = await http.post(
            f"{BROKER_URL.rstrip('/')}/sessions",
            headers={"Authorization": f"Bearer {token}"},
        )
    if r.status_code == 403:
        raise BrokerError(
            "Broker denied the claim (403). Your account is authenticated but "
            "not authorized — you must be a member of the sandbox-users group."
        )
    if r.status_code != 201:
        raise BrokerError(
            f"Broker POST /sessions failed: HTTP {r.status_code} {r.text[:200]}"
        )
    _broker_session = r.json()
    return _broker_session


# ---- upstream MCP client (to the in-pod /mcp hub) ----

# The Streamable HTTP MCP client runs its own internal anyio task group; if
# we cache a session across handler invocations, that task group gets torn
# down when the opening handler's task ends, which corrupts the session.
# So we open a fresh session for every list_tools/call_tool. The broker
# claim is cached, so the per-call cost is one TCP round-trip + MCP
# `initialize`.


@asynccontextmanager
async def upstream_session() -> AsyncIterator[Optional[ClientSession]]:
    """Yield a connected ClientSession to the in-pod /mcp hub, or None if
    no sandbox is active. Caller MUST await all use inside the `async with`."""
    if BROKER_URL:
        # Stage 1: claim a sandbox from the broker (cached per process).
        state = await _broker_claim()
    else:
        # Legacy path: read a claim written by the skill's claim.py.
        state = _read_state()
        if state is None or _state_is_expired(state):
            yield None
            return
    base_url = ROUTER_URL.rstrip("/") if ROUTER_URL else _ensure_tunnel().rstrip("/")
    url = f"{base_url}/mcp"
    headers = {
        "X-Sandbox-ID": state["sandbox_id"],
        "X-Sandbox-Namespace": state["namespace"],
        "X-Sandbox-Port": str(state.get("container_port", AIO_CONTAINER_PORT)),
    }
    if ROUTER_URL:
        headers["Authorization"] = f"Bearer {await _oidc_access_token()}"
    async with AsyncExitStack() as stack:
        if _NEW_STREAMABLE_API:
            http_client = await stack.enter_async_context(
                httpx.AsyncClient(headers=headers, timeout=httpx.Timeout(60.0))
            )
            read, write, _ = await stack.enter_async_context(
                streamable_http_client(url=url, http_client=http_client)
            )
        else:
            read, write, _ = await stack.enter_async_context(
                streamable_http_client(url=url, headers=headers)
            )
        session = await stack.enter_async_context(ClientSession(read, write))
        await session.initialize()
        yield session


# ---- stdio MCP server (what Claude Code talks to) ----

server = Server("aio-sandbox-proxy")


def _login_required_payload() -> dict:
    return {
        "error": "oidc_login_required",
        "hint": (
            "No cached OIDC token. Run aio_login.py once (see the auth "
            "bundle README) and retry."
        ),
    }


def _broker_error_payload(exc: "BrokerError") -> dict:
    return {"error": "broker_error", "hint": str(exc)}


@server.list_tools()
async def list_tools() -> list[types.Tool]:
    try:
        async with upstream_session() as session:
            if session is None:
                return [types.Tool(
                    name="_no_sandbox",
                    description=(
                        "No active sandbox. Calling this tool returns instructions "
                        "for claiming one via the aio-sandbox skill."
                    ),
                    inputSchema={"type": "object", "properties": {}},
                )]
            result = await session.list_tools()
            return list(result.tools)
    except OIDCLoginRequired:
        return [types.Tool(
            name="_oidc_login_required",
            description=(
                "No cached OIDC token. Calling this tool returns instructions "
                "for running aio_login.py."
            ),
            inputSchema={"type": "object", "properties": {}},
        )]
    except BrokerError as exc:
        return [types.Tool(
            name="_broker_error",
            description=f"Broker error: {exc}",
            inputSchema={"type": "object", "properties": {}},
        )]


@server.call_tool()
async def call_tool(name: str, arguments: dict) -> list[Any]:
    if name == "_no_sandbox":
        return [types.TextContent(
            type="text",
            text=json.dumps(_no_sandbox_payload()),
        )]
    if name == "_oidc_login_required":
        return [types.TextContent(
            type="text",
            text=json.dumps(_login_required_payload()),
        )]
    if name == "_broker_error":
        return [types.TextContent(
            type="text",
            text=json.dumps({"error": "broker_error",
                             "hint": "Re-run the failing tool for details."}),
        )]
    try:
        async with upstream_session() as session:
            if session is None:
                return [types.TextContent(
                    type="text",
                    text=json.dumps(_no_sandbox_payload()),
                )]
            result = await session.call_tool(name, arguments=arguments or {})
            return list(result.content)
    except OIDCLoginRequired:
        return [types.TextContent(
            type="text",
            text=json.dumps(_login_required_payload()),
        )]
    except BrokerError as exc:
        return [types.TextContent(
            type="text",
            text=json.dumps(_broker_error_payload(exc)),
        )]


async def main() -> None:
    async with stdio_server() as (read, write):
        await server.run(read, write, server.create_initialization_options())


if __name__ == "__main__":
    asyncio.run(main())
