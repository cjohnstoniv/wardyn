// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// What the review rounds found on the service grant's resolve, retirement and failure paths, each pinned by
// the interleaving that exposed it.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// svcResolve dispatches the fixture's run once and resolves as the service owner.
func (f *svcFix) svcResolve(plan dispatchLLMPlan) (int, map[string]any) {
	in := f.request(plan)
	in.sub = svcOwner
	w, _ := f.resolveWith(in, "")
	return w.Code, svcBody(f.t, w)
}

// putClaim stores a clean-up claim on uid for the record that is in the secret store now.
func (f *svcFix) putClaim(uid string) store.ServiceGrantCleanup {
	f.t.Helper()
	f.sec.mu.Lock()
	rev := f.sec.revOf(serviceGrantSecretName(uid))
	f.sec.mu.Unlock()
	c := store.ServiceGrantCleanup{ID: uuid.NewString(), ProviderID: "gone", Reason: cleanupReasonGrantRemoved, Revision: rev, Since: f.clock.Now()}
	f.st.gmu.Lock()
	if f.st.cleanups == nil {
		f.st.cleanups = map[string]store.ServiceGrantCleanup{}
	}
	f.st.cleanups[uid] = c
	f.st.gmu.Unlock()
	return c
}

func (f *svcFix) dropClaim(uid string) {
	f.st.gmu.Lock()
	delete(f.st.cleanups, uid)
	f.st.gmu.Unlock()
}

func (f *svcFix) cached() bool {
	f.srv.svcGrant.mu.Lock()
	defer f.srv.svcGrant.mu.Unlock()
	_, ok := f.srv.svcGrant.cache[f.uid]
	return ok
}

// CX-364: the owner is removed while a renewal fails transiently. The failed renewal still repeats the
// owner check, so the answer is the 403 that makes the proxy drop its last good header, not a 503 it rides out.
func TestServiceGrant_OwnerRemovedDuringFailedRenewalWithdraws(t *testing.T) {
	f := newSvcFixWithSecret(t)
	plan, _, _, ok := f.dispatch(t)
	if !ok {
		t.Fatal("dispatch refused")
	}
	if code, body := f.svcResolve(plan); code != http.StatusOK {
		t.Fatalf("first resolve: %d %v", code, body)
	}
	f.clock.Advance(55 * time.Minute) // inside the reuse margin: the next resolve renews
	f.idp.respond = func(int, svcReq) svcAnswer {
		f.setRow(func(p *types.ModelProvider) {
			g := *p.OIDC.ServiceGrant
			g.ServiceIdentities = []string{"service:someone-else"}
			o := *p.OIDC
			o.ServiceGrant = &g
			p.OIDC = &o
		})
		return svcAnswer{Status: 503, Body: `{}`}
	}
	code, body := f.svcResolve(plan)
	if code != http.StatusForbidden || body["reason"] != reasonServiceGrantNotListed {
		t.Fatalf("resolve = %d %v, want 403 %s", code, body, reasonServiceGrantNotListed)
	}
	if strings.Contains(f.auditAll(), svcToken(1)) {
		t.Error("the last good token reached the audit")
	}
}

// CX-364: a configuration that cannot be read, alone, withdraws nothing: 503 on the resolve, and no 403
// anywhere on the mint path.
func TestServiceGrant_ConfigReadErrorDoesNotWithdraw(t *testing.T) {
	f := newSvcFixWithSecret(t)
	plan, _, _, ok := f.dispatch(t)
	if !ok {
		t.Fatal("dispatch refused")
	}
	if code, body := f.svcResolve(plan); code != http.StatusOK {
		t.Fatalf("first resolve: %d %v", code, body)
	}
	f.clock.Advance(55 * time.Minute)
	var broken atomic.Bool
	f.st.getErr = func() error {
		if broken.Load() {
			return errSvcDown
		}
		return nil
	}
	f.idp.respond = func(int, svcReq) svcAnswer { broken.Store(true); return svcAnswer{Status: 503, Body: `{}`} }
	code, body := f.svcResolve(plan)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("resolve with the configuration unreadable = %d %v, want 503", code, body)
	}
	// The mint path: a failure whose re-read cannot read the row is a retryable unavailable, not drift.
	broken.Store(true)
	_, err := f.token(serviceGrantTriggerRefresh)
	var se *serviceGrantError
	if errors.Is(err, errServiceGrantDrift) || !errors.As(err, &se) || se.definitive || se.reason != svcReasonUnavailable {
		t.Fatalf("token with the configuration unreadable = %v, want a retryable unavailable", err)
	}
	broken.Store(false)
	f.idp.respond = nil
	if _, err := f.token(serviceGrantTriggerRefresh); err != nil {
		t.Fatalf("after the read recovers: %v", err)
	}
}

