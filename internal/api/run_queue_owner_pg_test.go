// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The owner a waiting run will start as (pool-queue.md sections 7, 8 and 17): a revocation, a retired
// service identity, a new user type or a refusal of the owner's own while the run waits. Guarded by
// WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// revocations wires the session-revocation store the production adapter is, over the test database.
func (e *queueEnv) revocations() *pgTestRevocations {
	rev := &pgTestRevocations{pool: e.pool, st: e.pg}
	e.srv.cfg.SessionRevocations = rev
	return rev
}

// startOf is a start for a run queued now as auth, built the way a promotion builds it.
func (e *queueEnv) startOf(auth queueAuthority, run types.AgentRun) *queuedStart {
	e.t.Helper()
	raw, _ := json.Marshal(auth)
	launch, _ := json.Marshal(queuedLaunch{Request: json.RawMessage(`{}`)})
	st, err := newQueuedStart(context.Background(), context.Background(), store.PromotedRun{Run: run,
		Entry: store.RunQueueEntry{RunID: run.ID, Authority: raw, Launch: launch, QueuedAt: time.Now().UTC()}})
	if err != nil {
		e.t.Fatal(err)
	}
	return st
}

// A person whose sessions are revoked while their run waits never has it started: the start asks the
// revocation cutoff with the moment the run was queued, as every stored credential asks with its issue time.
func TestPG_QueueRevokedSessionNeverStarts(t *testing.T) {
	e := newQueueEnv(t)
	rev := e.revocations()
	pool := e.cappedPool("Busy", 1)
	waiting, occupant := e.waitingPerson(pool, "")
	if err := rev.RevokeSub(context.Background(), e.personP); err != nil {
		t.Fatal(err)
	}
	e.free(occupant)
	got := e.waitFailed(waiting.ID)
	if got.StatusDetail != "queue: "+string(types.RunEndIdentityDeactivated) || got.SPIFFEID != "" || len(e.audits(got.ID, "identity.mint")) != 0 {
		t.Errorf("a revoked person's run ended %q with spiffe %q; want identity_deactivated and nothing minted", got.StatusDetail, got.SPIFFEID)
	}
}

// Revoking a person's credentials cancels the runs they have waiting, as the system, with the reason.
func TestPG_QueueRevokeCancelsWaitingRuns(t *testing.T) {
	e := newQueueEnv(t)
	e.revocations()
	pool := e.cappedPool("Busy", 1)
	waiting, _ := e.waitingPerson(pool, "")
	if _, err := e.srv.revokePersonCredentials(withActor(context.Background(), types.ActorHuman, "secadmin"), e.personP); err != nil {
		t.Fatal(err)
	}
	if got := e.run(waiting.ID); got.State != types.RunKilled || e.queueRows(waiting.ID) != 0 {
		t.Errorf("after the revocation the waiting run is %s with %d queue rows; want KILLED and none", got.State, e.queueRows(waiting.ID))
	}
	if rows := e.audits(waiting.ID, "run.kill"); len(rows) != 1 || queueAuditData(t, rows[0])["reason"] != queueCancelSessionRevoked {
		t.Errorf("run.kill rows = %+v; want one naming the revocation", rows)
	}
}

