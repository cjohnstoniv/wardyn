// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The grant's secret record against a real Postgres store and router: the route's refusals, rotation, deletion
// and their overlaps with a mint, across one or two processes, with the scripted identity provider and the
// revision-keeping secret double. Guarded by WARDYN_TEST_PG (throwawayPGPool); skipped cleanly when unset.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// svcHoldFirst makes the identity provider hold its first answer until the returned release runs; release
// also runs at cleanup, so no request is left blocked.
func svcHoldFirst(t *testing.T, idp *svcIdP) func() {
	t.Helper()
	release := make(chan struct{})
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	t.Cleanup(free)
	idp.mu.Lock()
	idp.respond = func(n int, _ svcReq) svcAnswer {
		a := svcOK(n, 3600)
		if n == 1 {
			a.Block = release
		}
		return a
	}
	idp.mu.Unlock()
	return free
}

// removeGrant is DELETE .../service-grant as an admin, then a check that the stored grant is gone: a PUT that
// leaves the grant out keeps it, so only this route removes it.
func (f *svcPG) removeGrant(id string) {
	f.t.Helper()
	uid := f.uid(id)
	if w := f.do(http.MethodDelete, "/model-providers/"+id+"/service-grant", f.admin, nil); w.Code/100 != 2 {
		f.t.Fatalf("remove %s's grant: %d %s", id, w.Code, w.Body.String())
	}
	if p := f.row(id); (p.OIDC != nil && p.OIDC.ServiceGrant != nil) || slices.Contains(f.storedGrantUIDs(), uid) {
		f.t.Fatalf("%s's grant is still stored after its removal", id)
	}
}

func TestServiceGrant_SecretWriteRefusesChangedDestination_PG(t *testing.T) {
	f := newSvcPG(t, true)
	if w := f.put(svcPGRow("corp-a", true)); w.Code/100 != 2 {
		t.Fatalf("seed: %d %s", w.Code, w.Body.String())
	}
	name := f.secretNameOf("corp-a")
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
		body := f.secretBody("corp-a", svcClientSecret)
		c.edit(&body)
		w := f.do(http.MethodPut, f.secretPath("corp-a"), f.admin, body)
		if w.Code != http.StatusConflict || svcBody(t, w)["reason"] != reasonServiceGrantDestinationChanged || f.sec.has(name) {
			t.Fatalf("%s: %d %s, record stored %v; want 409 and nothing stored", c.name, w.Code, w.Body.String(), f.sec.has(name))
		}
		rows := f.audit.find("model_provider.service_grant.secret_write")
		if len(rows) != i+1 {
			t.Fatalf("%s: audit rows = %d, want %d, one new row per refusal", c.name, len(rows), i+1)
		}
		d := string(rows[len(rows)-1].Data)
		for _, want := range c.wants {
			if !strings.Contains(d, want) {
				t.Errorf("%s: its audit row %s lacks %q", c.name, d, want)
			}
		}
	}
	if w := f.writeSecret("corp-a", svcClientSecret); w.Code != http.StatusNoContent || !f.sec.has(name) {
		t.Fatalf("the right destination: %d %s", w.Code, w.Body.String())
	}
}

func TestServiceGrant_SecretRemovalOnOneReplicaEvictsOnAnother_PG(t *testing.T) {
	f := newSvcPG(t, true)
	f.seedOne()
	b := f.newReplica()
	ctx := context.Background()
	if _, err := f.tokenOf("corp-a", serviceGrantTriggerRefresh); err != nil {
		t.Fatal(err)
	}
	if _, err := b.tokenOf("corp-a", serviceGrantTriggerRefresh); err != nil {
		t.Fatal(err)
	}
	if !svcMasked(b.srv, svcClientSecret) {
		t.Fatal("B does not mask the secret after its mint")
	}
	// The secret's DELETE on A: B's next pass retires its mask, swept after the grace.
	if w := f.do(http.MethodDelete, f.secretPath("corp-a"), f.admin, nil); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE on A: %d %s", w.Code, w.Body.String())
	}
	b.srv.reconcileServiceGrantMasks(ctx)
	b.srv.cfg.MaskRegistry.SweepGlobals(time.Now().Add(24 * time.Hour))
	if svcMasked(b.srv, svcClientSecret) {
		t.Error("B still masks a secret deleted on A")
	}
	// Separately: the grant removed on A.
	if w := f.writeSecret("corp-a", svcClientSecret); w.Code != http.StatusNoContent {
		t.Fatalf("rewrite on A: %d %s", w.Code, w.Body.String())
	}
	if _, err := b.tokenOf("corp-a", serviceGrantTriggerRefresh); err != nil {
		t.Fatal(err)
	}
	f.removeGrant("corp-a")
	b.srv.reconcileServiceGrantMasks(ctx)
	b.srv.cfg.MaskRegistry.SweepGlobals(time.Now().Add(24 * time.Hour))
	if svcMasked(b.srv, svcClientSecret) {
		t.Error("B still masks the secret of a grant removed on A")
	}
}

