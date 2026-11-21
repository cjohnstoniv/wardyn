// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The grant's client secret record and its masks: what the route writes and refuses, how a rotation, a
// deletion or an overlap of them reaches the mint, and which values stay masked until when.

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	svcSecretTwo   = "svc-client-secret-0002-abcdefgh"
	svcSecretThree = "svc-client-secret-0003-abcdefgh"
)

// masked reports whether the registry holds v, current or retired and not yet swept.
func (f *svcFix) masked(v string) bool { return svcMasked(f.srv, v) }

func svcMasked(s *Server, v string) bool {
	for _, b := range s.cfg.MaskRegistry.Snapshot(uuid.Nil) {
		if bytes.Equal(b, []byte(v)) {
			return true
		}
	}
	return false
}

// sweep drops every retired or expired value, as the registry's sweep does once the grace has passed.
func (f *svcFix) sweep() { f.srv.cfg.MaskRegistry.SweepGlobals(f.clock.Now().Add(24 * time.Hour)) }

// replica is a second server on the same store, secrets and identity provider, with its own memory.
func (f *svcFix) replica() *Server {
	cfg := f.srv.cfg
	cfg.MaskRegistry = secretmask.NewRegistry()
	return &Server{cfg: cfg}
}

func TestServiceGrant_SecretNameReservedAtEverySink(t *testing.T) {
	f := newSvcFix(t)
	name := f.secretName() // the generated name, not a literal
	// Every guard a secret name passes through: the broker's (exported predicate, which wraps
	// reservedBrokerSecret), the api's four. A change to any one pattern alone must fail here.
	for guard, reserved := range map[string]func(string) bool{
		"broker.ReservedSecretName (reservedBrokerSecret)": broker.ReservedSecretName,
		"reservedSecret":         reservedSecret,
		"sinkReservedSecret":     sinkReservedSecret,
		"nameSinkReservedSecret": nameSinkReservedSecret,
		"secretsAPIReserved":     secretsAPIReserved,
	} {
		if !reserved(name) {
			t.Errorf("%s does not reserve the generated service grant secret name %q", guard, name)
		}
	}
	p := f.provider()
	p.OIDC.ClientSecretRef = name
	if err := validateProviderOIDC(p, true); err == nil {
		t.Errorf("a provider's client_secret_ref may name %s", name)
	}
	if uid, ok := serviceGrantSecretUID(name); !ok || uid != f.uid {
		t.Errorf("serviceGrantSecretUID(%q) = %q, %v", name, uid, ok)
	}
}

func TestServiceGrant_SecretTooShortRefusedAtWriteAndRead(t *testing.T) {
	f := newSvcFix(t)
	w := f.serve(http.MethodPut, f.secretPath(), f.putSecretBody("short"))
	if w.Code != http.StatusBadRequest || svcBody(t, w)["reason"] != reasonSecretValueTooShort || f.sec.has(f.secretName()) {
		t.Fatalf("a 5 byte secret: %d %s", w.Code, w.Body.String())
	}
	if w = f.serve(http.MethodPut, f.secretPath(), f.putSecretBody("1234567\n")); w.Code != http.StatusBadRequest {
		t.Fatalf("7 bytes and a line break: %d, want 400 (the trailing break is not counted)", w.Code)
	}
	f.writeSecret("short")
	se := svcErr(t, func() error { _, err := f.token(serviceGrantTriggerRefresh); return err }())
	if se.reason != svcReasonSecretUnmaskable || f.idp.count() != 0 {
		t.Fatalf("a short stored secret: %+v with %d requests, want secret_unmaskable and none", se, f.idp.count())
	}
}

