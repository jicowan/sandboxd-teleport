"""AIO Sandbox Broker (sandboxd backend) — Streamable HTTP MCP server.

  Claude Code --MCP+OAuth--> agentgateway --(passthrough user JWT)--> THIS
      --> sandboxd router --> sandboxd worker (nested gVisor sandbox)

This is a variant of broker_mcp.py for the **sandboxd** control plane (this repo's
checkpoint/restore session-teleport backend) instead of the kubernetes-sigs
agent-sandbox project. broker_mcp.py remains the agent-sandbox broker; do NOT
assume sandboxd is used with it.

What's the SAME as broker_mcp.py (the front-door half — auth is identical):
  1. OAuth2 resource server: validate the passed-through user JWT against
     Keycloak's JWKS (iss, aud, exp, azp).
  2. Identity + group-gate: read the principal + groups; require REQUIRED_GROUP.
  3. Per-user quota: cap concurrent sessions per principal (broker is the only
     place sessions originate, so the cap can't be bypassed).
  4. Answer MCP `initialize` locally; transparent SSE/JSON passthrough.

What's DIFFERENT (the backend half — no claim):
  - No SandboxClaim / no k8s client. sandboxd state is portable, so ANY worker in
    the pool can serve a session — the broker doesn't pick a sandbox. It just
    passes a stable **session id** and a **pool** name; the sandboxd router +
    control plane place/teleport the session onto a worker (cold start, or
    restore-on-connect from S3) transparently.
  - The session id is **derived from the principal** (one durable session per
    user), so a user reconnecting on a new MCP session lands back on their own
    teleported state.
  - Routing headers are X-Session-ID + X-Session-Pool (not X-Sandbox-*).
"""

import asyncio
import hashlib
import json
import os
import ssl
import uuid
from typing import Optional

import certifi
import httpx
import jwt
from fastapi import FastAPI, Header, HTTPException, Request, Response
from fastapi.responses import StreamingResponse
from starlette.concurrency import run_in_threadpool
from jwt import PyJWKClient

# ---- config ----

# The sandboxd pool a session should run in (maps to a SandboxTemplate via a
# WarmPool). Passed as X-Session-Pool so the control plane lazily creates the
# Session CR on first contact (no k8s client in the broker).
POOL = os.environ.get("SANDBOXD_POOL", "aio-pool")

# The sandboxd router Service. Broker forwards MCP here with X-Session-ID.
ROUTER_URL = os.environ.get(
    "SANDBOXD_ROUTER_URL",
    "http://sandboxd-router.sandboxd-controlplane-system.svc.cluster.local:8080",
).rstrip("/")

# OAuth2 resource-server validation of the inbound OBO token (same as broker_mcp).
OIDC_ISSUER = os.environ.get("AIO_OIDC_ISSUER", "https://keycloak.example.com/realms/sandbox").rstrip("/")
EXPECTED_AUDIENCE = os.environ.get("AIO_EXPECTED_AUDIENCE", "sandbox-router")
GATEWAY_AZP = os.environ.get("AIO_GATEWAY_AZP", "aio-sandbox-client")
REQUIRED_GROUP = os.environ.get("AIO_REQUIRED_GROUP", "sandbox-users")

# Per-user quota. With principal-derived session ids one user maps to one durable
# session, so the "quota" here bounds distinct sessions we mint per principal.
# 1 = one durable session per user (the substrate "one actor per user" model);
# >1 allows a user to run multiple named sessions. 0 disables the cap.
MAX_SESSIONS_PER_USER = int(os.environ.get("SANDBOXD_MAX_SESSIONS_PER_USER", "1"))

JWKS_URL = f"{OIDC_ISSUER}/protocol/openid-connect/certs"
SESSION_HEADER = "mcp-session-id"
ADVERTISED_PROTOCOL_VERSION = os.environ.get("AIO_MCP_PROTOCOL_VERSION", "2025-11-25")

app = FastAPI(title="aio-sandbox-broker-sandboxd", version="0.1.0")

_jwks_client = PyJWKClient(
    JWKS_URL,
    ssl_context=ssl.create_default_context(cafile=certifi.where()),
    cache_keys=True,
    lifespan=3600,
)


@app.on_event("startup")
def _warm_jwks():
    try:
        _jwks_client.get_signing_keys()
    except Exception:
        pass


def _signing_key(token: str):
    import time as _t
    last = None
    for attempt in range(4):
        try:
            return _jwks_client.get_signing_key_from_jwt(token).key
        except Exception as exc:
            last = exc
            _t.sleep(0.5 * (attempt + 1))
    raise last