func TestServiceGrant_SharedSecretRotationRemintsWithinATick_PG(t *testing.T) {
	f := newSvcPG(t, true)
	f.seedOne()
	b := f.newReplica()
	ctx := context.Background()
	f.srv.serviceGrantPass(ctx)
	b.srv.serviceGrantPass(ctx)
	if f.idp.count() != 1 || b.idp.count() != 1 {
		t.Fatalf("setup: %d and %d requests, want one each", f.idp.count(), b.idp.count())
	}
	if w := f.writeSecret("corp-a", svcSecretTwo); w.Code != http.StatusNoContent {
		t.Fatalf("rotation: %d %s", w.Code, w.Body.String())
	}
	b.srv.serviceGrantPass(ctx) // within one tick of the rotation
	reqs := b.idp.requests()
	if len(reqs) != 2 {
		t.Fatalf("B's requests = %d, want its pass to mint again", len(reqs))
	}
	if _, secret := svcBasic(t, reqs[1].Header.Get("Authorization")); secret != svcSecretTwo {
		t.Fatalf("B's new mint used %q, want the rotated secret", secret)
	}
}

func TestServiceGrant_SecretDeletedStopsServing_PG(t *testing.T) {
	f := newSvcPG(t, true)
	f.seedOne()
	if _, err := f.tokenOf("corp-a", serviceGrantTriggerRefresh); err != nil {
		t.Fatal(err)
	}
	if w := f.do(http.MethodDelete, f.secretPath("corp-a"), f.admin, nil); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE: %d %s", w.Code, w.Body.String())
	}
	got, err := f.tokenOf("corp-a", serviceGrantTriggerResolve)
	if se := svcErr(t, err); se.reason != svcReasonSecretGone || got.token != "" {
		t.Fatalf("after the delete: %q, %+v, want secret_gone and no token", got.token, se)
	}
	if _, cached := cachedOn(f.srv, f.uid("corp-a")); cached {
		t.Error("the token is still cached")
	}
}

// Plant: drop the revision case from serviceGrantSupersededBy; the held mint then publishes token 1 and the
// caller's check fails.
func TestServiceGrant_RotateWhileMintPending_PG(t *testing.T) {
	f := newSvcPG(t, true)
	f.seedOne()
	row := f.row("corp-a")
	ids := f.captureRowIDs()
	free := svcHoldFirst(t, f.idp)
	type res struct {
		tok serviceGrantMinted
		err error
	}
	out := make(chan res, 1)
	go func() {
		tok, err := f.srv.serviceGrantToken(context.Background(), row, "", serviceGrantTriggerResolve)
		out <- res{tok, err}
	}()
	waitFor(t, "the first token request", func() bool { return f.idp.count() > 0 })
	f.sec.set(f.secretNameOf("corp-a"), mustJSON(serviceGrantRecord{V: 1, TokenURL: svcTokenURL, ClientID: svcClientID, BaseURL: row.BaseURL, ClientSecret: svcSecretTwo}))
	free()
	var r res
	select {
	case r = <-out:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the held mint")
	}
	if r.err != nil || r.tok.token != svcToken(2) {
		t.Fatalf("the caller got %q, %v, want the token minted under the new revision", r.tok.token, r.err)
	}
	if f.idp.count() != 2 {
		t.Fatalf("requests = %d, want 2", f.idp.count())
	}
	got := ids()
	if len(got) != 2 {
		t.Fatalf("row ids = %v, want one per request (2)", got)
	}
	oldRows, newRows := f.rowsByID(got[0]), f.rowsByID(got[1])
	if len(oldRows) != 1 || svcRowFields(t, oldRows[0])["superseded_by"] != "revision" ||
		len(newRows) != 1 || strings.Contains(string(newRows[0].Data), "superseded") {
		t.Fatalf("mint rows = %v and %v; want one each, the first superseded by the revision", oldRows, newRows)
	}
	copies := f.awaitSink(t, got...)
	all := string(mustJSON(f.mintRows())) + string(mustJSON(copies))
	for _, leak := range []string{svcClientSecret, svcSecretTwo, svcToken(1), svcToken(2)} {
		if strings.Contains(all, leak) {
			t.Errorf("a mint row or its sink copy carries %q", leak)
		}
	}
	if st, ok := f.pgStatus("corp-a", f.srv.svcGrant.replica()); !ok || st.Outcome != "ok" || st.ValidUntil == nil {
		t.Errorf("status = %+v, %v, want the second mint's", st, ok)
	}
	// The old token is never cached or returned, yet stays masked until it expires.
	if c, _ := cachedOn(f.srv, row.UID); c.token != svcToken(2) {
		t.Errorf("cached token = %q", c.token)
	}
	if !svcMasked(f.srv, svcToken(1)) {
		t.Error("the superseded token is not masked")
	}
}

