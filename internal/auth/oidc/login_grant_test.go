// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	writoidc "github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// baseLoginScopes is the scope string every Wardyn login has always requested.
// Spelled out rather than derived, because this file's central claim is that a
// deployment with no sink still sends exactly this.
const baseLoginScopes = "openid profile email"

// stubSink is a LoginGrantSink whose two halves are set per case. The capture
// half records what it was handed so a test can assert it was NOT called.
type stubSink struct {
	scopes   []string
	captured []writoidc.LoginGrant
	subjects []string
}

func (s *stubSink) LoginScopes(context.Context) []string { return s.scopes }
func (s *stubSink) CaptureLoginGrant(_ context.Context, subject string, g writoidc.LoginGrant) {
	s.subjects = append(s.subjects, subject)
	s.captured = append(s.captured, g)
}

// authorizeQuery drives LoginHandler and returns the query of the
// authorization URL it redirected to.
func authorizeQuery(t *testing.T, auth *writoidc.Authenticator) url.Values {
	t.Helper()
	w := httptest.NewRecorder()
	auth.LoginHandler(w, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	if w.Code != http.StatusFound {
		t.Fatalf("LoginHandler: status %d, want 302", w.Code)
	}
	u, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse the authorization URL: %v", err)
	}
	return u.Query()
}

// TestLoginRequestIsUnchangedWithoutASink IS THE NO-OP GUARANTEE, and it is the
// most important test in this file.
//
// The login-grant seam exists so that signing into the console can also acquire
// a downstream credential. Every deployment that wants nothing of the sort —
// which is every deployment that exists today — must send byte-for-byte the
// authorization request it sent before the seam was written. A `scope`
// parameter that gained or lost a value here would change what an organisation's
// identity provider prompts for, on every login, for a feature nobody enabled.
func TestLoginRequestIsUnchangedWithoutASink(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newAuth(t, nil)

	q := authorizeQuery(t, auth)
	if got := q.Get("scope"); got != baseLoginScopes {
		t.Fatalf("scope = %q; want exactly %q — an unconfigured deployment's login request must not move", got, baseLoginScopes)
	}
	// And the rest of the request is the shape it always was, so "unchanged"
	// means the whole request and not just the one parameter this seam touches.
	if q.Get("response_type") != "code" || q.Get("client_id") != env.clientID {
		t.Errorf("the authorization request lost its shape: %v", q)
	}
	if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" ||
		q.Get("state") == "" || q.Get("nonce") == "" {
		t.Errorf("the authorization request lost its PKCE/state/nonce parameters: %v", q)
	}
}

// TestLoginRequestIsUnchangedWhenTheSinkAsksForNothing: an ATTACHED sink that
// declines — the shape of a deployment that has the feature compiled in but no
// row configured, which is the common case — is indistinguishable from no sink
// at all.
func TestLoginRequestIsUnchangedWhenTheSinkAsksForNothing(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newAuth(t, nil)

	for _, tc := range []struct {
		name  string
		scope []string
	}{
		{"nil", nil},
		{"empty", []string{}},
		{"all values dropped", []string{"", "two words"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth.AttachLoginGrantSink(&stubSink{scopes: tc.scope})
			if got := authorizeQuery(t, auth).Get("scope"); got != baseLoginScopes {
				t.Fatalf("scope = %q; want exactly %q", got, baseLoginScopes)
			}
		})
	}
}

// TestLoginRequestCarriesTheSinksExtraScopes: when the sink asks, the extras
// are appended AFTER the base scopes, in the order given, and nothing else
// about the request moves.
func TestLoginRequestCarriesTheSinksExtraScopes(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newAuth(t, nil)
	extra := []string{"499b84ac-1321-427f-aa17-267ca6975798/vso.code_write", "offline_access"}
	auth.AttachLoginGrantSink(&stubSink{scopes: extra})

	q := authorizeQuery(t, auth)
	want := baseLoginScopes + " " + strings.Join(extra, " ")
	if got := q.Get("scope"); got != want {
		t.Fatalf("scope = %q; want %q", got, want)
	}
	// The base scopes keep their place at the front: an identity provider's
	// consent screen reads in order, and the sign-in the human asked for should
	// not be listed behind a resource they have never heard of.
	if !strings.HasPrefix(q.Get("scope"), baseLoginScopes) {
		t.Error("the widened request reordered the login's own scopes")
	}
	if q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" {
		t.Errorf("widening the request changed something else about it: %v", q)
	}
}

// TestDetachingTheSinkRestoresTheUnwidenedRequest: nil detaches, and the very
// next login is back to the request it would have sent with no seam at all.
func TestDetachingTheSinkRestoresTheUnwidenedRequest(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newAuth(t, nil)
	auth.AttachLoginGrantSink(&stubSink{scopes: []string{"some.scope"}})
	if got := authorizeQuery(t, auth).Get("scope"); got == baseLoginScopes {
		t.Fatal("the sink was never consulted, so this test proves nothing about detaching")
	}
	auth.AttachLoginGrantSink(nil)
	if got := authorizeQuery(t, auth).Get("scope"); got != baseLoginScopes {
		t.Fatalf("scope = %q; want exactly %q after detaching", got, baseLoginScopes)
	}
}

// TestLoginScopeSanitization holds a sink to the rules the authorization
// request needs and the sink is not trusted to have applied.
//
// The whitespace case is the one that matters: `scope` is space-delimited, so a
// value carrying a space is two scopes wearing one name. It is DROPPED rather
// than split, because splitting would silently request something nobody wrote —
// and a sink is configuration, which is to say operator input.
func TestLoginScopeSanitization(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newAuth(t, nil)

	many := make([]string, 0, 40)
	for i := range 40 {
		many = append(many, string(rune('a'+i%26))+"-scope-"+string(rune('0'+i%10)))
	}
	for _, tc := range []struct {
		name string
		ask  []string
		want string // the scope parameter, or "" to mean "the base scopes exactly"
	}{
		{"a scope with a space is dropped, never split", []string{"vso.code vso.work", "keep.me"}, baseLoginScopes + " keep.me"},
		{"a scope with a tab is dropped", []string{"bad\tscope", "keep.me"}, baseLoginScopes + " keep.me"},
		{"an empty value is dropped", []string{"", "keep.me"}, baseLoginScopes + " keep.me"},
		{"a base scope cannot be listed twice", []string{"openid", "email", "keep.me"}, baseLoginScopes + " keep.me"},
		{"a duplicate extra is listed once", []string{"keep.me", "keep.me"}, baseLoginScopes + " keep.me"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth.AttachLoginGrantSink(&stubSink{scopes: tc.ask})
			want := tc.want
			if want == "" {
				want = baseLoginScopes
			}
			if got := authorizeQuery(t, auth).Get("scope"); got != want {
				t.Fatalf("scope = %q; want %q", got, want)
			}
		})
	}

	t.Run("an unbounded list is truncated", func(t *testing.T) {
		auth.AttachLoginGrantSink(&stubSink{scopes: many})
		got := strings.Fields(authorizeQuery(t, auth).Get("scope"))
		// 3 base scopes plus the cap.
		if len(got) != 3+32 {
			t.Fatalf("the scope parameter carries %d values; want the 3 base scopes plus the 32-value cap", len(got))
		}
	})
}
