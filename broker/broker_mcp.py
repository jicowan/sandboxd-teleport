"""AIO Sandbox Broker — Streamable HTTP MCP server behind agentgateway.

  Claude Code --MCP+OAuth--> agentgateway --(passthrough user JWT)--> THIS
      --> sandbox-router --> AIO pod

agentgateway validates the inbound Keycloak user JWT and forwards that SAME
token to this broker (backendAuth: passthrough), so the broker sees the real
end-user identity. (This replaced an AgentCore Gateway design whose OBO/3LO
identity propagation could not work for a transparent live MCP target — see
docs/POSTMORTEM-agentcore-vs-agentgateway.md.)

Responsibilities:
  1. OAuth2 resource server: validate the passed-through user JWT against
     Keycloak's JWKS — iss, aud, exp, and azp == the expected client. Tolerates
     intermittent JWKS-fetch DNS failures (cache + retry).
  2. Identity + authz: read preferred_username (principal) and groups; require
     membership in AIO_REQUIRED_GROUP.
  3. Lifecycle: answer MCP `initialize` locally (instant — no AIO round trip);
     on the first real tool call, claim-or-reuse a sandbox labelled with the
     MCP session id (stateless: any replica reconstructs session->claim by
     label). Release on DELETE; the claim TTL is the backstop.
  4. Transparent forwarding: relay tools/list and tools/call to the
     sandbox-router with X-Sandbox-* headers; stream the response (json or SSE)
     back unchanged so the AIO hub's tools pass through verbatim.

Stage 1 (broker.py, REST) remains in the repo as the portable fallback.
"""

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

from k8s_agent_sandbox import SandboxClient
from k8s_agent_sandbox.constants import (
    CLAIM_API_GROUP,
    CLAIM_API_VERSION,
    CLAIM_PLURAL_NAME,
)

# ---- config ----

TEMPLATE_NAME = os.environ.get("AIO_TEMPLATE_NAME", "aio-sandbox-template")
NAMESPACE = os.environ.get("AIO_NAMESPACE", "default")
WARMPOOL = os.environ.get("AIO_WARMPOOL", "aio-sandbox-warmpool")
TTL_SECONDS = int(os.environ.get("AIO_TTL_SECONDS", "3600"))
READY_TIMEOUT = int(os.environ.get("AIO_READY_TIMEOUT", "180"))
AIO_CONTAINER_PORT = 8080

# Where to forward MCP traffic. The in-cluster router Service; the broker
# reaches it directly (it already holds the OBO identity, no second hop auth
# needed if the router trusts in-cluster callers — otherwise set AIO_ROUTER_BEARER).
ROUTER_URL = os.environ.get("AIO_ROUTER_URL", "http://sandbox-router-svc.default.svc.cluster.local:8080").rstrip("/")

# OAuth2 resource-server validation of the inbound OBO token.
OIDC_ISSUER = os.environ.get("AIO_OIDC_ISSUER", "https://keycloak.jicomusic.com/realms/sandbox").rstrip("/")
# The audience the token must carry (the router resource).
EXPECTED_AUDIENCE = os.environ.get("AIO_EXPECTED_AUDIENCE", "sandbox-router")
# With agentgateway passthrough the token is the user's own token, minted by
# the public client the user logged in through. Assert azp to confirm the
# token came via the expected client (defense-in-depth; agentgateway already
# validated the signature/issuer/audience inbound).
GATEWAY_AZP = os.environ.get("AIO_GATEWAY_AZP", "aio-sandbox-client")
# Coarse "may you get a sandbox at all" gate: the JWT must carry this group.
REQUIRED_GROUP = os.environ.get("AIO_REQUIRED_GROUP", "sandbox-users")
# Per-user quota: the maximum number of concurrent sandboxes a single principal
# may hold. The broker is the only component that can create claims, so this is
# the enforcement point. 0 disables the cap.
MAX_SANDBOXES_PER_USER = int(os.environ.get("AIO_MAX_SANDBOXES_PER_USER", "3"))

JWKS_URL = f"{OIDC_ISSUER}/protocol/openid-connect/certs"

