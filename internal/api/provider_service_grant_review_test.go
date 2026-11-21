// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// What the review rounds found on the service grant's resolve, retirement and failure paths, each pinned by
// the interleaving that exposed it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"

	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
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

// The owner is removed while a renewal fails transiently. The failed renewal still repeats the
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

// A configuration that cannot be read, alone, withdraws nothing: 503 on the resolve, and no 403
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

// A replacement grant on a uid whose predecessor is not yet retired serves nothing from the old
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

// A claim committed while a renewal fails. (a) with a usable cached token and (b) without one, the
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

// The final locked step compares the claim's identity. A claim that another attempt settled, and a
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

// An old failure that finishes after a newer success records nothing, in memory or in the status row,
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

// Concurrent waiters on one failing flight record one failure and one status write.
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

// Mixed outcomes: a pending claim that was read is decisive evidence against the old credential, and
// an unreadable roster beside it, at settlement or at the final resolve, changes nothing about the answer.
func TestServiceGrant_ConfirmedWithdrawalSurvivesAFailedRead(t *testing.T) {
	// (a) The claim read finds the claim, the roster read fails: the settlement is definitive.
	f := newSvcFixWithSecret(t)
	f.mustToken(serviceGrantTriggerRefresh)
	f.clock.Advance(55 * time.Minute)
	var broken atomic.Bool
	f.st.getErr = func() error {
		if broken.Load() {
			return errSvcDown
		}
		return nil
	}
	f.idp.respond = func(int, svcReq) svcAnswer {
		f.putClaim(f.uid)
		broken.Store(true)
		return svcAnswer{Status: 503, Body: `{}`}
	}
	got, err := f.token(serviceGrantTriggerRefresh)
	var se *serviceGrantError
	if !errors.As(err, &se) || !se.definitive || got.token != "" || f.cached() {
		t.Fatalf("(a) token = %q, %v, cached %v; want the definitive refusal and nothing cached", got.token, err, f.cached())
	}
	// (b) A definitive token result, then an unreadable final row and run: the resolve stays definitive.
	g := newSvcFixWithSecret(t)
	plan, _, _, ok := g.dispatch(t)
	if !ok {
		t.Fatal("dispatch refused")
	}
	g.srv.dropServiceGrantCache(g.uid)
	var down atomic.Bool
	g.st.getErr = func() error {
		if down.Load() {
			return errSvcDown
		}
		return nil
	}
	g.idp.respond = func(int, svcReq) svcAnswer {
		g.putClaim(g.uid)
		down.Store(true)
		return svcOAuthError(401, "invalid_client")
	}
	if code, body := g.svcResolve(plan); code != http.StatusFailedDependency || body["reason"] != reasonServiceGrantRefused {
		t.Fatalf("(b) resolve = %d %v, want 424 %s", code, body, reasonServiceGrantRefused)
	}
	// (c) Control: only an unreadable roster answers 503.
	h := newSvcFixWithSecret(t)
	plan, _, _, _ = h.dispatch(t)
	h.clock.Advance(55 * time.Minute)
	var off atomic.Bool
	h.st.getErr = func() error {
		if off.Load() {
			return errSvcDown
		}
		return nil
	}
	h.idp.respond = func(int, svcReq) svcAnswer { off.Store(true); return svcAnswer{Status: 503, Body: `{}`} }
	if code, body := h.svcResolve(plan); code != http.StatusServiceUnavailable {
		t.Fatalf("(c) resolve = %d %v, want 503", code, body)
	}
}

