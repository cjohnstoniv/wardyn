// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPortalKeySet_FreshKeysMissingKidIsNotAnOutage(t *testing.T) {
	h := newPortalHarness(t, "example.com", true, "/oauth2/a")
	// The cold exchange fetches keys itself and still lacks its kid: a plain refusal, not a cooldown answer.
	r := h.wantRefusal(h.kidToken("k9", 1), SubjectTokenInvalid)
	if r.Class != PortalRefusedToken {
		t.Fatalf("class = %v", r.Class)
	}
	h.wantKeyHits(1)
	// A later exchange for another never-seen kid inside the cooldown is told to retry, with the rest of it.
	h.clock.Advance(10 * time.Second)
	r = h.wantUnavailable(h.kidToken("k8", 1), portalCauseKeysCooldown)
	if r.RetryAfter != 20*time.Second {
		t.Fatalf("Retry-After = %v, want 20s", r.RetryAfter)
	}
	h.wantKeyHits(1)
}

func TestPortalKeySet_HourlyDiscoveryHonoursKeyBudget(t *testing.T) {
	t.Run("keys younger than the budget are not fetched again", func(t *testing.T) {
		h := newPortalHarness(t, "example.com", true, "/oauth2/a")
		warmPortal(h)
		h.idp.publish(t, "k2", 1)
		h.clock.Advance(59*time.Minute + 50*time.Second)
		h.mustAdmit(h.kidToken("k2", 1)) // an unknown-kid fetch, 10 s before the hour
		h.clock.Advance(20 * time.Second)
		h.mustAdmit(h.token(h.accessClaims())) // discovery is due; the keys are 20 s old
		if disc, keys := h.idp.hits(); disc != 2 || keys != 2 {
			t.Fatalf("(%d discovery, %d key) fetches, want (2, 2): one key fetch per 30 s across both paths", disc, keys)
		}
	})
	t.Run("a changed keys address closes the verifier until its keys load", func(t *testing.T) {
		h := newPortalHarness(t, "example.com", true, "/oauth2/a")
		warmPortal(h)
		h.idp.publish(t, "k2", 1)
		h.clock.Advance(59*time.Minute + 50*time.Second)
		h.mustAdmit(h.kidToken("k2", 1))
		h.clock.Advance(20 * time.Second)
		h.idp.set(func(f *fakeIdP) { f.jwksURI = h.reg.Issuer + "/v2/keys" })
		r := h.wantUnavailable(h.token(h.accessClaims()), portalCauseKeysCooldown)
		if r.RetryAfter != 10*time.Second {
			t.Fatalf("Retry-After = %v, want the rest of the key budget, 10s", r.RetryAfter)
		}
		// Still closed, still no fetch, and the old keys are not used for the new address.
		h.clock.Advance(5 * time.Second)
		r = h.wantUnavailable(h.token(h.accessClaims()), portalCauseKeysCooldown)
		if r.RetryAfter != 5*time.Second {
			t.Fatalf("Retry-After = %v, want 5s", r.RetryAfter)
		}
		if _, keys := h.idp.hits(); keys != 2 {
			t.Fatalf("%d key fetches while closed, want none beyond the 2 made", keys)
		}
		h.clock.Advance(6 * time.Second)
		h.mustAdmit(h.token(h.accessClaims()))
		if _, keys := h.idp.hits(); keys != 3 {
			t.Fatalf("%d key fetches, want the one for the new address", keys)
		}
		if last := h.idp.requests[len(h.idp.requests)-1]; !strings.HasSuffix(last, "/oauth2/a/v2/keys") {
			t.Fatalf("last request %q, want the new keys address", last)
		}
	})
}

func TestPortalVerifier_CallerCancelDoesNotCloseIssuer(t *testing.T) {
	h := newPortalHarness(t, "example.com", true, "/oauth2/a")
	warmPortal(h)
	h.clock.Advance(time.Hour) // discovery is due
	arrived, release := make(chan struct{}), make(chan struct{})
	var first, rel sync.Once
	defer rel.Do(func() { close(release) })
	h.idp.set(func(f *fakeIdP) {
		f.override = func(w http.ResponseWriter, r *http.Request) bool {
			if strings.HasSuffix(r.URL.Path, "/.well-known/openid-configuration") {
				first.Do(func() {
					close(arrived)
					<-release
				})
			}
			return false
		}
	})
	tok := h.token(h.accessClaims())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan PortalRefusal, 1)
	go func() {
		_, r := h.pi.VerifyDetail(ctx, h.reg, tok, PortalTokenTypeAccess)
		done <- r
	}()
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("the refresh never reached the provider")
	}
	cancel() // the caller leaves mid-refresh
	select {
	case r := <-done:
		if r.Reason != PortalIssuerUnavailable || r.Cause != portalCauseCanceled {
			t.Fatalf("the caller that left got %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the caller stayed bound to the refresh after cancelling")
	}
	rel.Do(func() { close(release) })
	// The shared refresh finished on its own context: the issuer is healthy and the next exchange is admitted.
	h.mustAdmit(h.token(h.accessClaims()))
	if v, _ := h.pi.verifierFor(h.reg); v.down.Load() != nil || v.discFailed {
		t.Fatal("one caller's cancellation marked the shared issuer unavailable")
	}
}

func TestPortalVerifier_RefreshFailureHook(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(prev)
	var mu sync.Mutex
	var got []PortalRefreshFailure
	h := newPortalHarness(t, "example.com", true, "/oauth2/a")
	h.rebuild(func(o *PortalIssuersOptions) {
		o.OnRefreshFailure = func(f PortalRefreshFailure) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, f)
		}
	})
	warmPortal(h)
	// The fetch an unknown kid triggers.
	h.clock.Advance(portalKeyCooldown + time.Second)
	h.idp.set(func(f *fakeIdP) { f.keyStatus = 500 })
	secret := h.kidToken("k9", 1)
	h.wantUnavailable(secret, portalCauseKeysUnreachable)
	// The scheduled refresh.
	h.clock.Advance(time.Hour)
	h.idp.set(func(f *fakeIdP) { f.discStatus = 500 })
	h.wantUnavailable(h.token(h.accessClaims()), portalCauseUnreachable)
	want := []PortalRefreshFailure{
		{Issuer: h.reg.Issuer, Cause: portalCauseUnreachable, Source: "unknown_kid"},
		{Issuer: h.reg.Issuer, Cause: portalCauseUnreachable, Source: "scheduled"},
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(got, want) {
		t.Fatalf("hook calls = %+v, want %+v", got, want)
	}
	if !strings.Contains(logs.String(), "refresh failed") || !strings.Contains(logs.String(), h.reg.Issuer) || strings.Contains(logs.String(), secret) {
		t.Fatalf("log lines %q do not name the issuer and cause, or carry a token", logs.String())
	}
}
