"""One-shot OIDC device-flow login for aio_proxy.py.

Runs the OAuth 2.0 Device Authorization Grant against the issuer in
AIO_OIDC_ISSUER, prompts the user to approve in a browser, and caches
the resulting tokens to ~/.config/aio-sandbox/oidc.json (mode 0600).

After this runs successfully, aio_proxy.py will use the cached refresh
token to mint new access tokens silently for as long as the refresh
token is valid.

Run before the first Claude Code MCP tool call:

    AIO_OIDC_ISSUER=https://keycloak.jicomusic.com/realms/sandbox \
    AIO_OIDC_CLIENT_ID=aio-sandbox-client \
    python3 aio_login.py

Re-run any time the cached refresh token is expired or revoked.
"""

import json
import os
import sys
import time
from pathlib import Path

import httpx

STATE_DIR = Path(os.environ.get(
    "AIO_STATE_DIR",
    str(Path.home() / ".config" / "aio-sandbox"),
))
CACHE_FILE = STATE_DIR / "oidc.json"

ISSUER = os.environ.get("AIO_OIDC_ISSUER", "").strip().rstrip("/")
CLIENT_ID = os.environ.get("AIO_OIDC_CLIENT_ID", "").strip()
SCOPES = os.environ.get("AIO_OIDC_SCOPES", "openid sandbox offline_access").strip()


_http = httpx.Client(timeout=httpx.Timeout(30.0))


def _post(url: str, data: dict) -> tuple[int, dict]:
    r = _http.post(url, data=data)
    try:
        return r.status_code, r.json()
    except ValueError:
        return r.status_code, {"error": "non_json", "body": r.text}


def main() -> int:
    if not (ISSUER and CLIENT_ID):
        print(
            "AIO_OIDC_ISSUER and AIO_OIDC_CLIENT_ID must both be set.",
            file=sys.stderr,
        )
        return 2

    device_url = f"{ISSUER}/protocol/openid-connect/auth/device"
    token_url = f"{ISSUER}/protocol/openid-connect/token"

    status, d = _post(device_url, {"client_id": CLIENT_ID, "scope": SCOPES})
    if status != 200:
        print(f"Device authorize failed: {d}", file=sys.stderr)
        return 1

    user_code = d["user_code"]
    interval = int(d.get("interval", 5))
    expires_at = time.time() + int(d.get("expires_in", 600))
    verify_url = d.get("verification_uri_complete") or d["verification_uri"]

    print(f"\nOpen this URL in a browser, log in, and approve:\n  {verify_url}")
    print(f"User code: {user_code}\n")
    print("Polling for approval (Ctrl-C to abort)...")

    while time.time() < expires_at:
        time.sleep(interval)
        status, body = _post(token_url, {
            "client_id": CLIENT_ID,
            "grant_type": "urn:ietf:params:oauth:grant-type:device_code",
            "device_code": d["device_code"],
        })
        if status == 200:
            STATE_DIR.mkdir(parents=True, exist_ok=True)
            CACHE_FILE.write_text(json.dumps({
                "access_token": body["access_token"],
                "refresh_token": body.get("refresh_token"),
                "exp": time.time() + int(body["expires_in"]),
            }))
            try:
                os.chmod(CACHE_FILE, 0o600)
            except OSError:
                pass
            print(f"\nLogged in. Token cached at {CACHE_FILE}.")
            return 0
        err = body.get("error", "")
        if err == "slow_down":
            interval += 5
        elif err == "authorization_pending":
            continue
        else:
            print(f"\nToken exchange failed: {body}", file=sys.stderr)
            return 1

    print("\nDevice code expired before approval.", file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main())
