"""Tests for the broker's multi-app resolution + session-id folding
(docs/PRD-broker-multi-app.md, Level B). Self-contained: run with `python3
test_broker_sandboxd_multiapp.py` (no pytest needed). Reloads broker_sandboxd
under different env to exercise legacy vs multi-app config.
"""
import importlib
import json
import os
import re
import sys


def _load(apps_env=None, pool="aio-pool", app="aio-app", default_app=""):
    os.environ["AIO_OIDC_ISSUER"] = "https://kc.example/realms/sandbox"
    os.environ["SANDBOXD_POOL"] = pool
    os.environ["SANDBOXD_APP"] = app
    os.environ["SANDBOXD_DEFAULT_APP"] = default_app
    if apps_env is None:
        os.environ.pop("SANDBOXD_APPS", None)
    else:
        os.environ["SANDBOXD_APPS"] = apps_env
    sys.modules.pop("broker_sandboxd", None)
    return importlib.import_module("broker_sandboxd")


SID_RE = re.compile(r"^[a-z0-9][a-z0-9-]{0,62}$")


def test_legacy_mode_unchanged():
    b = _load(apps_env=None)
    assert b.APPS == {}
    # header is ignored; env pool/app used; app_key empty
    assert b._resolve_app("anything", []) == ("", "aio-pool", "aio-app")
    # sid is principal-only and byte-identical to the pre-multi-app id
    assert b._sid_for("jicowan") == b._sid_for("jicowan", app_key="")
    assert b._sid_for("jicowan") == "sess-jicowan-93b7baf854168a42"


def test_multiapp_resolution_and_entitlement():
    from fastapi import HTTPException
    b = _load(apps_env=json.dumps({
        "aio": {"appTemplate": "aio-app", "pool": "aio-generic-pool", "group": "sandbox-users"},
        "devbox": {"appTemplate": "devbox-app", "pool": "aio-generic-pool", "group": "sandbox-power"},
    }))
    assert b._resolve_app("aio", ["sandbox-users"]) == ("aio", "aio-generic-pool", "aio-app")
    assert b._resolve_app("devbox", ["sandbox-power"]) == ("devbox", "aio-generic-pool", "devbox-app")

    def _expect(code, *args):
        try:
            b._resolve_app(*args)
        except HTTPException as e:
            assert e.status_code == code, f"want {code}, got {e.status_code}"
        else:
            raise AssertionError(f"expected HTTP {code}")

    _expect(403, "devbox", ["sandbox-users"])   # not entitled
    _expect(404, "nope", ["sandbox-power"])      # unknown app
    _expect(400, None, ["sandbox-users"])        # no header, no default


def test_default_app_fills_missing_header():
    b = _load(apps_env=json.dumps({
        "aio": {"appTemplate": "aio-app", "pool": "aio-generic-pool", "group": "sandbox-users"},
    }), default_app="aio")
    assert b._resolve_app(None, ["sandbox-users"]) == ("aio", "aio-generic-pool", "aio-app")


def test_sid_folds_app_no_collision():
    b = _load(apps_env=json.dumps({"aio": {"appTemplate": "aio-app", "pool": "p"},
                                   "devbox": {"appTemplate": "devbox-app", "pool": "p"}}))
    s_aio = b._sid_for("jicowan", app_key="aio")
    s_dev = b._sid_for("jicowan", app_key="devbox")
    assert s_aio != s_dev, "different apps must map to different durable sessions"
    assert s_aio.startswith("sess-jicowan-aio-")
    assert s_dev.startswith("sess-jicowan-devbox-")
    # stable across reconnects: same (principal, app) always yields the same durable sid
    assert b._sid_for("jicowan", app_key="aio") == b._sid_for("jicowan", app_key="aio")
    for s in (s_aio, s_dev):
        assert SID_RE.match(s), f"bad sandbox id: {s}"


def test_bad_apps_json_is_fatal():
    try:
        _load(apps_env="{not json")
    except SystemExit:
        pass
    else:
        raise AssertionError("invalid SANDBOXD_APPS should exit")


if __name__ == "__main__":
    fns = [v for k, v in sorted(globals().items()) if k.startswith("test_")]
    for fn in fns:
        fn()
        print(f"PASS {fn.__name__}")
    print(f"ALL {len(fns)} TESTS PASS")