// A retired service identity's waiting run never starts: the retirement cancels it, and the start of one
// queued under it refuses it.
func TestPG_QueueRetiredServiceIdentityNeverStarts(t *testing.T) {
	ctx := context.Background()
	e := newQueueEnv(t)
	e.revocations()
	if _, err := e.pg.IssueLoginIdentity(ctx, store.LoginIdentity{Principal: e.personP, Issuer: "https://idp.example", TenantID: "t", ObjectID: "o-1", Email: "person@corp.example"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	si, err := e.pg.CreateServiceIdentity(ctx, types.ServiceIdentity{Name: "ci-queue", Sponsor: e.personP, UserType: types.UserTypeStandard, CreatedBy: "seed"})
	if err != nil {
		t.Fatal(err)
	}
	ageSponsorships(t, e.pg.Pool)
	auth := queueAuthority{Sub: si.Principal(), Role: oidc.RoleUser, UserType: types.UserTypeStandard, Groups: []string{}, Service: si.Name, Sponsor: e.personP}
	if end := e.srv.queueOwnerStanding(e.startOf(auth, types.AgentRun{ID: uuid.New()})); end != nil {
		t.Fatalf("a live service identity's start ended: %+v", end)
	}

	pool := e.cappedPool("Busy", 1)
	e.occupy(pool)
	pool = e.reread(pool)
	run := types.AgentRun{ID: uuid.New(), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), CreatedBy: si.Principal(), Agent: "claude-code",
		ConfinementClass: types.CC2, State: types.RunQueued, RunnerTarget: "docker",
		RunnerPoolID: &pool.ID, RunnerPoolRevision: pool.Revision, RunnerPoolSelection: types.RunnerPoolSelectedExplicit}
	if _, err := e.pg.CreateQueuedRun(ctx, run, store.RunQueueEntry{PoolID: pool.ID, Owner: si.Principal(), QueuedAt: run.CreatedAt, DeadlineAt: run.CreatedAt.Add(time.Hour)}, 0); err != nil {
		t.Fatal(err)
	}
	if w := e.admin(http.MethodDelete, "/api/v1/service-identities/ci-queue", ""); w.Code != http.StatusOK {
		t.Fatalf("retire = %d %s", w.Code, w.Body)
	}
	if got := e.run(run.ID); got.State != types.RunKilled || e.queueRows(run.ID) != 0 {
		t.Errorf("the retired identity's waiting run is %s with %d queue rows; want KILLED", got.State, e.queueRows(run.ID))
	}
	if end := e.srv.queueOwnerStanding(e.startOf(auth, types.AgentRun{ID: uuid.New()})); end == nil || end.token != string(types.RunEndIdentityDeactivated) {
		t.Errorf("a retired service identity's start = %+v; want identity_deactivated", end)
	}
}

// The start judges the owner by the user type the current role mappings give them, not the one their
// session carried at launch: a person moved to a type whose agent door is shut has the run ended.
func TestPG_QueueRetypedOwnerIsJudgedByTheCurrentType(t *testing.T) {
	ctx := context.Background()
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 1)
	waiting, occupant := e.waitingPerson(pool, "")
	if _, err := e.pg.CreateUserType(ctx, types.UserType{ID: "contractor", Name: "Contractor"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pg.UpsertRoleMapping(ctx, types.RoleMapping{Value: "person@corp.example", Role: oidc.RoleUser, UserType: "contractor", CreatedBy: "seed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pg.UpsertCapabilityGrant(ctx, types.CapabilityGrant{SubjectType: types.CapabilitySubjectUserType,
		Subject: "contractor", Capability: capAgent, Value: "claude-code", Effect: types.CapabilityDeny, CreatedBy: "seed"}); err != nil {
		t.Fatal(err)
	}
	e.free(occupant)
	if got := e.waitFailed(waiting.ID); got.StatusDetail != "queue: capability_agent" {
		t.Errorf("a retyped owner's run ended %q; want the new type's agent refusal", got.StatusDetail)
	}
}

// The role a start acts with only narrows: a different derived role is the user tier, never a gain.
func TestNarrowRole(t *testing.T) {
	for _, tc := range []struct{ launched, derived, want string }{
		{oidc.RoleAdmin, oidc.RoleAdmin, oidc.RoleAdmin},
		{oidc.RoleAdmin, oidc.RoleUser, oidc.RoleUser},
		{oidc.RoleUser, oidc.RoleAdmin, oidc.RoleUser},
		{oidc.RoleAdmin, oidc.RoleSecurityAdmin, oidc.RoleUser},
	} {
		if got := narrowRole(tc.launched, tc.derived); got != tc.want {
			t.Errorf("narrowRole(%s, %s) = %s, want %s", tc.launched, tc.derived, got, tc.want)
		}
	}
}

// A waiting run whose owner's own state refuses the pool read (a user type that has gone) is admitted for
// its start to judge, never left to stop the pool: the run behind it starts too.
func TestPG_QueueOwnerRefusalDoesNotStallThePool(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 2)
	first, second := e.occupy(pool), e.occupy(pool)
	head := e.queuedLaunch(e.person, pool, "")
	next := e.queuedLaunch(e.other, pool, "")
	e.exec(`UPDATE run_queue SET authority = jsonb_set(authority, '{user_type}', '"gone"') WHERE run_id = $1`, head.ID)
	e.free(first)
	e.sweep()
	if got := e.run(head.ID); got.State == types.RunQueued {
		t.Fatalf("the head whose owner's type has gone is still waiting (%q); its start must judge it", got.StatusDetail)
	}
	e.free(second)
	e.sweep()
	e.waitStarted(next.ID)
}

// queueFaultStore is the PG store with faults a race or a busy store would cause.
type queueFaultStore struct {
	store.PG
	identityFails atomic.Int32
	stale         []types.AgentRun
}

func (f *queueFaultStore) IdentityBlocked(ctx context.Context, principal string, epoch int64) (bool, error) {
	if f.identityFails.Add(-1) >= 0 {
		return false, db.ErrLockNoCapacity
	}
	return f.PG.IdentityBlocked(ctx, principal, epoch)
}

func (f *queueFaultStore) StalePromotedRuns(ctx context.Context, staleAfter time.Duration) ([]types.AgentRun, error) {
	if f.stale != nil {
		return f.stale, nil
	}
	return f.PG.StalePromotedRuns(ctx, staleAfter)
}

// An owner check the store could not answer for a moment is asked again; it does not end a run that waited.
func TestPG_QueueOwnerCheckRetriesABusyStore(t *testing.T) {
	e := newQueueEnv(t)
	fs := &queueFaultStore{PG: e.pg}
	fs.identityFails.Store(1)
	e.srv.cfg.Store = fs
	st := e.startOf(queueAuthority{Sub: e.personP, Role: oidc.RoleUser, UserType: types.UserTypeStandard, Groups: []string{}}, types.AgentRun{ID: uuid.New()})
	if end := e.srv.queueOwnerStanding(st); end != nil {
		t.Errorf("one busy answer ended the start: %+v", end)
	}
}

// A run a promoter took before its create finished recording it is ended, so the create's error is true.
func TestPG_QueueAbortReachesARunAlreadyPromoted(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 1)
	waiting, _ := e.waitingPerson(pool, "")
	e.exec(`WITH moved AS (UPDATE agent_runs SET state = 'PENDING', admitted_at = now() WHERE id = $1 RETURNING id)
		DELETE FROM run_queue WHERE run_id IN (SELECT id FROM moved)`, waiting.ID)
	e.srv.abortQueuedRun(context.Background(), waiting.ID, createRunComponentsAbortHint)
	if got := e.run(waiting.ID); !got.State.IsTerminal() {
		t.Errorf("a promoted run whose create failed is %s; it must not go on to start", got.State)
	}
}

