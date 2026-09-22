// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

// login_grant.go is the seam that lets the console's own SSO login ALSO acquire
// a downstream credential, instead of making every person run a second errand
// for one.
//
// WHY IT EXISTS. An organisation's admin should be able to configure downstream
// access once and have it work for everyone; a member should at most click
// "allow" once, and with tenant-wide administrator consent not even that. The
// login is already an authorization-code flow against the organisation's own
// identity provider, so the same request can ask for the downstream scopes and
// the same callback can hand the resulting refresh token to whoever stores it.
// The alternative — a separate sign-in per person — is the fallback, not the
// design.
//
// WHAT THIS PACKAGE DOES NOT DO. It does not store anything, does not know what
// the extra scopes mean, and does not keep a token of its own: Session still
// carries no access or refresh token and this file does not change that. It
// asks a sink what to request, and hands the sink what came back. Everything
// about WHERE that lands — whose namespace, under what name, with what audit —
// stays outside this package, which cannot import the package that owns it.
//
// THE LOGIN IS NEVER AT RISK. The sink is consulted best-effort at both ends. A
// sink that returns nothing widens nothing; a sink that fails to store gets no
// say in whether the person is signed in. A downstream credential is a
// convenience; a console session is the thing the human came for, and an
// organisation must never be locked out of its own console because a second
// resource declined a scope.

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

// maxExtraLoginScopes bounds what a sink may add to the authorization request.
// The request is a URL a browser has to carry, and a sink is configuration —
// so this is a bound on a composed URL, not a policy about which scopes are
// reasonable.
const maxExtraLoginScopes = 32

// LoginGrant is what the login token exchange returned BESIDES the identity.
//
// It deliberately carries no access token. An access token from the login
// exchange is minutes old by the time anything downstream wants one and cannot
// be renewed by whoever holds it; the refresh token is the durable half, and
// the party that stores it is the only party that may redeem it.
type LoginGrant struct {
	// RefreshToken is the durable half of the grant. Empty means the provider
	// issued none — `offline_access` was not asked for, or not granted.
	RefreshToken string
	// Scope is the GRANTED scope string, verbatim and unparsed. It is not the
	// requested one: a provider may grant more or less than was asked for, and
	// only the sink knows which scopes it was hoping for.
	Scope string
	// Expiry is the access token's expiry, carried as the one freshness signal
	// the exchange actually reported.
	Expiry time.Time
}

// LoginGrantSink is the two halves of the seam: what to ADD to the
// authorization request, and what to DO with the grant that comes back.
//
// Both are best-effort by contract. LoginScopes returning nil (or a sink that
// was never attached) leaves the authorization request byte-identical to a
// deployment that never heard of this file. CaptureLoginGrant's outcome is not
// reported and cannot be: its caller has already decided to sign this person
// in, and a storage failure must not undo that.
//
// An implementation MUST NOT panic and MUST NOT block for long: it runs inside
// a browser's login redirect, with a human waiting on it.
type LoginGrantSink interface {
	// LoginScopes returns the extra scopes this login should request, or nil
	// for none. Called once per authorization request.
	LoginScopes(ctx context.Context) []string
	// CaptureLoginGrant is offered the grant once the login is APPROVED —
	// past every denial branch, so a refused login never yields a credential.
	// subject is the verified id_token subject, which is the principal the
	// session is about to be issued for.
	CaptureLoginGrant(ctx context.Context, subject string, grant LoginGrant)
}

// loginGrantHook holds the attached sink. It is guarded because the edge is
// attached AFTER construction: the sink is owned by a component that needs the
// Authenticator to exist first, so the two are joined once both do. The lock is
// read-only on every login and taken once per request, which is nothing beside
// the round trip the login is already making.
type loginGrantHook struct {
	mu   sync.RWMutex
	sink LoginGrantSink
}

// AttachLoginGrantSink joins the sink to this Authenticator. Called once at
// boot, before anything is served; a nil sink detaches, which restores the
// unwidened login exactly.
func (a *Authenticator) AttachLoginGrantSink(sink LoginGrantSink) {
	a.grants.mu.Lock()
	defer a.grants.mu.Unlock()
	a.grants.sink = sink
}

func (a *Authenticator) loginGrantSink() LoginGrantSink {
	a.grants.mu.RLock()
	defer a.grants.mu.RUnlock()
	return a.grants.sink
}