# ---- auth (identical policy to broker_mcp.py) ----

class AuthContext:
    def __init__(self, principal: str, groups: list[str]):
        self.principal = principal
        self.groups = groups


def _authenticate(authorization: Optional[str]) -> AuthContext:
    if not authorization or not authorization.lower().startswith("bearer "):
        raise HTTPException(status_code=401, detail="Missing bearer token.")
    token = authorization.split(None, 1)[1]
    try:
        signing_key = _signing_key(token)
        claims = jwt.decode(
            token, signing_key, algorithms=["RS256"],
            audience=EXPECTED_AUDIENCE, issuer=OIDC_ISSUER,
            options={"require": ["exp", "iss", "aud"]},
        )
    except jwt.PyJWTError as exc:
        raise HTTPException(status_code=401, detail=f"Invalid token: {exc}")

    if claims.get("azp") != GATEWAY_AZP:
        raise HTTPException(status_code=403, detail=f"Token azp != {GATEWAY_AZP}; not via the Gateway.")

    groups = claims.get("groups") or []
    if REQUIRED_GROUP and REQUIRED_GROUP not in groups:
        raise HTTPException(status_code=403, detail=f"Not authorized: missing group '{REQUIRED_GROUP}'.")

    principal = (
        claims.get("preferred_username") or claims.get("email") or claims.get("sub") or ""
    ).strip()
    if not principal:
        raise HTTPException(status_code=401, detail="No principal claim in token.")
    return AuthContext(principal=principal, groups=groups)


# ---- session identity ----

def _sid_for(principal: str, mcp_session_id: Optional[str], slot: int = 0) -> str:
    """Derive a stable, DNS-safe session id from the principal (one durable
    session per user; teleportable across MCP reconnects). With
    MAX_SESSIONS_PER_USER>1, `slot` distinguishes a user's concurrent sessions
    (keyed off the MCP session id). The control plane treats this string as the
    opaque session id / sandbox id (must match ^[a-z0-9][a-z0-9-]{0,62}$)."""
    base = principal if slot == 0 else f"{principal}:{mcp_session_id or slot}"
    h = hashlib.sha256(base.encode()).hexdigest()[:16]
    # human-readable prefix + hash; sanitized to the sandbox-id charset.
    safe = "".join(c if (c.isalnum()) else "-" for c in principal.lower())[:24].strip("-") or "u"
    return f"sess-{safe}-{h}"


# ---- MCP forwarding ----

def _mcp_method(body: bytes) -> Optional[str]:
    try:
        msg = json.loads(body or b"{}")
    except ValueError:
        return None
    if isinstance(msg, list):
        return next((m.get("method") for m in msg if isinstance(m, dict) and m.get("method")), None)
    return msg.get("method") if isinstance(msg, dict) else None


def _msg_id(body: bytes):
    try:
        msg = json.loads(body or b"{}")
        return msg.get("id") if isinstance(msg, dict) else None
    except ValueError:
        return None


def _is_initialize(body: bytes) -> bool:
    return _mcp_method(body) == "initialize"


def _synthetic_initialize(req_id) -> dict:
    return {
        "jsonrpc": "2.0",
        "id": req_id,
        "result": {
            "protocolVersion": ADVERTISED_PROTOCOL_VERSION,
            "capabilities": {"tools": {"listChanged": False}},
            "serverInfo": {"name": "aio-sandbox-broker-sandboxd", "version": "0.1.0"},
        },
    }


def _rewrite_init_version(payload: bytes) -> bytes:
    text = payload.decode("utf-8", errors="replace")

    def _fix(obj):
        if isinstance(obj, dict) and isinstance(obj.get("result"), dict) and "protocolVersion" in obj["result"]:
            obj["result"]["protocolVersion"] = ADVERTISED_PROTOCOL_VERSION
        return obj

    if "data:" in text:
        out = []
        for line in text.splitlines(keepends=True):
            s = line.strip()
            if s.startswith("data:"):
                frag = s[5:].strip()
                try:
                    out.append("data: " + json.dumps(_fix(json.loads(frag))) + "\n")
                    continue
                except ValueError:
                    pass
            out.append(line)
        return "".join(out).encode()
    try:
        return json.dumps(_fix(json.loads(text))).encode()
    except ValueError:
        return payload


