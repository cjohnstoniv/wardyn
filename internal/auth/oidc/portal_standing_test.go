// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A refresh whose own key fetch fails as an outage answers keys_unreachable with the rest of the retry interval,
// unless a key fetch that broke a rule dropped the verifier after the refresh published: then the refusal's cause
// stands, for the refresh's own caller and in the closed state, with no Retry-After, since a network recovery
// does not clear it.
func TestPortalRefresh_KeysOutageKeepsAStandingRefusal(t *testing.T) {
	h := newPortalHarness(t, "example.com", true, "/oauth2/a")
	var hold atomic.Bool
	arrived, release := make(chan struct{}), make(chan struct{})
	var once, relOnce sync.Once
	rel := func() { relOnce.Do(func() { close(release) }) }
	t.Cleanup(rel)
	h.idp.set(func(f *fakeIdP) {
		f.override = func(w http.ResponseWriter, r *http.Request) bool {
			if !hold.Load() || !strings.HasSuffix(r.URL.Path, "/keys") {
				return false
			}
			first := false
			once.Do(func() { close(arrived); <-release; first = true })
			if first {
				http.Redirect(w, r, "https://other.example/keys", http.StatusFound)
			}
			return first
		}
	})
	warmPortal(h)
	h.idp.publish(t, "k2", 1)
	v, _ := h.pi.verifierFor(h.reg)
	h.clock.Advance(59*time.Minute + 50*time.Second)
	x := pauseAfterPrepare(t, h, h.kidToken("k2", 1))
	h.clock.Advance(11 * time.Second) // discovery is due
	hold.Store(true)
	published, publishGo := make(chan struct{}), make(chan struct{})
	var pubOnce, pubGoOnce sync.Once
	relPub := func() { pubGoOnce.Do(func() { close(publishGo) }) }
	t.Cleanup(relPub)
	v.afterPublish = func() { pubOnce.Do(func() { close(published); <-publishGo }) }
	close(x.resume) // the unknown kid starts a key fetch, held at the provider
	wait := func(c <-chan struct{}, what string) {
		t.Helper()
		select {
		case <-c:
		case <-time.After(5 * time.Second):
			t.Fatal(what)
		}
	}
	wait(arrived, "the key fetch never reached the provider")
	y := make(chan PortalRefusal, 1)
	tok := h.token(h.accessClaims())
	go func() {
		_, r := h.pi.VerifyDetail(context.Background(), h.reg, tok, PortalTokenTypeAccess)
		y <- r
	}()
	wait(published, "the refresh never published")
	rel() // the key fetch is redirected: a rule failure drops the keys, their address and the discovery record
	select {
	case r := <-x.done:
		if r.Reason != PortalIssuerUnavailable || r.Cause != portalCauseAddressRefused {
			t.Fatalf("the exchange whose key fetch broke a rule got %+v, want 503 %q", r, portalCauseAddressRefused)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the exchange whose key fetch broke a rule never answered")
	}
	// The key budget is free again: the refresh's own fetch runs, has no address, and fails as an outage.
	h.clock.Advance(portalKeyCooldown + time.Second)
	relPub()
	select {
	case r := <-y:
		if r.Reason != PortalIssuerUnavailable || r.Cause != portalCauseAddressRefused || r.RetryAfter != 0 {
			t.Fatalf("the refresh's caller got %+v, want 503 %q with no Retry-After", r, portalCauseAddressRefused)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the refresh's caller never answered")
	}
	if c := v.down.Load(); c == nil || *c != portalCauseAddressRefused {
		t.Fatalf("closed under %v, want %q", c, portalCauseAddressRefused)
	}
}