func TestServiceGrant_RotatedSecretMaskRetiredAfterGrace(t *testing.T) {
	f := newSvcFixWithSecret(t)
	f.mustToken(serviceGrantTriggerRefresh)
	if !f.masked(svcClientSecret) {
		t.Fatal("the secret is not masked after a mint")
	}
	if w := f.serve(http.MethodPut, f.secretPath(), f.putSecretBody(svcSecretTwo)); w.Code != http.StatusNoContent {
		t.Fatalf("rotation: %d %s", w.Code, w.Body.String())
	}
	if !f.masked(svcClientSecret) || !f.masked(svcSecretTwo) {
		t.Fatal("the old value must stay masked through the grace and the new one be current")
	}
	f.sweep()
	if f.masked(svcClientSecret) || !f.masked(svcSecretTwo) {
		t.Fatalf("after the grace: old masked %v, new masked %v, want only the new one", f.masked(svcClientSecret), f.masked(svcSecretTwo))
	}
}

func TestServiceGrant_ExternalRotationRetiresOldMaskAfterGrace(t *testing.T) {
	f := newSvcFixWithSecret(t)
	f.mustToken(serviceGrantTriggerRefresh)
	f.writeSecret(svcSecretTwo) // written in the external store, not through the route
	f.mustToken(serviceGrantTriggerResolve)
	if f.idp.count() != 2 {
		t.Fatalf("requests = %d, want a new one under the new revision", f.idp.count())
	}
	if !f.masked(svcClientSecret) || !f.masked(svcSecretTwo) {
		t.Fatal("both values are masked until the grace has passed")
	}
	f.sweep()
	if f.masked(svcClientSecret) || !f.masked(svcSecretTwo) {
		t.Fatalf("after the grace: old %v, new %v", f.masked(svcClientSecret), f.masked(svcSecretTwo))
	}
}

func TestServiceGrant_SecretDeleteRetiresMask(t *testing.T) {
	// The route's DELETE.
	f := newSvcFixWithSecret(t)
	f.mustToken(serviceGrantTriggerRefresh)
	if w := f.serve(http.MethodDelete, f.secretPath(), nil); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE: %d %s", w.Code, w.Body.String())
	}
	f.sweep()
	if f.masked(svcClientSecret) {
		t.Error("the route's DELETE left the secret masked")
	}
	// A gone read.
	g := newSvcFixWithSecret(t)
	g.mustToken(serviceGrantTriggerRefresh)
	g.sec.mu.Lock()
	delete(g.sec.m, g.secretName())
	g.sec.rev[g.secretName()]++
	g.sec.mu.Unlock()
	if se := svcErr(t, func() error { _, err := g.token(serviceGrantTriggerResolve); return err }()); se.reason != svcReasonSecretGone {
		t.Fatalf("a gone record: %+v", se)
	}
	g.sweep()
	if g.masked(svcClientSecret) {
		t.Error("a gone read left the secret masked")
	}
	// A removed grant: the next pass retires it.
	h := newSvcFixWithSecret(t)
	h.mustToken(serviceGrantTriggerRefresh)
	h.setRow(func(p *types.ModelProvider) { p.OIDC.ServiceGrant = nil })
	h.srv.reconcileServiceGrantMasks(context.Background())
	h.sweep()
	if h.masked(svcClientSecret) {
		t.Error("a removed grant left the secret masked")
	}
}

