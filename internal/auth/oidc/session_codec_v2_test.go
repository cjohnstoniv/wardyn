// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc_test

import (
	"fmt"
	"testing"
	"time"

	writoidc "github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// TestPre08MemberCookieIsNotASession: a 0.7 cookie (codec 1) still carrying the
// retired tier word "member" is not a session. It is signed with the live key,
// so only the version check stands between it and a principal whose role no
// predicate in this binary names. It even carries a type, so the missing-type
// refusal is not what turns it away.
func TestPre08MemberCookieIsNotASession(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newAuth(t, nil)

	payload := []byte(fmt.Sprintf(`{"v":1,"sub":"sub-07","email":"m@example.com","role":"member","ut":"standard","expiry":%q,"groups":[]}`,
		time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)))
	if sub, _, ok := sessionThroughMiddleware(t, auth, writoidc.EncodeRawSessionForTest(auth, payload)); ok || sub != "" {
		t.Fatalf("a codec-1 member cookie authenticated (sub=%q); the 0.8 bump must sign every 0.7 session out once", sub)
	}
}

// TestSessionWithoutUserTypeIsNotASession: a current-version payload with no
// "ut" key is refused, exactly like one with no role. Every cookie this binary
// writes carries a type, so a typeless one was not written by it.
func TestSessionWithoutUserTypeIsNotASession(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newAuth(t, nil)
	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)

	typeless := []byte(fmt.Sprintf(`{"v":%d,"sub":"sub-x","email":"x@example.com","role":%q,"expiry":%q,"groups":[]}`,
		writoidc.SessionCodecVersion, writoidc.RoleUser, exp))
	if sub, _, ok := sessionThroughMiddleware(t, auth, writoidc.EncodeRawSessionForTest(auth, typeless)); ok || sub != "" {
		t.Fatalf("a session with no user type authenticated (sub=%q)", sub)
	}

	typed := []byte(fmt.Sprintf(`{"v":%d,"sub":"sub-x","email":"x@example.com","role":%q,"ut":"standard","expiry":%q,"groups":[]}`,
		writoidc.SessionCodecVersion, writoidc.RoleUser, exp))
	if sub, _, ok := sessionThroughMiddleware(t, auth, writoidc.EncodeRawSessionForTest(auth, typed)); !ok || sub != "sub-x" {
		t.Fatalf("the same session with a user type did not authenticate (sub=%q)", sub)
	}
}
