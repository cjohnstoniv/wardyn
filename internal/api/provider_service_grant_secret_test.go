// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The grant's client secret record and its masks: what the route writes and refuses, how a rotation, a
// deletion or an overlap of them reaches the mint, and which values stay masked until when.

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

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

func TestServiceGrant_SecretWriteRefusesChangedDestination_PG(t *testing.T) {
	f := newSvcFix(t)
	cases := []struct {
		name  string
		edit  func(*putServiceGrantSecretRequest)
		wants []string // what the new audit row must name: both hosts of the changed pair
	}{
		{"token_url", func(b *putServiceGrantSecretRequest) { b.TokenURL = "https://other-idp.example.test/token" },
			[]string{"body_token_host", "other-idp.example.test", "token_host", "idp.example.test"}},
		{"client_id", func(b *putServiceGrantSecretRequest) { b.ClientID = "another-client" }, []string{"client_id"}},
		{"base_url", func(b *putServiceGrantSecretRequest) { b.BaseURL = "https://other-gw.corp.example" },
			[]string{"body_base_host", "other-gw.corp.example", "base_host"}},
	}
	for i, c := range cases {
		body := f.putSecretBody(svcClientSecret)
		c.edit(&body)
		w := f.serve(http.MethodPut, f.secretPath(), body)
		if w.Code != http.StatusConflict || svcBody(t, w)["reason"] != reasonServiceGrantDestinationChanged || f.sec.has(f.secretName()) {
			t.Fatalf("%s: %d %s, record stored %v; want 409 and nothing stored", c.name, w.Code, w.Body.String(), f.sec.has(f.secretName()))
		}
		rows := f.audit.find("model_provider.service_grant.secret_write")
		if len(rows) != i+1 {
			t.Fatalf("%s: audit rows = %d, want %d, one new row per refusal", c.name, len(rows), i+1)
		}
		d := string(rows[len(rows)-1].Data) // this case's own row, whatever order the cases ran in
		for _, want := range c.wants {
			if !strings.Contains(d, want) {
				t.Errorf("%s: its audit row %s lacks %q", c.name, d, want)
			}
		}
	}
	if w := f.serve(http.MethodPut, f.secretPath(), f.putSecretBody(svcClientSecret)); w.Code != http.StatusNoContent || !f.sec.has(f.secretName()) {
		t.Fatalf("the right destination: %d %s", w.Code, w.Body.String())
	}
}