// CX-372: a replacement grant on a uid whose predecessor is not yet retired serves nothing from the old
// generation on any replica, whether the cleanup is held or has failed; it serves again only after the claim is
// settled and a fresh secret is written.
func TestServiceGrant_ReplacementUnavailableUntilPredecessorRetired(t *testing.T) {
	f := newSvcFixWithSecret(t)
	b := f.replica()
	f.mustToken(serviceGrantTriggerRefresh)
	if _, err := b.serviceGrantToken(context.Background(), f.provider(), "", serviceGrantTriggerRefresh); err != nil {
		t.Fatal(err)
	}
	before := f.idp.count()
	f.putClaim(f.uid) // the door committed the replacement with the predecessor's claim
	for name, srv := range map[string]*Server{"replica A": f.srv, "replica B": b} {
		got, err := srv.serviceGrantToken(context.Background(), f.provider(), "", serviceGrantTriggerResolve)
		var se *serviceGrantError
		if !errors.As(err, &se) || !se.definitive || got.token != "" {
			t.Fatalf("%s with the cleanup held: %q, %v, want a definitive refusal and no token", name, got.token, err)
		}
	}
	// The cleanup fails (the delete errors): the claim stays and nothing is served.
	f.sec.delErr = errSvcDown
	if out := f.srv.serviceGrantCleanupAttempt(context.Background(), f.uid); out == cleanupSettled {
		t.Fatalf("a failed delete settled the claim")
	}
	f.sec.delErr = nil
	if got, err := b.serviceGrantToken(context.Background(), f.provider(), "", serviceGrantTriggerResolve); err == nil || got.token != "" {
		t.Fatalf("replica B with the cleanup failed served %q", got.token)
	}
	if f.idp.count() != before {
		t.Fatalf("a request reached the identity provider while the predecessor was retiring: %d new", f.idp.count()-before)
	}
	// The cleanup succeeds; the record is gone, so the grant stays unavailable until a fresh secret is written.
	if out := f.srv.serviceGrantCleanupAttempt(context.Background(), f.uid); out != cleanupSettled {
		t.Fatalf("cleanup = %v, want settled", out)
	}
	if _, err := f.token(serviceGrantTriggerResolve); err == nil || f.idp.count() != before {
		t.Fatalf("after settlement and before a fresh secret: %v, %d requests", err, f.idp.count()-before)
	}
	f.writeSecret(svcSecretTwo)
	if _, err := f.token(serviceGrantTriggerResolve); err != nil {
		t.Fatalf("with a fresh secret: %v", err)
	}
}

// CX-372: a claim committed while a renewal fails. (a) with a usable cached token and (b) without one, the
// answer is the definitive 424 refusal, and the cached token is dropped.
func TestServiceGrant_ClaimCommittedDuringFailedRenewalIsDefinitive(t *testing.T) {
	for _, withCache := range []bool{true, false} {
		f := newSvcFixWithSecret(t)
		plan, _, _, ok := f.dispatch(t)
		if !ok {
			t.Fatal("dispatch refused")
		}
		if withCache {
			if code, body := f.svcResolve(plan); code != http.StatusOK {
				t.Fatalf("first resolve: %d %v", code, body)
			}
			f.clock.Advance(55 * time.Minute)
		} else {
			f.srv.dropServiceGrantCache(f.uid)
		}
		f.idp.respond = func(int, svcReq) svcAnswer {
			f.putClaim(f.uid)
			return svcAnswer{Status: 503, Body: `{}`}
		}
		code, body := f.svcResolve(plan)
		if code != http.StatusFailedDependency || body["reason"] != reasonServiceGrantRefused {
			t.Fatalf("withCache=%v: resolve = %d %v, want 424 %s", withCache, code, body, reasonServiceGrantRefused)
		}
		if f.cached() {
			t.Errorf("withCache=%v: the old generation's token is still cached", withCache)
		}
	}
}