func TestServiceGrant_SecretMaskBlockedPutThenRead(t *testing.T) {
	f := newSvcFixWithSecret(t)
	f.mustToken(serviceGrantTriggerRefresh)
	held, release := make(chan struct{}), make(chan struct{})
	f.sec.hold = func(op, _ string) {
		if op == "put" {
			close(held)
			<-release
		}
	}
	done := make(chan *svcHTTP, 1)
	go func() {
		w := f.serve(http.MethodPut, f.secretPath(), f.putSecretBody(svcSecretTwo))
		done <- &svcHTTP{Code: w.Code}
	}()
	<-held
	minted := make(chan struct{})
	go func() {
		defer close(minted)
		f.srv.dropServiceGrantCache(f.uid)
		_, _ = f.token(serviceGrantTriggerResolve)
	}()
	time.Sleep(50 * time.Millisecond)
	if f.idp.count() != 1 {
		t.Fatalf("a mint read the record while the put was blocked: %d requests", f.idp.count())
	}
	f.sec.hold = nil
	close(release)
	if res := <-done; res.Code != http.StatusNoContent {
		t.Fatalf("put: %d", res.Code)
	}
	<-minted
	reqs := f.idp.requests()
	if _, secret := svcBasic(t, reqs[len(reqs)-1].Header.Get("Authorization")); secret != svcSecretTwo {
		t.Fatalf("the mint after the blocked put sent %q, want the new value", secret)
	}
	f.sweep()
	if f.masked(svcClientSecret) || !f.masked(svcSecretTwo) {
		t.Fatalf("after the grace: old %v new %v, want the old one retired and swept", f.masked(svcClientSecret), f.masked(svcSecretTwo))
	}
	// A failed Put applies what the store holds.
	f.sec.putErr = errSvcDown
	if w := f.serve(http.MethodPut, f.secretPath(), f.putSecretBody(svcSecretThree)); w.Code < 500 {
		t.Fatalf("a failing put answered %d", w.Code)
	}
	if !f.masked(svcSecretTwo) || f.masked(svcSecretThree) {
		t.Errorf("after a failed put the masks are two=%v three=%v, want what the store holds", f.masked(svcSecretTwo), f.masked(svcSecretThree))
	}
}

