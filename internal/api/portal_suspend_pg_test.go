// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A suspension by the admin route cuts the person's grants and their next exchange, writes the request row, the
// deactivation row and the deprovision row once each, and a replayed request writes nothing more.
func TestPortalSuspend_AdminSuspendCutsExchangeAndGrants_PG(t *testing.T) {
	l := newPortalLife(t)
	principal, bearer := l.signIn(t, "u1")
	if got := l.meCode(t, bearer); got != http.StatusOK {
		t.Fatalf("control: GET /me before the suspension = %d", got)
	}
	// A security admin may suspend.
	w := l.post(t, l.secAdmin, principal, "suspend", "")
	if w.Code != http.StatusOK {
		t.Fatalf("suspend = %d %s, want 200", w.Code, w.Body.String())
	}
	ident := l.ident(t, principal)
	if ident.DeactivatedAt == nil || ident.PurgeAfter != nil || ident.PurgedAt != nil {
		t.Fatalf("identity after suspend = deactivated %v, purge_after %v, purged %v; want deactivated, no deadline", ident.DeactivatedAt, ident.PurgeAfter, ident.PurgedAt)
	}
	if n := l.count(t, `SELECT count(*) FROM portal_grants WHERE principal = $1`, principal); n != 0 {
		t.Errorf("portal_grants rows after the suspension = %d, want 0", n)
	}
	if got := l.meCode(t, bearer); got != http.StatusUnauthorized {
		t.Errorf("GET /me with the old grant = %d, want 401", got)
	}
	if got := l.exchangeFor(t, "u1"); got != http.StatusBadRequest {
		t.Errorf("exchange after the suspension = %d, want 400", got)
	}
	if got := l.deniedReasons(); len(got) != 1 || got[0] != string(oidc.DenialIdentityDeactivated) {
		t.Errorf("denied exchange reasons = %v, want one %s", got, oidc.DenialIdentityDeactivated)
	}
	if !l.allDone(t, ident.ID) {
		t.Errorf("ledger not done after a successful suspension:\n%s", ledgerText(t, l.pool, ident.ID))
	}
	// A replay finishes nothing new and writes no second deactivation row.
	if w := l.post(t, l.admin, principal, "suspend", ""); w.Code != http.StatusOK {
		t.Fatalf("replayed suspend = %d %s, want 200", w.Code, w.Body.String())
	}
	for action, want := range map[string]int{"identity.deactivate": 1, "person.deprovision": 1, "person.suspend": 2} {
		if got := len(l.rows(action)); got != want {
			t.Errorf("%s rows = %d, want %d", action, got, want)
		}
	}
	for _, ev := range l.rows("identity.deactivate") {
		if ev.Target != ident.ID.String() || ev.Outcome != "success" {
			t.Errorf("identity.deactivate row = %+v", ev)
		}
	}
	if got := len(l.rows("scim.user.deactivate")); got != 0 {
		t.Errorf("scim.user.deactivate rows = %d, a portal suspension writes none", got)
	}
}

// auditHook is a recorder that calls before for each event and then records it on inner.
type auditHook struct {
	inner  audit.Recorder
	before func(types.AuditEvent)
}

func (h *auditHook) Record(ctx context.Context, ev types.AuditEvent) error {
	if h.before != nil {
		h.before(ev)
	}
	return h.inner.Record(ctx, ev)
}

// yieldsWithin answers true when ch yields or is closed within d.
func yieldsWithin(ch <-chan struct{}, d time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(d):
		return false
	}
}

