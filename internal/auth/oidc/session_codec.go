// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The wardyn_session cookie codec: the signed payload's format version, and the
// encode/decode/HMAC trio around it. Split from oidc.go by seam (the file-size
// gate), exactly as context.go was; no behaviour lives here that oidc.go's
// package doc does not describe.
package oidc

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// SessionCodecVersion is the wardyn_session payload's format version, stamped
// by encodeSession and REQUIRED to match by decodeSession.
//
// It exists for one field that cannot be tolerantly decoded: GroupsTruncated. A
// 0.6 cookie carries no such key, so a JSON decode reads it as false — and
// false is the FAIL-OPEN answer (see the field). A human whose group snapshot
// was ALREADY truncated at their last login would keep a cookie asserting
// "complete" and silently shed the group-assigned governance profile walling
// them, which is exactly the evaporation the bit closes. Nothing in-band
// distinguishes "0.6 cookie" from "0.7 cookie, not truncated", so the payload
// gets a version and a mismatch is simply not a session.
//
// THE COST IS ONE EXTRA LOGIN, once, for every human holding a pre-0.7 cookie —
// folded into the same re-login PF-12 already documents for the pre-0.6
// snapshot. A mismatch behaves exactly like the empty-Role case below:
// Middleware falls through with no principal, the browser is bounced to sign
// in, and CallbackHandler mints a current cookie. Of the three options — this,
// tolerant-decode-false, or a permanent second guess — it is the only one that
// is neither a silent widening nor a permanent one.
//
// The compare is EXACT rather than `<`: under `<`, an OLD binary in a rolling
// upgrade accepts a NEW cookie and reads its unknown fields as zero, which is
// the same fail-open this constant exists to prevent. A mixed-version rollout
// therefore costs logins, not containment.
//
// EXPORTED because a hand-minted payload is otherwise unverifiable: anything
// that builds a session cookie without going through encodeSession — this
// module's own integration tests, an embedder's harness — has to stamp the
// current version or produce a cookie the very next request rejects.
const SessionCodecVersion = 1

// ─── session encoding ────────────────────────────────────────────────────────

// encodeSession JSON-encodes the session, appends an HMAC-SHA256 tag, and
// returns a signed HttpOnly SameSite=Lax cookie.
func (a *Authenticator) encodeSession(sess Session) (*http.Cookie, error) {
	// Stamped HERE rather than at the mint site so no caller can forget it and
	// write a cookie its own binary then refuses.
	sess.V = SessionCodecVersion
	payload, err := json.Marshal(sess)
	if err != nil {
		return nil, fmt.Errorf("oidc: marshal session: %w", err)
	}
	sig := sessionHMAC(a.hmacKey, payload)
	// Encode as base64(payload) + "." + base64(sig).
	encoded := base64.RawURLEncoding.EncodeToString(payload) +
		"." +
		base64.RawURLEncoding.EncodeToString(sig)

	cookie := &http.Cookie{
		Name:     sessionCookieName,
		Value:    encoded,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   a.cfg.SecureCookies, // true only under TLS (direct or terminated); false over plain HTTP
		Expires:  sess.Expiry,
	}
	return cookie, nil
}

// decodeSession reads and verifies the session cookie from the request.
// Returns ErrNoSession if the cookie is absent, ErrInvalidSession if tampered
// or expired according to the signature.
func (a *Authenticator) decodeSession(r *http.Request) (Session, error) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return Session{}, ErrNoSession
	}
	parts := strings.SplitN(c.Value, ".", 2)
	if len(parts) != 2 {
		return Session{}, ErrInvalidSession
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Session{}, ErrInvalidSession
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Session{}, ErrInvalidSession
	}
	expected := sessionHMAC(a.hmacKey, payload)
	if !hmac.Equal(sig, expected) {
		return Session{}, ErrInvalidSession
	}
	var sess Session
	if err := json.Unmarshal(payload, &sess); err != nil {
		return Session{}, ErrInvalidSession
	}
	if sess.V != SessionCodecVersion {
		// A pre-0.7 cookie (no "v" key at all, so 0), or one written by a
		// different codec version. Treated exactly like the empty-Role case
		// below and for the same reason: a payload this binary cannot read
		// field-for-field must not be half-trusted. See SessionCodecVersion for
		// why tolerant decoding is not an option here and what the one extra
		// login buys.
		return Session{}, ErrInvalidSession
	}
	if sess.Role == "" {
		// Pre-0.5 cookie (the role field didn't exist yet) or a corrupt/empty
		// payload: never treat an undefined role as authenticated. Middleware
		// falls through on this exactly like any other invalid session,
		// forcing a re-login where CallbackHandler derives and stamps a role
		// fresh — never a 500.
		return Session{}, ErrInvalidSession
	}
	return sess, nil
}

// sessionHMAC returns the HMAC-SHA256 of payload under key.
func sessionHMAC(key, payload []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(payload)
	return h.Sum(nil)
}
