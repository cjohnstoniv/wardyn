// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

// login_grant.go lets SSO login also acquire a downstream credential in the
// same authorization-code flow, instead of a second errand per person.
//
// This package stores nothing and knows nothing about the extra scopes — it
// asks a sink what to request and hands back what came in.
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

// maxExtraLoginScopes bounds what a sink may add — a bound on the composed
// URL a browser must carry, not a policy on which scopes are reasonable.
const maxExtraLoginScopes = 32

// LoginGrant is what the token exchange returned besides identity. It
// carries no access token (stale within minutes) — the refresh token is
// the durable half, and only the party that stores it may redeem it.
type LoginGrant struct {
	// RefreshToken is the durable half of the grant; empty if the provider
	// issued none (`offline_access` not asked for, or not granted).
	RefreshToken string
	// Scope is the GRANTED scope string, verbatim — may differ from what was
	// requested; only the sink knows what it was hoping for.
	Scope string
	// Expiry is the access token's expiry, as reported by the exchange.
	Expiry time.Time
}

// LoginGrantSink is the seam: what to ADD to the authorization request, and
// what to DO with the grant. Both are best-effort by contract — a storage
// failure in CaptureLoginGrant must not undo an already-decided login. An
// implementation MUST NOT panic or block for long: it runs inside a
// browser's login redirect, with a human waiting.
type LoginGrantSink interface {
	// LoginScopes returns extra scopes to request, or nil. Called once per
	// authorization request.
	LoginScopes(ctx context.Context) []string
	// CaptureLoginGrant is offered the grant once login is APPROVED — past
	// every denial branch, so a refused login never yields a credential.
	// subject is the verified id_token subject the session is about to be
	// issued for.
	CaptureLoginGrant(ctx context.Context, subject string, grant LoginGrant)
}

// loginGrantHook holds the attached sink, guarded because the sink is
// attached after construction.
type loginGrantHook struct {
	mu   sync.RWMutex
	sink LoginGrantSink
	// timeout overrides loginGrantSinkTimeout for tests; zero = the constant.
	timeout time.Duration
}

// loginGrantSinkTimeout bounds each sink call here, not trusted to the sink,
// since this package promises the login is never at risk. Three seconds:
// long enough for a healthy call, short enough a stall reads as slow, not
// broken.
const loginGrantSinkTimeout = 3 * time.Second

func (a *Authenticator) sinkTimeout() time.Duration {
	a.grants.mu.RLock()
	defer a.grants.mu.RUnlock()
	if a.grants.timeout > 0 {
		return a.grants.timeout
	}
	return loginGrantSinkTimeout
}

// boundedSinkCall runs fn with a deadline and returns when it expires,
// whether or not fn finished. ok=false means it didn't finish in time (or
// panicked); the caller proceeds as if the sink had declined. fn runs on
// its own goroutine since an ignored context otherwise runs to completion
// unwaited in the background; a panic there is recovered so it can't take
// the whole daemon down.
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

// AttachLoginGrantSink joins the sink to this Authenticator, called once at
// boot before anything is served; a nil sink detaches, restoring the
// unwidened login.
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

// extraLoginScopes asks the sink what to add and enforces rules the sink
// isn't trusted to have applied itself: a scope with whitespace is dropped
// (the parameter is space-delimited), a scope already in the base request is
// deduped, and the list is truncated at maxExtraLoginScopes.
func (a *Authenticator) extraLoginScopes(ctx context.Context) []string {
	sink := a.loginGrantSink()
	if sink == nil {
		return nil
	}
	// Bounded: a timeout means no widening.
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

// loginScopeParam composes the widened `scope` value: base scopes first, in
// order, then extras. "" leaves the parameter unset.
func (a *Authenticator) loginScopeParam(extra []string) string {
	if len(extra) == 0 {
		return ""
	}
	return strings.Join(append(slices.Clone(a.oauth2.Scopes), extra...), " ")
}

// captureLoginGrant hands the exchanged grant to the sink — the one call
// site, checking every reason to skip: no sink attached, no token, or no
// refresh token (declined/unsupported `offline_access`). Reports nothing,
// deliberately: login is already approved, and a downstream credential is
// not a condition of it.
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
	// Bounded: runs before the session cookie is written, so a stall would
	// hold an approved human at a blank page.
	a.boundedSinkCall(ctx, "capture the login grant", func(ctx context.Context) {
		sink.CaptureLoginGrant(ctx, subject, grant)
	})
}

// expireWidenedMarker deletes the widened marker via loginCookie, keeping
// the same Secure posture as every login cookie.
func (a *Authenticator) expireWidenedMarker(w http.ResponseWriter) {
	stale := a.loginCookie(widenedCookieName, "")
	stale.MaxAge = -1
	http.SetCookie(w, stale)
}

// maxLoggedErrorDescription bounds the IdP-supplied text logged — external
// prose, not ours.
const maxLoggedErrorDescription = 256

// logUnretriedRefusal logs an identity-provider refusal the callback is NOT
// retrying; the response itself is unchanged.
func (a *Authenticator) logUnretriedRefusal(r *http.Request, widened bool, code string) {
	desc := r.URL.Query().Get("error_description")
	if len(desc) > maxLoggedErrorDescription {
		desc = desc[:maxLoggedErrorDescription] + "…"
	}
	slog.Warn("oidc: the identity provider refused the sign-in",
		"issuer", a.cfg.IssuerURL, "error", code, "error_description", desc, "widened", widened)
}

// widenRetryableError reports whether a refusal was plausibly caused by the
// EXTRA scopes, and so is worth retrying without them.
//
// THIS IS THE LOCK-OUT VALVE: without it, a tenant that won't issue the
// extra scopes, a Conditional Access policy, or a declined consent screen
// would lock an organisation out of Wardyn over an admin's convenience
// setting. Anything else falls through untouched.
func widenRetryableError(code string) bool {
	switch code {
	case "consent_required", "interaction_required", "access_denied", "invalid_scope":
		return true
	}
	return false
}

// retryLoginUnwidened restarts the login WITHOUT the extra scopes and
// reports whether it did. Bounded to one attempt by construction: the
// retry is unwidened, so it sets no widened marker, so its callback can't
// reach this function again.
func (a *Authenticator) retryLoginUnwidened(w http.ResponseWriter, r *http.Request, widened bool, code string) bool {
	if !widened || !widenRetryableError(code) {
		return false
	}
	slog.Warn("oidc: the identity provider refused the widened sign-in; retrying with the login's own scopes only, so this person is not kept out of the console",
		"issuer", a.cfg.IssuerURL, "error", code)
	a.startLogin(w, r, false)
	return true
}