// C: a disable, or a change of the lane's auth format, while a renewal is failing is a 403 refusal, and the
// last good header is not kept.
func TestServiceGrant_DisableAndFormatChangeDuringRenewalRefused(t *testing.T) {
	for name, edit := range map[string]func(*types.ModelProvider){
		"disabled": func(p *types.ModelProvider) { p.Disabled = true },
		"auth format": func(p *types.ModelProvider) {
			p.Auth = &types.ProviderAuth{Header: "Authorization", Format: "Token %s"}
		},
	} {
		f := newSvcFixWithSecret(t)
		plan, _, _, ok := f.dispatch(t)
		if !ok {
			t.Fatal("dispatch refused")
		}
		if code, _ := f.svcResolve(plan); code != http.StatusOK {
			t.Fatalf("%s: first resolve %d", name, code)
		}
		f.clock.Advance(55 * time.Minute)
		f.idp.respond = func(int, svcReq) svcAnswer {
			f.setRow(edit)
			return svcAnswer{Status: 503, Body: `{}`}
		}
		code, body := f.svcResolve(plan)
		if code != http.StatusForbidden {
			t.Fatalf("%s: resolve during a failing renewal = %d %v, want 403", name, code, body)
		}
		if v, _ := body["value"].(string); v != "" {
			t.Errorf("%s: a header came back with the refusal", name)
		}
	}
}

// J: a paced answer from a failure is a plain nil error when it serves a live cached token, never a typed nil.
func TestServiceGrant_PacedAnswerIsAPlainNilError(t *testing.T) {
	f := newSvcFixWithSecret(t)
	f.mustToken(serviceGrantTriggerRefresh)
	f.clock.Advance(55 * time.Minute)
	f.idp.respond = func(int, svcReq) svcAnswer { return svcAnswer{Status: 503, Body: `{}`} }
	for i, trigger := range []string{serviceGrantTriggerRefresh, serviceGrantTriggerResolve, serviceGrantTriggerDispatch} {
		tok, err := f.token(trigger)
		if err != nil || tok.token != svcToken(1) {
			t.Fatalf("call %d (%s): %q, %v; want the live cached token and an untyped nil error", i, trigger, tok.token, err)
		}
	}
	if n := f.idp.count(); n != 2 {
		t.Fatalf("requests = %d, want the first mint and one failed renewal; the later calls are paced", n)
	}
}

// L: a status write that is held does not delay another provider's pass or the heartbeat.
func TestServiceGrant_HeldStatusWriteDelaysNoOtherProvider(t *testing.T) {
	f := newSvcFixWithSecret(t)
	uid2 := f.addSecondProvider()
	release := make(chan struct{})
	f.st.upsertGate = func(r store.ServiceGrantStatusRow) {
		if r.ProviderUID == f.uid {
			<-release
		}
	}
	go f.srv.serviceGrantPass(context.Background())
	deadline := time.Now().Add(5 * time.Second)
	for {
		if row, ok := f.st.statusOf(uid2, f.srv.svcGrant.replica()); ok && row.Outcome == "ok" {
			break
		}
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("the healthy provider's status was not written while another's status write was held")
		}
		time.Sleep(5 * time.Millisecond)
	}
	hb := make(chan struct{})
	go func() { f.srv.serviceGrantHeartbeat(context.Background()); close(hb) }()
	select {
	case <-hb:
	case <-time.After(2 * time.Second):
		t.Error("the heartbeat waited for a held status write")
	}
	close(release)
}

// addSecondProvider adds a second grant row with its own secret and returns its uid.
func (f *svcFix) addSecondProvider() string {
	f.t.Helper()
	f.site.mu.Lock()
	rows := append([]types.ModelProvider(nil), f.site.sc.ModelProviders.Providers...)
	p := rows[0]
	o := *p.OIDC
	p.ID, p.Name, p.UID, p.OIDC = "second", "second", uuid.NewString(), &o
	p.BaseURL = "https://second.corp.example"
	rows = append(rows, p)
	f.site.sc.ModelProviders = &types.ModelProviders{Providers: rows}
	f.site.mu.Unlock()
	raw := mustJSON(serviceGrantRecord{V: 1, TokenURL: svcTokenURL, ClientID: svcClientID, BaseURL: p.BaseURL, ClientSecret: svcSecretTwo})
	f.sec.set(serviceGrantSecretName(p.UID), raw)
	return p.UID
}