// CX-368: the final locked step compares the claim's identity. A claim that another attempt settled, and a
// secret written since, is no pending claim; a replacement claim is never settled or audited as its predecessor.
func TestServiceGrant_CleanupSettlesOnlyItsOwnClaim(t *testing.T) {
	ctx := context.Background()
	// (a) Another attempt settles the claim during this attempt's unlocked delete, and a new secret is written.
	f := newSvcFix(t)
	uid := f.addClaim(0)
	name := serviceGrantSecretName(uid)
	f.sec.hold = func(op, _ string) {
		if op == "delete" {
			f.sec.hold = nil
			f.dropClaim(uid)
			f.sec.set(name, mustJSON(serviceGrantRecord{V: 1, TokenURL: svcTokenURL, ClientID: svcClientID, BaseURL: "https://new.corp.example", ClientSecret: svcSecretTwo}))
		}
	}
	if out := f.srv.serviceGrantCleanupAttempt(ctx, uid); out != cleanupSettled {
		t.Fatalf("(a) outcome = %v, want settled with nothing pending", out)
	}
	if !f.sec.has(name) || len(f.audit.find("model_provider.service_grant.secret_delete")) != 0 || f.st.settles != 0 {
		t.Fatalf("(a) the replacement record survives %v, secret_delete rows %d, settles %d; want kept, 0, 0",
			f.sec.has(name), len(f.audit.find("model_provider.service_grant.secret_delete")), f.st.settles)
	}
	// (b) A replacement claim on the same uid is written across the unlocked delete, and its record has gone.
	g := newSvcFix(t)
	uid = g.addClaim(0)
	name = serviceGrantSecretName(uid)
	var replacement store.ServiceGrantCleanup
	g.sec.hold = func(op, _ string) {
		if op == "delete" {
			g.sec.hold = nil
			replacement = g.putClaim(uid)
		}
	}
	if out := g.srv.serviceGrantCleanupAttempt(ctx, uid); out != cleanupKept {
		t.Fatalf("(b) outcome = %v, want kept: the claim is not this attempt's", out)
	}
	if got := g.claims()[uid]; got.ID != replacement.ID || g.st.settles != 0 || len(g.audit.find("model_provider.service_grant.secret_delete")) != 0 {
		t.Fatalf("(b) claim %q (want %q), settles %d, secret_delete rows %d; want the replacement kept and unaudited",
			got.ID, replacement.ID, g.st.settles, len(g.audit.find("model_provider.service_grant.secret_delete")))
	}
}

// CX-374: an old failure that finishes after a newer success records nothing, in memory or in the status row,
// and the refresher does not see a failure come back.
func TestServiceGrant_OldFailureAfterNewerSuccessIsDropped(t *testing.T) {
	f := newSvcFixWithSecret(t)
	release := make(chan struct{})
	f.idp.respond = func(n int, _ svcReq) svcAnswer {
		if n == 1 {
			return svcAnswer{Status: 503, Body: `{}`, Block: release} // the older attempt, held
		}
		return svcAnswer{}
	}
	old := make(chan error, 2)
	go func() { _, err := f.token(serviceGrantTriggerRefresh); old <- err }()
	for f.idp.count() == 0 {
		time.Sleep(time.Millisecond)
	}
	f.writeSecret(svcSecretTwo) // a different revision: a separate flight, and a newer attempt
	f.mustToken(serviceGrantTriggerRefresh)
	close(release)
	<-old
	f.srv.svcGrant.mu.Lock()
	_, failed := f.srv.svcGrant.failed[f.uid]
	f.srv.svcGrant.mu.Unlock()
	row, _ := f.st.statusOf(f.uid, f.srv.svcGrant.replica())
	if failed || row.Outcome != "ok" {
		t.Fatalf("failure recorded = %v, status %q; want none and ok", failed, row.Outcome)
	}
	if f.srv.serviceGrantDue(context.Background(), f.provider()) {
		t.Error("the refresher still sees the row as due")
	}
}

// CX-374: concurrent waiters on one failing flight record one failure and one status write.
func TestServiceGrant_FailedFlightRecordsOneFailure(t *testing.T) {
	f := newSvcFixWithSecret(t)
	release := make(chan struct{})
	f.idp.respond = func(int, svcReq) svcAnswer { return svcAnswer{Status: 503, Body: `{}`, Block: release} }
	var writes atomic.Int32
	f.st.upsertGate = func(r store.ServiceGrantStatusRow) {
		if r.Outcome != "ok" {
			writes.Add(1)
		}
	}
	done := make(chan struct{}, 4)
	for range 4 {
		go func() { _, _ = f.token(serviceGrantTriggerResolve); done <- struct{}{} }()
	}
	for f.idp.count() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	for range 4 {
		<-done
	}
	if f.idp.count() != 1 || writes.Load() != 1 {
		t.Fatalf("requests %d, failure status writes %d; want one flight and one write", f.idp.count(), writes.Load())
	}
}