func TestServiceGrant_RecordOverlapsNeverCrossDestinations_PG(t *testing.T) {
	f := newSvcPG(t, true)
	f.seedOne()
	// A mint held open while the secret is deleted, the address changed and a new secret written for it.
	row := f.row("corp-a")
	free := svcHoldFirst(t, f.idp)
	out := make(chan struct{})
	go func() {
		defer close(out)
		_, _ = f.srv.serviceGrantToken(context.Background(), row, "", serviceGrantTriggerResolve)
	}()
	waitFor(t, "the first token request", func() bool { return f.idp.count() > 0 })
	if w := f.do(http.MethodDelete, f.secretPath("corp-a"), f.admin, nil); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE: %d %s", w.Code, w.Body.String())
	}
	moved := f.row("corp-a")
	moved.OIDC.ServiceGrant.TokenURL = "https://idp-b.example.test/token"
	if w := f.put(moved); w.Code/100 != 2 {
		t.Fatalf("change the token address: %d %s", w.Code, w.Body.String())
	}
	if w := f.writeSecret("corp-a", svcSecretTwo); w.Code != http.StatusNoContent {
		t.Fatalf("the new destination's secret: %d %s", w.Code, w.Body.String())
	}
	free()
	svcAwait(t, out, "the held mint")
	if _, err := f.tokenOf("corp-a", serviceGrantTriggerResolve); err != nil {
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
	g := newSvcPG(t, true)
	if w := g.put(svcPGRow("corp-a", true)); w.Code/100 != 2 {
		t.Fatalf("seed: %d %s", w.Code, w.Body.String())
	}
	done := make(chan int, 2)
	for _, v := range []string{svcSecretTwo, svcSecretThree} {
		body := g.secretBody("corp-a", v)
		go func() {
			done <- doSSO(t, g.srv, http.MethodPut, "/api/v1"+g.secretPath("corp-a"), g.admin, string(mustJSON(body))).Code
		}()
	}
	for range 2 {
		select {
		case code := <-done:
			if code != http.StatusNoContent {
				t.Fatalf("an overlapping write answered %d, want both stored", code)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for the overlapping writes")
		}
	}
	raw, _ := g.sec.Get(context.Background(), g.secretNameOf("corp-a"))
	if rec, ok := parseServiceGrantRecord(raw); !ok || (rec.ClientSecret != svcSecretTwo && rec.ClientSecret != svcSecretThree) || rec.TokenURL != svcTokenURL {
		t.Fatalf("record after two overlapping writes = %s", raw)
	}
}

func TestServiceGrant_DirectRotationOverlappingDeleteFailsClosed_PG(t *testing.T) {
	f := newSvcPG(t, true)
	f.seedOne()
	row := f.row("corp-a")
	name := f.secretNameOf("corp-a")
	// A rotation written in the store itself while the grant's removal deletes the record. It moves no revision,
	// so the removal's revision-guarded delete removes the rotated record too: that fails closed, the mint answers
	// gone, final and definitive, and no request reaches an identity provider.
	var fired atomic.Bool
	var revAtDelete string
	f.sec.mu.Lock()
	revBefore := f.sec.revOf(name)
	f.sec.hold = func(op, n string) {
		if op == "delete" && n == name && fired.CompareAndSwap(false, true) {
			f.sec.rotateInPlace(name, mustJSON(serviceGrantRecord{V: 1, TokenURL: svcTokenURL, ClientID: svcClientID, BaseURL: row.BaseURL, ClientSecret: svcSecretTwo}))
			f.sec.mu.Lock()
			revAtDelete = f.sec.revOf(name)
			f.sec.mu.Unlock()
		}
	}
	f.sec.mu.Unlock()
	before := f.idp.count()
	f.removeGrant("corp-a")
	if !fired.Load() {
		t.Fatal("the grant's removal never deleted the record: the rotation did not overlap it")
	}
	if revAtDelete != revBefore || revBefore == "" {
		t.Fatalf("the revision the delete saw = %q, want the stored %q: a rotation in the store moves none", revAtDelete, revBefore)
	}
	if f.sec.has(name) {
		t.Fatal("the rotated record outlived the removal's delete")
	}
	_, err := f.srv.serviceGrantToken(context.Background(), svcRowWithGrant(f.row("corp-a")), "", serviceGrantTriggerRefresh)
	var se *serviceGrantError
	if !errors.As(err, &se) || se.reason != svcReasonSecretGone || !se.definitive || !se.final || se.sent {
		t.Fatalf("the mint after the removal = %v (%+v), want a final, definitive %s that sent nothing", err, se, svcReasonSecretGone)
	}
	if f.idp.count() != before {
		t.Fatalf("a request reached the identity provider after the removal (%d new)", f.idp.count()-before)
	}
}

func TestServiceGrant_FailedRotationKeepsGrantMinting_PG(t *testing.T) {
	f := newSvcPG(t, true)
	f.seedOne()
	uid, name := f.uid("corp-a"), f.secretNameOf("corp-a")
	if _, err := f.tokenOf("corp-a", serviceGrantTriggerRefresh); err != nil {
		t.Fatal(err)
	}
	lastSecret := func() string {
		reqs := f.idp.requests()
		_, secret := svcBasic(t, reqs[len(reqs)-1].Header.Get("Authorization"))
		return secret
	}
	// A Put that fails leaves the old record: the next mint uses it.
	f.sec.mu.Lock()
	f.sec.putErr = errSvcDown
	f.sec.mu.Unlock()
	if w := f.writeSecret("corp-a", svcSecretTwo); w.Code < 500 {
		t.Fatalf("a failing put answered %d", w.Code)
	}
	f.sec.mu.Lock()
	f.sec.putErr = nil
	f.sec.mu.Unlock()
	f.srv.dropServiceGrantCache(uid)
	if _, err := f.tokenOf("corp-a", serviceGrantTriggerResolve); err != nil {
		t.Fatal(err)
	}
	if s := lastSecret(); s != svcClientSecret {
		t.Fatalf("after a failed rotation the mint sent %q, want the old record's value", s)
	}
	// The documented partial write: the value reaches the store and the row is not written. The route answers
	// the failure, and the value now live is masked at once, before any mint.
	f.sec.mu.Lock()
	f.sec.putPartial = true
	f.sec.mu.Unlock()
	sent := f.idp.count()
	if w := f.writeSecret("corp-a", svcSecretTwo); w.Code < 500 {
		t.Fatalf("a partial write answered %d, want the failure", w.Code)
	}
	f.sec.mu.Lock()
	f.sec.putPartial = false
	f.sec.mu.Unlock()
	if !svcMasked(f.srv, svcSecretTwo) || f.idp.count() != sent {
		t.Fatalf("after the partial write: new value masked %v, %d requests sent; want it masked before any mint",
			svcMasked(f.srv, svcSecretTwo), f.idp.count()-sent)
	}
	if raw, _ := f.sec.Get(context.Background(), name); !strings.Contains(string(raw), svcSecretTwo) {
		t.Fatal("setup: the partial write did not leave the new value in the store")
	}
	f.srv.dropServiceGrantCache(uid)
	if _, err := f.tokenOf("corp-a", serviceGrantTriggerResolve); err != nil {
		t.Fatal(err)
	}
	reqs := f.idp.requests()
	if s := lastSecret(); s != svcSecretTwo {
		t.Fatalf("with the new record live the mint sent %q", s)
	}
	if !strings.Contains(reqs[len(reqs)-1].URL, "idp.example.test") {
		t.Errorf("the value was sent to %s, not its own destination", reqs[len(reqs)-1].URL)
	}
}

// Plant: leave previous_valid_until out of a failure row's data; that row, read by its id once settled, then fails
// its check.
func TestServiceGrant_MintAuditRows_PG(t *testing.T) {
	clock := &svcClock{t: time.Now().UTC()}
	f := newSvcPGClocked(t, true, clock)
	f.seedOne()
	ids := f.captureRowIDs()
	if _, err := f.tokenOf("corp-a", serviceGrantTriggerRefresh); err != nil {
		t.Fatal(err)
	}
	// rowOf waits for the n-th request's row and returns it: a failure row settles after its attempt's answer.
	var copies []types.AuditEvent
	rowOf := func(n int) types.AuditEvent {
		t.Helper()
		got := ids()
		if len(got) <= n {
			t.Fatalf("row ids = %v, want at least %d", got, n+1)
		}
		f.awaitRows(t, got[n])
		copies = append(copies, f.awaitSink(t, got[n])...)
		return f.rowsByID(got[n])[0]
	}
	first := rowOf(0)
	if len(f.mintRows()) != 1 || first.Outcome != "success" || f.idp.count() != 1 {
		t.Fatalf("after one success: %d rows, %d requests", len(f.mintRows()), f.idp.count())
	}
	data := svcRowFields(t, first)
	if data["client_id"] != "svc-client" || data["trigger"] != "refresh" || data["expires_at"] == nil {
		t.Errorf("success row %s lacks client_id svc-client, trigger refresh or expires_at", first.Data)
	}
	if _, ok := data["previous_valid_until"]; ok {
		t.Errorf("a success row carries previous_valid_until: %s", first.Data)
	}
	f.idp.mu.Lock()
	f.idp.respond = func(int, svcReq) svcAnswer {
		return svcAnswer{Status: 400, Body: `{"error":"invalid_scope","error_description":"secret words svc-client-secret-0001-abcdefgh"}`}
	}
	f.idp.mu.Unlock()
	clock.Advance(55 * time.Minute)
	if _, err := f.tokenOf("corp-a", serviceGrantTriggerRefresh); err != nil {
		t.Fatalf("the cached token should cover the failure: %v", err)
	}
	second := rowOf(1)
	clock.Advance(10 * time.Minute) // expired: the next failure holds no earlier token
	_, _ = f.tokenOf("corp-a", serviceGrantTriggerRefresh)
	third := rowOf(2)
	if n := len(f.mintRows()); n != 3 || f.idp.count() != 3 {
		t.Fatalf("rows = %d, requests = %d, want one row per request sent (3)", n, f.idp.count())
	}
	if d := svcRowFields(t, second); second.Outcome != "failure" || d["reason"] != "invalid_scope" || d["oauth_error"] != "invalid_scope" ||
		d["http_status"] != float64(400) || d["client_id"] != "svc-client" || d["previous_valid_until"] == nil {
		t.Errorf("failure row with a live earlier token = %s", second.Data)
	}
	if _, ok := svcRowFields(t, third)["previous_valid_until"]; ok {
		t.Errorf("a failure row after the token expired names previous_valid_until: %s", third.Data)
	}
	f.idp.mu.Lock()
	f.idp.respond = func(int, svcReq) svcAnswer { return svcAnswer{Status: 400, Body: `{"error":"made_up_code_xyz"}`} }
	f.idp.mu.Unlock()
	clock.Advance(2 * time.Minute)
	_, _ = f.tokenOf("corp-a", serviceGrantTriggerRefresh)
	fourth := rowOf(3)
	if n := len(f.mintRows()); n != 4 {
		t.Fatalf("rows = %d, want 4", n)
	}
	f.audit.mu.Lock()
	all := string(mustJSON(f.audit.rows)) + string(mustJSON(f.mintRows())) + string(mustJSON(copies))
	f.audit.mu.Unlock()
	for _, leak := range []string{svcClientSecret, svcToken(1), "secret words", "made_up_code_xyz", "error_description"} {
		if strings.Contains(all, leak) {
			t.Errorf("an audit row carries %q", leak)
		}
	}
	if last := svcRowFields(t, fourth); last["oauth_error"] != "other" || last["reason"] != svcReasonRefused {
		t.Errorf("an unrecognised error code was not recorded as other, refused: %s", fourth.Data)
	}
}

// svcRowFields parses an audit row's data: Postgres hands jsonb back in its own spacing, so a row is compared by
// field, not by its text.
func svcRowFields(t *testing.T, e types.AuditEvent) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(e.Data, &m); err != nil {
		t.Fatalf("row data %s: %v", e.Data, err)
	}
	return m
}