// M: a scheduled service run whose grant deteriorates between scheduling and promotion meets the grant's own
// refusal at its start; it is not held to wait for a person's sign-in. A person's expired sign-in still waits.
func TestServiceGrant_ScheduledRunDeteriorationIsNotASignInWait(t *testing.T) {
	t.Setenv(envScheduledRuns, "true")
	f := newSvcFixWithSecret(t)
	at := f.clock.Now().Add(time.Hour)
	launch := func() queuedLaunch {
		req, _ := json.Marshal(createRunRequest{Agent: "claude-code"})
		return queuedLaunch{Request: req, ProviderID: f.provider().ID}
	}
	entry := func(owner string) store.RunQueueEntry { return store.RunQueueEntry{Owner: owner, StartAt: &at} }
	ctx := context.Background()
	// The grant's mint fails definitively after the run was scheduled.
	f.idp.respond = func(int, svcReq) svcAnswer { return svcOAuthError(401, "invalid_client") }
	f.token(serviceGrantTriggerRefresh) //nolint:errcheck // the failure is the point
	if got := f.srv.queueOwnerSignIn(ctx, entry(svcOwner), launch()); got != "" {
		t.Fatalf("a service run with a failing grant waits as %q, want no sign-in wait", got)
	}
	// Control: a person with no sign-in waits for it.
	if got := f.srv.queueOwnerSignIn(ctx, entry(oidcPersonSub), launch()); got != runnerpool.WaitOwnerSignIn {
		t.Fatalf("a person with no sign-in waits as %q, want %q", got, runnerpool.WaitOwnerSignIn)
	}
}

// The existing token callers are unchanged by the shared request: an ID token comes back, a changed refresh
// token is returned as the new one, and a consent refusal is classified from error_description alone.
func TestPostToken_ExistingCallersUnchanged(t *testing.T) {
	answer := ""
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, answer)
	}))
	defer ts.Close()
	s := &Server{cfg: Config{}}
	ta := tokenAuthority{tokenURL: ts.URL, clientID: "c", clientSecret: "s"}
	form := func() url.Values {
		return url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"old-refresh"}}
	}

	answer = `{"access_token":"at-0123456789","id_token":"idt-0123456789","refresh_token":"new-refresh","expires_in":3600,"scope":"openid api.read"}`
	out, err := s.postToken(context.Background(), ta, form())
	if err != nil || out.IDToken != "idt-0123456789" || out.RefreshToken != "new-refresh" || out.Scope != "openid api.read" {
		t.Fatalf("postToken = %+v, %v; want the ID token, the changed refresh token and the scope kept", out, err)
	}
	answer = `{"error":"invalid_grant","error_description":"AADSTS65001: The user or administrator has not consented"}`
	if _, err = s.postToken(context.Background(), ta, form()); !errors.Is(err, ErrADOEntraConsentRequired) {
		t.Fatalf("a consent refusal named only in error_description = %v, want ErrADOEntraConsentRequired", err)
	}
	answer = `{"error":"invalid_grant","error_description":"expired"}`
	if _, err = s.postToken(context.Background(), ta, form()); !errors.Is(err, ErrADOEntraDeadCredential) {
		t.Fatalf("invalid_grant = %v, want a dead credential", err)
	}
	answer = `{"error":"invalid_client"}`
	if _, err = s.postToken(context.Background(), ta, form()); !errors.Is(err, ErrADOEntraUnavailable) {
		t.Fatalf("invalid_client = %v, want unavailable, the sign-ins kept", err)
	}
}