// The window the exchange leaves between committing the person's identity and committing their grant, in the
// order that matters: the exchange pauses once the identity has committed (at its introduction row, which is
// written after that commit and before the grant is minted); the person is suspended; the exchange resumes and
// finishes; the person is reactivated. The bearer the exchange made, if it made one, is refused, and a fresh
// exchange works.
func TestPortalSuspend_BetweenIdentityAndGrantCommits_PG(t *testing.T) {
	l := newPortalLife(t)
	l.ver.subj = l.person("u-race", "")
	principal := l.ver.subj.Principal
	held, release := make(chan struct{}), make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce()
	var once sync.Once
	l.h.srv.cfg.Audit = &auditHook{inner: l.h.srv.cfg.Audit, before: func(ev types.AuditEvent) {
		if ev.Action != "person.create" || ev.Target != principal {
			return
		}
		once.Do(func() {
			close(held)
			yieldsWithin(release, 30*time.Second)
		})
	}}
	exchanged := make(chan *httptest.ResponseRecorder, 1)
	go func() { exchanged <- l.exchange(t, l.d.ID, l.cred, "subject-token") }()
	if !yieldsWithin(held, 10*time.Second) {
		t.Fatal("the exchange never reached its introduction row")
	}
	// The identity has committed and the grant has not: the control for the order.
	if n := l.count(t, `SELECT count(*) FROM principal_identities WHERE principal = $1`, principal); n != 1 {
		t.Fatalf("control: identity rows = %d, want 1", n)
	}
	if n := l.count(t, `SELECT count(*) FROM portal_grants WHERE principal = $1`, principal); n != 0 {
		t.Fatalf("control: portal_grants rows = %d before the grant commits, want 0", n)
	}
	if w := l.post(t, l.admin, principal, "suspend", ""); w.Code != http.StatusOK {
		t.Fatalf("suspend = %d %s, want 200", w.Code, w.Body.String())
	}
	releaseOnce()
	var ex *httptest.ResponseRecorder
	select {
	case ex = <-exchanged:
	case <-time.After(20 * time.Second):
		t.Fatal("the paused exchange did not finish after the hold was released")
	}
	// The resumed exchange committed its grant: the pinned store checks only a permanent purge before the insert,
	// so the order under test is a grant that exists for a suspended person.
	if ex.Code != http.StatusOK {
		t.Fatalf("the resumed exchange = %d %s, want 200 (its grant commits)", ex.Code, ex.Body.String())
	}
	stale, _ := portalToken(t, ex)
	if n := l.count(t, `SELECT count(*) FROM portal_grants WHERE principal = $1`, principal); n != 1 {
		t.Errorf("portal_grants rows after the resumed exchange = %d, want 1", n)
	}
	if got := l.meCode(t, stale); got != http.StatusUnauthorized {
		t.Errorf("the committed grant answers %d while the person is suspended, want 401", got)
	}
	if w := l.post(t, l.admin, principal, "reactivate", ""); w.Code != http.StatusOK {
		t.Fatalf("reactivate = %d %s, want 200", w.Code, w.Body.String())
	}
	if got := l.meCode(t, stale); got != http.StatusUnauthorized {
		t.Errorf("the grant that committed during the suspension answers %d after the reactivation, want 401", got)
	}
	fresh := l.freshBearer(t, "u-race")
	if fresh == stale {
		t.Fatal("control: the fresh exchange returned the stale bearer")
	}
	if got := l.meCode(t, fresh); got != http.StatusOK {
		t.Errorf("a fresh exchange's bearer after the reactivation answers %d, want 200", got)
	}
	if ident := l.ident(t, principal); ident.DeactivatedAt != nil {
		t.Errorf("the person is still deactivated after the reactivation (%v)", ident.DeactivatedAt)
	}
}

// cutCounter is the revocation store that remembers every session cut it was asked to write.
type cutCounter struct {
	*pgTestRevocations
	cmu  sync.Mutex
	subs []string
}

func (c *cutCounter) RevokeSub(ctx context.Context, sub string) error {
	c.cmu.Lock()
	c.subs = append(c.subs, sub)
	c.cmu.Unlock()
	return c.pgTestRevocations.RevokeSub(ctx, sub)
}

func (c *cutCounter) cuts() []string {
	c.cmu.Lock()
	defer c.cmu.Unlock()
	return slices.Clone(c.subs)
}

// killHook is the runner that calls on before each sandbox kill.
type killHook struct {
	*scimRunner
	on func()
}

func (k *killHook) KillSandbox(ctx context.Context, ref string) error {
	if k.on != nil {
		k.on()
	}
	return k.scimRunner.KillSandbox(ctx, ref)
}

// A suspension's tail writes no session cut of its own: the one cut of the person's principal is step one's, in
// the transaction that deactivated them, so a reactivation and a fresh exchange that land inside the tail are not
// ended by a late write. Here they land at the kill, and the fresh bearer outlives the suspension's remaining
// steps while the bearer from before it stays refused.
func TestPortalSuspend_TailWritesNoCutAndFreshGrantSurvives_PG(t *testing.T) {
	l := newPortalLife(t)
	counter := &cutCounter{pgTestRevocations: l.h.srv.cfg.SessionRevocations.(*pgTestRevocations)}
	l.h.srv.cfg.SessionRevocations = counter
	principal, old := l.signIn(t, "u1")
	run := l.seedRun(t, principal, types.RunRunning)
	var fresh string
	var once sync.Once
	l.h.srv.cfg.Runner = &killHook{scimRunner: l.run, on: func() {
		once.Do(func() {
			if w := l.post(t, l.admin, principal, "reactivate", ""); w.Code != http.StatusOK {
				t.Errorf("reactivate inside the tail = %d %s", w.Code, w.Body.String())
			}
			fresh = l.freshBearer(t, "u1")
		})
	}}
	if w := l.post(t, l.admin, principal, "suspend", ""); w.Code != http.StatusOK && w.Code != http.StatusServiceUnavailable {
		t.Fatalf("suspend = %d %s", w.Code, w.Body.String())
	}
	if got := counter.cuts(); len(got) != 0 {
		t.Errorf("the tail wrote session cuts for %v, want none (step one's transaction holds the cut)", got)
	}
	if fresh == "" {
		t.Fatal("the fresh exchange inside the tail did not run")
	}
	if got := l.meCode(t, fresh); got != http.StatusOK {
		t.Errorf("the grant made after the reactivation answers %d once the suspension's tail finished, want 200", got)
	}
	if got := l.meCode(t, old); got != http.StatusUnauthorized {
		t.Errorf("the grant made before the suspension answers %d, want 401", got)
	}
	if st := l.runState(t, run); st != types.RunKilled {
		t.Errorf("the run the suspension owed = %s, want KILLED", st)
	}
}

