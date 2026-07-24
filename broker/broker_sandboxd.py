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
  2. Identity + group-gate: read the principal + groups; require REQUIRED_GROUP
     (and, in multi-app mode, the selected app's own required group).
  3. TRANSPARENT MCP proxy: forward every method (incl. initialize) to the
     sandbox's own MCP server, relaying its Mcp-Session-Id (supports stateful servers).

There is no per-user session *count* quota: a session id is derived from the
(user, app) pair, so a principal maps to exactly one durable session per app — the
bound is structural (# apps entitled), not a counter. Real quota levers live
elsewhere: per-app Keycloak group entitlement, WarmPool capacity, and the ForkSet
count cap. (See docs/sandboxd/HOWTO-tool-access-and-quotas.md.)

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

# The AppTemplate to run on a GENERIC pool (sent as X-Session-App). Empty ⇒ the pool
# is DEDICATED and supplies its own image (classic behavior). Set this when SANDBOXD_POOL
# is a generic pool. See docs/PRD-arbitrary-image-sessions.md §13.
APP = os.environ.get("SANDBOXD_APP", "")

# Multi-app mode (docs/PRD-broker-multi-app.md, Level B). SANDBOXD_APPS is a JSON map
# of app-id -> {appTemplate, pool, group}, letting ONE broker front several sandbox
# apps. The app-id arrives per request in the X-Sandbox-App header, which an
# agentgateway per-app route injects (route /aio -> add X-Sandbox-App: aio). The broker
# resolves the id -> (pool, AppTemplate) and ENFORCES the app's required Keycloak group
# (the header is a hint; this group check is the security boundary). Example:
#   SANDBOXD_APPS='{"aio":{"appTemplate":"aio-app","pool":"aio-generic-pool","group":"sandbox-users"},
#                   "devbox":{"appTemplate":"devbox-app","pool":"aio-generic-pool","group":"sandbox-power"}}'
# When SANDBOXD_APPS is UNSET, the broker runs in legacy SINGLE-app mode using POOL/APP
# above (the X-Sandbox-App header is ignored) — existing deploys are unchanged.
_APPS_RAW = os.environ.get("SANDBOXD_APPS", "").strip()
try:
    APPS = json.loads(_APPS_RAW) if _APPS_RAW else {}
except ValueError as _e:
    raise SystemExit(f"SANDBOXD_APPS is not valid JSON: {_e}")
# Fallback app-id when a multi-app request omits X-Sandbox-App (optional).
DEFAULT_APP_ID = os.environ.get("SANDBOXD_DEFAULT_APP", "").strip()


def _resolve_app(app_id: Optional[str], groups: list) -> tuple:
    """Resolve a request's (app_key, pool, app_template), enforcing entitlement.

    Legacy mode (SANDBOXD_APPS unset): ignore app_id, use env POOL/APP; app_key="" so
    the session id stays principal-only (unchanged behavior).

    Multi-app mode: map the X-Sandbox-App id via the registry, require the app's
    Keycloak group, and return that app's pool + AppTemplate."""
    if not APPS:
        return ("", POOL, APP)
    key = (app_id or DEFAULT_APP_ID).strip()
    if not key:
        raise HTTPException(status_code=400, detail="No app selected (missing X-Sandbox-App and no default).")
    entry = APPS.get(key)
    if not entry:
        raise HTTPException(status_code=404, detail=f"Unknown app '{key}'.")
    grp = entry.get("group")
    if grp and grp not in groups:
        raise HTTPException(status_code=403, detail=f"Not authorized for app '{key}': missing group '{grp}'.")
    return (key, entry.get("pool") or POOL, entry.get("appTemplate") or "")


def _session_headers(sid: str, pool: str, app_template: str) -> dict:
    """Routing headers the broker sends to the sandboxd router: session id, the
    pool (capacity), and — on a generic pool — the AppTemplate (workload). The
    control plane uses pool+app only to lazily create the Session CR on first
    contact; both are ignored once the session exists."""
    h = {"X-Session-ID": sid, "X-Session-Pool": pool}
    if app_template:
        h["X-Session-App"] = app_template
    return h

# The sandboxd router Service. Broker forwards MCP here with X-Session-ID.
ROUTER_URL = os.environ.get(
    "SANDBOXD_ROUTER_URL",
    "http://sandboxd-router.sandboxd-controlplane-system.svc.cluster.local:8080",
).rstrip("/")

# OAuth2 resource-server validation of the inbound OBO token (same as broker_mcp).
# NOTE: AIO_OIDC_ISSUER's default is a non-functional PLACEHOLDER (keycloak.example.com,
# from the public-repo scrub). It MUST be overridden via env to your real Keycloak realm
# issuer, or EVERY token is rejected (issuer mismatch + JWKS unreachable). The deploy
# manifest (controlplane/deploy/aio/broker-sandboxd.yaml) sets it explicitly — do not
# rely on this default.
OIDC_ISSUER = os.environ.get("AIO_OIDC_ISSUER", "https://keycloak.example.com/realms/sandbox").rstrip("/")
EXPECTED_AUDIENCE = os.environ.get("AIO_EXPECTED_AUDIENCE", "sandbox-router")
GATEWAY_AZP = os.environ.get("AIO_GATEWAY_AZP", "aio-sandbox-client")
REQUIRED_GROUP = os.environ.get("AIO_REQUIRED_GROUP", "sandbox-users")

JWKS_URL = f"{OIDC_ISSUER}/protocol/openid-connect/certs"
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

def _sid_for(principal: str, app_key: str = "") -> str:
    """Derive a stable, DNS-safe session id from the principal: ONE durable session
    per (user, app), teleportable across MCP reconnects. `app_key` (multi-app mode)
    folds the selected app into BOTH the hash and the human-readable prefix so a
    user's different apps get DISTINCT durable sessions (sess-<user>-<app>-<hash>)
    that never clobber each other; empty app_key keeps the legacy principal-only id
    byte-for-byte. The control plane treats this string as the opaque session id /
    sandbox id (must match ^[a-z0-9][a-z0-9-]{0,62}$)."""
    base = principal
    if app_key:
        base = f"{app_key}:{base}"
    h = hashlib.sha256(base.encode()).hexdigest()[:16]
    # human-readable prefix + hash; sanitized to the sandbox-id charset.
    user = "".join(c if c.isalnum() else "-" for c in principal.lower())[:24].strip("-") or "u"
    if app_key:
        ak = "".join(c if c.isalnum() else "-" for c in app_key.lower())[:16].strip("-")
        prefix = f"{user}-{ak}" if ak else user
    else:
        prefix = user
    return f"sess-{prefix}-{h}"


# ---- MCP forwarding ----

def _mcp_method(body: bytes) -> Optional[str]:
    try:
        msg = json.loads(body or b"{}")
    except ValueError:
        return None
    if isinstance(msg, list):
        return next((m.get("method") for m in msg if isinstance(m, dict) and m.get("method")), None)
    return msg.get("method") if isinstance(msg, dict) else None


def _is_initialize(body: bytes) -> bool:
    return _mcp_method(body) == "initialize"


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


async def _forward(method: str, body: bytes, sid: str, pool: str, app_template: str,
                   content_type: str, accept: str, client_mcp_session: Optional[str] = None):
    """TRANSPARENTLY proxy an MCP request to the sandboxd router → the sandbox's own
    MCP server. The router resolves X-Session-ID → worker (cold start / restore via
    the control plane). This is a full proxy (not a synthesized handshake), so it
    supports ANY MCP server — including STATEFUL ones (e.g. the everything reference
    server) that issue and require their own Mcp-Session-Id:

      - the CLIENT's Mcp-Session-Id (if any) is passed THROUGH to the sandbox, so the
        sandbox's MCP server keeps its protocol session across calls;
      - the SANDBOX's Mcp-Session-Id response header is relayed BACK to the client
        unchanged (we do NOT overwrite it — that was the bug that broke stateful
        servers). X-Session-ID (routing) and Mcp-Session-Id (MCP protocol session)
        are orthogonal: the former is broker-derived per user+app, the latter is the
        sandbox MCP server's own.

    method is GET (SSE server→client stream) or POST (requests/notifications)."""
    headers = _session_headers(sid, pool, app_template)
    if content_type:
        headers["Content-Type"] = content_type
    if accept:
        headers["Accept"] = accept
    if client_mcp_session:
        headers["Mcp-Session-Id"] = client_mcp_session
    rewrite = method == "POST" and _is_initialize(body)
    client = httpx.AsyncClient(timeout=httpx.Timeout(300.0))
    req = client.build_request(method, f"{ROUTER_URL}/mcp",
                               content=body if method == "POST" else None, headers=headers)
    resp = await client.send(req, stream=True)

    # Relay ALL response headers except hop-by-hop — crucially KEEP the sandbox's
    # Mcp-Session-Id so a stateful server's session round-trips to the client.
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
    sandbox_app: Optional[str] = Header(default=None, alias="X-Sandbox-App"),
):
    auth = await run_in_threadpool(_authenticate, authorization)
    body = await request.body()
    content_type = request.headers.get("content-type", "application/json")
    accept = request.headers.get("accept", "application/json, text/event-stream")

    # Resolve the selected app (multi-app mode) → (app_key, pool, AppTemplate),
    # enforcing the app's required group. Legacy single-app mode returns
    # ("", POOL, APP) and ignores the header. Raises 400/403/404 on a bad/unentitled
    # app. app_key folds into the durable X-Session-ID so a user's apps don't collide.
    app_key, pool, app_template = _resolve_app(sandbox_app, auth.groups)

    # X-Session-ID (control-plane routing) is derived per user+app and is STABLE
    # (slot 0 = one durable session per user+app, teleport-safe across reconnects).
    # It is INDEPENDENT of the MCP-protocol Mcp-Session-Id, which is owned by the
    # sandbox's own MCP server and passed through transparently by _forward.
    sid = _sid_for(auth.principal, app_key=app_key)

    # TRANSPARENT PROXY: forward EVERY method (incl. initialize + notifications) to
    # the sandbox's MCP server, so stateful servers keep their protocol session and
    # any server (not just AIO) works. Forwarding initialize also warms the session
    # (cold start / restore) — no separate synthetic handshake. The sandbox's
    # Mcp-Session-Id round-trips via _forward's relayed headers; we do NOT overwrite
    # it (overwriting it was what broke stateful servers like the everything server).
    return await _forward(request.method, body, sid, pool, app_template,
                          content_type, accept, client_mcp_session=mcp_session_id)


@app.get("/")
async def mcp_get(
    request: Request,
    authorization: Optional[str] = Header(default=None),
    mcp_session_id: Optional[str] = Header(default=None, alias="Mcp-Session-Id"),
    sandbox_app: Optional[str] = Header(default=None, alias="X-Sandbox-App"),
):
    # Streamable-HTTP GET = the server→client SSE stream a stateful MCP server opens
    # for async notifications. Proxy it through transparently (previously this 405'd
    # because there was no GET route). No body; the sandbox's server decides whether
    # it supports the stream (may itself 405, which is fine).
    auth = await run_in_threadpool(_authenticate, authorization)
    accept = request.headers.get("accept", "text/event-stream")
    app_key, pool, app_template = _resolve_app(sandbox_app, auth.groups)
    sid = _sid_for(auth.principal, app_key=app_key)
    return await _forward("GET", b"", sid, pool, app_template, "", accept,
                          client_mcp_session=mcp_session_id)


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