// Every combination: one outcome per attempt, the strongest of what was read. A withdrawal seen with
// authority (a pending claim, a gone secret, a removed row) is final whatever else failed to read; an
// unreadable read with no withdrawal beside it is transient; with neither the attempt's own result stands.
func TestServiceGrant_OutcomeIsTheStrongestOfTheReads(t *testing.T) {
	type read int
	const (
		fine read = iota
		withdrawn
		unreadable
	)
	for _, upstream := range []string{"success", "transient"} {
		for _, claim := range []read{fine, withdrawn, unreadable} {
			for _, secret := range []read{fine, withdrawn, unreadable} {
				for _, roster := range []read{fine, withdrawn, unreadable} {
					name := fmt.Sprintf("upstream=%s claim=%d secret=%d roster=%d", upstream, claim, secret, roster)
					f := newSvcFixWithSecret(t)
					f.mustToken(serviceGrantTriggerRefresh)
					f.clock.Advance(55 * time.Minute) // the cached token is due, and still serves as the fallback
					var flip atomic.Bool
					f.st.getErr = func() error {
						if flip.Load() && roster == unreadable {
							return errSvcDown
						}
						return nil
					}
					f.st.docErr = func() error {
						if flip.Load() && claim == unreadable {
							return errSvcDown
						}
						return nil
					}
					f.idp.respond = func(n int, _ svcReq) svcAnswer {
						flip.Store(true)
						if claim == withdrawn {
							f.putClaim(f.uid)
						}
						switch secret {
						case withdrawn:
							f.sec.mu.Lock()
							delete(f.sec.m, f.secretName())
							f.sec.rev[f.secretName()]++
							f.sec.mu.Unlock()
						case unreadable:
							f.sec.mu.Lock()
							f.sec.revErr = errSvcDown
							f.sec.mu.Unlock()
						}
						if roster == withdrawn {
							f.setRow(func(p *types.ModelProvider) { p.Disabled = true })
						}
						if upstream == "transient" {
							return svcAnswer{Status: 503, Body: `{}`}
						}
						return svcAnswer{}
					}
					got, err := f.token(serviceGrantTriggerRefresh)
					var se *serviceGrantError
					decisive := errors.Is(err, errServiceGrantDrift) || (errors.As(err, &se) && se.definitive)
					wantDecisive := claim == withdrawn || secret == withdrawn || roster == withdrawn
					anyUnreadable := claim == unreadable || secret == unreadable || roster == unreadable
					switch {
					case wantDecisive:
						if !decisive || got.token != "" {
							t.Errorf("%s: %q, %v; want the definitive refusal", name, got.token, err)
						}
						if (claim == withdrawn || secret == withdrawn) && f.cached() && roster != withdrawn {
							t.Errorf("%s: the withdrawn credential's cache entry survived", name)
						}
					case anyUnreadable:
						if err == nil || decisive || got.token != "" {
							t.Errorf("%s: %q, %v; want a transient refusal and no token", name, got.token, err)
						}
					default:
						if err != nil || got.token == "" {
							t.Errorf("%s: %q, %v; want a token", name, got.token, err)
						}
					}
				}
			}
		}
	}
}