# Labels that make session->claim mapping durable and replica-independent.
LABEL_MANAGED = "aio-sandbox.broker/managed"
LABEL_SESSION = "aio-sandbox.broker/session-id"
LABEL_PRINCIPAL = "aio-sandbox.broker/principal"

SESSION_HEADER = "mcp-session-id"

app = FastAPI(title="aio-sandbox-broker-mcp", version="0.4.0")

# Use certifi's CA bundle explicitly so JWKS fetching works regardless of the
# base image / OS trust store configuration. Cache keys long and tolerate
# transient JWKS-fetch failures (this cluster's DNS is intermittently flaky;
# a cold or mid-flight resolution error must not fail an otherwise-valid token).
_jwks_client = PyJWKClient(
    JWKS_URL,
    ssl_context=ssl.create_default_context(cafile=certifi.where()),
    cache_keys=True,
    lifespan=3600,
)


@app.on_event("startup")
def _warm_jwks():
    # Best-effort: populate the JWKS cache at startup so the first request
    # doesn't race flaky DNS. Failures here are non-fatal.
    try:
        _jwks_client.get_signing_keys()
    except Exception:
        pass


def _signing_key(token: str):
    """Fetch the JWT signing key, retrying transient JWKS-fetch errors (DNS)."""
    import time as _t
    last = None
    for attempt in range(4):
        try:
            return _jwks_client.get_signing_key_from_jwt(token).key
        except Exception as exc:  # PyJWKClientError wraps DNS/URL errors
            last = exc
            _t.sleep(0.5 * (attempt + 1))
    raise last


# ---- auth: validate the OBO token as a resource server ----

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
            token,
            signing_key,
            algorithms=["RS256"],
            audience=EXPECTED_AUDIENCE,
            issuer=OIDC_ISSUER,
            options={"require": ["exp", "iss", "aud"]},
        )
    except jwt.PyJWTError as exc:
        raise HTTPException(status_code=401, detail=f"Invalid token: {exc}")

    # Assert the token came through the Gateway's OBO exchange.
    if claims.get("azp") != GATEWAY_AZP:
        raise HTTPException(
            status_code=403,
            detail=f"Token azp != {GATEWAY_AZP}; not via the Gateway.",
        )

    groups = claims.get("groups") or []
    if REQUIRED_GROUP and REQUIRED_GROUP not in groups:
        raise HTTPException(
            status_code=403,
            detail=f"Not authorized: missing group '{REQUIRED_GROUP}'.",
        )

    principal = (
        claims.get("preferred_username")
        or claims.get("email")
        or claims.get("sub")
        or ""
    ).strip()
    if not principal:
        raise HTTPException(status_code=401, detail="No principal claim in token.")
    return AuthContext(principal=principal, groups=groups)


# ---- lifecycle (reused from Stage 1, with label support) ----

def _client() -> SandboxClient:
    return SandboxClient()


def _label_safe(value: str) -> str:
    safe = "".join(c if (c.isalnum() or c in "._-") else "-" for c in value)
    return safe[:63].strip("-._") or "unknown"


def _list_claims(client: SandboxClient, selector: str) -> list:
    resp = client.k8s_helper.custom_objects_api.list_namespaced_custom_object(
        group=CLAIM_API_GROUP, version=CLAIM_API_VERSION,
        namespace=NAMESPACE, plural=CLAIM_PLURAL_NAME, label_selector=selector,
    )
    return resp.get("items", [])


def _claim_for_session(
    client: SandboxClient, session_id: str, principal: str
) -> Optional[dict]:
    # Scope the lookup by BOTH session id and principal. The session id is a
    # random UUID, but binding it to the authenticated principal makes the
    # mapping defense-in-depth: a session id can never resolve to another
    # user's sandbox even if one were guessed/leaked (tenant isolation).
    selector = (
        f"{LABEL_SESSION}={_label_safe(session_id)},"
        f"{LABEL_PRINCIPAL}={_label_safe(principal)}"
    )
    items = _list_claims(client, selector)
    return items[0] if items else None


def _count_claims_for_principal(client: SandboxClient, principal: str) -> int:
    selector = (
        f"{LABEL_MANAGED}=true,{LABEL_PRINCIPAL}={_label_safe(principal)}"
    )
    return len(_list_claims(client, selector))


