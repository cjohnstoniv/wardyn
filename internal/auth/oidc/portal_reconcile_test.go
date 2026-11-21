// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
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
	var once sync.Once
	v.afterPrepare = func() { once.Do(func() { close(paused); <-x.resume }) }
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
