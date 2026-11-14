// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// afterIdentityRead runs hook once, with the identity row the first read of id returned, before the caller
// acts on it.
type afterIdentityRead struct {
	store.PG
	mu   sync.Mutex
	id   uuid.UUID
	hook func(store.PrincipalIdentity)
}

func (s *afterIdentityRead) GetIdentity(ctx context.Context, id uuid.UUID) (store.PrincipalIdentity, error) {
	out, err := s.PG.GetIdentity(ctx, id)
	s.mu.Lock()
	hook := s.hook
	if id == s.id {
		s.hook = nil
	}
	s.mu.Unlock()
	if hook != nil && id == s.id && err == nil {
		hook(out)
	}
	return out, err
}

// pausedSuspension runs each hook once: afterStep1 right after a suspension's step 1 commits, before its sweep
// and kill, afterRunProbe right after a suspension tail's run probe (RunsInsertedAfter) answers, and
// beforeAuditFinish right before the tail closes its scim.user.deactivate step.
type pausedSuspension struct {
	store.PG
	mu                                           sync.Mutex
	afterStep1, afterRunProbe, beforeAuditFinish func()
}

func (s *pausedSuspension) take(hook *func()) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := *hook
	*hook = nil
	return f
}

func (s *pausedSuspension) SuspendIdentity(ctx context.Context, p store.SuspendPlan) (store.SuspendResult, error) {
	res, err := s.PG.SuspendIdentity(ctx, p)
	if err == nil {
		if f := s.take(&s.afterStep1); f != nil {
			f()
		}
	}
	return res, err
}

func (s *pausedSuspension) RunsInsertedAfter(ctx context.Context, identity uuid.UUID, ids []uuid.UUID, at time.Time) (map[uuid.UUID]bool, error) {
	out, err := s.PG.RunsInsertedAfter(ctx, identity, ids, at)
	if f := s.take(&s.afterRunProbe); f != nil {
		f()
	}
	return out, err
}

func (s *pausedSuspension) FinishSuspensionJobAt(ctx context.Context, identity uuid.UUID, k store.JobKey, detail map[string]int, at time.Time) error {
	if k.Step == jobStepAuditDeact {
		if f := s.take(&s.beforeAuditFinish); f != nil {
			f()
		}
	}
	return s.PG.FinishSuspensionJobAt(ctx, identity, k, detail, at)
}

// suspendAgainStepOneOnly reactivates the person and runs a later suspension's step 1 alone, as a request that
// died right after it: the ledger is reset and holds that suspension's pending audit steps and one sweep step.
func (e *scimEnv) suspendAgainStepOneOnly(id string) {
	e.t.Helper()
	if w := e.patch(e.b, id, patchOf("true")); w.Code != http.StatusOK {
		e.t.Errorf("reactivate = %d %s", w.Code, w.Body.String())
	}
	ident := e.identity(id)
	plan := store.SuspendPlan{IdentityID: ident.ID, Principal: ident.Principal, Principals: []string{purgeSub}, CutoffSubs: []string{purgeSub},
		PendingJobs: []store.JobKey{{Step: jobStepAuditDeact}, {Step: jobStepAuditDeprov}, {Step: jobStepSweep, Target: purgeSub}}}
	if _, err := e.st.SuspendIdentity(context.Background(), plan); err != nil {
		e.t.Errorf("the later suspension's step 1 = %v", err)
	}
}