def _sandbox_headers(sandbox_id: str) -> dict:
    return {
        "X-Sandbox-ID": sandbox_id,
        "X-Sandbox-Namespace": NAMESPACE,
        "X-Sandbox-Port": str(AIO_CONTAINER_PORT),
    }


def _claim_sandbox(principal: str, session_id: str) -> str:
    """Claim a template-backed, warmpool-adopted sandbox; label it with the
    session id and principal. Returns the sandbox_id.

    Enforces the per-user quota: a principal may hold at most
    MAX_SANDBOXES_PER_USER concurrent sandboxes. Since this is the only place a
    claim is created, the cap can't be bypassed by opening more MCP sessions.
    """
    client = _client()
    if MAX_SANDBOXES_PER_USER > 0:
        current = _count_claims_for_principal(client, principal)
        if current >= MAX_SANDBOXES_PER_USER:
            raise HTTPException(
                status_code=429,
                detail=(
                    f"Sandbox quota reached: {current}/{MAX_SANDBOXES_PER_USER} "
                    f"for '{principal}'. Release a sandbox (DELETE the MCP "
                    f"session) or wait for one to expire."
                ),
            )
    sandbox = client.create_sandbox(
        template=TEMPLATE_NAME,
        namespace=NAMESPACE,
        warmpool=WARMPOOL,
        shutdown_after_seconds=TTL_SECONDS,
        sandbox_ready_timeout=READY_TIMEOUT,
        labels={
            LABEL_MANAGED: "true",
            LABEL_SESSION: _label_safe(session_id),
            LABEL_PRINCIPAL: _label_safe(principal),
        },
    )
    return sandbox.sandbox_id


def _resolve_sandbox_id(session_id: str, principal: str) -> Optional[str]:
    client = _client()
    claim = _claim_for_session(client, session_id, principal)
    if not claim:
        return None
    return (claim.get("status", {}) or {}).get("sandbox", {}).get("name")


def _release_session(session_id: str, principal: str) -> None:
    client = _client()
    claim = _claim_for_session(client, session_id, principal)
    if not claim:
        return
    try:
        client.delete_sandbox(claim["metadata"]["name"], namespace=NAMESPACE)
    except Exception:
        pass  # idempotent; TTL is the backstop


# ---- MCP forwarding ----

# The MCP protocol version this broker advertises to the Gateway. A
# 2025-11-25 gateway requires the backend to speak 2025-11-25 end-to-end, but
# the AIO hub negotiates an older version. Since the broker is the MCP server
# the Gateway talks to, we override the initialize response's protocolVersion
# to this value. Tool-call forwarding is version-agnostic JSON-RPC passthrough.
ADVERTISED_PROTOCOL_VERSION = os.environ.get("AIO_MCP_PROTOCOL_VERSION", "2025-11-25")


def _is_initialize(body: bytes) -> bool:
    try:
        msg = json.loads(body or b"{}")
    except ValueError:
        return False
    if isinstance(msg, list):
        return any(isinstance(m, dict) and m.get("method") == "initialize" for m in msg)
    return isinstance(msg, dict) and msg.get("method") == "initialize"


def _rewrite_init_version(payload: bytes) -> bytes:
    """Rewrite result.protocolVersion in an initialize response (json or SSE)."""
    text = payload.decode("utf-8", errors="replace")

    def _fix_json_obj(obj):
        if isinstance(obj, dict) and isinstance(obj.get("result"), dict) \
                and "protocolVersion" in obj["result"]:
            obj["result"]["protocolVersion"] = ADVERTISED_PROTOCOL_VERSION
        return obj

    # SSE frames: rewrite each `data:` JSON line; else treat whole body as JSON.
    if "data:" in text:
        out = []
        for line in text.splitlines(keepends=True):
            s = line.strip()
            if s.startswith("data:"):
                frag = s[5:].strip()
                try:
                    out.append("data: " + json.dumps(_fix_json_obj(json.loads(frag))) + "\n")
                    continue
                except ValueError:
                    pass
            out.append(line)
        return "".join(out).encode()
    try:
        return json.dumps(_fix_json_obj(json.loads(text))).encode()
    except ValueError:
        return payload


