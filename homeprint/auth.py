"""Who is this? Cloudflare Access signs each request; we check that signature.

Cloudflare Access puts a signed JWT in the Cf-Access-Jwt-Assertion header. We verify it
against the team's public keys and the application's AUD tag, then use its email claim.
The app only listens on 127.0.0.1, so the tunnel is the only way in, but we still verify
the token so a mistake in the network setup can't let unsigned requests through.
"""

import json
import threading

import jwt
from fastapi import HTTPException, Request

from . import config

_jwks = None
_jwks_lock = threading.Lock()


def _client():
    global _jwks
    with _jwks_lock:
        if _jwks is None:
            _jwks = jwt.PyJWKClient(f"https://{config.CF_TEAM_DOMAIN}/cdn-cgi/access/certs",
                                    cache_keys=True, lifespan=3600)
        return _jwks


def _names():
    try:
        with open(config.NAMES_FILE) as f:
            return {k.lower(): v for k, v in json.load(f).items()}
    except (OSError, ValueError):
        return {}


def display_name(email):
    name = _names().get(email.lower())
    if name:
        return name
    return email.split("@")[0].replace(".", " ").title()


def current_user(request: Request) -> str:
    """Return the signed-in email, or raise 401."""
    if config.DEV_USER and not config.CF_TEAM_DOMAIN:
        return config.DEV_USER
    if not (config.CF_TEAM_DOMAIN and config.CF_AUD):
        raise HTTPException(500, "Sign-in isn't configured on the server (CF_ACCESS_TEAM_DOMAIN / CF_ACCESS_AUD).")
    token = request.headers.get("cf-access-jwt-assertion") or request.cookies.get("CF_Authorization")
    if not token:
        raise HTTPException(401, "Not signed in. Open the site through its normal address to sign in.")
    try:
        key = _client().get_signing_key_from_jwt(token)
        claims = jwt.decode(token, key.key, algorithms=["RS256"], audience=config.CF_AUD,
                            issuer=f"https://{config.CF_TEAM_DOMAIN}")
    except jwt.PyJWTError as e:
        raise HTTPException(401, f"Sign-in token isn't valid ({e.__class__.__name__}). Reload the page to sign in again.")
    email = claims.get("email")
    if not email:
        raise HTTPException(401, "Sign-in token has no email.")
    return email.lower()


def can_maintain(email):
    return not config.MAINT_USERS or email.lower() in config.MAINT_USERS
