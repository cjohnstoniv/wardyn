// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// pausedExchange is an exchange held once it is through prepare.
type pausedExchange struct {
	t      *testing.T
	resume chan struct{}
	done   chan PortalRefusal
}

// pauseAfterPrepare starts an exchange for raw and returns once it is through prepare and not yet at its key.
func pauseAfterPrepare(t *testing.T, h *portalHarness, raw string) *pausedExchange {
	t.Helper()
	v, err := h.pi.verifierFor(h.reg)
	if err != nil {
		t.Fatal(err)
	}
	x := &pausedExchange{t: t, resume: make(chan struct{}), done: make(chan PortalRefusal, 1)}
	paused := make(chan struct{})
	var first atomic.Bool // not a sync.Once: a second exchange must pass while the first is held
	v.afterPrepare = func() {
		if first.CompareAndSwap(false, true) {
			close(paused)
			<-x.resume
		}
	}
	go func() {
		_, r := h.pi.VerifyDetail(context.Background(), h.reg, raw, portalProfileType(h.reg.TokenProfile))
		x.done <- r
	}()
	select {
	case <-paused:
	case <-time.After(5 * time.Second):
		t.Fatal("the exchange never got through prepare")
	}
	return x
}

func (x *pausedExchange) release() PortalRefusal {
	x.t.Helper()
	close(x.resume)
	select {
	case r := <-x.done:
		return r
	case <-time.After(5 * time.Second):
		x.t.Fatal("the released exchange never answered")
		return PortalRefusal{}
	}
}

func TestPortalRecovery_UnknownKidKeysReopenTheVerifier(t *testing.T) {
	t.Run("keys loaded by an unknown-kid fetch reopen a verifier whose discovery is healthy", func(t *testing.T) {
		h := newPortalHarness(t, "example.com", true, "/oauth2/a")
		warmPortal(h)
		h.idp.publish(t, "k2", 1)
		h.clock.Advance(59*time.Minute + 50*time.Second)
		h.mustAdmit(h.kidToken("k2", 1)) // an unknown-kid fetch starts the key budget 10 s before the hour
		x := pauseAfterPrepare(t, h, h.token(h.accessClaims()))
		h.idp.set(func(f *fakeIdP) { f.jwksURI = h.reg.Issuer + "/v2/keys" })
		h.clock.Advance(20 * time.Second)
		// Discovery moves the keys address inside the budget: the old keys are dropped and the verifier waits.
		h.wantUnavailable(h.token(h.accessClaims()), portalCauseKeysCooldown)
		h.clock.Advance(11 * time.Second)
		// The held exchange now meets an unknown kid and loads the new address's keys itself.
		if r := x.release(); !r.OK() {
			t.Fatalf("the exchange that loaded the new keys was refused: %+v", r)
		}
		// The very next exchange with a known current key is admitted, without another refresh.
		h.mustAdmit(h.token(h.accessClaims()))
		h.mustAdmit(h.kidToken("k2", 1))
		if disc, keys := h.idp.hits(); disc != 2 || keys != 3 {
			t.Fatalf("(%d discovery, %d key) fetches, want (2, 3): the unknown-kid fetch was the recovery", disc, keys)
		}
		var last string
		h.idp.set(func(f *fakeIdP) { last = f.requests[len(f.requests)-1] })
		if !strings.HasSuffix(last, "/oauth2/a/v2/keys") {
			t.Fatalf("last request %q, want the new keys address", last)
		}
	})
	t.Run("a key success does not reopen a verifier whose discovery is failing", func(t *testing.T) {
		h := newPortalHarness(t, "example.com", true, "/oauth2/a")
		warmPortal(h)
		h.idp.publish(t, "k2", 1)
		h.clock.Advance(59*time.Minute + 50*time.Second)
		x := pauseAfterPrepare(t, h, h.kidToken("k2", 1))
		h.idp.set(func(f *fakeIdP) { f.discStatus = http.StatusInternalServerError })
		h.clock.Advance(20 * time.Second)
		if _, r := h.verify(h.token(h.accessClaims())); r.Reason != PortalIssuerUnavailable {
			t.Fatalf("refusal = %+v, want the discovery outage", r)
		}
		// The held exchange loads keys for the current address; discovery is still failing, so it is refused.
		if r := x.release(); r.Reason != PortalIssuerUnavailable || r.Cause != portalCauseIssuerDown {
			t.Fatalf("refusal = %+v, want 503 cause %q", r, portalCauseIssuerDown)
		}
		if _, keys := h.idp.hits(); keys != 2 {
			t.Fatalf("%d key fetches, want 2: the held exchange's fetch succeeded", keys)
		}
		h.wantUnavailable(h.token(h.accessClaims()), portalCauseIssuerDown)
	})
	t.Run("a key success does not reopen a verifier whose discovery was refused", func(t *testing.T) {
		h := newPortalHarness(t, "example.com", true, "/oauth2/a")
		warmPortal(h)
		h.clock.Advance(time.Hour)
		h.idp.set(func(f *fakeIdP) { f.docIssuer = h.reg.Issuer + "/other" })
		h.wantUnavailable(h.token(h.accessClaims()), portalCauseIssuerMismatch)
		v, _ := h.pi.verifierFor(h.reg)
		v.ks.onOK() // a key success reported after the refusal
		if c := v.down.Load(); c == nil || *c != portalCauseIssuerMismatch {
			t.Fatalf("down = %v after a key success, want the refusal to stand", c)
		}
		h.wantUnavailable(h.token(h.accessClaims()), portalCauseIssuerMismatch)
	})
}