// The successful upstream path: the claim is seen at publication; the settlement's later reads fail.
// The withdrawal is final, the cache is evicted, and the resolve answers the definitive refusal, so the proxy
// stops serving its previous header.
func TestServiceGrant_WithdrawalSeenAtPublicationSurvivesFailedConfirmationReads(t *testing.T) {
	f := newSvcFixWithSecret(t)
	plan, _, _, ok := f.dispatch(t)
	if !ok {
		t.Fatal("dispatch refused")
	}
	if code, _ := f.svcResolve(plan); code != http.StatusOK {
		t.Fatalf("first resolve: %d", code)
	}
	f.clock.Advance(55 * time.Minute) // a warm previous header, due for renewal
	var flip atomic.Bool
	var reads atomic.Int32
	f.st.docErr = func() error {
		if flip.Load() && reads.Add(1) > 1 {
			return errSvcDown // the first read after the upstream answer finds the claim; later ones fail
		}
		return nil
	}
	f.st.getErr = func() error {
		if flip.Load() {
			return errSvcDown
		}
		return nil
	}
	f.idp.respond = func(int, svcReq) svcAnswer {
		f.putClaim(f.uid)
		flip.Store(true)
		return svcAnswer{} // the upstream succeeds
	}
	code, body := f.svcResolve(plan)
	if code != http.StatusFailedDependency || body["reason"] != reasonServiceGrantRefused {
		t.Fatalf("resolve = %d %v, want the definitive 424 %s", code, body, reasonServiceGrantRefused)
	}
	if f.cached() {
		t.Error("the withdrawn credential is still cached")
	}
	// The empty revision seen at publication stands when the other reads fail.
	g := newSvcFixWithSecret(t)
	g.mustToken(serviceGrantTriggerRefresh)
	g.clock.Advance(55 * time.Minute)
	var on atomic.Bool
	g.st.getErr = func() error {
		if on.Load() {
			return errSvcDown
		}
		return nil
	}
	g.st.docErr = g.st.getErr
	g.idp.respond = func(int, svcReq) svcAnswer {
		on.Store(true)
		g.sec.mu.Lock()
		delete(g.sec.m, g.secretName())
		g.sec.rev[g.secretName()]++
		g.sec.mu.Unlock()
		return svcAnswer{}
	}
	got, err := g.token(serviceGrantTriggerRefresh)
	var se *serviceGrantError
	if !errors.As(err, &se) || !se.definitive || se.reason != svcReasonSecretGone || got.token != "" || g.cached() {
		t.Fatalf("empty revision at publication = %q, %v, cached %v; want the final secret_gone", got.token, err, g.cached())
	}
}

// The record is gone at its read, and every read after it is inconclusive: the roster cannot be read, and the
// revision either fails or is not kept by the store. The absence the record read saw is the verdict: no
// request is sent, the cached token is dropped, and the proxy is refused with the definitive 424.
func TestServiceGrant_RecordGoneAtReadStaysFinalOverInconclusiveRereads(t *testing.T) {
	for _, noRev := range []bool{false, true} {
		t.Run(fmt.Sprintf("noRevision=%v", noRev), func(t *testing.T) {
			f := newSvcFixWithSecret(t)
			f.sec.noRev = noRev
			plan, _, _, ok := f.dispatch(t)
			if !ok || !f.cached() {
				t.Fatalf("dispatch ok=%v cached=%v; want a minted, cached token", ok, f.cached())
			}
			sent := f.idp.count()
			f.clock.Advance(55 * time.Minute) // under the reuse margin, so the resolve reads the record again
			var armed, broken atomic.Bool
			armed.Store(true)
			f.st.getErr = func() error {
				if broken.Load() {
					return errSvcDown
				}
				return nil
			}
			f.sec.hold = func(op, name string) {
				if op != "get" || name != f.secretName() || !armed.CompareAndSwap(true, false) {
					return
				}
				f.sec.mu.Lock()
				delete(f.sec.m, name)
				f.sec.rev[name]++
				f.sec.revErr = errSvcDown
				f.sec.mu.Unlock()
				broken.Store(true)
			}
			code, body := f.svcResolve(plan)
			if armed.Load() {
				t.Fatal("the record was never read; the case did not run")
			}
			if code != http.StatusFailedDependency || body["reason"] != reasonServiceGrantRefused {
				t.Fatalf("resolve = %d %v, want 424 %s", code, body, reasonServiceGrantRefused)
			}
			if n := f.idp.count() - sent; n != 0 {
				t.Fatalf("%d requests sent to the identity provider after the record was gone, want 0", n)
			}
			if f.cached() {
				t.Fatal("the cached token survived the record's absence")
			}
		})
	}
}

