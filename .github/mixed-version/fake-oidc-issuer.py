#!/usr/bin/env python3
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# fake-oidc-issuer.py: the smallest OpenID Connect provider the released wardynd's sign-in can complete
# against, for .github/mixed-version/portal-artifacts.sh's callback check. Standard library only; the
# RS256 signature is made by openssl with a key the job generates at run time (never one in the tree).
#
#   fake-oidc-issuer.py <bind ip> <port> <private key pem> <client id>
#
# The issuer is http://<bind ip>:<port>. /authorize takes two extra query parameters the test appends,
# mv_sub and mv_email, the subject and email the id_token carries; it answers with a code at once (there
# is no person to ask). /token checks the PKCE verifier and the client id, then returns an id_token
# with the nonce the authorization request carried.
import base64
import hashlib
import json
import secrets
import subprocess
import sys
import time
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

BIND, PORT, KEY, CLIENT_ID = sys.argv[1], int(sys.argv[2]), sys.argv[3], sys.argv[4]
ISSUER = f"http://{BIND}:{PORT}"
KID = "mixed-version"
CODES = {}


def b64url(raw):
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode()


def modulus():
    out = subprocess.run(["openssl", "rsa", "-in", KEY, "-noout", "-modulus"], check=True, capture_output=True, text=True).stdout
    return bytes.fromhex(out.strip().split("=", 1)[1])


JWKS = {"keys": [{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": KID, "n": b64url(modulus()), "e": "AQAB"}]}
DISCOVERY = {
    "issuer": ISSUER,
    "authorization_endpoint": ISSUER + "/authorize",
    "token_endpoint": ISSUER + "/token",
    "jwks_uri": ISSUER + "/jwks",
    "response_types_supported": ["code"],
    "subject_types_supported": ["public"],
    "id_token_signing_alg_values_supported": ["RS256"],
    "scopes_supported": ["openid", "profile", "email"],
    "code_challenge_methods_supported": ["S256"],
    "token_endpoint_auth_methods_supported": ["none"],
}


def sign(claims):
    head = b64url(json.dumps({"alg": "RS256", "typ": "JWT", "kid": KID}).encode())
    body = b64url(json.dumps(claims).encode())
    signing_input = f"{head}.{body}".encode()
    sig = subprocess.run(["openssl", "dgst", "-sha256", "-sign", KEY], input=signing_input, check=True, capture_output=True).stdout
    return f"{head}.{body}.{b64url(sig)}"


class Handler(BaseHTTPRequestHandler):
    def reply(self, status, doc=None, location=None):
        self.send_response(status)
        if location:
            self.send_header("Location", location)
        body = json.dumps(doc).encode() if doc is not None else b""
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        u = urllib.parse.urlsplit(self.path)
        q = dict(urllib.parse.parse_qsl(u.query))
        if u.path == "/.well-known/openid-configuration":
            return self.reply(200, DISCOVERY)
        if u.path == "/jwks":
            return self.reply(200, JWKS)
        if u.path == "/authorize":
            if q.get("client_id") != CLIENT_ID or q.get("response_type") != "code" or q.get("code_challenge_method") != "S256":
                return self.reply(400, {"error": "invalid_request"})
            code = secrets.token_urlsafe(24)
            CODES[code] = q
            sep = "&" if "?" in q["redirect_uri"] else "?"
            return self.reply(302, location=q["redirect_uri"] + sep + urllib.parse.urlencode({"code": code, "state": q.get("state", "")}))
        return self.reply(404, {"error": "not_found"})

    def do_POST(self):
        if urllib.parse.urlsplit(self.path).path != "/token":
            return self.reply(404, {"error": "not_found"})
        form = dict(urllib.parse.parse_qsl(self.rfile.read(int(self.headers.get("Content-Length", "0"))).decode()))
        q = CODES.pop(form.get("code", ""), None)
        if q is None or form.get("grant_type") != "authorization_code" or form.get("client_id") != CLIENT_ID:
            return self.reply(400, {"error": "invalid_grant"})
        challenge = b64url(hashlib.sha256(form.get("code_verifier", "").encode()).digest())
        if challenge != q.get("code_challenge") or form.get("redirect_uri") != q.get("redirect_uri"):
            return self.reply(400, {"error": "invalid_grant"})
        now = int(time.time())
        claims = {"iss": ISSUER, "sub": q["mv_sub"], "aud": CLIENT_ID, "iat": now, "exp": now + 300,
                  "nonce": q.get("nonce", ""), "email": q["mv_email"], "email_verified": True, "name": q["mv_sub"]}
        self.reply(200, {"access_token": secrets.token_urlsafe(24), "token_type": "Bearer", "expires_in": 300, "id_token": sign(claims)})

    def log_message(self, fmt, *args):
        # The request line only: a code or verifier in a query never reaches the log.
        sys.stderr.write("%s %s\n" % (self.command, urllib.parse.urlsplit(self.path).path))


ThreadingHTTPServer((BIND, PORT), Handler).serve_forever()