// extraLoginScopes asks the sink what to add and holds the answer to the rules
// the authorization request needs, which the sink is not trusted to have
// applied:
//
//   - a scope carrying whitespace is DROPPED, not split. The scope parameter is
//     space-delimited, so a value with a space in it is two scopes wearing one
//     name, and splitting it would silently request something nobody wrote.
//   - a scope already in the base request is dropped, so the parameter cannot
//     list `openid` twice.
//   - the list is truncated at maxExtraLoginScopes.
//
// Returns nil when there is nothing to add, which is what keeps a deployment
// with no sink byte-identical to before this seam existed.
func (a *Authenticator) extraLoginScopes(ctx context.Context) []string {
	sink := a.loginGrantSink()
	if sink == nil {
		return nil
	}
	asked := sink.LoginScopes(ctx)
	if len(asked) == 0 {
		return nil
	}
	out := make([]string, 0, len(asked))
	for _, sc := range asked {
		if sc == "" || strings.ContainsAny(sc, " \t\r\n") {
			continue
		}
		if slices.Contains(a.oauth2.Scopes, sc) || slices.Contains(out, sc) {
			continue
		}
		out = append(out, sc)
		if len(out) == maxExtraLoginScopes {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// loginScopeParam composes the `scope` value for an authorization request that
// is being widened: the base scopes first, in their existing order, then the
// extras. "" means do not set the parameter at all — let oauth2 compose the
// base scopes exactly as it always has.
func (a *Authenticator) loginScopeParam(extra []string) string {
	if len(extra) == 0 {
		return ""
	}
	return strings.Join(append(slices.Clone(a.oauth2.Scopes), extra...), " ")
}

// captureLoginGrant hands the exchanged grant to the sink. It is the ONE call
// site, and every reason to do nothing is checked here rather than in the sink:
// no sink attached, no token, or no refresh token — the last being exactly what
// a declined or unsupported `offline_access` looks like, and a case in which
// there is nothing durable to store.
//
// It reports nothing, deliberately. The login has already been approved by the
// time this runs, and a downstream credential is not a condition of it.
func (a *Authenticator) captureLoginGrant(ctx context.Context, subject string, token *oauth2.Token) {
	sink := a.loginGrantSink()
	if sink == nil || token == nil || token.RefreshToken == "" || subject == "" {
		return
	}
	scope, _ := token.Extra("scope").(string)
	sink.CaptureLoginGrant(ctx, subject, LoginGrant{
		RefreshToken: token.RefreshToken,
		Scope:        scope,
		Expiry:       token.Expiry,
	})
}

// widenRetryableError reports whether an authorization refusal is one the EXTRA
// scopes plausibly caused, and therefore one worth retrying without them.
//
// THIS IS THE LOCK-OUT VALVE. Widening the login means an identity provider now
// gets to refuse the console's own sign-in over a second resource's consent: a
// tenant that will not issue those scopes, a Conditional Access policy, or a
// person who clicks "cancel" on a consent screen for something they have never
// heard of. Without a way back, every one of those is an organisation locked
// out of Wardyn by a configuration change an admin made for a convenience.
//
// The four codes are the ways that refusal actually arrives. Anything else
// falls through to the handler's existing behaviour untouched, because a
// refusal this function does not recognise is not evidence that the extras
// caused it.
func widenRetryableError(code string) bool {
	switch code {
	case "consent_required", "interaction_required", "access_denied", "invalid_scope":
		return true
	}
	return false
}

// retryLoginUnwidened restarts the login WITHOUT the extra scopes and reports
// whether it did.
//
// It is bounded at exactly one attempt by construction, not by a counter: the
// retry it issues is unwidened, so it sets no widened marker, so its own
// callback cannot reach this function. A second refusal is handled as any
// ordinary login refusal is.
//
// Only reachable when the widened marker was present, i.e. when THIS browser's
// authorization request really did ask for more than a login.
func (a *Authenticator) retryLoginUnwidened(w http.ResponseWriter, r *http.Request, widened bool, code string) bool {
	if !widened || !widenRetryableError(code) {
		return false
	}
	slog.Warn("oidc: the identity provider refused the widened sign-in; retrying with the login's own scopes only, so this person is not kept out of the console",
		"issuer", a.cfg.IssuerURL, "error", code)
	a.startLogin(w, r, false)
	return true
}