// A tail bound to an earlier suspension tears nothing down once a later one owns the identity, whatever the person
// has made since: the later suspension's own tail does that, under its own attribution.
func TestPortalSuspend_StaleTailLeavesLaterSuspensionsRunAlone_PG(t *testing.T) {
	l := newPortalLife(t)
	ctx := withActor(context.Background(), types.ActorSystem, portalLifecycleActor)
	principal, _ := l.signIn(t, "u1")
	if w := l.post(t, l.admin, principal, "suspend", ""); w.Code != http.StatusOK {
		t.Fatalf("first suspend = %d", w.Code)
	}
	id := l.ident(t, principal).ID
	first := *l.ident(t, principal).DeactivatedAt
	if w := l.post(t, l.admin, principal, "reactivate", ""); w.Code != http.StatusOK {
		t.Fatalf("reactivate = %d", w.Code)
	}
	if w := l.post(t, l.admin, principal, "suspend", ""); w.Code != http.StatusOK {
		t.Fatalf("second suspend = %d", w.Code)
	}
	second := *l.ident(t, principal).DeactivatedAt
	if second.Equal(first) {
		t.Fatalf("control: both suspensions deactivated at %v", first)
	}
	// A run the second suspension's ledger will owe: made while the person is suspended for the second time.
	run := l.seedRun(t, principal, types.RunRunning)
	kills := l.run.killCount()
	o := portalOrigin(types.ActorSystem, portalLifecycleActor)
	err := l.h.srv.killStep(withPortalSuspensionBound(ctx, id, first), o, l.st, id, []string{principal})
	if !errors.Is(err, store.ErrSuperseded) {
		t.Errorf("the first suspension's kill step = %v, want ErrSuperseded", err)
	}
	if st := l.runState(t, run); st != types.RunRunning || l.run.killCount() != kills {
		t.Errorf("the stale tail touched the later suspension's run: state %s, kills %d -> %d", st, kills, l.run.killCount())
	}
	// The same step bound to the suspension that is current does tear it down.
	if err := l.h.srv.killStep(withPortalSuspensionBound(ctx, id, second), o, l.st, id, []string{principal}); err != nil {
		t.Errorf("the second suspension's kill step = %v, want nil", err)
	}
	if st := l.runState(t, run); st != types.RunKilled {
		t.Errorf("the run after the current tail's kill step = %s, want KILLED", st)
	}
	for _, ev := range l.rows("run.kill") {
		if ev.Actor != portalLifecycleActor {
			t.Errorf("run.kill actor = %q, want %q", ev.Actor, portalLifecycleActor)
		}
	}
}

