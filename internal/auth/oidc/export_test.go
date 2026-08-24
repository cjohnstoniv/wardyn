// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// export_test.go exposes internal functions for white-box testing.
// This file is only compiled in test builds.
package oidc

import (
	"encoding/base64"
	"net/http"
)

// EncodeSessionForTest calls the unexported encodeSession method so that
// test files can build synthetic session cookies without going through the
// full OAuth2 flow.
func EncodeSessionForTest(a *Authenticator, sess Session) (*http.Cookie, error) {
	return a.encodeSession(sess)
}

// EncodeRawSessionForTest signs an ARBITRARY payload with the session HMAC. It
// exists for exactly one case encodeSession cannot reach: a cookie written by
// an OLDER binary, whose JSON carries no "groups" key at all rather than a null
// one. Session.Groups's nil-vs-empty contract only means anything if that
// cookie is reproducible byte for byte.
func EncodeRawSessionForTest(a *Authenticator, payload []byte) *http.Cookie {
	return &http.Cookie{
		Name: sessionCookieName,
		Value: base64.RawURLEncoding.EncodeToString(payload) + "." +
			base64.RawURLEncoding.EncodeToString(sessionHMAC(a.hmacKey, payload)),
		Path: "/",
	}
}

// SessionGroupsForTest exposes the unexported claim normalizer so the union /
// dedupe / sort / byte-cap rules can be pinned directly, without driving a
// signed ID token through the whole callback once per case.
func SessionGroupsForTest(rolesClaim, groupsClaim []string) []string {
	return sessionGroups(rolesClaim, groupsClaim)
}

// MaxSessionGroupsBytesForTest exposes the cookie byte budget.
const MaxSessionGroupsBytesForTest = maxSessionGroupsBytes

// NewRewriteTransportForTest exposes the unexported split-horizon transport
// constructor for white-box testing.
func NewRewriteTransportForTest(publicURL, internalURL string, base *http.Client) (http.RoundTripper, error) {
	return newRewriteTransport(publicURL, internalURL, base)
}

// SetRevocationsForTest wires a SessionRevocations store onto an already-built
// Authenticator (D16's Middleware revocation-check tests build the
// Authenticator through the normal full-discovery newAuth helper, then add
// the store afterward — cfg is otherwise unexported).
func SetRevocationsForTest(a *Authenticator, r SessionRevocations) {
	a.cfg.Revocations = r
}
