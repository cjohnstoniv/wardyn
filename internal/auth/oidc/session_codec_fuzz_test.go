// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"crypto/hmac"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var codecTestKey = []byte("0123456789abcdef0123456789abcdef")

func sessionRequest(value string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: value})
	return r
}

// signedWith builds a cookie value over payload with key's HMAC.
func signedWith(key []byte, payload string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(sessionHMAC(key, []byte(payload)))
}

// sessionExpiry is an hour ahead of the clock, so no fixture here rots into the past.
var sessionExpiry = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)

var validSessionJSON = `{"v":2,"sub":"sub-alice","email":"alice@corp.example","role":"admin","ut":"standard","expiry":"` + sessionExpiry + `"}`

// TestDecodeSession_RefusesMalformedCookies: each malformed shape is refused,
// never half-read into a session.
func TestDecodeSession_RefusesMalformedCookies(t *testing.T) {
	a := &Authenticator{hmacKey: codecTestKey}
	payload := base64.RawURLEncoding.EncodeToString([]byte(validSessionJSON))
	sig := base64.RawURLEncoding.EncodeToString(sessionHMAC(codecTestKey, []byte(validSessionJSON)))

	if _, err := a.decodeSession(sessionRequest(signedWith(codecTestKey, validSessionJSON))); err != nil {
		t.Fatalf("the well-formed control cookie was refused: %v", err)
	}
	for name, value := range map[string]string{
		"no separator":                 payload + sig,
		"malformed base64 payload":     "!!" + payload + "." + sig,
		"malformed base64 signature":   payload + ".!!" + sig,
		"valid HMAC over invalid JSON": signedWith(codecTestKey, `{"v":1,"role":`),
		"signed with another key":      signedWith([]byte("ffffffffffffffffffffffffffffffff"), validSessionJSON),
		"payload changed under its signature": base64.RawURLEncoding.EncodeToString(
			[]byte(strings.Replace(validSessionJSON, "sub-alice", "sub-mallory", 1))) + "." + sig,
	} {
		if _, err := a.decodeSession(sessionRequest(value)); err == nil {
			t.Errorf("%s: decoded into a session", name)
		}
	}
}

// FuzzDecodeSession: a cookie whose HMAC does not verify under the server's
// key is never a session, whatever else it carries. The seed corpus runs on
// every `go test`; the nightly lane fuzzes it.
func FuzzDecodeSession(f *testing.F) {
	f.Add(signedWith(codecTestKey, validSessionJSON))
	f.Add(signedWith([]byte("ffffffffffffffffffffffffffffffff"), validSessionJSON))
	f.Add(signedWith(codecTestKey, `{"v":1,"role":`))
	f.Add(signedWith(codecTestKey, `{"v":0,"sub":"s","role":"admin","expiry":"`+sessionExpiry+`"}`))
	f.Add(base64.RawURLEncoding.EncodeToString([]byte(validSessionJSON)) + ".")
	f.Add("..")
	f.Add("")

	a := &Authenticator{hmacKey: codecTestKey}
	f.Fuzz(func(t *testing.T, value string) {
		r := sessionRequest(value)
		sess, err := a.decodeSession(r)
		if err != nil {
			return
		}
		// Decoded: the cookie as the server read it must carry a valid tag.
		c, _ := r.Cookie(sessionCookieName)
		p, s, ok := strings.Cut(c.Value, ".")
		payload, perr := base64.RawURLEncoding.DecodeString(p)
		sig, serr := base64.RawURLEncoding.DecodeString(s)
		if !ok || perr != nil || serr != nil || !hmac.Equal(sig, sessionHMAC(codecTestKey, payload)) {
			t.Fatalf("decoded %q into a session (%+v) without a valid HMAC", value, sess)
		}
		if sess.V != SessionCodecVersion || sess.Role == "" {
			t.Fatalf("decoded a session this codec must not accept: %+v", sess)
		}
	})
}
