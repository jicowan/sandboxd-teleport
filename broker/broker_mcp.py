"""AIO Sandbox Broker — Stage 2 (MCP server behind AgentCore Gateway).

Reshapes the Stage-1 REST broker into a Streamable HTTP MCP server that sits
as the Gateway's MCP-server target:

  Claude Code --MCP+OAuth--> AgentCore Gateway --OBO Bearer--> THIS --> router --> AIO pod

Responsibilities:
  1. OAuth2 resource server: validate the inbound Bearer (the Gateway's OBO
     token) against Keycloak's JWKS — iss, aud, exp, and azp == the gateway
     outbound client. Unlike Stage 1 (which trusted an oauth2-proxy sidecar),
     this validates the signature itself.
  2. Identity + authz: read preferred_username (principal) and groups; require
     membership in AIO_REQUIRED_GROUP.
  3. Lifecycle: on MCP `initialize`, claim-or-reuse a sandbox and label the
     SandboxClaim with the session id so the broker stays stateless (any
     replica reconstructs session->claim with a label lookup). Release on
     MCP session DELETE; the claim TTL is the backstop.
  4. Transparent forwarding: relay MCP JSON-RPC to the sandbox-router with the
     X-Sandbox-* headers; stream the response (json or SSE) back unchanged, so
     the AIO hub's tools pass through verbatim.

Stage 1 (broker.py, REST) remains in the repo as the portable, Gateway-free
fallback (see docs/adr/0002).
"""

import os
import ssl
import uuid
from typing import Optional

import certifi
import httpx
import jwt
from fastapi import FastAPI, Header, HTTPException, Request, Response
from fastapi.responses import StreamingResponse
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
# The OBO actor: the Gateway's outbound client. We assert the token was minted
# through it (azp), proving it arrived via the Gateway's token-exchange.
GATEWAY_AZP = os.environ.get("AIO_GATEWAY_AZP", "sandbox-gateway-outbound")
REQUIRED_GROUP = os.environ.get("AIO_REQUIRED_GROUP", "sandbox-users")

JWKS_URL = f"{OIDC_ISSUER}/protocol/openid-connect/certs"

# Labels that make session->claim mapping durable and replica-independent.
LABEL_MANAGED = "aio-sandbox.broker/managed"
LABEL_SESSION = "aio-sandbox.broker/session-id"
LABEL_PRINCIPAL = "aio-sandbox.broker/principal"

SESSION_HEADER = "mcp-session-id"

app = FastAPI(title="aio-sandbox-broker-mcp", version="0.2.0")

# Use certifi's CA bundle explicitly so JWKS fetching works regardless of the
# base image / OS trust store configuration.
_jwks_client = PyJWKClient(JWKS_URL, ssl_context=ssl.create_default_context(cafile=certifi.where()))


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
        signing_key = _jwks_client.get_signing_key_from_jwt(token).key
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


def _claim_for_session(client: SandboxClient, session_id: str) -> Optional[dict]:
    items = _list_claims(client, f"{LABEL_SESSION}={_label_safe(session_id)}")
    return items[0] if items else None


def _sandbox_headers(sandbox_id: str) -> dict:
    return {
        "X-Sandbox-ID": sandbox_id,
        "X-Sandbox-Namespace": NAMESPACE,
        "X-Sandbox-Port": str(AIO_CONTAINER_PORT),
    }


def _claim_sandbox(principal: str, session_id: str) -> str:
    """Claim a template-backed, warmpool-adopted sandbox; label it with the
    session id and principal. Returns the sandbox_id."""
    client = _client()
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


def _resolve_sandbox_id(session_id: str) -> Optional[str]:
    client = _client()
    claim = _claim_for_session(client, session_id)
    if not claim:
        return None
    return (claim.get("status", {}) or {}).get("sandbox", {}).get("name")


def _release_session(session_id: str) -> None:
    client = _client()
    claim = _claim_for_session(client, session_id)
    if not claim:
        return
    try:
        client.delete_sandbox(claim["metadata"]["name"], namespace=NAMESPACE)
    except Exception:
        pass  # idempotent; TTL is the backstop


# ---- MCP forwarding ----

async def _forward(body: bytes, sandbox_id: str, content_type: str, accept: str):
    """Forward the raw MCP request to the router with X-Sandbox-* headers and
    relay the response (json or SSE) back unchanged."""
    headers = _sandbox_headers(sandbox_id)
    headers["Content-Type"] = content_type or "application/json"
    if accept:
        headers["Accept"] = accept
    client = httpx.AsyncClient(timeout=httpx.Timeout(300.0))
    req = client.build_request("POST", f"{ROUTER_URL}/mcp", content=body, headers=headers)
    resp = await client.send(req, stream=True)

    async def _gen():
        try:
            async for chunk in resp.aiter_raw():
                yield chunk
        finally:
            await resp.aclose()
            await client.aclose()

    relay_headers = {
        k: v for k, v in resp.headers.items()
        if k.lower() not in ("content-length", "transfer-encoding", "connection")
    }
    return StreamingResponse(
        _gen(), status_code=resp.status_code,
        headers=relay_headers, media_type=resp.headers.get("content-type"),
    )


# ---- routes ----

@app.get("/healthz")
def healthz() -> dict:
    return {"status": "ok", "template": TEMPLATE_NAME, "router": ROUTER_URL}


@app.post("/")
async def mcp(
    request: Request,
    authorization: Optional[str] = Header(default=None),
    mcp_session_id: Optional[str] = Header(default=None, alias="Mcp-Session-Id"),
):
    auth = _authenticate(authorization)
    body = await request.body()
    content_type = request.headers.get("content-type", "application/json")
    accept = request.headers.get("accept", "application/json, text/event-stream")

    # Determine the session id: use the one the caller sent, else mint one.
    # We do NOT require it to already resolve — this API version's Gateway does
    # not reliably round-trip Mcp-Session-Id after a backend reconnect (a slow
    # tool call can trigger one), so a hard 404 on an unknown id breaks live
    # sessions. Instead we claim-or-reuse a sandbox for whatever session id we
    # end up with. Per-session sandbox mapping is preserved; the failure mode
    # (404 Unknown MCP session) is eliminated.
    session_id = mcp_session_id or uuid.uuid4().hex

    sandbox_id = _resolve_sandbox_id(session_id)
    if not sandbox_id:
        # No (ready) sandbox for this session yet — claim one bound to it.
        # Covers both a genuine new session and a session whose id the Gateway
        # dropped/reset mid-stream.
        sandbox_id = _claim_sandbox(auth.principal, session_id)

    resp = await _forward(body, sandbox_id, content_type, accept)
    # Always advertise the session id so a client/Gateway that does track it
    # converges on a stable value.
    resp.headers[SESSION_HEADER] = session_id
    return resp


@app.delete("/")
async def mcp_delete(
    authorization: Optional[str] = Header(default=None),
    mcp_session_id: Optional[str] = Header(default=None, alias="Mcp-Session-Id"),
) -> Response:
    _authenticate(authorization)
    if mcp_session_id:
        _release_session(mcp_session_id)
    return Response(status_code=204)