// A start's end and the stale-lease pass write their failure row only when their own transition won: a
// run another writer already ended gets no second row.
func TestPG_QueueEndsAuditOnlyWhatTheyEnded(t *testing.T) {
	ctx := context.Background()
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 1)
	waiting, _ := e.waitingPerson(pool, "")
	e.exec(`WITH moved AS (UPDATE agent_runs SET state = 'FAILED', admitted_at = now(), ended_at = now() WHERE id = $1 RETURNING id)
		DELETE FROM run_queue WHERE run_id IN (SELECT id FROM moved)`, waiting.ID)
	run := e.run(waiting.ID)
	e.srv.endPromoted(e.startOf(queueAuthority{Sub: e.personP}, run), &queueEnd{token: string(types.RunEndQueuePolicyChanged)})
	fs := &queueFaultStore{PG: e.pg, stale: []types.AgentRun{run}}
	e.srv.cfg.Store = fs
	_ = e.srv.failStalePromoted(ctx)
	if rows := e.audits(run.ID, "run.queue.start"); len(rows) != 0 {
		t.Errorf("run.queue.start rows for a run already ended = %d, want 0", len(rows))
	}
}

// Revoking every session and cutting only one person's browser sessions also stop a run that person
// queued from a session: the start asks both with the moment the run was queued.
func TestPG_QueueRevokeAllAndSessionCutStopAWaitingRun(t *testing.T) {
	for _, tc := range []struct {
		name   string
		revoke func(rev *pgTestRevocations, sub string) error
	}{
		{"revoke all", func(rev *pgTestRevocations, _ string) error { return rev.RevokeAll(context.Background()) }},
		{"sessions only", func(rev *pgTestRevocations, sub string) error { return rev.CutSessions(context.Background(), sub) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newQueueEnv(t)
			rev := e.revocations()
			pool := e.cappedPool("Busy", 1)
			waiting, occupant := e.waitingPerson(pool, "")
			if err := tc.revoke(rev, e.personP); err != nil {
				t.Fatal(err)
			}
			e.free(occupant)
			if got := e.waitFailed(waiting.ID); got.StatusDetail != "queue: "+string(types.RunEndIdentityDeactivated) || got.SPIFFEID != "" {
				t.Errorf("the run ended %q with spiffe %q; want identity_deactivated and nothing minted", got.StatusDetail, got.SPIFFEID)
			}
		})
	}
}

