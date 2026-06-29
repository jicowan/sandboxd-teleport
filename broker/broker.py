"""AIO Sandbox Broker — Stage 1.

A small in-cluster service that owns sandbox lifecycle so clients don't need
the agent-sandbox SDK or RBAC to create SandboxClaims.

It sits behind the same ALB + oauth2-proxy that fronts the sandbox-router.
oauth2-proxy validates the caller's Keycloak JWT and forwards the identity in
X-Auth-Request-* headers; the broker trusts those (it is only reachable
through the sidecar) and records the caller as the session owner.

Endpoints:
  POST   /sessions        claim a sandbox for the caller, return its identity
  GET    /sessions/{id}   status + remaining TTL
  DELETE /sessions/{id}   release the sandbox (delete the claim)
  GET    /healthz         liveness

The session id IS the SandboxClaim name. That keeps every operation on the
agent-sandbox SDK (create_sandbox / get_sandbox / delete_sandbox) with no
raw Kubernetes API access. The claim's TTL (shutdown_after_seconds) is the
reclamation backstop; DELETE /sessions/{id} is the happy-path release.

Stage 1 keeps the client's local aio_proxy.py for MCP forwarding; this
service only owns lifecycle. Stage 2 folds MCP forwarding in and drops the
proxy.
"""

import os
import base64
import binascii
import json
import time
from typing import Optional

from fastapi import FastAPI, Header, HTTPException, Response
from pydantic import BaseModel

from k8s_agent_sandbox import SandboxClient

# ---- config ----

TEMPLATE_NAME = os.environ.get("AIO_TEMPLATE_NAME", "aio-sandbox-template")
NAMESPACE = os.environ.get("AIO_NAMESPACE", "default")
WARMPOOL = os.environ.get("AIO_WARMPOOL", "aio-sandbox-warmpool")
TTL_SECONDS = int(os.environ.get("AIO_TTL_SECONDS", "3600"))
READY_TIMEOUT = int(os.environ.get("AIO_READY_TIMEOUT", "180"))
AIO_CONTAINER_PORT = 8080

app = FastAPI(title="aio-sandbox-broker", version="0.1.0")


def _client() -> SandboxClient:
    # Lifecycle-only use: create/get/delete go through the SDK's K8sHelper,
    # which calls load_incluster_config() and uses the pod's ServiceAccount.
    # No connection_config is needed — that only matters when actually
    # connecting to a sandbox (port-forward/tunnel), which the broker never
    # does in Stage 1.
    return SandboxClient()


def _claim_from_jwt(authorization: Optional[str]) -> Optional[str]:
    """Extract a stable principal from the forwarded bearer token.

    We do NOT verify the signature here: the oauth2-proxy sidecar already
    validated the token (issuer, audience, expiry) and enforced group
    membership before forwarding. The broker is only reachable through the
    sidecar, so the token is trustworthy by the time we see it. We just read
    the identity claim. (When the broker becomes a full resource server for
    fine-grained authz, it will verify the signature itself — see DESIGN.md.)
    """
    if not authorization or not authorization.lower().startswith("bearer "):
        return None
    token = authorization.split(None, 1)[1]
    parts = token.split(".")
    if len(parts) != 3:
        return None
    payload_b64 = parts[1] + "=" * (-len(parts[1]) % 4)
    try:
        claims = json.loads(base64.urlsafe_b64decode(payload_b64))
    except (binascii.Error, ValueError, json.JSONDecodeError):
        return None
    principal = (
        claims.get("preferred_username")
        or claims.get("email")
        or claims.get("sub")
        or ""
    ).strip()
    return principal or None


def _principal(
    user: Optional[str],
    email: Optional[str],
    authorization: Optional[str] = None,
) -> str:
    """Resolve the caller identity from oauth2-proxy's forwarded headers.

    Prefer oauth2-proxy's X-Auth-Request-* headers when present; otherwise
    fall back to decoding the forwarded bearer token (in skip-jwt-bearer mode
    the proxy validates the token but does not always synthesize the
    X-Auth-Request-* headers). A missing identity means we're not actually
    behind the proxy — reject."""
    principal = (email or user or "").strip() or _claim_from_jwt(authorization)
    if not principal:
        raise HTTPException(
            status_code=401,
            detail="No authenticated identity. This service must be reached "
                   "through the oauth2-proxy sidecar.",
        )
    return principal


# ---- response model ----

class Session(BaseModel):
    session_id: str          # == SandboxClaim name
    sandbox_id: str
    namespace: str
    container_port: int
    expires_at: float
    principal: str


# ---- routes ----

@app.get("/healthz")
def healthz() -> dict:
    return {"status": "ok", "template": TEMPLATE_NAME, "warmpool": WARMPOOL}


@app.post("/sessions", status_code=201, response_model=Session)
def create_session(
    x_auth_request_user: Optional[str] = Header(default=None),
    x_auth_request_email: Optional[str] = Header(default=None),
    authorization: Optional[str] = Header(default=None),
) -> Session:
    principal = _principal(x_auth_request_user, x_auth_request_email, authorization)
    client = _client()

    # Always template-backed (so a headless Service is created and the router
    # can reach it) AND warmpool-adopted (fast start, with the controller
    # falling back to a cold start when the pool is empty).
    sandbox = client.create_sandbox(
        template=TEMPLATE_NAME,
        namespace=NAMESPACE,
        warmpool=WARMPOOL,
        shutdown_after_seconds=TTL_SECONDS,
        sandbox_ready_timeout=READY_TIMEOUT,
    )

    return Session(
        session_id=sandbox.claim_name,
        sandbox_id=sandbox.sandbox_id,
        namespace=sandbox.namespace,
        container_port=AIO_CONTAINER_PORT,
        expires_at=time.time() + TTL_SECONDS,
        principal=principal,
    )


@app.get("/sessions/{session_id}", response_model=Session)
def get_session(
    session_id: str,
    x_auth_request_user: Optional[str] = Header(default=None),
    x_auth_request_email: Optional[str] = Header(default=None),
    authorization: Optional[str] = Header(default=None),
) -> Session:
    principal = _principal(x_auth_request_user, x_auth_request_email, authorization)
    client = _client()
    try:
        sandbox = client.get_sandbox(session_id, namespace=NAMESPACE)
    except Exception:
        raise HTTPException(status_code=404, detail="No such session.")
    if sandbox is None:
        raise HTTPException(status_code=404, detail="No such session.")
    return Session(
        session_id=session_id,
        sandbox_id=sandbox.sandbox_id,
        namespace=sandbox.namespace,
        container_port=AIO_CONTAINER_PORT,
        expires_at=0.0,  # TTL is enforced cluster-side; not re-derived here.
        principal=principal,
    )


@app.delete("/sessions/{session_id}", status_code=204)
def delete_session(
    session_id: str,
    x_auth_request_user: Optional[str] = Header(default=None),
    x_auth_request_email: Optional[str] = Header(default=None),
    authorization: Optional[str] = Header(default=None),
) -> Response:
    _principal(x_auth_request_user, x_auth_request_email, authorization)
    client = _client()
    try:
        client.delete_sandbox(session_id, namespace=NAMESPACE)
    except Exception:
        # Idempotent: already gone (released, or TTL-reclaimed) is success.
        pass
    return Response(status_code=204)