func TestServiceGrant_ReconcileKeepsFreshlyAddedGrant(t *testing.T) {
	f := newSvcFix(t)
	held, release := make(chan struct{}), make(chan struct{})
	f.sec.hold = func(op, _ string) {
		if op == "put" {
			close(held)
			<-release
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.serve(http.MethodPut, f.secretPath(), f.putSecretBody(svcClientSecret))
	}()
	<-held
	// The reconciliation meets the route mid-write: it leaves that name to the route.
	f.srv.noteServiceGrantMasked(f.secretName())
	f.srv.reconcileServiceGrantMasks(context.Background())
	f.sec.hold = nil
	close(release)
	<-done
	f.srv.reconcileServiceGrantMasks(context.Background())
	f.sweep()
	if !f.masked(svcClientSecret) {
		t.Fatal("the pass retired the value the route had just applied")
	}
}

func TestServiceGrant_RevisionThreeAnswers(t *testing.T) {
	// An error: the mint is unavailable and no request is sent.
	f := newSvcFixWithSecret(t)
	f.sec.revErr = errSvcDown
	if _, err := f.token(serviceGrantTriggerRefresh); err == nil || f.idp.count() != 0 {
		t.Fatalf("a revision error: %v with %d requests, want a refusal and none", err, f.idp.count())
	}
	if se, ok := err2se(f.token(serviceGrantTriggerRefresh)); ok && se.definitive {
		t.Errorf("a revision error is not definitive: %+v", se)
	}
	// A gone entry: authoritative absence.
	f.sec.revErr = nil
	f.sec.mu.Lock()
	delete(f.sec.m, f.secretName())
	f.sec.mu.Unlock()
	if se := svcErr(t, func() error { _, err := f.token(serviceGrantTriggerRefresh); return err }()); se.reason != svcReasonSecretGone || f.idp.count() != 0 {
		t.Fatalf("a gone entry: %+v with %d requests", se, f.idp.count())
	}
	// A store with no revision: the mint proceeds and the cache is keyed by the digest alone.
	g := newSvcFixWithSecret(t)
	g.sec.noRev = true
	g.mustToken(serviceGrantTriggerRefresh)
	g.writeSecret(svcSecretTwo)
	g.mustToken(serviceGrantTriggerResolve)
	if g.idp.count() != 1 {
		t.Fatalf("a no-revision store minted again on a changed value: %d requests", g.idp.count())
	}
}

func err2se(_ serviceGrantMinted, err error) (*serviceGrantError, bool) {
	se, ok := err.(*serviceGrantError)
	return se, ok
}

func TestServiceGrant_ExternalStoreValueUsedAtNextMint(t *testing.T) {
	f := newSvcFixWithSecret(t)
	f.sec.noRev = true // the vaultkv fake keeps no revision
	f.mustToken(serviceGrantTriggerRefresh)
	f.writeSecret(svcSecretTwo)
	f.clock.Advance(55 * time.Minute) // inside the reuse margin: the next mint reads the store again
	f.mustToken(serviceGrantTriggerRefresh)
	reqs := f.idp.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	if _, secret := svcBasic(t, reqs[1].Header.Get("Authorization")); secret != svcSecretTwo {
		t.Fatalf("the second mint sent %q, want the externally written value", secret)
	}
}

func TestServiceGrant_SupersededTokenStaysMaskedToItsExpiry(t *testing.T) {
	f := newSvcFixWithSecret(t)
	release := make(chan struct{})
	f.idp.respond = func(n int, _ svcReq) svcAnswer {
		a := svcOK(n, 3600)
		if n == 1 {
			a.Block = release
		}
		return a
	}
	out := make(chan struct{})
	go func() { defer close(out); _, _ = f.token(serviceGrantTriggerResolve) }()
	for f.idp.count() == 0 {
		time.Sleep(time.Millisecond)
	}
	f.writeSecret(svcSecretTwo)
	close(release)
	<-out
	f.srv.cfg.MaskRegistry.SweepGlobals(f.clock.Now().Add(30 * time.Minute))
	if !f.masked(svcToken(1)) {
		t.Fatal("the superseded token stopped being masked before its expiry")
	}
	f.srv.cfg.MaskRegistry.SweepGlobals(f.clock.Now().Add(24 * time.Hour))
	if f.masked(svcToken(1)) {
		t.Error("the superseded token is masked after its expiry and grace")
	}
}

func TestServiceGrant_DefinitiveFailureEvictsCacheOnRemoteDeletion(t *testing.T) {
	f := newSvcFixWithSecret(t)
	f.mustToken(serviceGrantTriggerRefresh)
	f.clock.Advance(55 * time.Minute) // a reusable cache entry, due for renewal
	release := make(chan struct{})
	f.idp.respond = func(int, svcReq) svcAnswer {
		a := svcOAuthError(500, "")
		a.Status, a.Body, a.Block = 500, `{}`, release
		return a
	}
	out := make(chan error, 1)
	go func() { _, err := f.token(serviceGrantTriggerRefresh); out <- err }()
	for f.idp.count() < 2 {
		time.Sleep(time.Millisecond)
	}
	f.sec.mu.Lock() // the record is deleted remotely while the refresh fails
	delete(f.sec.m, f.secretName())
	f.sec.rev[f.secretName()]++
	f.sec.mu.Unlock()
	close(release)
	if err := <-out; err == nil {
		t.Fatal("a failed refresh served the cached token after the record was deleted")
	}
	f.srv.svcGrant.mu.Lock()
	_, cached := f.srv.svcGrant.cache[f.uid]
	f.srv.svcGrant.mu.Unlock()
	if cached {
		t.Error("the cache entry survived the remote deletion")
	}
}

func TestServiceGrant_RotationDuringFailedRefreshServesNoOldToken(t *testing.T) {
	f := newSvcFixWithSecret(t)
	f.mustToken(serviceGrantTriggerRefresh)
	f.clock.Advance(55 * time.Minute)
	release := make(chan struct{})
	f.idp.respond = func(n int, _ svcReq) svcAnswer {
		if n == 2 {
			return svcAnswer{Status: 500, Body: `{}`, Block: release}
		}
		return svcOK(n, 3600)
	}
	out := make(chan serviceGrantMinted, 1)
	go func() { tok, _ := f.token(serviceGrantTriggerRefresh); out <- tok }()
	for f.idp.count() < 2 {
		time.Sleep(time.Millisecond)
	}
	f.writeSecret(svcSecretTwo) // rotated while the refresh fails
	close(release)
	if tok := <-out; tok.token == svcToken(1) {
		t.Fatal("the failed refresh served the token minted under the old secret")
	}
}
