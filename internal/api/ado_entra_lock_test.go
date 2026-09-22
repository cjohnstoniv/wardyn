// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// gatedSecrets holds the first Put issued after arm() until release is closed,
// so a test can stop a renewal between its read and its write.
type gatedSecrets struct {
	secretstore.Store
	mu      sync.Mutex
	armed   bool
	held    chan struct{}
	release chan struct{}
}

func (g *gatedSecrets) arm() {
	g.mu.Lock()
	g.armed, g.held, g.release = true, make(chan struct{}), make(chan struct{})
	g.mu.Unlock()
}

func (g *gatedSecrets) For(owner string) secretstore.Store {
	return &gatedView{Store: g.Store.For(owner), g: g}
}

type gatedView struct {
	secretstore.Store
	g *gatedSecrets
}

func (v *gatedView) Put(ctx context.Context, name string, value []byte) error {
	v.g.mu.Lock()
	hold := v.g.armed
	v.g.armed = false
	held, release := v.g.held, v.g.release
	v.g.mu.Unlock()
	if hold {
		close(held)
		<-release
	}
	return v.Store.Put(ctx, name, value)
}

// TestADOCapture_WaitsForARenewalInFlight: a renewal reads the stored sign-in,
// calls the authority, then writes back its rotation of that sign-in. A new
// sign-in captured in between must not be overwritten by the renewal's copy
// of the old one, so the callback takes the same per-person lock.
func TestADOCapture_WaitsForARenewalInFlight(t *testing.T) {
	f := newADOFixture(t)
	gate := &gatedSecrets{Store: f.srv.cfg.Secrets}
	f.srv.cfg.Secrets = gate
	subject := f.fake.Subject()
	if w := f.capture(t, subject); w.Code != http.StatusFound {
		t.Fatalf("first capture: status %d body %q", w.Code, w.Body.String())
	}

	// The second sign-in's browser leg completes before the renewal starts.
	authURL, cookies := f.signIn(t, subject, "")
	q := follow(t, authURL)

	gate.arm()
	redeemed := make(chan error, 1)
	go func() {
		_, err := f.srv.RedeemADOEntraAccess(context.Background(), f.cfg, subject, f.cfg.Scopes)
		redeemed <- err
	}()
	<-gate.held // the renewal has read the old sign-in and is about to write

	captured := make(chan int, 1)
	go func() { captured <- f.callback(t, subject, q, cookies).Code }()
	select {
	case <-captured:
		t.Log("the callback stored while the renewal was in flight")
		captured <- http.StatusFound // re-queue for the read below
	case <-time.After(time.Second):
		// Waiting on the per-person lock, as it should.
	}
	close(gate.release)

	if err := <-redeemed; err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if code := <-captured; code != http.StatusFound {
		t.Fatalf("second capture: status %d", code)
	}

	got, found := f.stored(t, subject)
	if !found {
		t.Fatal("the credential vanished")
	}
	if !got.RenewedAt.IsZero() {
		raw, _ := json.Marshal(got.Scopes)
		t.Fatalf("the renewal's copy of the old sign-in overwrote the new capture (RenewedAt=%v scopes=%s)", got.RenewedAt, raw)
	}
	if live, known := f.fake.RefreshTokenState(got.RefreshToken); !known || !live {
		t.Fatalf("the stored refresh token is not the new capture's live one (live=%v known=%v)", live, known)
	}
}
