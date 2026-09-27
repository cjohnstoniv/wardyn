// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

// login_grant.go lets the console's own SSO login ALSO acquire a downstream
// credential, instead of making every person run a second errand for one: the
// login is already an authorization-code flow, so the same request can ask
// for the downstream scopes and the same callback can hand back the result.
//
// This package does not store anything, does not know what the extra scopes
// mean, and keeps no token of its own — it asks a sink what to request and
// hands the sink what came back. Where that lands stays outside this
// package, which cannot import the package that owns it.
//
// THE LOGIN IS NEVER AT RISK: the sink is consulted best-effort at both
// ends. A sink that returns nothing widens nothing; a sink that fails to
// store gets no say in whether the person is signed in.

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
// It deliberately carries no access token: that would be minutes old by the
// time anything downstream wants one, so the refresh token is the durable
// half, and the party that stores it is the only party that may redeem it.
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
// authorization request, and what to DO with the grant that comes back. Both
// are best-effort by contract — a storage failure in CaptureLoginGrant must
// not undo a login that was already decided. An implementation MUST NOT
// panic and MUST NOT block for long: it runs inside a browser's login
// redirect, with a human waiting on it.
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

// loginGrantHook holds the attached sink. It is guarded because the sink is
// attached AFTER construction, once the owning component and the
// Authenticator both exist.
type loginGrantHook struct {
	mu   sync.RWMutex
	sink LoginGrantSink
	// timeout overrides loginGrantSinkTimeout for tests. Zero means the
	// constant; nothing in the daemon sets it.
	timeout time.Duration
}

// loginGrantSinkTimeout bounds EACH sink call, enforced here rather than
// trusted to the sink, since this package promises the login is never at
// risk. Three seconds is long enough for a healthy read/write and short
// enough that a stalled one reads as a slow login, not a broken one.
const loginGrantSinkTimeout = 3 * time.Second

func (a *Authenticator) sinkTimeout() time.Duration {
	a.grants.mu.RLock()
	defer a.grants.mu.RUnlock()
	if a.grants.timeout > 0 {
		return a.grants.timeout
	}
	return loginGrantSinkTimeout
}

// boundedSinkCall runs fn with a deadline and RETURNS when the deadline does,
// whether or not fn has. ok=false means it did not finish in time (or
// panicked); the caller then proceeds as if the sink had declined. fn runs
// on its own goroutine since a context only bounds a callee that honours it;
// one that ignores it runs to completion in the background unwaited. A panic
// is RECOVERED here since on its own goroutine it would otherwise take the
// whole daemon down.
func (a *Authenticator) boundedSinkCall(ctx context.Context, what string, fn func(context.Context)) bool {
	ctx, cancel := context.WithTimeout(ctx, a.sinkTimeout())
	defer cancel()
	done := make(chan bool, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				slog.Error("oidc: the login-grant sink panicked; the login proceeds without it", "step", what, "panic", p)
				done <- false
			}
		}()
		fn(ctx)
		done <- true
	}()
	select {
	case ok := <-done:
		return ok
	case <-ctx.Done():
		slog.Warn("oidc: the login-grant sink did not answer in time; the login proceeds without it",
			"step", what, "timeout", a.sinkTimeout().String())
		return false
	}
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

// extraLoginScopes asks the sink what to add and holds the answer to the
// rules the authorization request needs, which the sink is not trusted to
// have applied: a scope carrying whitespace is DROPPED rather than split (the
// scope parameter is space-delimited), a scope already in the base request
// is deduped, and the list is truncated at maxExtraLoginScopes. Returns nil
// when there is nothing to add.
func (a *Authenticator) extraLoginScopes(ctx context.Context) []string {
	sink := a.loginGrantSink()
	if sink == nil {
		return nil
	}
	// Bounded: a timeout means "no widening", and the login goes out unwidened.
	result := make(chan []string, 1)
	if !a.boundedSinkCall(ctx, "compose the login request", func(ctx context.Context) {
		result <- sink.LoginScopes(ctx)
	}) {
		return nil
	}
	asked := <-result
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
// site, checking every reason to do nothing here rather than in the sink: no
// sink attached, no token, or no refresh token (a declined/unsupported
// `offline_access`). It reports nothing, deliberately: the login has already
// been approved, and a downstream credential is not a condition of it.
func (a *Authenticator) captureLoginGrant(ctx context.Context, subject string, token *oauth2.Token) {
	sink := a.loginGrantSink()
	if sink == nil || token == nil || token.RefreshToken == "" || subject == "" {
		return
	}
	scope, _ := token.Extra("scope").(string)
	grant := LoginGrant{
		RefreshToken: token.RefreshToken,
		Scope:        scope,
		Expiry:       token.Expiry,
	}
	// Bounded: this runs BEFORE the session cookie is written, so a stalled
	// capture would hold an approved human at a blank page.
	a.boundedSinkCall(ctx, "capture the login grant", func(ctx context.Context) {
		sink.CaptureLoginGrant(ctx, subject, grant)
	})
}

// expireWidenedMarker deletes the widened marker through loginCookie, so the
// deletion carries the same Secure posture as every cookie the login writes.
func (a *Authenticator) expireWidenedMarker(w http.ResponseWriter) {
	stale := a.loginCookie(widenedCookieName, "")
	stale.MaxAge = -1
	http.SetCookie(w, stale)
}

// maxLoggedErrorDescription bounds the IdP-supplied text logUnretriedRefusal
// writes. It is the provider's prose, not ours, and a log line is not the
// place for an unbounded string an external party chose.
const maxLoggedErrorDescription = 256

// logUnretriedRefusal names an identity-provider refusal the callback is NOT
// retrying. The response is unchanged, but the cause reaches the log.
func (a *Authenticator) logUnretriedRefusal(r *http.Request, widened bool, code string) {
	desc := r.URL.Query().Get("error_description")
	if len(desc) > maxLoggedErrorDescription {
		desc = desc[:maxLoggedErrorDescription] + "…"
	}
	slog.Warn("oidc: the identity provider refused the sign-in",
		"issuer", a.cfg.IssuerURL, "error", code, "error_description", desc, "widened", widened)
}

// widenRetryableError reports whether an authorization refusal is one the
// EXTRA scopes plausibly caused, and therefore one worth retrying without
// them.
//
// THIS IS THE LOCK-OUT VALVE: without it, a tenant that won't issue the
// extra scopes, a Conditional Access policy, or a declined consent screen
// would lock an organisation out of Wardyn over a convenience an admin
// configured. Anything outside these four codes falls through untouched,
// since it is not evidence the extras caused it.
func widenRetryableError(code string) bool {
	switch code {
	case "consent_required", "interaction_required", "access_denied", "invalid_scope":
		return true
	}
	return false
}

// retryLoginUnwidened restarts the login WITHOUT the extra scopes and reports
// whether it did. Bounded at exactly one attempt by construction: the retry
// it issues is unwidened, so it sets no widened marker, so its own callback
// cannot reach this function again. Only reachable when the widened marker
// was present.
func (a *Authenticator) retryLoginUnwidened(w http.ResponseWriter, r *http.Request, widened bool, code string) bool {
	if !widened || !widenRetryableError(code) {
		return false
	}
	slog.Warn("oidc: the identity provider refused the widened sign-in; retrying with the login's own scopes only, so this person is not kept out of the console",
		"issuer", a.cfg.IssuerURL, "error", code)
	a.startLogin(w, r, false)
	return true
}