// A token that answers late in the token request's window is still published: the publish reads run under a bound
// of their own, never the time the request left over. The bounds are shortened to real milliseconds, since a context's
// deadline runs on the real clock: the provider answers 300 ms into a 400 ms request, and the publish's revision read
// then takes 200 ms, ending after the request's deadline and inside the publish's own 300 ms. The controls run the
// same read under a child of the request's context and under an expired one, and both must fail; a read that takes
// longer than the publish bound is cut at that bound.
func TestServiceGrant_LateTokenAnswerIsPublished(t *testing.T) {
	const answerAt, readTakes = 300 * time.Millisecond, 200 * time.Millisecond
	shorten := func(t *testing.T) {
		tokenBound, publishBound := serviceGrantTokenBound, serviceGrantPublishBound
		serviceGrantTokenBound, serviceGrantPublishBound = 400*time.Millisecond, 300*time.Millisecond
		t.Cleanup(func() { serviceGrantTokenBound, serviceGrantPublishBound = tokenBound, publishBound })
	}
	lateFix := func(t *testing.T, read time.Duration) *svcFix {
		f := newSvcFixWithSecret(t)
		f.idp.mu.Lock()
		f.idp.respond = func(n int, r svcReq) svcAnswer {
			if time.Until(r.Deadline) < answerAt {
				return svcAnswer{Err: errors.New("the request has no time left to answer late in")}
			}
			time.Sleep(answerAt) // the provider's own latency, inside the request's 400 ms
			return svcOK(n, 3600)
		}
		f.idp.mu.Unlock()
		f.sec.mu.Lock()
		f.sec.revDelay = read
		f.sec.mu.Unlock()
		return f
	}
	// publishUnder runs the publish step alone under ctx, as the mint would after a late answer.
	publishUnder := func(t *testing.T, f *svcFix, ctx context.Context) error {
		p := f.provider()
		f.sec.mu.Lock()
		rev := f.sec.revOf(serviceGrantSecretName(p.UID))
		f.sec.mu.Unlock()
		tok := serviceGrantMinted{token: svcToken(9), expiresAt: f.clock.Now().Add(time.Hour)}
		_, err := f.srv.publishServiceGrantToken(ctx, p, serviceGrantDigest(p), rev, true, serviceGrantTriggerRefresh,
			f.clock.Now(), f.srv.nextServiceGrantGen(), f.srv.serviceGrantCacheGen(p.UID), tok)
		return err
	}

	t.Run("published under its own bound", func(t *testing.T) {
		shorten(t)
		f := lateFix(t, readTakes)
		started := time.Now()
		tok, err := f.token(serviceGrantTriggerRefresh)
		if err != nil || tok.token != svcToken(1) || !f.cached() {
			t.Fatalf("late answer = %q, %v, cached %v; want the token published", tok.token, err, f.cached())
		}
		if took := time.Since(started); took < answerAt+readTakes || took >= serviceGrantTokenBound+serviceGrantPublishBound {
			t.Fatalf("the mint took %v, want the late answer and the full read (at least %v) inside both bounds", took, answerAt+readTakes)
		}
	})
	t.Run("control: a child of the request's context fails", func(t *testing.T) {
		shorten(t)
		f := lateFix(t, readTakes)
		req, cancel := context.WithTimeout(context.Background(), serviceGrantTokenBound)
		defer cancel()
		time.Sleep(answerAt) // the request's time used by the late answer
		pctx, pcancel := context.WithTimeout(req, serviceGrantPublishBound)
		defer pcancel()
		if err := publishUnder(t, f, pctx); err == nil || f.cached() {
			t.Fatalf("publish under the request's context = %v, cached %v; want it cut at the request's deadline", err, f.cached())
		}
	})
	t.Run("control: an expired context fails", func(t *testing.T) {
		shorten(t)
		f := lateFix(t, readTakes)
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		if err := publishUnder(t, f, ctx); err == nil || f.cached() {
			t.Fatalf("publish under an expired context = %v, cached %v; want it refused", err, f.cached())
		}
	})
	t.Run("a read longer than the publish bound is cut at it", func(t *testing.T) {
		shorten(t)
		f := lateFix(t, 10*time.Second)
		started := time.Now()
		_, err := f.token(serviceGrantTriggerRefresh)
		took := time.Since(started)
		var se *serviceGrantError
		if !errors.As(err, &se) || se.reason != svcReasonUnavailable || se.definitive || f.cached() {
			t.Fatalf("a publish read past its bound = %v (%+v), cached %v; want the transient unavailable, nothing cached", err, se, f.cached())
		}
		if took < answerAt+serviceGrantPublishBound || took >= answerAt+serviceGrantPublishBound+2*time.Second {
			t.Fatalf("the mint took %v, want it to end at the publish bound (about %v)", took, answerAt+serviceGrantPublishBound)
		}
	})
}