func TestPortalDrop_OldFailureCannotDropNewerState(t *testing.T) {
	for _, tc := range []struct{ name, keysPath string }{
		{"a new keys address", "/v2/keys"},
		{"the same address with newer keys", "/keys"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPortalHarness(t, "example.com", true, "/oauth2/a")
			h.idp.set(func(f *fakeIdP) {
				f.override = func(w http.ResponseWriter, r *http.Request) bool {
					w.Header().Set("Connection", "close") // every request resolves and dials again
					return false
				}
			})
			h.mustAdmit(h.token(h.accessClaims()))
			h.idp.publish(t, "k2", 1)
			v, _ := h.pi.verifierFor(h.reg)
			held, resume := make(chan struct{}), make(chan struct{})
			var once sync.Once
			v.beforeDrop = func() { once.Do(func() { close(held); <-resume }) }
			h.clock.Advance(portalKeyCooldown + time.Second)
			h.setLookup(func(string) []net.IP { return []net.IP{net.ParseIP("127.0.0.1")} })
			old := make(chan PortalRefusal, 1)
			tok := h.kidToken("k2", 1)
			go func() {
				_, r := h.pi.VerifyDetail(context.Background(), h.reg, tok, PortalTokenTypeAccess)
				old <- r
			}()
			select {
			case <-held:
			case <-time.After(5 * time.Second):
				t.Fatal("the refused key fetch never reached its drop")
			}
			// The old failure is recorded and reported; before its drop runs, a refresh publishes a new discovery
			// and loads its keys.
			h.setLookup(func(string) []net.IP { return []net.IP{portalPublicIP} })
			h.idp.set(func(f *fakeIdP) { f.jwksURI = h.reg.Issuer + tc.keysPath })
			h.clock.Advance(portalKeyCooldown + time.Second)
			h.mustAdmit(h.kidToken("k2", 1))
			disc, keys := h.idp.hits()
			close(resume)
			select {
			case r := <-old:
				if r.Reason != PortalIssuerUnavailable || r.Cause != portalCauseAddressRefused {
					t.Fatalf("the old exchange got %+v, want its own address_refused", r)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the old exchange never answered")
			}
			// The newer discovery and keys survive the old failure: admitted with no further fetch.
			h.mustAdmit(h.token(h.accessClaims()))
			h.mustAdmit(h.kidToken("k2", 1))
			if d, k := h.idp.hits(); d != disc || k != keys {
				t.Fatalf("(%d, %d) fetches after the old failure, want (%d, %d)", d, k, disc, keys)
			}
		})
	}
}