async def _forward(body: bytes, sandbox_id: str, content_type: str, accept: str):
    """Forward the raw MCP request to the router with X-Sandbox-* headers.

    For initialize, buffer the response and rewrite protocolVersion so the
    Gateway's end-to-end version check passes. For everything else, relay the
    response stream unchanged (transparent passthrough)."""
    headers = _sandbox_headers(sandbox_id)
    headers["Content-Type"] = content_type or "application/json"
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
        return Response(
            content=_rewrite_init_version(raw), status_code=resp.status_code,
            headers=relay_headers, media_type=resp.headers.get("content-type"),
        )

    async def _gen():
        try:
            async for chunk in resp.aiter_raw():
                yield chunk
        finally:
            await resp.aclose()
            await client.aclose()

    return StreamingResponse(
        _gen(), status_code=resp.status_code,
        headers=relay_headers, media_type=resp.headers.get("content-type"),
    )


# ---- routes ----

@app.get("/healthz")
def healthz() -> dict:
    return {"status": "ok", "template": TEMPLATE_NAME, "router": ROUTER_URL}


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


def _synthetic_initialize(req_id) -> dict:
    """Answer MCP initialize locally — instantly — without touching the AIO hub.

    The handshake doesn't need a sandbox; we claim lazily on the first real
    tool call. This decouples the (slow, sometimes >20s) AIO-hub init from the
    MCP handshake, which front proxies (agentgateway, AgentCore Gateway) time
    out on. The sandbox tools still pass through transparently on tools/list
    and tools/call."""
    return {
        "jsonrpc": "2.0",
        "id": req_id,
        "result": {
            "protocolVersion": ADVERTISED_PROTOCOL_VERSION,
            "capabilities": {"tools": {"listChanged": False}},
            "serverInfo": {"name": "aio-sandbox-broker", "version": "0.4.0"},
        },
    }


@app.post("/")
async def mcp(
    request: Request,
    authorization: Optional[str] = Header(default=None),
    mcp_session_id: Optional[str] = Header(default=None, alias="Mcp-Session-Id"),
):
    # _authenticate + the SandboxClient calls below are BLOCKING (JWKS HTTP,
    # k8s API). Run them in the threadpool so they don't stall the single
    # uvicorn event loop (which would starve /healthz and serialize requests).
    auth = await run_in_threadpool(_authenticate, authorization)
    body = await request.body()
    content_type = request.headers.get("content-type", "application/json")
    accept = request.headers.get("accept", "application/json, text/event-stream")
    method = _mcp_method(body)

    # One sandbox per session; claim-or-reuse so a dropped/reset session id
    # doesn't hard-fail. session id is echoed back on every response.
    session_id = mcp_session_id or uuid.uuid4().hex

    # MCP lifecycle messages the broker answers itself — no sandbox needed,
    # no AIO-hub round trip, so the handshake is instant.
    if method == "initialize":
        resp = Response(
            content=json.dumps(_synthetic_initialize(_msg_id(body))).encode(),
            media_type="application/json",
        )
        resp.headers[SESSION_HEADER] = session_id
        return resp
    if method in ("notifications/initialized", "ping"):
        # notifications get no JSON-RPC response body
        resp = Response(status_code=202)
        resp.headers[SESSION_HEADER] = session_id
        return resp

    # Real work (tools/list, tools/call, ...) — ensure a sandbox, then forward.
    sandbox_id = await run_in_threadpool(_resolve_sandbox_id, session_id, auth.principal)
    if not sandbox_id:
        sandbox_id = await run_in_threadpool(_claim_sandbox, auth.principal, session_id)

    resp = await _forward(body, sandbox_id, content_type, accept)
    resp.headers[SESSION_HEADER] = session_id
    return resp


@app.delete("/")
async def mcp_delete(
    authorization: Optional[str] = Header(default=None),
    mcp_session_id: Optional[str] = Header(default=None, alias="Mcp-Session-Id"),
) -> Response:
    auth = await run_in_threadpool(_authenticate, authorization)
    if mcp_session_id:
        await run_in_threadpool(_release_session, mcp_session_id, auth.principal)
    return Response(status_code=204)