// Each audit step of a suspension is one event, whoever writes it and however often: a request and the sweep that
// both run the step, and a retry after the row was written but before the step closed, add no second row, and the
// row keeps its first writer's actor.
func TestPortalSuspend_AuditStepsRecordOnce_PG(t *testing.T) {
	l := newPortalLife(t)
	ctx := context.Background()
	l.h.srv.cfg.Audit = store.Recorder{Pool: l.pool}
	principal, _ := l.signIn(t, "u1")
	id := l.ident(t, principal).ID
	admin := portalOrigin(types.ActorHuman, "root")
	sweep := portalSweepOrigin()
	if _, _, _, err := l.h.srv.portalSuspendStepOne(withActor(ctx, admin.actorType, admin.actor), l.st, id, admin, 0, nil); err != nil {
		t.Fatalf("step one: %v", err)
	}
	rows := func(action string) (n int) {
		return l.count(t, `SELECT count(*) FROM audit_events WHERE action = $1 AND target = $2`, action, id.String())
	}
	runAudit := func(o leaverOrigin) error {
		return l.h.srv.auditSuspension(withActor(ctx, o.actorType, o.actor), o, l.st, id, portalSlot(o), nil)
	}
	// The request and the sweep run the step at once.
	errs := make(chan error, 2)
	for _, o := range []leaverOrigin{admin, sweep} {
		go func() { errs <- runAudit(o) }()
	}
	for range 2 {
		select {
		case err := <-errs:
			if err != nil {
				t.Errorf("auditSuspension = %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("an audit step did not finish")
		}
	}
	for _, action := range []string{"identity.deactivate", "person.deprovision"} {
		if n := rows(action); n != 1 {
			t.Errorf("%s rows after an overlap = %d, want 1", action, n)
		}
	}
	actors := map[string]string{}
	for _, action := range []string{"identity.deactivate", "person.deprovision"} {
		var actor string
		if err := l.pool.QueryRow(ctx, `SELECT actor FROM audit_events WHERE action = $1 AND target = $2`, action, id.String()).Scan(&actor); err != nil {
			t.Fatal(err)
		}
		actors[action] = actor
	}
	// The ledger's pinned event is the row's own id.
	if n := l.count(t, `SELECT count(*) FROM deprovision_jobs j JOIN audit_events a ON a.id = j.audit_event_id AND a."time" = j.audit_event_at
	                     WHERE j.identity_id = $1 AND j.kind = $2 AND j.step IN ($3, $4)`, id, store.JobKindSuspend, jobStepAuditDeact, jobStepAuditDeprov); n != 2 {
		t.Errorf("audit rows that carry their ledger row's pinned id and time = %d, want 2", n)
	}
	// A crash after the write and before the step closed: the step reopens and a third writer runs it.
	if _, err := l.pool.Exec(ctx, `UPDATE deprovision_jobs SET state = 'pending' WHERE identity_id = $1 AND kind = $2 AND step IN ($3, $4)`,
		id, store.JobKindSuspend, jobStepAuditDeact, jobStepAuditDeprov); err != nil {
		t.Fatal(err)
	}
	if err := runAudit(portalOrigin(types.ActorHuman, "someone-else")); err != nil {
		t.Fatalf("retry: %v", err)
	}
	for action, actor := range actors {
		if n := rows(action); n != 1 {
			t.Errorf("%s rows after a retry = %d, want 1", action, n)
		}
		var got string
		if err := l.pool.QueryRow(ctx, `SELECT actor FROM audit_events WHERE action = $1 AND target = $2`, action, id.String()).Scan(&got); err != nil || got != actor {
			t.Errorf("%s actor after a retry = %q, %v; want the first writer's %q", action, got, err, actor)
		}
	}
	var open int
	if err := l.pool.QueryRow(ctx, `SELECT count(*) FROM deprovision_jobs WHERE identity_id = $1 AND kind = $2 AND state <> 'done' AND step IN ($3, $4)`,
		id, store.JobKindSuspend, jobStepAuditDeact, jobStepAuditDeprov).Scan(&open); err != nil || open != 0 {
		t.Errorf("audit steps still open after the retry: %d, %v", open, err)
	}
}

// A grant that existed before the suspension, on either of the person's portals, is refused at its next request.
func TestPortalSuspend_ExistingGrantRefusedAtNextRequest_PG(t *testing.T) {
	l := newPortalLife(t)
	principal, first := l.signIn(t, "u1")
	d2, cred2 := l.portal(t, portalTestIssuer, nil)
	second := l.token(t, d2, cred2)
	for name, b := range map[string]string{"first": first, "second": second} {
		if got := l.meCode(t, b); got != http.StatusOK {
			t.Fatalf("control: %s portal's grant = %d before the suspension", name, got)
		}
	}
	if w := l.post(t, l.admin, principal, "suspend", ""); w.Code != http.StatusOK {
		t.Fatalf("suspend = %d %s", w.Code, w.Body.String())
	}
	for name, b := range map[string]string{"first": first, "second": second} {
		if got := l.meCode(t, b); got != http.StatusUnauthorized {
			t.Errorf("%s portal's grant after the suspension = %d, want 401", name, got)
		}
	}
}

// A console person's principal is refused at every lifecycle route: 422 portal_person_only, a denied audit row,
// and nothing written. It needs no database: the principal is judged before the store is asked anything.
func TestPortalSuspend_ConsolePersonRefused(t *testing.T) {
	h := newHarness(t)
	st := &personMintFakeStore{Store: newTokenMemStore(), people: map[string]types.Person{}}
	cfg := baseTestConfig(h, st)
	cfg.OIDC = newAccessAuth(t, map[string]string{
		"sec@corp.example": oidc.RoleSecurityAdmin, "root@corp.example": oidc.RoleAdmin,
	}, oidc.RoleUser, nil, nil)
	srv := New(cfg)
	root := accessSession(t, "root", "root@corp.example", oidc.RoleAdmin, []string{})
	sec := accessSession(t, "sec", "sec@corp.example", oidc.RoleSecurityAdmin, []string{})
	for _, c := range []struct {
		action string
		as     *http.Cookie
	}{
		{"suspend", sec}, {"suspend", root}, {"reactivate", root}, {"purge", root},
	} {
		for _, target := range []string{"alice@corp.example", "delegate:person", "sub-of-a-console-person"} {
			w := doSSO(t, srv, http.MethodPost, "/api/v1/people/"+target+"/"+c.action, c.as, "")
			if w.Code != http.StatusUnprocessableEntity || errorReason(w) != "portal_person_only" {
				t.Errorf("%s %s = %d %s, want 422 portal_person_only", c.action, target, w.Code, w.Body.String())
			}
		}
	}
	denied := 0
	for _, ev := range h.audit.snapshot() {
		switch ev.Action {
		case "person.suspend", "person.reactivate", "person.purge":
			if ev.Outcome != "denied" {
				t.Errorf("%s row outcome = %s, want denied", ev.Action, ev.Outcome)
			}
			denied++
		case "identity.deactivate", "person.deprovision":
			t.Errorf("a refused request wrote %s", ev.Action)
		}
	}
	if denied != 12 {
		t.Errorf("denied lifecycle rows = %d, want 12", denied)
	}
	// A security admin may not reactivate or purge.
	for _, action := range []string{"reactivate", "purge"} {
		if w := doSSO(t, srv, http.MethodPost, "/api/v1/people/x/"+action, sec, ""); w.Code != http.StatusForbidden {
			t.Errorf("security admin %s = %d, want 403", action, w.Code)
		}
	}
}

// SCIM's purge delay is the workforce's. A portal person an admin suspended with no deadline is never purged by
// it, by either sweep, and their next exchange is refused as deactivated, not purged.
func TestPortalSuspend_SCIMPurgeDelayNeverPurgesPortalPerson_PG(t *testing.T) {
	l := newPortalLife(t)
	l.scimOn(lifeWorkforceIssuer, time.Hour)
	ctx := context.Background()
	principal, _ := l.signIn(t, "u1")
	if w := l.post(t, l.admin, principal, "suspend", ""); w.Code != http.StatusOK {
		t.Fatalf("suspend = %d %s", w.Code, w.Body.String())
	}
	id := l.ident(t, principal).ID
	l.advance(48 * time.Hour)
	if err := l.h.srv.SweepSCIMPurge(ctx); err != nil {
		t.Fatalf("SCIM sweep: %v", err)
	}
	if err := l.h.srv.SweepPortalLifecycle(ctx); err != nil {
		t.Fatalf("portal sweep: %v", err)
	}
	ident := l.ident(t, principal)
	if ident.PurgedAt != nil || ident.PurgeAfter != nil || ident.DeactivatedAt == nil {
		t.Errorf("identity = purged %v, purge_after %v, deactivated %v; want still deactivated with no deadline", ident.PurgedAt, ident.PurgeAfter, ident.DeactivatedAt)
	}
	if n := l.count(t, `SELECT count(*) FROM deprovision_jobs WHERE identity_id = $1 AND kind = $2`, id, store.JobKindPurge); n != 0 {
		t.Errorf("purge ledger rows = %d, want none", n)
	}
	if got := l.exchangeFor(t, "u1"); got != http.StatusBadRequest {
		t.Errorf("exchange = %d, want 400", got)
	}
	if got := l.deniedReasons(); len(got) != 1 || got[0] != string(oidc.DenialIdentityDeactivated) {
		t.Errorf("denied reasons = %v", got)
	}
}

// The deadline an admin gave is the portal sweep's: before it nothing is purged, after it the person is purged
// and the purge finished.
func TestPortalSuspend_AdminDeadlinePurges_PG(t *testing.T) {
	l := newPortalLife(t)
	ctx := context.Background()
	principal, _ := l.signIn(t, "u1")
	if w := l.post(t, l.admin, principal, "suspend", `{"purge_after_seconds": 1}`); w.Code != http.StatusOK {
		t.Fatalf("suspend with a deadline = %d %s", w.Code, w.Body.String())
	}
	id := l.ident(t, principal).ID
	if l.ident(t, principal).PurgeAfter == nil {
		t.Fatal("no purge deadline stored")
	}
	l.waitDue(t, id)
	if err := l.h.srv.SweepPortalLifecycle(ctx); err != nil {
		t.Fatalf("portal sweep: %v", err)
	}
	ident := l.ident(t, principal)
	if ident.PurgedAt == nil {
		t.Fatal("the person past their deadline was not purged by the portal sweep")
	}
	if !l.allDone(t, id) {
		t.Errorf("ledger not done:\n%s", ledgerText(t, l.pool, id))
	}
	var purges int
	for _, ev := range l.rows("person.deprovision") {
		if lifeData(ev)["kind"] == store.JobKindPurge {
			purges++
		}
	}
	if purges != 1 {
		t.Errorf("person.deprovision purge rows = %d, want 1", purges)
	}
	// Out-of-range deadlines are refused with nothing written.
	p2, _ := l.signIn(t, "u2")
	for _, body := range []string{`{"purge_after_seconds": -1}`, `{"purge_after_seconds": 99999999999}`} {
		if w := l.post(t, l.admin, p2, "suspend", body); w.Code != http.StatusUnprocessableEntity || errorReason(w) != "portal_person_param_invalid" {
			t.Errorf("suspend %s = %d %s, want 422 portal_person_param_invalid", body, w.Code, w.Body.String())
		}
	}
	if l.ident(t, p2).DeactivatedAt != nil {
		t.Error("a refused deadline suspended the person")
	}
}

// SCIM pointed at a portal person's real issuer still never reaches a portal row through any of its entry
// points: the row, its ledger and its grants are unchanged.
func TestPortalSuspend_SCIMReconfiguredToPortalIssuerStillUnreachable_PG(t *testing.T) {
	l := newPortalLife(t)
	l.scimOn(portalTestIssuer, time.Hour)
	l.scimLeavesPortalRowAlone(t)
}

// A crash after the deactivation committed and before the tail: with SCIM off the portal sweep finishes the
// suspension.
func TestPortalSuspend_CrashResumedWithSCIMOff_PG(t *testing.T) {
	l := newPortalLife(t)
	ctx := context.Background()
	principal, _ := l.signIn(t, "u1")
	run := l.seedRun(t, principal, types.RunRunning)
	id := l.ident(t, principal).ID
	if _, _, _, err := l.h.srv.portalSuspendStepOne(ctx, l.st, id, portalOrigin(types.ActorHuman, "admin"), 0, nil); err != nil {
		t.Fatalf("step one: %v", err)
	}
	if l.runState(t, run) != types.RunRunning || len(l.rows("identity.deactivate")) != 0 || l.allDone(t, id) {
		t.Fatal("control: the crash left more than step one done")
	}
	if l.h.srv.cfg.SCIM != nil {
		t.Fatal("control: SCIM must be off")
	}
	if err := l.h.srv.SweepPortalLifecycle(ctx); err != nil {
		t.Fatalf("portal sweep: %v", err)
	}
	if st := l.runState(t, run); st != types.RunKilled {
		t.Errorf("run after the sweep = %s, want KILLED", st)
	}
	if !l.allDone(t, id) {
		t.Errorf("ledger not done after the sweep:\n%s", ledgerText(t, l.pool, id))
	}
	rows := l.rows("identity.deactivate")
	if len(rows) != 1 || rows[0].Actor != portalLifecycleActor || rows[0].ActorType != types.ActorSystem {
		t.Errorf("identity.deactivate rows = %+v, want one by the portal sweep", rows)
	}
}

// With SCIM off the portal sweep resumes no workforce work: a suspended workforce person with pending ledger
// rows, a due deadline and a running run is left exactly as found.
func TestPortalSuspend_SCIMOffDoesNotResumeWorkforceJobs_PG(t *testing.T) {
	l := newPortalLife(t)
	ctx := context.Background()
	pending, pendingRun := l.seedWorkforceLeaver(t, "wf-pending", false)
	due, dueRun := l.seedWorkforceLeaver(t, "wf-due", true)
	if _, err := l.pool.Exec(ctx, `UPDATE deprovision_jobs SET updated_at = now() - interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	before := ledgerText(t, l.pool, pending.ID) + ledgerText(t, l.pool, due.ID)
	if err := l.h.srv.SweepPortalLifecycle(ctx); err != nil {
		t.Fatalf("portal sweep: %v", err)
	}
	if after := ledgerText(t, l.pool, pending.ID) + ledgerText(t, l.pool, due.ID); after != before {
		t.Errorf("the portal sweep touched workforce ledger rows:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	for _, run := range []string{pendingRun.String(), dueRun.String()} {
		if st := l.count(t, `SELECT count(*) FROM agent_runs WHERE id = $1 AND state = 'RUNNING'`, run); st != 1 {
			t.Errorf("workforce run %s was killed by the portal sweep", run)
		}
	}
	if got := l.ident(t, "wf-due"); got.PurgedAt != nil {
		t.Error("the portal sweep purged a workforce person")
	}
	if l.run.killCount() != 0 {
		t.Errorf("runner kills = %d, want 0", l.run.killCount())
	}
}

// A suspension kills the person's running runs, with the suspension's own reason, and no one else's.
func TestPortalSuspend_KillsRunningRuns_PG(t *testing.T) {
	l := newPortalLife(t)
	principal, _ := l.signIn(t, "u1")
	other, _ := l.signIn(t, "u2")
	mine := []uuid.UUID{l.seedRun(t, principal, types.RunRunning), l.seedRun(t, principal, types.RunRunning)}
	theirs := l.seedRun(t, other, types.RunRunning)
	if w := l.post(t, l.admin, principal, "suspend", ""); w.Code != http.StatusOK {
		t.Fatalf("suspend = %d %s", w.Code, w.Body.String())
	}
	for _, r := range mine {
		if st := l.runState(t, r); st != types.RunKilled {
			t.Errorf("run %s = %s, want KILLED", r, st)
		}
	}
	if st := l.runState(t, theirs); st != types.RunRunning {
		t.Errorf("another person's run = %s, want RUNNING", st)
	}
	if l.run.killCount() != 2 {
		t.Errorf("runner kills = %d, want 2", l.run.killCount())
	}
	kills := l.rows("run.kill")
	if len(kills) != 2 {
		t.Fatalf("run.kill rows = %d, want 2", len(kills))
	}
	for _, ev := range kills {
		if lifeData(ev)["reason"] != "person_suspend" {
			t.Errorf("run.kill reason = %v, want person_suspend", lifeData(ev)["reason"])
		}
	}
}

// A resume listed before a reactivation is superseded: nothing is written and the person stays active.
func TestPortalSuspend_ReactivatedBeforeStepOneSuperseded_PG(t *testing.T) {
	l := newPortalLife(t)
	ctx := context.Background()
	principal, _ := l.signIn(t, "u1")
	id := l.ident(t, principal).ID
	if w := l.post(t, l.admin, principal, "suspend", ""); w.Code != http.StatusOK {
		t.Fatalf("suspend = %d", w.Code)
	}
	stale := l.ident(t, principal).DeactivatedAt
	if w := l.post(t, l.admin, principal, "reactivate", ""); w.Code != http.StatusOK {
		t.Fatalf("reactivate = %d %s", w.Code, w.Body.String())
	}
	deactivations, snapshot := len(l.rows("identity.deactivate")), ledgerText(t, l.pool, id)
	o := portalSweepOrigin()
	if _, _, _, err := l.h.srv.portalSuspendStepOne(ctx, l.st, id, o, 0, stale); !errors.Is(err, store.ErrSuperseded) {
		t.Fatalf("step one of a stale resume = %v, want ErrSuperseded", err)
	}
	if err := l.h.srv.runPortalSuspension(ctx, l.st, id, o, 0, stale); err != nil {
		t.Fatalf("a superseded resume = %v, want nil", err)
	}
	if l.ident(t, principal).DeactivatedAt != nil {
		t.Error("a stale resume suspended a reactivated person")
	}
	if got := len(l.rows("identity.deactivate")); got != deactivations {
		t.Errorf("identity.deactivate rows %d -> %d, want unchanged", deactivations, got)
	}
	if after := ledgerText(t, l.pool, id); after != snapshot {
		t.Errorf("the ledger changed:\nbefore:\n%s\nafter:\n%s", snapshot, after)
	}
}

// A tail bound to a suspension that was undone spares the run the person made after the reactivation.
func TestPortalSuspend_ReactivatedBeforeTailSparesNewRun_PG(t *testing.T) {
	l := newPortalLife(t)
	ctx := context.Background()
	principal, _ := l.signIn(t, "u1")
	id := l.ident(t, principal).ID
	o := portalOrigin(types.ActorHuman, "admin")
	ident, forms, _, err := l.h.srv.portalSuspendStepOne(ctx, l.st, id, o, 0, nil)
	if err != nil || ident.DeactivatedAt == nil {
		t.Fatalf("step one: %v (deactivated %v)", err, ident.DeactivatedAt)
	}
	if w := l.post(t, l.admin, principal, "reactivate", ""); w.Code != http.StatusOK {
		t.Fatalf("reactivate = %d", w.Code)
	}
	newRun := l.seedRun(t, principal, types.RunRunning)
	bound := withSuspensionBound(ctx, id, *ident.DeactivatedAt)
	err = l.h.srv.killStep(bound, o, l.st, id, forms.targets)
	if st := l.runState(t, newRun); st != types.RunRunning {
		t.Errorf("the run made after the reactivation = %s, want RUNNING (kill step err %v)", st, err)
	}
	if l.run.killCount() != 0 {
		t.Errorf("runner kills = %d, want 0", l.run.killCount())
	}
}

// A second suspension after a reactivation owns the ledger: it finishes and audits itself once, and the first
// suspension's tail applies nothing more.
func TestPortalSuspend_SecondSuspensionOwnsLedger_PG(t *testing.T) {
	l := newPortalLife(t)
	ctx := context.Background()
	principal, _ := l.signIn(t, "u1")
	id := l.ident(t, principal).ID
	o := portalOrigin(types.ActorHuman, "admin")
	first, _, _, err := l.h.srv.portalSuspendStepOne(ctx, l.st, id, o, 0, nil)
	if err != nil || first.DeactivatedAt == nil {
		t.Fatalf("first step one: %v", err)
	}
	if w := l.post(t, l.admin, principal, "reactivate", ""); w.Code != http.StatusOK {
		t.Fatalf("reactivate = %d", w.Code)
	}
	if w := l.post(t, l.admin, principal, "suspend", ""); w.Code != http.StatusOK {
		t.Fatalf("second suspend = %d %s", w.Code, w.Body.String())
	}
	second := l.ident(t, principal).DeactivatedAt
	if second == nil || second.Equal(*first.DeactivatedAt) {
		t.Fatalf("control: the second suspension kept the first's deactivation (%v, %v)", second, first.DeactivatedAt)
	}
	done := ledgerText(t, l.pool, id)
	if !l.allDone(t, id) || len(l.rows("identity.deactivate")) != 1 {
		t.Fatalf("the second suspension did not finish itself: %d deactivate rows\n%s", len(l.rows("identity.deactivate")), done)
	}
	// The first tail runs late, bound to the first deactivation.
	bound := withSuspensionBound(ctx, id, *first.DeactivatedAt)
	err = l.h.srv.auditSuspension(bound, o, l.st, id, portalAdminSlot, nil)
	if got := len(l.rows("identity.deactivate")); got != 1 {
		t.Errorf("identity.deactivate rows after the first tail = %d, want 1", got)
	}
	if got := len(l.rows("person.deprovision")); got != 1 {
		t.Errorf("person.deprovision rows after the first tail = %d, want 1", got)
	}
	if after := ledgerText(t, l.pool, id); after != done {
		t.Errorf("the first tail changed the second suspension's ledger (err %v):\n%s\n%s", err, done, after)
	}
}

// A purge deadline is the permanent purge scheduled, so only a super admin may set one. A security admin who asks
// for one is refused before anything changes; the same caller suspends without one; the super admin's deadline is
// kept and the sweep purges at it.
func TestPortalSuspend_PurgeDeadlineNeedsSuperAdmin_PG(t *testing.T) {
	l := newPortalLife(t)
	ctx := context.Background()
	principal, bearer := l.signIn(t, "u1")
	id := l.ident(t, principal).ID
	audits := len(l.h.audit.snapshot())
	w := l.post(t, l.secAdmin, principal, "suspend", `{"purge_after_seconds": 3600}`)
	if w.Code != http.StatusForbidden || errorReason(w) != "portal_purge_deadline_operator_only" {
		t.Fatalf("security admin suspend with a deadline = %d %s, want 403 portal_purge_deadline_operator_only", w.Code, w.Body.String())
	}
	if ident := l.ident(t, principal); ident.DeactivatedAt != nil || ident.PurgeAfter != nil {
		t.Errorf("identity after the refusal = deactivated %v, purge_after %v; want neither", ident.DeactivatedAt, ident.PurgeAfter)
	}
	if n := l.count(t, `SELECT count(*) FROM deprovision_jobs WHERE identity_id = $1`, id); n != 0 {
		t.Errorf("ledger rows after the refusal = %d, want 0", n)
	}
	if got := l.meCode(t, bearer); got != http.StatusOK {
		t.Errorf("GET /me after the refusal = %d, want 200 (nothing was cut)", got)
	}
	for _, ev := range l.h.audit.snapshot()[audits:] {
		if ev.Action != "person.suspend" || ev.Outcome != "denied" {
			t.Errorf("the refusal wrote %s (%s)", ev.Action, ev.Outcome)
		}
	}
	// The same caller suspends with no deadline, or a zero one.
	if w := l.post(t, l.secAdmin, principal, "suspend", `{"purge_after_seconds": 0}`); w.Code != http.StatusOK {
		t.Fatalf("security admin suspend with no deadline = %d %s, want 200", w.Code, w.Body.String())
	}
	if ident := l.ident(t, principal); ident.DeactivatedAt == nil || ident.PurgeAfter != nil {
		t.Errorf("identity = deactivated %v, purge_after %v; want deactivated with no deadline", ident.DeactivatedAt, ident.PurgeAfter)
	}
	// The super admin schedules one, and the sweep purges at it.
	p2, _ := l.signIn(t, "u2")
	if w := l.post(t, l.admin, p2, "suspend", `{"purge_after_seconds": 1}`); w.Code != http.StatusOK {
		t.Fatalf("super admin suspend with a deadline = %d %s, want 200", w.Code, w.Body.String())
	}
	id2 := l.ident(t, p2).ID
	l.waitDue(t, id2)
	if err := l.h.srv.SweepPortalLifecycle(ctx); err != nil {
		t.Fatalf("portal sweep: %v", err)
	}
	if l.ident(t, p2).PurgedAt == nil {
		t.Error("the super admin's deadline did not purge at the sweep")
	}
	if l.ident(t, principal).PurgedAt != nil {
		t.Error("the person suspended with no deadline was purged")
	}
}