func TestPortalDrop_OldFailureKeepsSameAddressRecoveryInFlight(t *testing.T) {
	h := newPortalHarness(t, "example.com", true, "/oauth2/a")
	var holdKeys atomic.Bool
	keysArrived, keysRelease := make(chan struct{}), make(chan struct{})
	var keysOnce, keysRelOnce sync.Once
	releaseKeys := func() { keysRelOnce.Do(func() { close(keysRelease) }) }
	t.Cleanup(releaseKeys) // a failed check never leaves the provider's handler held
	h.idp.set(func(f *fakeIdP) {
		f.override = func(w http.ResponseWriter, r *http.Request) bool {
			w.Header().Set("Connection", "close") // every request resolves and dials again
			if holdKeys.Load() && strings.HasSuffix(r.URL.Path, "/keys") {
				keysOnce.Do(func() { close(keysArrived); <-keysRelease })
			}
			return false
		}
	})
	h.mustAdmit(h.token(h.accessClaims()))
	h.idp.publish(t, "k2", 1)
	v, _ := h.pi.verifierFor(h.reg)
	held, resume := make(chan struct{}), make(chan struct{})
	var once, resumeOnce sync.Once
	v.beforeDrop = func() { once.Do(func() { close(held); <-resume }) }
	releaseDrop := func() { resumeOnce.Do(func() { close(resume) }) }
	t.Cleanup(releaseDrop)
	h.clock.Advance(portalKeyCooldown + time.Second)
	h.setLookup(func(string) []net.IP { return []net.IP{net.ParseIP("127.0.0.1")} })
	old := make(chan PortalRefusal, 1)
	oldTok := h.kidToken("k2", 1)
	go func() {
		_, r := h.pi.VerifyDetail(context.Background(), h.reg, oldTok, PortalTokenTypeAccess)
		old <- r
	}()
	wait := func(c <-chan struct{}, what string) {
		t.Helper()
		select {
		case <-c:
		case <-time.After(5 * time.Second):
			t.Fatal(what)
		}
	}
	wait(held, "the refused key fetch never reached its drop")
	// A refresh publishes a discovery with the same keys address and reserves the next key fetch, which is held
	// in flight at the provider.
	h.setLookup(func(string) []net.IP { return []net.IP{portalPublicIP} })
	holdKeys.Store(true)
	h.clock.Advance(portalKeyCooldown + time.Second)
	fresh := make(chan PortalRefusal, 1)
	freshTok := h.token(h.accessClaims())
	go func() {
		_, r := h.pi.VerifyDetail(context.Background(), h.reg, freshTok, PortalTokenTypeAccess)
		fresh <- r
	}()
	wait(keysArrived, "the recovery's key fetch never reached the provider")
	// The old failure's drop runs now, after the fresh discovery and before its keys arrive.
	releaseDrop()
	select {
	case r := <-old:
		if r.Reason != PortalIssuerUnavailable || r.Cause != portalCauseAddressRefused {
			t.Fatalf("the old exchange got %+v, want its own address_refused", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the old exchange never answered")
	}
	if okAt, _, failed := v.ks.state(); !okAt.IsZero() && !failed {
		t.Fatal("the verifier opened before the current keys loaded")
	}
	releaseKeys()
	select {
	case r := <-fresh:
		if !r.OK() {
			t.Fatalf("the recovery's exchange was refused: %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the recovery's exchange never answered")
	}
	disc, keys := h.idp.hits()
	// The verifier reopened on that fetch: admitted with no further retry or fetch.
	h.mustAdmit(h.token(h.accessClaims()))
	h.mustAdmit(h.kidToken("k2", 1))
	if d, k := h.idp.hits(); d != disc || k != keys {
		t.Fatalf("(%d, %d) fetches after the recovery, want (%d, %d)", d, k, disc, keys)
	}
}

func TestPortalDrop_OldFailureKeepsSameAddressDiscovery(t *testing.T) {
	h := newPortalHarness(t, "example.com", true, "/oauth2/a")
	h.idp.set(func(f *fakeIdP) {
		f.override = func(w http.ResponseWriter, r *http.Request) bool {
			w.Header().Set("Connection", "close") // every request resolves and dials again
			return false
		}
	})
	h.mustAdmit(h.token(h.accessClaims()))
	h.idp.publish(t, "k2", 1)
	v, _ := h.pi.verifierFor(h.reg)
	held, resume := make(chan struct{}), make(chan struct{})
	published, publishGo := make(chan struct{}), make(chan struct{})
	var dropOnce, resumeOnce, pubOnce, pubGoOnce sync.Once
	releaseDrop := func() { resumeOnce.Do(func() { close(resume) }) }
	releasePublish := func() { pubGoOnce.Do(func() { close(publishGo) }) }
	t.Cleanup(releaseDrop)
	t.Cleanup(releasePublish)
	v.beforeDrop = func() { dropOnce.Do(func() { close(held); <-resume }) }
	v.afterPublish = func() { pubOnce.Do(func() { close(published); <-publishGo }) }
	wait := func(c <-chan struct{}, what string) {
		t.Helper()
		select {
		case <-c:
		case <-time.After(5 * time.Second):
			t.Fatal(what)
		}
	}
	answer := func(c <-chan PortalRefusal, what string) PortalRefusal {
		t.Helper()
		select {
		case r := <-c:
			return r
		case <-time.After(5 * time.Second):
			t.Fatal(what)
			return PortalRefusal{}
		}
	}
	verifyAsync := func(raw string) <-chan PortalRefusal {
		c := make(chan PortalRefusal, 1)
		go func() {
			_, r := h.pi.VerifyDetail(context.Background(), h.reg, raw, PortalTokenTypeAccess)
			c <- r
		}()
		return c
	}
	h.clock.Advance(portalKeyCooldown + time.Second)
	h.setLookup(func(string) []net.IP { return []net.IP{net.ParseIP("127.0.0.1")} })
	old := verifyAsync(h.kidToken("k2", 1))
	wait(held, "the refused key fetch never reached its drop")
	// A refresh publishes a discovery with the same keys address and is held before it reserves its key fetch.
	h.setLookup(func(string) []net.IP { return []net.IP{portalPublicIP} })
	h.clock.Advance(portalKeyCooldown + time.Second)
	fresh := verifyAsync(h.token(h.accessClaims()))
	wait(published, "the refresh never published discovery")
	// The old failure's drop runs now, between the publish and the reservation.
	releaseDrop()
	if r := answer(old, "the old exchange never answered"); r.Reason != PortalIssuerUnavailable || r.Cause != portalCauseAddressRefused {
		t.Fatalf("the old exchange got %+v, want its own address_refused", r)
	}
	releasePublish()
	if r := answer(fresh, "the recovery's exchange never answered"); !r.OK() {
		t.Fatalf("the recovery's exchange was refused: %+v", r)
	}
	disc, keys := h.idp.hits()
	h.mustAdmit(h.kidToken("k2", 1))
	if d, k := h.idp.hits(); d != disc || k != keys {
		t.Fatalf("(%d, %d) fetches after the recovery, want (%d, %d)", d, k, disc, keys)
	}
}

func TestPortalKeySet_RuleFailureNeedsFreshDiscovery(t *testing.T) {
	portalTestKeys(t)
	const keysURL = "https://idp.test/keys"
	g := newGatedKeys(t)
	clk := newPortalClock()
	ks := newPortalKeySet(clk.Now, 5*time.Second, g.fetch)
	type seen struct {
		at     portalKeyAt
		url    string
		keys   int
		failed bool
	}
	seenAt := make(chan seen, 2)
	ks.onFail = func(_ string, _ error, at portalKeyAt) {
		ks.mu.RLock()
		seenAt <- seen{at: at, url: ks.url, keys: len(ks.keys), failed: ks.failed}
		ks.mu.RUnlock()
	}
	ks.setURL(keysURL)
	warm := startEnsure(ks)
	g.waitStarted(keysURL)
	g.gate(keysURL) <- gatedAnswer{kid: "k1"}
	warm.wait(t)
	clk.Advance(portalKeyCooldown + time.Second)
	first := startEnsure(ks)
	g.waitStarted(keysURL)
	g.gate(keysURL) <- gatedAnswer{err: &portalFetchError{cause: portalCauseAddressRefused}}
	first.wait(t)
	got := <-seenAt
	// By the time the failure is reported the keys and their address are already gone.
	if got.url != "" || got.keys != 0 || !got.failed {
		t.Fatalf("at the report: url %q, %d keys, failed %v; want no address, no keys, failed", got.url, got.keys, got.failed)
	}
	// A key-only reservation after the budget has no address to fetch and does not supersede the failure.
	clk.Advance(portalKeyCooldown + time.Second)
	next := startEnsure(ks)
	g.waitStarted("")
	g.gate("") <- gatedAnswer{err: &portalFetchError{cause: portalCauseUnreachable, detail: "no keys address"}}
	next.wait(t)
	<-seenAt
	if !ks.dropAt(got.at) {
		t.Fatal("a key-only reservation superseded a failure that broke a rule")
	}
	// A verified discovery published after the failure does supersede it.
	ks2 := newPortalKeySet(clk.Now, 5*time.Second, g.fetch)
	ks2.setURL(keysURL)
	at := portalKeyAt{epoch: ks2.epoch}
	ks2.setURL(keysURL)
	if ks2.dropAt(at) {
		t.Fatal("a failure dropped the set after a newer discovery was published")
	}
}

func TestPortalRecovery_RuleFailureNeedsFreshDiscovery(t *testing.T) {
	h := newPortalHarness(t, "example.com", true, "/oauth2/a")
	h.idp.set(func(f *fakeIdP) {
		f.override = func(w http.ResponseWriter, r *http.Request) bool {
			w.Header().Set("Connection", "close") // every request resolves and dials again
			return false
		}
	})
	warmPortal(h)
	h.idp.publish(t, "k2", 1)
	v, _ := h.pi.verifierFor(h.reg)
	held, resume := make(chan struct{}), make(chan struct{})
	var dropOnce, resumeOnce sync.Once
	releaseDrop := func() { resumeOnce.Do(func() { close(resume) }) }
	t.Cleanup(releaseDrop)
	v.beforeDrop = func() { dropOnce.Do(func() { close(held); <-resume }) }
	h.clock.Advance(portalKeyCooldown + time.Second)
	// An exchange is through prepare while the verifier is healthy and has not reached its key yet.
	late := pauseAfterPrepare(t, h, h.kidToken("k2", 1))
	h.setLookup(func(string) []net.IP { return []net.IP{net.ParseIP("127.0.0.1")} })
	old := make(chan PortalRefusal, 1)
	oldTok := h.kidToken("k2", 1)
	go func() {
		_, r := h.pi.VerifyDetail(context.Background(), h.reg, oldTok, PortalTokenTypeAccess)
		old <- r
	}()
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("the refused key fetch never reached its drop")
	}
	// The address is healthy again and the budget has passed, but no discovery has run: the late exchange's
	// key-only fetch must not reuse the refused address, and nothing is admitted.
	h.setLookup(func(string) []net.IP { return []net.IP{portalPublicIP} })
	h.clock.Advance(portalKeyCooldown + time.Second)
	disc, keys := h.idp.hits()
	if r := late.release(); r.Reason != PortalIssuerUnavailable {
		t.Fatalf("the late exchange got %+v, want 503: a key-only fetch reopened a refused address", r)
	}
	if d, k := h.idp.hits(); d != disc || k != keys {
		t.Fatalf("(%d, %d) fetches by the key-only attempt, want none (%d, %d)", d, k, disc, keys)
	}
	releaseDrop()
	select {
	case r := <-old:
		if r.Reason != PortalIssuerUnavailable || r.Cause != portalCauseAddressRefused {
			t.Fatalf("the old exchange got %+v, want its own address_refused", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the old exchange never answered")
	}
	// Closed under the rule's cause until a fresh discovery passes.
	h.wantUnavailable(h.token(h.accessClaims()), portalCauseAddressRefused)
	h.clock.Advance(portalKeyCooldown + time.Second)
	h.mustAdmit(h.kidToken("k2", 1))
	if d, _ := h.idp.hits(); d != disc+1 {
		t.Fatalf("%d discovery fetches, want one more (%d) before anything was admitted", d, disc+1)
	}
}

func TestPortalRecheck_HeldPastSelectionDuringAddressChangeIs503(t *testing.T) {
	h := newPortalHarness(t, "example.com", true, "/oauth2/a")
	var holdKeys atomic.Bool
	keysArrived, keysRelease := make(chan struct{}), make(chan struct{})
	var keysOnce, keysRelOnce sync.Once
	releaseKeys := func() { keysRelOnce.Do(func() { close(keysRelease) }) }
	t.Cleanup(releaseKeys)
	h.idp.set(func(f *fakeIdP) {
		f.override = func(w http.ResponseWriter, r *http.Request) bool {
			if holdKeys.Load() && strings.HasSuffix(r.URL.Path, "/v2/keys") {
				keysOnce.Do(func() { close(keysArrived); <-keysRelease })
			}
			return false
		}
	})
	warmPortal(h)
	v, _ := h.pi.verifierFor(h.reg)
	selected, resume := make(chan struct{}), make(chan struct{})
	var selOnce, resumeOnce sync.Once
	releaseSel := func() { resumeOnce.Do(func() { close(resume) }) }
	t.Cleanup(releaseSel)
	v.ks.afterSelect = func() { selOnce.Do(func() { close(selected); <-resume }) }
	wait := func(c <-chan struct{}, what string) {
		t.Helper()
		select {
		case <-c:
		case <-time.After(5 * time.Second):
			t.Fatal(what)
		}
	}
	verifyAsync := func(raw string) <-chan PortalRefusal {
		c := make(chan PortalRefusal, 1)
		go func() {
			_, r := h.pi.VerifyDetail(context.Background(), h.reg, raw, PortalTokenTypeAccess)
			c <- r
		}()
		return c
	}
	answer := func(c <-chan PortalRefusal, what string) PortalRefusal {
		t.Helper()
		select {
		case r := <-c:
			return r
		case <-time.After(5 * time.Second):
			t.Fatal(what)
			return PortalRefusal{}
		}
	}
	h.clock.Advance(59*time.Minute + 50*time.Second)
	held := verifyAsync(h.token(h.accessClaims()))
	wait(selected, "the exchange never selected its key")
	// Discovery moves the keys address: the keys are dropped and the new address's fetch is held at the provider.
	h.idp.set(func(f *fakeIdP) { f.jwksURI = h.reg.Issuer + "/v2/keys" })
	holdKeys.Store(true)
	h.clock.Advance(20 * time.Second)
	recovery := verifyAsync(h.token(h.accessClaims()))
	wait(keysArrived, "the new address's key fetch never reached the provider")
	// The held exchange's valid token is answered 503 keys_cooldown, not 400: its keys are pending, not removed.
	releaseSel()
	if r := answer(held, "the held exchange never answered"); r.Reason != PortalIssuerUnavailable || r.Cause != portalCauseKeysCooldown || r.Class != PortalRefusedUnavailable {
		t.Fatalf("the held exchange got %+v, want 503 cause %q", r, portalCauseKeysCooldown)
	}
	releaseKeys()
	if r := answer(recovery, "the recovery's exchange never answered"); !r.OK() {
		t.Fatalf("the recovery's exchange was refused: %+v", r)
	}
	h.mustAdmit(h.token(h.accessClaims()))
}

func TestPortalLookup_KeysDroppedAfterOwnFetchArePending(t *testing.T) {
	t.Run("the fetch the exchange's refresh made, then a refused discovery", func(t *testing.T) {
		h := newPortalHarness(t, "example.com", true, "/oauth2/a")
		// A cold exchange: its prepare runs the refresh that fetches the keys, then it is held before its key.
		x := pauseAfterPrepare(t, h, h.token(h.accessClaims()))
		h.idp.set(func(f *fakeIdP) { f.docIssuer = h.reg.Issuer + "/other" })
		h.clock.Advance(time.Hour)
		h.wantUnavailable(h.token(h.accessClaims()), portalCauseIssuerMismatch)
		// The held exchange's own fetch answered, but the keys it brought are gone: 503, not 400.
		if r := x.release(); r.Reason != PortalIssuerUnavailable || r.Cause != portalCauseKeysCooldown {
			t.Fatalf("the held exchange got %+v, want 503 cause %q", r, portalCauseKeysCooldown)
		}
	})
	t.Run("the fetch the lookup waited on, then an address change", func(t *testing.T) {
		portalTestKeys(t)
		const urlA, urlB = "https://idp.test/a/keys", "https://idp.test/b/keys"
		g := newGatedKeys(t)
		clk := newPortalClock()
		ks := newPortalKeySet(clk.Now, 5*time.Second, g.fetch)
		ks.setURL(urlA)
		ks.onOK = func() { ks.setURL(urlB) } // the address changes after the fetch lands, before the lookup reads
		got := make(chan portalKeyLookup, 1)
		go func() { got <- ks.lookup(context.Background(), "k1", portalFresh{}) }()
		g.waitStarted(urlA)
		g.gate(urlA) <- gatedAnswer{kid: "k1"}
		select {
		case r := <-got:
			if r.found || r.fault == nil || r.fault.cause != portalCauseKeysCooldown {
				t.Fatalf("lookup = %+v (fault %+v), want a keys_cooldown fault", r, r.fault)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the lookup never answered")
		}
	})
	t.Run("a key a successful fetch removed is still a plain refusal", func(t *testing.T) {
		h := newPortalHarness(t, "example.com", true, "/oauth2/a")
		warmPortal(h)
		h.idp.unpublish("k1")
		h.idp.publish(t, "k2", 1)
		h.clock.Advance(portalKeyCooldown + time.Second)
		h.mustAdmit(h.kidToken("k2", 1)) // a successful fetch that no longer lists k1
		if r := h.wantRefusal(h.token(h.accessClaims()), SubjectTokenInvalid); r.Class != PortalRefusedToken {
			t.Fatalf("class = %v, want a token refusal", r.Class)
		}
	})
}