func TestServiceGrant_SecretNameReservedAtEverySink(t *testing.T) {
	f := newSvcFix(t)
	name := f.secretName()
	if !nameSinkReservedSecret(name) {
		t.Fatalf("%s is not reserved at the secret sinks", name)
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

func TestServiceGrant_SecretRemovalOnOneReplicaEvictsOnAnother_PG(t *testing.T) {
	f := newSvcFixWithSecret(t)
	b := f.replica()
	f.mustToken(serviceGrantTriggerRefresh)
	if _, err := b.serviceGrantToken(context.Background(), f.provider(), "", serviceGrantTriggerRefresh); err != nil {
		t.Fatal(err)
	}
	if !svcMasked(b, svcClientSecret) {
		t.Fatal("B does not mask the secret after its mint")
	}
	// The secret's DELETE on A: B's next pass retires its mask, swept after the grace.
	if w := f.serve(http.MethodDelete, f.secretPath(), nil); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE on A: %d", w.Code)
	}
	b.reconcileServiceGrantMasks(context.Background())
	b.cfg.MaskRegistry.SweepGlobals(f.clock.Now().Add(24 * time.Hour))
	if svcMasked(b, svcClientSecret) {
		t.Error("B still masks a secret deleted on A")
	}
	// Separately: the grant removed on A.
	f.writeSecret(svcClientSecret)
	if _, err := b.serviceGrantToken(context.Background(), f.provider(), "", serviceGrantTriggerRefresh); err != nil {
		t.Fatal(err)
	}
	f.setRow(func(p *types.ModelProvider) { p.OIDC.ServiceGrant = nil })
	b.reconcileServiceGrantMasks(context.Background())
	b.cfg.MaskRegistry.SweepGlobals(f.clock.Now().Add(24 * time.Hour))
	if svcMasked(b, svcClientSecret) {
		t.Error("B still masks the secret of a grant removed on A")
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

func TestServiceGrant_SharedSecretRotationRemintsWithinATick_PG(t *testing.T) {
	f := newSvcFixWithSecret(t)
	b := f.replica()
	ctx := context.Background()
	f.srv.serviceGrantPass(ctx)
	b.serviceGrantPass(ctx)
	if f.idp.count() != 2 {
		t.Fatalf("setup: %d requests", f.idp.count())
	}
	if w := f.serve(http.MethodPut, f.secretPath(), f.putSecretBody(svcSecretTwo)); w.Code != http.StatusNoContent {
		t.Fatalf("rotation: %d", w.Code)
	}
	b.serviceGrantPass(ctx) // within one tick of the rotation
	reqs := f.idp.requests()
	if len(reqs) != 3 {
		t.Fatalf("requests = %d, want B's pass to mint again", len(reqs))
	}
	if _, secret := svcBasic(t, reqs[2].Header.Get("Authorization")); secret != svcSecretTwo {
		t.Fatalf("B's new mint used %q, want the rotated secret", secret)
	}
}

func TestServiceGrant_SecretDeletedStopsServing_PG(t *testing.T) {
	f := newSvcFixWithSecret(t)
	f.mustToken(serviceGrantTriggerRefresh)
	if w := f.serve(http.MethodDelete, f.secretPath(), nil); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE: %d", w.Code)
	}
	got, err := f.token(serviceGrantTriggerResolve)
	if se := svcErr(t, err); se.reason != svcReasonSecretGone || got.token != "" {
		t.Fatalf("after the delete: %q, %+v, want secret_gone and no token", got.token, se)
	}
	f.srv.svcGrant.mu.Lock()
	_, cached := f.srv.svcGrant.cache[f.uid]
	f.srv.svcGrant.mu.Unlock()
	if cached {
		t.Error("the token is still cached")
	}
}

func TestServiceGrant_RotateWhileMintPending_PG(t *testing.T) {
	f := newSvcFixWithSecret(t)
	release := make(chan struct{})
	f.idp.respond = func(n int, _ svcReq) svcAnswer {
		a := svcOK(n, 3600)
		if n == 1 {
			a.Block = release
		}
		return a
	}
	type res struct {
		tok serviceGrantMinted
		err error
	}
	out := make(chan res, 1)
	go func() { tok, err := f.token(serviceGrantTriggerResolve); out <- res{tok, err} }()
	for f.idp.count() == 0 {
		time.Sleep(time.Millisecond)
	}
	f.sec.set(f.secretName(), mustJSON(serviceGrantRecord{V: 1, TokenURL: svcTokenURL, ClientID: svcClientID, BaseURL: f.provider().BaseURL, ClientSecret: svcSecretTwo}))
	close(release)
	r := <-out
	if r.err != nil || r.tok.token != svcToken(2) {
		t.Fatalf("the caller got %q, %v, want the token minted under the new revision", r.tok.token, r.err)
	}
	if f.idp.count() != 2 {
		t.Fatalf("requests = %d, want 2", f.idp.count())
	}
	rows := f.mintRows()
	if len(rows) != 2 || !strings.Contains(string(rows[0].Data), `"superseded":true`) || strings.Contains(string(rows[1].Data), "superseded") {
		t.Fatalf("mint rows = %d; want two, the first marked superseded", len(rows))
	}
	row, _ := f.st.statusOf(f.uid, f.srv.svcGrant.replica())
	if row.Outcome != "ok" || row.ValidUntil == nil {
		t.Errorf("status = %+v, want the second mint's", row)
	}
	// The old token is never cached or returned, yet stays masked until it expires.
	f.srv.svcGrant.mu.Lock()
	cached := f.srv.svcGrant.cache[f.uid].token
	f.srv.svcGrant.mu.Unlock()
	if cached != svcToken(2) {
		t.Errorf("cached token = %q", cached)
	}
	if !f.masked(svcToken(1)) {
		t.Error("the superseded token is not masked")
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

func TestServiceGrant_RecordOverlapsNeverCrossDestinations_PG(t *testing.T) {
	f := newSvcFixWithSecret(t)
	// A mint held open while the secret is deleted, the address changed and a new secret written for it.
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
	f.serve(http.MethodDelete, f.secretPath(), nil)
	f.setRow(func(p *types.ModelProvider) { p.OIDC.ServiceGrant.TokenURL = "https://idp-b.example.test/token" })
	f.writeSecret(svcSecretTwo)
	close(release)
	<-out
	if _, err := f.token(serviceGrantTriggerResolve); err != nil {
		t.Fatalf("mint under the new destination: %v", err)
	}
	// Each request carried only the credential written for its own destination.
	for _, r := range f.idp.requests() {
		_, secret := svcBasic(t, r.Header.Get("Authorization"))
		switch {
		case strings.Contains(r.URL, "idp-b.") && secret != svcSecretTwo, strings.Contains(r.URL, "//idp.example.test") && secret != svcClientSecret:
			t.Errorf("request to %s carried %q: a credential crossed destinations", r.URL, secret)
		}
	}
	// Two overlapping writes of the route end with one whole record.
	g := newSvcFix(t)
	done := make(chan struct{}, 2)
	for _, v := range []string{svcSecretTwo, svcSecretThree} {
		go func() { g.serve(http.MethodPut, g.secretPath(), g.putSecretBody(v)); done <- struct{}{} }()
	}
	<-done
	<-done
	raw, _ := g.sec.Get(context.Background(), g.secretName())
	if rec, ok := parseServiceGrantRecord(raw); !ok || (rec.ClientSecret != svcSecretTwo && rec.ClientSecret != svcSecretThree) || rec.TokenURL != svcTokenURL {
		t.Fatalf("record after two overlapping writes = %s", raw)
	}
}

func TestServiceGrant_DelayedDeleteKeepsNewerRecord_PG(t *testing.T) {
	f := newSvcFix(t)
	uid := f.addClaim(0)
	name := serviceGrantSecretName(uid)
	// The earlier removal's delete runs with the revision it read; a record written since has another one.
	f.sec.set(name, mustJSON(serviceGrantRecord{V: 1, TokenURL: svcTokenURL, ClientID: svcClientID, BaseURL: "https://new.corp.example", ClientSecret: svcSecretTwo}))
	if res := f.retry(uid); res.Code != http.StatusConflict || !f.sec.has(name) {
		t.Fatalf("a delayed delete with the earlier revision: %d, record kept %v, want the newer record kept", res.Code, f.sec.has(name))
	}
}

func TestServiceGrant_DirectRotationOverlappingDeleteFailsClosed_PG(t *testing.T) {
	f := newSvcFixWithSecret(t)
	// A rotation written in the store while the grant's removal is deleting: the mint answers gone, or the
	// record is kept; no request ever reaches an identity provider with a value of the removed grant.
	f.sec.hold = func(op, _ string) {
		if op == "delete" {
			f.sec.hold = nil
			f.sec.set(f.secretName(), mustJSON(serviceGrantRecord{V: 1, TokenURL: svcTokenURL, ClientID: svcClientID, BaseURL: f.provider().BaseURL, ClientSecret: svcSecretTwo}))
		}
	}
	w := f.serve(http.MethodDelete, f.secretPath(), nil)
	if w.Code != http.StatusConflict && w.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s", w.Code, w.Body.String())
	}
	f.setRow(func(p *types.ModelProvider) { p.OIDC.ServiceGrant = nil })
	before := f.idp.count()
	if _, err := f.srv.serviceGrantToken(context.Background(), svcRowWithGrant(f.provider()), "", serviceGrantTriggerRefresh); err == nil {
		t.Fatal("a mint for a removed grant succeeded")
	}
	if f.idp.count() != before {
		t.Fatalf("a request reached the identity provider after the removal (%d new)", f.idp.count()-before)
	}
}

func TestServiceGrant_FailedRotationKeepsGrantMinting_PG(t *testing.T) {
	f := newSvcFixWithSecret(t)
	f.mustToken(serviceGrantTriggerRefresh)
	// A Put that fails leaves the old record: the next mint uses it.
	f.sec.putErr = errSvcDown
	if w := f.serve(http.MethodPut, f.secretPath(), f.putSecretBody(svcSecretTwo)); w.Code < 500 {
		t.Fatalf("a failing put answered %d", w.Code)
	}
	f.sec.putErr = nil
	f.srv.dropServiceGrantCache(f.uid)
	f.mustToken(serviceGrantTriggerResolve)
	reqs := f.idp.requests()
	if _, secret := svcBasic(t, reqs[len(reqs)-1].Header.Get("Authorization")); secret != svcClientSecret {
		t.Fatalf("after a failed rotation the mint sent %q, want the old record's value", secret)
	}
	// A Put whose value reached the store but whose row was not written (ErrRowNotWritten) leaves the new one live.
	f.sec.mu.Lock()
	f.sec.putErr = nil
	f.sec.mu.Unlock()
	f.sec.set(f.secretName(), mustJSON(serviceGrantRecord{V: 1, TokenURL: svcTokenURL, ClientID: svcClientID, BaseURL: f.provider().BaseURL, ClientSecret: svcSecretTwo}))
	f.mustToken(serviceGrantTriggerResolve)
	reqs = f.idp.requests()
	if _, secret := svcBasic(t, reqs[len(reqs)-1].Header.Get("Authorization")); secret != svcSecretTwo {
		t.Fatalf("with the new record live the mint sent %q", secret)
	}
	if !strings.Contains(reqs[len(reqs)-1].URL, "idp.example.test") {
		t.Errorf("the value was sent to %s, not its own destination", reqs[len(reqs)-1].URL)
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