// A rotation that completes after an attempt's last revision read never lets that attempt publish: the attempt is
// held after the read inside its publish, the rotation is written through the route, and the attempt then resumes.
// With a newer-revision attempt published in between, and with none, the old token is never cached or returned, the
// old attempt writes one superseded row and no success row, and its caller gets the new revision's token.
func TestServiceGrant_RotationAfterLastRevisionReadNeverPublishesOldToken(t *testing.T) {
	for _, newerFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("newer attempt publishes first=%v", newerFirst), func(t *testing.T) {
			f := newSvcFixWithSecret(t)
			var oldSent, held atomic.Bool
			paused, resume := make(chan struct{}), make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(resume) }) }
			t.Cleanup(release)
			f.idp.mu.Lock()
			f.idp.respond = func(n int, _ svcReq) svcAnswer {
				if n == 1 {
					oldSent.Store(true)
				}
				return svcOK(n, 3600)
			}
			f.idp.mu.Unlock()
			f.sec.mu.Lock()
			f.sec.afterRev = func() { // the old attempt's publish read, the first revision read after its request
				if oldSent.Load() && held.CompareAndSwap(false, true) {
					close(paused)
					select {
					case <-resume:
					case <-time.After(10 * time.Second):
					}
				}
			}
			f.sec.mu.Unlock()
			type result struct {
				tok serviceGrantMinted
				err error
			}
			old := make(chan result, 1)
			go func() {
				tok, err := f.token(serviceGrantTriggerRefresh)
				old <- result{tok, err}
			}()
			svcAwait(t, paused, "the old attempt to pass its last revision read")
			if w := f.serve(http.MethodPut, f.secretPath(), f.putSecretBody(svcSecretTwo)); w.Code != http.StatusNoContent {
				t.Fatalf("rotation: %d %s", w.Code, w.Body.String())
			}
			if newerFirst {
				if tok, err := f.token(serviceGrantTriggerRefresh); err != nil || tok.token != svcToken(2) {
					t.Fatalf("the newer attempt = %q, %v; want the second token", tok.token, err)
				}
			}
			release()
			var r result
			select {
			case r = <-old:
			case <-time.After(10 * time.Second):
				t.Fatal("the old attempt never returned")
			}
			if r.tok.token == svcToken(1) {
				t.Fatal("the old attempt returned its token after the rotation")
			}
			var se *serviceGrantError
			if r.err != nil && (!errors.As(r.err, &se) || se.reason != svcReasonUnavailable || se.definitive) {
				t.Fatalf("the old attempt's caller got %v, want the new revision's token or the transient error", r.err)
			}
			if r.err == nil && r.tok.token != svcToken(2) {
				t.Fatalf("the old attempt's caller got %q, want the new revision's token", r.tok.token)
			}
			f.srv.svcGrant.mu.Lock()
			c, ok := f.srv.svcGrant.cache[f.uid]
			f.srv.svcGrant.mu.Unlock()
			if !ok || c.token != svcToken(2) {
				t.Fatalf("the cache holds %q (present %v), want the second token", c.token, ok)
			}
			var superseded, success int
			for _, ev := range f.mintRows() {
				var d map[string]any
				_ = json.Unmarshal(ev.Data, &d)
				switch {
				case d["superseded"] == true:
					superseded++
				case ev.Outcome == "success":
					success++
				}
			}
			if f.idp.count() != 2 || superseded != 1 || success != 1 {
				t.Fatalf("%d token requests, %d superseded and %d success rows; want 2, 1 (the old) and 1 (the new)",
					f.idp.count(), superseded, success)
			}
		})
	}
}