// A run that names its runner and finds it busy holds only itself; an unpinned run whose runners are all
// busy holds its owner's later runs for the pass too.
func TestPG_QueueAdmitHoldsAPinnedRunAlone(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 1)
	delete(e.runner.execFacts, "alpha")
	e.runner.silent = map[string]bool{"alpha": true}
	for _, tc := range []struct {
		request string
		owner   bool
	}{{`{"agent":"claude-code"}`, true}, {`{"agent":"claude-code","runner_id":"` + uuid.NewString() + `"}`, false}} {
		launch, _ := json.Marshal(queuedLaunch{Request: json.RawMessage(tc.request)})
		auth, _ := json.Marshal(queueAuthority{Sub: e.personP, Role: oidc.RoleUser, UserType: types.UserTypeStandard, Groups: []string{}})
		v, err := e.srv.queueAdmit(context.Background(), e.reread(pool), store.RunQueueEntry{RunID: uuid.New(), Owner: e.personP, Launch: launch, Authority: auth})
		if err != nil || v.Admit || v.Wait != "runner_pool_no_runner_free" || v.Owner != tc.owner {
			t.Errorf("%s: verdict %+v, %v; want a no_runner_free hold with owner scope %v", tc.request, v, err, tc.owner)
		}
	}
}

// A waiting run pinned to one of its owner's runners is judged by that runner alone: with the pinned
// runner offline it keeps waiting, owner-scope off, though another of the owner's runners is online; the
// same run pinned to the online runner is admitted.
func TestPG_QueuePinnedRunWaitsForItsOwnRunner(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.createPool("Laptops", types.RunnerPoolUser)
	asleep := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2)
	awake := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2)
	e.addToPool(e.person, pool.ID, asleep)
	e.addToPool(e.person, pool.ID, awake)
	e.online[asleep] = false
	auth, _ := json.Marshal(queueAuthority{Sub: e.personP, Role: oidc.RoleUser, UserType: types.UserTypeStandard, Groups: []string{}})
	for _, tc := range []struct {
		pin   uuid.UUID
		admit bool
	}{{asleep, false}, {awake, true}} {
		launch, _ := json.Marshal(queuedLaunch{Request: json.RawMessage(`{"agent":"claude-code","placement":"local","runner_id":"` + tc.pin.String() + `"}`)})
		v, err := e.srv.queueAdmit(context.Background(), e.reread(pool), store.RunQueueEntry{RunID: uuid.New(), Owner: e.personP, Launch: launch, Authority: auth})
		if err != nil || v.Admit != tc.admit || v.Owner {
			t.Errorf("pinned to %s: verdict %+v, %v; want admit %v and no owner scope", tc.pin, v, err, tc.admit)
		}
		if !tc.admit && v.Wait != "runner_pool_no_runner_free" {
			t.Errorf("a run whose pinned runner is offline waits with %q", v.Wait)
		}
	}
}