async def _warm(sid: str) -> None:
    """Background warm-on-initialize (O8): ask the router's generic /_warm
    primitive to ensure the session is Running before the first tool call. This
    is protocol-agnostic — the router just resumes the session and returns 204
    (no MCP payload fabricated, no workload round trip). Fire-and-forget:
    best-effort, all errors swallowed (a failed warm just means the first real
    call pays the cold start, as before)."""
    try:
        async with httpx.AsyncClient(timeout=httpx.Timeout(300.0)) as client:
            await client.post(
                f"{ROUTER_URL}/_warm",
                headers={"X-Session-ID": sid, "X-Session-Pool": POOL},
            )
    except Exception:
        pass


async def _forward(body: bytes, sid: str, content_type: str, accept: str):
    """Forward the MCP request to the sandboxd router. The router resolves
    X-Session-ID -> worker (cold start / restore-on-connect via the control
    plane) and streams the response back. X-Session-Pool tells the control plane
    which pool to place a brand-new session in."""
    headers = {
        "X-Session-ID": sid,
        "X-Session-Pool": POOL,
        "Content-Type": content_type or "application/json",
    }
    if accept:
        headers["Accept"] = accept
    rewrite = _is_initialize(body)
    client = httpx.AsyncClient(timeout=httpx.Timeout(300.0))
    req = client.build_request("POST", f"{ROUTER_URL}/mcp", content=body, headers=headers)
    resp = await client.send(req, stream=True)

    relay_headers = {
        k: v for k, v in resp.headers.items()
        if k.lower() not in ("content-length", "transfer-encoding", "connection")
    }

    if rewrite:
        raw = await resp.aread()
        await resp.aclose()
        await client.aclose()
        return Response(content=_rewrite_init_version(raw), status_code=resp.status_code,
                        headers=relay_headers, media_type=resp.headers.get("content-type"))

    async def _gen():
        try:
            async for chunk in resp.aiter_raw():
                yield chunk
        finally:
            await resp.aclose()
            await client.aclose()

    return StreamingResponse(_gen(), status_code=resp.status_code,
                             headers=relay_headers, media_type=resp.headers.get("content-type"))


# ---- routes ----

@app.get("/healthz")
def healthz() -> dict:
    return {"status": "ok", "pool": POOL, "router": ROUTER_URL}


@app.post("/")
async def mcp(
    request: Request,
    authorization: Optional[str] = Header(default=None),
    mcp_session_id: Optional[str] = Header(default=None, alias="Mcp-Session-Id"),
):
    auth = await run_in_threadpool(_authenticate, authorization)
    body = await request.body()
    content_type = request.headers.get("content-type", "application/json")
    accept = request.headers.get("accept", "application/json, text/event-stream")
    method = _mcp_method(body)

    mcp_sess = mcp_session_id or uuid.uuid4().hex

    # Answer lifecycle locally (no backend round trip) — instant handshake — AND
    # kick off a background warm of the session so it's Running before the first
    # tool call (hides AIO's ~45s cold start; avoids the miss-path 503 herd, O8).
    if method == "initialize":
        sid = _sid_for(auth.principal, mcp_sess)
        asyncio.create_task(_warm(sid))
        resp = Response(content=json.dumps(_synthetic_initialize(_msg_id(body))).encode(),
                        media_type="application/json")
        resp.headers[SESSION_HEADER] = mcp_sess
        return resp
    if method in ("notifications/initialized", "ping"):
        resp = Response(status_code=202)
        resp.headers[SESSION_HEADER] = mcp_sess
        return resp

    # Real work: derive the durable session id and forward. No claim — the
    # control plane places/teleports the session. (Quota note: with
    # principal-derived ids, one principal maps to MAX_SESSIONS_PER_USER distinct
    # session ids; slot 0 is the default durable session. A future enhancement
    # can map multiple MCP sessions to slots 1..N and enforce the cap here.)
    sid = _sid_for(auth.principal, mcp_sess)
    resp = await _forward(body, sid, content_type, accept)
    resp.headers[SESSION_HEADER] = mcp_sess
    return resp


@app.delete("/")
async def mcp_delete(
    authorization: Optional[str] = Header(default=None),
    mcp_session_id: Optional[str] = Header(default=None, alias="Mcp-Session-Id"),
) -> Response:
    # A durable per-user session is NOT destroyed on MCP DELETE — it idle-suspends
    # (checkpoint->S3) via the control plane and teleport-resumes on the user's
    # next connect. DELETE just closes the MCP transport. (Explicit teardown, if
    # ever needed, would be a separate control-plane call.)
    await run_in_threadpool(_authenticate, authorization)
    return Response(status_code=204)