// keyExists reports whether the SSH key with fingerprint fp is still registered.
func (e *scimEnv) keyExists(fp string) bool {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM ssh_public_keys WHERE fingerprint = $1`, fp).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n == 1
}

// cutMark is the newest session cut that names sub or email: a new cut moves it.
func (e *scimEnv) cutMark(sub, email string) int64 {
	e.t.Helper()
	var seq int64
	if err := e.pool.QueryRow(context.Background(), `SELECT COALESCE(MAX(seq), 0) FROM oidc_session_revocations
		WHERE sub = $1 OR lower(sub) = lower($2)`, sub, email).Scan(&seq); err != nil {
		e.t.Fatal(err)
	}
	return seq
}

// The sweeper resumes a suspension whose sweep and kill stopped after step 1, and the identity provider
// reactivates the person right after the resume's first read. The resume still finishes what the suspension
// owed: the run, the token and the SSH key made before it, and those the database stamped exactly at its
// deactivation, are killed, revoked and deleted. What the person made after the reactivation stands: that run
// keeps running and its kill row closes with no kill counted, that token is not revoked and still
// authenticates, that key is still registered, and no new session cut is written. Guarded by WARDYN_TEST_PG (newSCIMEnv).
func TestSCIMResumeLeavesWhatTheReactivatedPersonMade(t *testing.T) {
	purge := func(c *Config) { c.SCIM.PurgeAfter = 48 * time.Hour }
	e := newSCIMEnv(t, purge)
	f := e.seedPurgeSubject(purgeSub, purgeEmail, purgeOID)
	ctx := context.Background()
	before := e.seedRun(purgeSub, types.RunRunning)
	beforeTok, _ := e.seedToken(purgeSub, purgeEmail)
	e.a.runner.setFailKills(1000)
	if w := e.patch(e.a, f.id, patchOf("false")); w.Code != http.StatusInternalServerError {
		t.Fatalf("suspend with a failing runner = %d, want 5xx", w.Code)
	}
	e.a.runner.setFailKills(0)
	// The suspension stopped right after step 1: its sweep is pending again and the token it revoked is live.
	if _, err := e.pool.Exec(ctx, `UPDATE deprovision_jobs SET state = 'pending' WHERE identity_id = $1 AND step = $2`, f.id, jobStepSweep); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE api_tokens SET revoked_at = NULL WHERE id = $1`, beforeTok); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE deprovision_jobs SET updated_at = now() - interval '1 hour' WHERE identity_id = $1`, f.id); err != nil {
		t.Fatal(err)
	}
	// A key from before the suspension that the stopped sweep never reached.
	beforeKey := e.seedKey(purgeSub)
	if _, err := e.pool.Exec(ctx, `UPDATE ssh_public_keys SET inserted_at = (SELECT deactivated_at FROM principal_identities WHERE id = $2) - interval '1 second'
		WHERE fingerprint = $1`, beforeKey, f.id); err != nil {
		t.Fatal(err)
	}

	var after, at, atTok, afterTok uuid.UUID
	var afterRaw, afterKey, atKey string
	var mark int64
	reads := &afterIdentityRead{PG: e.st, id: uuid.MustParse(f.id), hook: func(read store.PrincipalIdentity) {
		if read.DeactivatedAt == nil {
			t.Error("the resume read an active identity")
			return
		}
		bound := *read.DeactivatedAt
		if w := e.patch(e.b, f.id, patchOf("true")); w.Code != http.StatusOK {
			t.Errorf("reactivate = %d %s", w.Code, w.Body.String())
		}
		mark = e.cutMark(purgeSub, purgeEmail)
		after = e.seedRun(purgeSub, types.RunRunning)
		afterTok, afterRaw = e.seedToken(purgeSub, purgeEmail)
		at = e.seedRun(purgeSub, types.RunRunning)
		atTok, _ = e.seedToken(purgeSub, purgeEmail)
		afterKey, atKey = e.seedKey(purgeSub), e.seedKey(purgeSub)
		if _, err := e.pool.Exec(ctx, `UPDATE ssh_public_keys SET inserted_at = $2 WHERE fingerprint = $1`, atKey, bound); err != nil {
			t.Error(err)
		}
		if _, err := e.pool.Exec(ctx, `UPDATE agent_runs SET inserted_at = $2 WHERE id = $1`, at, bound); err != nil {
			t.Error(err)
		}
		if _, err := e.pool.Exec(ctx, `UPDATE api_tokens SET inserted_at = $2 WHERE id = $1`, atTok, bound); err != nil {
			t.Error(err)
		}
	}}
	n := e.node(purge, func(c *Config) { c.Store = reads })
	if err := n.srv.SweepSCIMPurge(ctx); err != nil {
		t.Errorf("sweep = %v", err)
	}
	if after == uuid.Nil {
		t.Fatal("the resume never read the identity")
	}

	if state := e.runState(after); state != types.RunRunning {
		t.Errorf("the run made after the reactivation is %s, want RUNNING", state)
	}
	if e.tokenRevoked(afterTok) || !e.tokenWorks(n, afterRaw) {
		t.Error("the token made after the reactivation was revoked or no longer authenticates")
	}
	if got := e.cutMark(purgeSub, purgeEmail); got != mark {
		t.Errorf("the resume wrote a new session cut (newest seq %d, was %d)", got, mark)
	}
	if !e.keyExists(afterKey) || e.keyExists(atKey) || e.keyExists(beforeKey) {
		t.Errorf("keys after the resume: after %v, at %v, before %v; want only the one added after the reactivation",
			e.keyExists(afterKey), e.keyExists(atKey), e.keyExists(beforeKey))
	}
	if state := e.runState(at); state != types.RunKilled {
		t.Errorf("the run stamped at the deactivation is %s, want KILLED", state)
	}
	if !e.tokenRevoked(beforeTok) || !e.tokenRevoked(atTok) {
		t.Errorf("tokens made up to the deactivation: before revoked %v, at revoked %v; want both", e.tokenRevoked(beforeTok), e.tokenRevoked(atTok))
	}
	jobs, err := e.st.ListDeprovisionJobs(ctx, uuid.MustParse(f.id), store.JobKindSuspend)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		run    uuid.UUID
		killed int
	}{{before, 1}, {at, 1}, {after, 0}} {
		j, ok := jobByKey(jobs, jobStepKillRun, c.run.String())
		if !ok || !j.Done || j.Detail["runs_killed"] != c.killed {
			t.Errorf("kill row of run %s = %+v (found %v), want done with runs_killed %d", c.run, j, ok, c.killed)
		}
	}
}

// The identity provider deactivates a person and reactivates them right after that request's step 1 commits.
// The first request still finishes what its deactivation owed: the run, the token and the SSH key from before
// it are killed, revoked and deleted. What the person made after the reactivation stands, and no new session
// cut is written. Guarded by WARDYN_TEST_PG (newSCIMEnv).
func TestSCIMFirstSuspensionLeavesWhatAReactivatedPersonMade(t *testing.T) {
	e := newSCIMEnv(t)
	f := e.seedPurgeSubject(purgeSub, purgeEmail, purgeOID)
	before := e.seedRun(purgeSub, types.RunRunning)
	beforeTok, _ := e.seedToken(purgeSub, purgeEmail)
	beforeKey := e.seedKey(purgeSub)
	var after, afterTok uuid.UUID
	var afterRaw, afterKey string
	var mark int64
	paused := &pausedSuspension{PG: e.st, afterStep1: func() {
		if w := e.patch(e.b, f.id, patchOf("true")); w.Code != http.StatusOK {
			t.Errorf("reactivate = %d %s", w.Code, w.Body.String())
		}
		mark = e.cutMark(purgeSub, purgeEmail)
		after = e.seedRun(purgeSub, types.RunRunning)
		afterTok, afterRaw = e.seedToken(purgeSub, purgeEmail)
		afterKey = e.seedKey(purgeSub)
	}}
	n := e.node(func(c *Config) { c.Store = paused })
	if w := e.patch(n, f.id, patchOf("false")); w.Code != http.StatusOK {
		t.Fatalf("suspend = %d %s", w.Code, w.Body.String())
	}
	if after == uuid.Nil {
		t.Fatal("step 1 never ran")
	}
	if e.identity(f.id).DeactivatedAt != nil {
		t.Error("the reactivation did not stand")
	}
	if state := e.runState(after); state != types.RunRunning {
		t.Errorf("the run made after the reactivation is %s, want RUNNING", state)
	}
	if e.tokenRevoked(afterTok) || !e.tokenWorks(n, afterRaw) || !e.keyExists(afterKey) {
		t.Error("a token or key made after the reactivation was revoked, stopped authenticating or was deleted")
	}
	if got := e.cutMark(purgeSub, purgeEmail); got != mark {
		t.Errorf("the first request's tail wrote a new session cut (newest seq %d, was %d)", got, mark)
	}
	if state := e.runState(before); state != types.RunKilled {
		t.Errorf("the run from before the suspension is %s, want KILLED", state)
	}
	if !e.tokenRevoked(beforeTok) || e.keyExists(beforeKey) {
		t.Error("the token or key from before the suspension was left")
	}
}

// A suspension nobody reversed spares nothing, whatever the insert stamps say: while the person is still
// deactivated, rows stamped a day after the deactivation go as at base. For a bound person that is a run, an API
// token, an SSH key and a token of a service identity they sponsor (whose mint reads no identity guard); for a
// person no sign-in bound, whose principal no identity row guards, a run and a token. Guarded by WARDYN_TEST_PG
// (newSCIMEnv).
func TestSCIMSuspensionOfAPersonStillDeactivatedSparesNothing(t *testing.T) {
	ahead := func(e *scimEnv, table, key string, id any) {
		e.t.Helper()
		if _, err := e.pool.Exec(context.Background(), `UPDATE `+table+` SET inserted_at = clock_timestamp() + interval '1 day' WHERE `+key+` = $1`, id); err != nil {
			e.t.Fatal(err)
		}
	}
	t.Run("bound", func(t *testing.T) {
		e := newSCIMEnv(t)
		f := e.seedPurgeSubject(purgeSub, purgeEmail, purgeOID)
		run := e.seedRun(purgeSub, types.RunRunning)
		tok, _ := e.seedToken(purgeSub, purgeEmail)
		key := e.seedKey(purgeSub)
		si, err := e.st.CreateServiceIdentity(context.Background(), types.ServiceIdentity{Name: "ci-ahead", Sponsor: purgeSub, UserType: types.UserTypeStandard, CreatedBy: "seed"})
		if err != nil {
			t.Fatal(err)
		}
		svcTok, _ := e.seedToken(si.Principal(), "")
		ahead(e, "agent_runs", "id", run)
		ahead(e, "api_tokens", "id", tok)
		ahead(e, "api_tokens", "id", svcTok)
		ahead(e, "ssh_public_keys", "fingerprint", key)
		if w := e.patch(e.a, f.id, patchOf("false")); w.Code != http.StatusOK {
			t.Fatalf("suspend = %d %s", w.Code, w.Body.String())
		}
		if state := e.runState(run); state != types.RunKilled {
			t.Errorf("run = %s, want KILLED", state)
		}
		if !e.tokenRevoked(tok) || !e.tokenRevoked(svcTok) || e.keyExists(key) {
			t.Errorf("token revoked %v, sponsored token revoked %v, key left %v; want both revoked and the key deleted",
				e.tokenRevoked(tok), e.tokenRevoked(svcTok), e.keyExists(key))
		}
	})
	t.Run("unbound", func(t *testing.T) {
		e := newSCIMEnv(t)
		const addr = "unbound.leaver@corp.example"
		id := e.postUserID(e.a, leaverOID, addr, addr)
		run := e.seedRun(addr, types.RunRunning)
		tok, _ := e.seedToken(addr, addr)
		ahead(e, "agent_runs", "id", run)
		ahead(e, "api_tokens", "id", tok)
		if w := e.patch(e.a, id, patchOf("false")); w.Code != http.StatusOK {
			t.Fatalf("suspend = %d %s", w.Code, w.Body.String())
		}
		if state := e.runState(run); state != types.RunKilled || !e.tokenRevoked(tok) {
			t.Errorf("run = %s, token revoked %v; want KILLED and revoked", state, e.tokenRevoked(tok))
		}
	})
}

// A request's suspension spares a run its probe read as made after a reactivation, and the person is suspended
// again before it closes that run's kill row; the later suspension's kill of the run fails. The earlier pass
// closes no step of the later suspension's ledger, so the kill row stays pending for the sweeper, and it writes
// no audit row from that ledger. Guarded by WARDYN_TEST_PG (newSCIMEnv).
func TestSCIMEarlierSuspensionClosesNoStepOfALaterOne(t *testing.T) {
	e := newSCIMEnv(t)
	f := e.seedPurgeSubject(purgeSub, purgeEmail, purgeOID)
	var later uuid.UUID
	paused := &pausedSuspension{PG: e.st}
	paused.afterStep1 = func() {
		if w := e.patch(e.b, f.id, patchOf("true")); w.Code != http.StatusOK {
			t.Errorf("reactivate = %d %s", w.Code, w.Body.String())
		}
		later = e.seedRun(purgeSub, types.RunRunning)
	}
	paused.afterRunProbe = func() {
		e.b.runner.setFailKills(1000)
		if w := e.patch(e.b, f.id, patchOf("false")); w.Code != http.StatusInternalServerError {
			t.Errorf("the later suspension with a failing runner = %d %s, want 5xx", w.Code, w.Body.String())
		}
	}
	n := e.node(func(c *Config) { c.Store = paused })
	if w := e.patch(n, f.id, patchOf("false")); w.Code != http.StatusOK {
		t.Fatalf("suspend = %d %s", w.Code, w.Body.String())
	}
	if later == uuid.Nil {
		t.Fatal("step 1 never ran")
	}
	if j, ok := jobByKey(e.ledger(f.id), jobStepKillRun, later.String()); !ok || j.Done {
		t.Errorf("the later suspension's kill row of %s = %+v (found %v), want pending", later, j, ok)
	}
	if rows := e.rows(n, "person.deprovision"); len(rows) != 0 {
		t.Errorf("the earlier pass wrote %d person.deprovision rows from the later suspension's ledger, want none", len(rows))
	}
}

// A request's suspension spares a run its probe read as made after a reactivation; before it closes that run's kill
// row the person is suspended again, that suspension's kill of the run fails, and the person is reactivated again.
// The earlier pass reads the person active, as when it spared the run, but the ledger records the later
// deactivation: that suspension's kill row stays pending with its failure, and the earlier pass writes no audit row
// from it. Guarded by WARDYN_TEST_PG (newSCIMEnv).
func TestSCIMEarlierSuspensionLeavesALaterOnesFailedKillAfterAnotherReactivation(t *testing.T) {
	e := newSCIMEnv(t)
	f := e.seedPurgeSubject(purgeSub, purgeEmail, purgeOID)
	var later uuid.UUID
	paused := &pausedSuspension{PG: e.st}
	paused.afterStep1 = func() {
		if w := e.patch(e.b, f.id, patchOf("true")); w.Code != http.StatusOK {
			t.Errorf("reactivate = %d %s", w.Code, w.Body.String())
		}
		later = e.seedRun(purgeSub, types.RunRunning)
	}
	paused.afterRunProbe = func() {
		e.b.runner.setFailKills(1000)
		if w := e.patch(e.b, f.id, patchOf("false")); w.Code != http.StatusInternalServerError {
			t.Errorf("the later suspension with a failing runner = %d %s, want 5xx", w.Code, w.Body.String())
		}
		if w := e.patch(e.b, f.id, patchOf("true")); w.Code != http.StatusOK {
			t.Errorf("reactivate again = %d %s", w.Code, w.Body.String())
		}
	}
	n := e.node(func(c *Config) { c.Store = paused })
	if w := e.patch(n, f.id, patchOf("false")); w.Code != http.StatusOK {
		t.Fatalf("suspend = %d %s", w.Code, w.Body.String())
	}
	if later == uuid.Nil {
		t.Fatal("step 1 never ran")
	}
	if e.identity(f.id).DeactivatedAt != nil {
		t.Error("the second reactivation did not stand")
	}
	if j, ok := jobByKey(e.ledger(f.id), jobStepKillRun, later.String()); !ok || j.Done || j.LastError == "" {
		t.Errorf("the later suspension's kill row of %s = %+v (found %v), want pending with its failure", later, j, ok)
	}
	if rows := e.rows(n, "person.deprovision"); len(rows) != 0 {
		t.Errorf("the earlier pass wrote %d person.deprovision rows from the later suspension's ledger, want none", len(rows))
	}
}

// A person is reactivated and suspended again right after an earlier request's step 1, and the later suspension's
// request dies after its own step 1. The earlier pass still revokes and kills, which the later suspension owes
// too, but closes none of its steps: the sweep and kill rows stay pending for the sweeper, the answer is 200 and
// it writes no person.deprovision row. Guarded by WARDYN_TEST_PG (newSCIMEnv).
func TestSCIMEarlierSuspensionLeavesALaterOnesSweepAndKillStepsPending(t *testing.T) {
	e := newSCIMEnv(t)
	f := e.seedPurgeSubject(purgeSub, purgeEmail, purgeOID)
	run := e.seedRun(purgeSub, types.RunRunning)
	paused := &pausedSuspension{PG: e.st}
	paused.afterStep1 = func() { e.suspendAgainStepOneOnly(f.id) }
	n := e.node(func(c *Config) { c.Store = paused })
	if w := e.patch(n, f.id, patchOf("false")); w.Code != http.StatusOK {
		t.Fatalf("suspend = %d %s", w.Code, w.Body.String())
	}
	if state := e.runState(run); state != types.RunKilled {
		t.Errorf("run = %s, want KILLED", state)
	}
	sweeps := 0
	for _, j := range e.ledger(f.id) {
		if j.Step != jobStepSweep && j.Step != jobStepKillRun {
			continue
		}
		if j.Step == jobStepSweep {
			sweeps++
		}
		if j.Done {
			t.Errorf("the earlier pass closed the later suspension's %s row for %q", j.Step, j.Target)
		}
	}
	if _, ok := jobByKey(e.ledger(f.id), jobStepKillRun, run.String()); !ok || sweeps == 0 {
		t.Errorf("kill row found %v, %d sweep rows; want the kill row and a sweep row, pending", ok, sweeps)
	}
	if rows := e.rows(n, "person.deprovision"); len(rows) != 0 {
		t.Errorf("the earlier pass wrote %d person.deprovision rows, want none", len(rows))
	}
}

// A person is reactivated and suspended again after an earlier pass wrote its scim.user.deactivate row and before
// it closed that step. The earlier pass closes neither audit step of the later suspension and still answers 200.
// Guarded by WARDYN_TEST_PG (newSCIMEnv).
func TestSCIMEarlierSuspensionClosesNoAuditStepOfALaterOne(t *testing.T) {
	e := newSCIMEnv(t)
	f := e.seedPurgeSubject(purgeSub, purgeEmail, purgeOID)
	paused := &pausedSuspension{PG: e.st}
	paused.beforeAuditFinish = func() { e.suspendAgainStepOneOnly(f.id) }
	n := e.node(func(c *Config) { c.Store = paused })
	if w := e.patch(n, f.id, patchOf("false")); w.Code != http.StatusOK {
		t.Fatalf("suspend = %d %s", w.Code, w.Body.String())
	}
	for _, step := range []string{jobStepAuditDeact, jobStepAuditDeprov} {
		if j, ok := jobByKey(e.ledger(f.id), step, ""); !ok || j.Done {
			t.Errorf("the later suspension's %s row = %+v (found %v), want pending", step, j, ok)
		}
	}
}

// A resumed suspension still writes the session cut a form owes: one whose cutoff predates the deactivation.
// Guarded by WARDYN_TEST_PG (newSCIMEnv).
func TestSCIMResumeCutsAFormNotCutSinceItsDeactivation(t *testing.T) {
	e := newSCIMEnv(t)
	f := e.seedPurgeSubject(purgeSub, purgeEmail, purgeOID)
	ctx := context.Background()
	e.seedRun(purgeSub, types.RunRunning)
	e.a.runner.setFailKills(1000)
	if w := e.patch(e.a, f.id, patchOf("false")); w.Code != http.StatusInternalServerError {
		t.Fatalf("suspend with a failing runner = %d, want 5xx", w.Code)
	}
	e.a.runner.setFailKills(0)
	for _, q := range []string{
		`UPDATE deprovision_jobs SET state = 'pending' WHERE identity_id = $1 AND step = '` + jobStepSweep + `'`,
		`UPDATE oidc_session_revocations SET revoked_at = (SELECT deactivated_at FROM principal_identities WHERE id = $1) - interval '1 second' WHERE sub = '` + purgeSub + `'`,
		`UPDATE deprovision_jobs SET updated_at = now() - interval '1 hour' WHERE identity_id = $1`,
	} {
		if _, err := e.pool.Exec(ctx, q, f.id); err != nil {
			t.Fatal(err)
		}
	}
	mark := e.cutMark(purgeSub, purgeEmail)
	if err := e.b.srv.SweepSCIMPurge(ctx); err != nil {
		t.Errorf("sweep = %v", err)
	}
	if got := e.cutMark(purgeSub, purgeEmail); got <= mark {
		t.Errorf("the resume wrote no cut for a form last cut before its deactivation (newest seq %d, was %d)", got, mark)
	}
}

// A suspension tail whose person is active again leaves a run they queued after its deactivation waiting in
// its pool's queue. Guarded by WARDYN_TEST_PG (newQueueEnv).
func TestPG_QueueSuspensionTailLeavesARunQueuedAfterAReactivation(t *testing.T) {
	ctx := context.Background()
	e := newQueueEnv(t)
	var bound time.Time
	if err := e.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&bound); err != nil {
		t.Fatal(err)
	}
	waiting, _ := e.waitingPerson(e.cappedPool("Busy", 1), "")
	if _, err := e.pg.IssueLoginIdentity(ctx, store.LoginIdentity{Principal: e.personP, Issuer: "https://idp.example", TenantID: "t", ObjectID: "o-1", Email: "person@corp.example"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	var id uuid.UUID
	if err := e.pool.QueryRow(ctx, `SELECT id FROM principal_identities WHERE principal = $1`, e.personP).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.srv.cancelQueuedRunsOf(withSuspensionBound(ctx, id, bound), e.personP, queueCancelSessionRevoked); err != nil {
		t.Fatal(err)
	}
	if got := e.run(waiting.ID); got.State != types.RunQueued || e.queueRows(waiting.ID) != 1 {
		t.Errorf("the run queued after the bound is %s with %d queue rows; want QUEUED and still waiting", got.State, e.queueRows(waiting.ID))
	}
}
