// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Repeating schedules against Postgres (#3458): a tick books one one-time scheduled run through the create path
// as the owner, and pauses a schedule whose owner may no longer act. Guarded by WARDYN_TEST_PG: skipped cleanly
// when unset, must PASS when set.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type schedEnv struct {
	*queueEnv
	runnerPool types.RunnerPool
}

// newSchedEnv is a queue env with scheduled runs on, a pool with room, and the person's SSO sign-in recorded
// with no groups.
func newSchedEnv(t *testing.T) *schedEnv {
	t.Helper()
	q := newQueueEnv(t)
	t.Setenv(envScheduledRuns, "1")
	e := &schedEnv{queueEnv: q, runnerPool: q.cappedPool("Night", 5)}
	e.signIn(nil)
	return e
}

// signIn records the person's latest SSO sign-in with groups, as the sign-in gate does.
func (e *schedEnv) signIn(groups []string) {
	e.t.Helper()
	if err := e.pg.PutPrincipalGroups(context.Background(), e.personP, "", "person@corp.example", groups, false, time.Time{}); err != nil {
		e.t.Fatal(err)
	}
}

// personSession is the person's session carrying groups (nil: no snapshot) and a revocation sequence, issued now.
func personSession(t *testing.T, groups []string, seq int64) *http.Cookie {
	t.Helper()
	now := time.Now().UTC()
	payload, err := json.Marshal(oidc.Session{V: oidc.SessionCodecVersion, Sub: "sub-person", Email: "person@corp.example", Role: oidc.RoleUser,
		UserType: types.UserTypeStandard, Groups: groups, RevocationSeq: seq, IssuedAt: now.Add(time.Second), Expiry: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	return signedSessionCookie(payload)
}

func (e *schedEnv) scheduleBody(every, extra string) string {
	return `{"name":"Nightly","every":"` + every + `","time":"09:00","time_zone":"Europe/London","request":` +
		endBody("background", `"runner_pool_id":"`+e.runnerPool.ID.String()+`"`+extra) + `}`
}

func (e *schedEnv) createAs(c *http.Cookie, extra string) runScheduleView {
	e.t.Helper()
	w := e.call(c, http.MethodPost, "/api/v1/run-schedules", e.scheduleBody(scheduleEveryDaily, extra))
	if w.Code != http.StatusCreated {
		e.t.Fatalf("create schedule = %d %s", w.Code, w.Body)
	}
	return decodeJSON[runScheduleView](e.t, w)
}

func (e *schedEnv) routeAs(c *http.Cookie, method string, id uuid.UUID, verb string) (int, string) {
	e.t.Helper()
	path := "/api/v1/run-schedules/" + id.String()
	if verb != "" {
		path += "/" + verb
	}
	w := e.call(c, method, path, "")
	return w.Code, w.Body.String()
}

// dueIn puts a schedule's next start d from the database's now, on a whole second, and returns it.
func (e *schedEnv) dueIn(id uuid.UUID, d time.Duration) time.Time {
	e.t.Helper()
	var at time.Time
	if err := e.pool.QueryRow(context.Background(), `UPDATE run_schedules SET next_at = date_trunc('second', now() + make_interval(secs => $2))
		WHERE id = $1 RETURNING next_at`, id, d.Seconds()).Scan(&at); err != nil {
		e.t.Fatal(err)
	}
	return at
}

func (e *schedEnv) tick() {
	e.t.Helper()
	if err := e.srv.RunScheduleSweep(context.Background()); err != nil {
		e.t.Fatal(err)
	}
}

func (e *schedEnv) schedule(id uuid.UUID) store.RunSchedule {
	e.t.Helper()
	sc, err := e.pg.GetRunSchedule(context.Background(), id)
	if err != nil {
		e.t.Fatalf("read schedule %s: %v", id, err)
	}
	return sc
}

func (e *schedEnv) gone(id uuid.UUID) bool {
	_, err := e.pg.GetRunSchedule(context.Background(), id)
	return errors.Is(err, store.ErrNotFound)
}

// bookedRuns are the person's runs in the schedule pool, oldest first.
func (e *schedEnv) bookedRuns() []uuid.UUID {
	e.t.Helper()
	rows, err := e.pool.Query(context.Background(), `SELECT id FROM agent_runs WHERE created_by = $1 AND runner_pool_id = $2 ORDER BY created_at, id`,
		e.personP, e.runnerPool.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}

// scheduleAudits are the rows of action written for the schedule id.
func (e *schedEnv) scheduleAudits(id uuid.UUID, action string) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range e.h.audit.snapshot() {
		if ev.Action == action && ev.Target == id.String() {
			out = append(out, ev)
		}
	}
	return out
}

func (e *schedEnv) wantPaused(id uuid.UUID, reason string) store.RunSchedule {
	e.t.Helper()
	sc := e.schedule(id)
	if sc.PausedAt == nil || sc.PausedReason != reason {
		e.t.Fatalf("schedule paused %v with %q, want paused %q", sc.PausedAt != nil, sc.PausedReason, reason)
	}
	return sc
}

// scheduleQueueCapture records the authority of every queue row the create writes.
type scheduleQueueCapture struct {
	store.PG
	authority []json.RawMessage
}

func (c *scheduleQueueCapture) CreateQueuedRun(ctx context.Context, r types.AgentRun, en store.RunQueueEntry, maxPerOwner int) (types.AgentRun, error) {
	c.authority = append(c.authority, en.Authority)
	return c.PG.CreateQueuedRun(ctx, r, en, maxPerOwner)
}

func (e *schedEnv) captureQueue() *scheduleQueueCapture {
	c := &scheduleQueueCapture{PG: e.pg}
	e.srv.cfg.Store = c
	return c
}

func (c *scheduleQueueCapture) only(t *testing.T) queueAuthority {
	t.Helper()
	if len(c.authority) != 1 {
		t.Fatalf("queue rows written = %d, want 1", len(c.authority))
	}
	var a queueAuthority
	if err := json.Unmarshal(c.authority[0], &a); err != nil {
		t.Fatal(err)
	}
	return a
}

// One tick books one QUEUED one-time scheduled run starting at the slot, and advances the schedule; a second tick
// before the advance books none, because it meets the same Idempotency-Key. Positive control: this test must fail
// if the booking's create (scheduleSweep.book's call of handleCreateRunRoute) is replaced by a no-op.
func TestRunSchedule_BooksOneScheduledRun_PG(t *testing.T) {
	e := newSchedEnv(t)
	v := e.createAs(e.person, "")
	due := e.dueIn(v.ID, time.Minute)
	e.tick()
	runs := e.bookedRuns()
	if len(runs) != 1 {
		t.Fatalf("booked runs = %d, want 1", len(runs))
	}
	if r := e.run(runs[0]); r.State != types.RunQueued {
		t.Errorf("the booked run is %s, want QUEUED", r.State)
	}
	var startAt time.Time
	if err := e.pool.QueryRow(context.Background(), `SELECT start_at FROM run_queue WHERE run_id = $1`, runs[0]).Scan(&startAt); err != nil || !startAt.Equal(due) {
		t.Errorf("the booked run's start_at = %s (%v), want the slot %s", startAt, err, due)
	}
	sc := e.schedule(v.ID)
	if sc.LastRunID == nil || *sc.LastRunID != runs[0] || sc.LastOutcome != scheduleOutcomeBooked || !sc.NextAt.After(due) {
		t.Errorf("after the booking the schedule is %+v; want last run %s booked and next_at after %s", sc, runs[0], due)
	}
	if fires := e.scheduleAudits(v.ID, "run.schedule.fire"); len(fires) != 1 || fires[0].Outcome != "success" {
		t.Errorf("run.schedule.fire rows = %+v, want one success", fires)
	}
	e.exec(`UPDATE run_schedules SET next_at = $2 WHERE id = $1`, v.ID, due)
	e.tick()
	if again := e.bookedRuns(); len(again) != 1 {
		t.Errorf("a second tick for the same slot booked %d runs in all, want 1", len(again))
	}
}

// The advance is a compare-and-set: a second advance of a slot already advanced writes nothing.
func TestRunSchedule_AdvanceCAS_PG(t *testing.T) {
	e := newSchedEnv(t)
	v := e.createAs(e.person, "")
	due := e.dueIn(v.ID, time.Minute)
	e.tick()
	before := e.schedule(v.ID)
	ok, err := e.pg.AdvanceRunSchedule(context.Background(), v.ID, due, due.Add(24*time.Hour), nil, scheduleOutcomeMissed)
	if err != nil || ok {
		t.Fatalf("a second advance of slot %s = %v %v, want false", due, ok, err)
	}
	if after := e.schedule(v.ID); after.LastOutcome != scheduleOutcomeBooked || !after.NextAt.Equal(before.NextAt) {
		t.Errorf("the refused advance changed the schedule: %+v", after)
	}
}

func TestRunSchedule_PausesOnDeactivation_PG(t *testing.T) {
	e := newSchedEnv(t)
	v := e.createAs(e.person, "")
	if _, err := e.pg.IssueLoginIdentity(context.Background(), store.LoginIdentity{Principal: e.personP, Issuer: "https://idp.example",
		TenantID: "t", ObjectID: "o-1", Email: "person@corp.example"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	e.exec(`UPDATE principal_identities SET deactivated_at = now() WHERE principal = $1`, e.personP)
	e.dueIn(v.ID, time.Minute)
	e.tick()
	e.wantPaused(v.ID, schedulePausedIdentity)
	if n := len(e.bookedRuns()); n != 0 {
		t.Errorf("a deactivated owner's schedule booked %d runs", n)
	}
	if rows := e.scheduleAudits(v.ID, "run.schedule.pause"); len(rows) != 1 || queueAuditData(t, rows[0])["reason"] != schedulePausedIdentity {
		t.Errorf("run.schedule.pause rows = %+v", rows)
	}
}

func TestRunSchedule_PausesOnRevocation_PG(t *testing.T) {
	e := newSchedEnv(t)
	rev := e.revocations()
	v := e.createAs(e.person, "")
	if err := rev.RevokeSub(context.Background(), e.personP); err != nil {
		t.Fatal(err)
	}
	e.dueIn(v.ID, time.Minute)
	e.tick()
	e.wantPaused(v.ID, schedulePausedIdentity)
	if n := len(e.bookedRuns()); n != 0 {
		t.Errorf("a revoked owner's schedule booked %d runs", n)
	}
}

// An owner with no SSO sign-in for 30 days, or none recorded at all, pauses owner_idle.
func TestRunSchedule_PausesAfter30DaysIdle_PG(t *testing.T) {
	for name, change := range map[string]string{
		"signed in 31 days ago": `UPDATE principal_groups SET signed_in_at = now() - interval '31 days' WHERE principal = $1`,
		"no sign-in recorded":   `DELETE FROM principal_groups WHERE principal = $1`,
	} {
		t.Run(name, func(t *testing.T) {
			e := newSchedEnv(t)
			v := e.createAs(e.person, "")
			e.exec(change, e.personP)
			e.dueIn(v.ID, time.Minute)
			e.tick()
			e.wantPaused(v.ID, schedulePausedOwnerIdle)
			if n := len(e.bookedRuns()); n != 0 {
				t.Errorf("an idle owner's schedule booked %d runs", n)
			}
		})
	}
}

// A profile that denies scheduled runs after the schedule was made refuses the booking 403, which pauses it.
func TestRunSchedule_PausesOnGovernanceDeny_PG(t *testing.T) {
	e := newSchedEnv(t)
	v := e.createAs(e.person, "")
	e.seedAssignment(e.seedScheduleUnit(uuid.Nil, types.GovernanceLimits{DenyScheduledRuns: true}), e.personP)
	e.dueIn(v.ID, time.Minute)
	e.tick()
	sc := e.wantPaused(v.ID, scheduleRefusedPrefix+reasonScheduledRunsDenied)
	if sc.LastOutcome != scheduleOutcomeRefused || len(e.bookedRuns()) != 0 {
		t.Errorf("schedule %+v with %d runs; want outcome refused and no run", sc, len(e.bookedRuns()))
	}
}

// A full queue skips the slot and leaves the schedule active for the next.
func TestRunSchedule_SkipsOnQueueFull_PG(t *testing.T) {
	e := newSchedEnv(t)
	v := e.createAs(e.person, "")
	pool := e.reread(e.runnerPool)
	for i := range queueMaxPerOwner {
		now := time.Now().UTC().Add(time.Duration(i) * time.Millisecond)
		if _, err := e.pg.CreateQueuedRun(context.Background(), types.AgentRun{
			ID: uuid.New(), CreatedAt: now, UpdatedAt: now, CreatedBy: e.personP, Agent: "claude-code", ConfinementClass: types.CC2,
			State: types.RunQueued, RunnerTarget: "docker", RunnerPoolID: &pool.ID, RunnerPoolRevision: pool.Revision, RunnerPoolSelection: types.RunnerPoolSelectedExplicit,
		}, store.RunQueueEntry{QueuedAt: now, DeadlineAt: now.Add(time.Hour)}, 0); err != nil {
			t.Fatal(err)
		}
	}
	due := e.dueIn(v.ID, time.Minute)
	e.tick()
	sc := e.schedule(v.ID)
	if sc.PausedAt != nil || !strings.HasPrefix(sc.LastOutcome, scheduleOutcomeSkippedPrefix) || !sc.NextAt.After(due) {
		t.Errorf("after a full queue the schedule is %+v; want active, skipped and advanced", sc)
	}
	if n := len(e.bookedRuns()); n != queueMaxPerOwner {
		t.Errorf("runs = %d, want only the %d seeded", n, queueMaxPerOwner)
	}
}

// A slot that passed unbooked is recorded missed and never started late: an hour old with no booking under its
// key (the replay meets start_at_past), and 25 hours old with no replay at all.
func TestRunSchedule_MissedIsSkipped_PG(t *testing.T) {
	for name, age := range map[string]time.Duration{"an hour old": time.Hour, "25 hours old": 25 * time.Hour} {
		t.Run(name, func(t *testing.T) {
			e := newSchedEnv(t)
			v := e.createAs(e.person, "")
			e.dueIn(v.ID, -age)
			e.tick()
			sc := e.schedule(v.ID)
			if sc.LastOutcome != scheduleOutcomeMissed || !sc.NextAt.After(time.Now()) || sc.PausedAt != nil {
				t.Errorf("after a missed slot the schedule is %+v; want missed, next_at ahead, active", sc)
			}
			if n := len(e.bookedRuns()); n != 0 {
				t.Errorf("a missed slot booked %d runs", n)
			}
			if fires := e.scheduleAudits(v.ID, "run.schedule.fire"); len(fires) != 1 || queueAuditData(t, fires[0])["reason"] != scheduleOutcomeMissed {
				t.Errorf("run.schedule.fire rows = %+v, want one naming missed", fires)
			}
		})
	}
}

func TestRunSchedule_ErasureDeletesSchedules_PG(t *testing.T) {
	e := newSchedEnv(t)
	v := e.createAs(e.person, "")
	if _, err := e.srv.eraseRunTasks(context.Background(), e.personP); err != nil {
		t.Fatal(err)
	}
	if !e.gone(v.ID) {
		t.Error("the run tasks erasure left the person's schedule")
	}
}

// A member sees, pauses, resumes and deletes their own schedules only: another member's id is the missing 404. An
// operator's admin view lists everyone's with the owner.
func TestRunSchedule_ListScopedToOwner_PG(t *testing.T) {
	e := newSchedEnv(t)
	v := e.createAs(e.person, "")
	list := func(c *http.Cookie, query string) []runScheduleView {
		w := e.call(c, http.MethodGet, "/api/v1/run-schedules"+query, "")
		if w.Code != http.StatusOK {
			t.Fatalf("list = %d %s", w.Code, w.Body)
		}
		return decodeJSON[[]runScheduleView](t, w)
	}
	if got := list(e.person, ""); len(got) != 1 || got[0].ID != v.ID || got[0].Owner != "" {
		t.Errorf("the owner's list = %+v", got)
	}
	if got := list(e.other, ""); len(got) != 0 {
		t.Errorf("another member's list = %+v, want empty", got)
	}
	if got := list(e.other, "?view=admin"); len(got) != 0 {
		t.Errorf("another member's view=admin = %+v, want their own (none)", got)
	}
	for _, route := range []struct{ method, verb string }{{http.MethodPost, "pause"}, {http.MethodPost, "resume"}, {http.MethodDelete, ""}} {
		if code, body := e.routeAs(e.other, route.method, v.ID, route.verb); code != http.StatusNotFound || !strings.Contains(body, reasonRunScheduleNotFound) {
			t.Errorf("another member's %s %s = %d %s, want 404 %s", route.method, route.verb, code, body, reasonRunScheduleNotFound)
		}
	}
	if got := list(e.alice, "?view=admin"); !slices.ContainsFunc(got, func(s runScheduleView) bool { return s.ID == v.ID && s.Owner == e.personP }) {
		t.Errorf("the admin view = %+v, want the person's schedule with its owner", got)
	}
	if sc := e.schedule(v.ID); sc.PausedAt != nil {
		t.Error("another member's pause paused the schedule")
	}
}

// A resume runs the checks again as the resuming session and stores its authority afresh: a schedule paused for a
// revocation books again after its owner signs in and resumes.
func TestRunSchedule_ResumeTakesFreshAuthority_PG(t *testing.T) {
	e := newSchedEnv(t)
	rev := e.revocations()
	v := e.createAs(e.person, "")
	if err := rev.RevokeSub(context.Background(), e.personP); err != nil {
		t.Fatal(err)
	}
	e.dueIn(v.ID, time.Minute)
	e.tick()
	before := e.wantPaused(v.ID, schedulePausedIdentity)
	time.Sleep(10 * time.Millisecond)
	if code, body := e.routeAs(personSession(t, nil, 0), http.MethodPost, v.ID, "resume"); code != http.StatusOK {
		t.Fatalf("resume = %d %s", code, body)
	}
	after := e.schedule(v.ID)
	if after.PausedAt != nil || !after.AuthorityAt.After(before.AuthorityAt) {
		t.Fatalf("after the resume the schedule is %+v; want active with a fresh authority", after)
	}
	if rows := e.scheduleAudits(v.ID, "run.schedule.resume"); len(rows) != 1 {
		t.Errorf("run.schedule.resume rows = %d, want 1", len(rows))
	}
	e.dueIn(v.ID, time.Minute)
	e.tick()
	if n := len(e.bookedRuns()); n != 1 {
		t.Errorf("the resumed schedule booked %d runs, want 1", n)
	}
}

// bumpedSeq makes a revocation of somebody else so the sequence is not 0, and returns it.
func bumpedSeq(t *testing.T, rev *pgTestRevocations) int64 {
	t.Helper()
	if err := rev.RevokeSub(context.Background(), "sub-somebody-else"); err != nil {
		t.Fatal(err)
	}
	seq, err := rev.RevocationSeq(context.Background())
	if err != nil || seq == 0 {
		t.Fatalf("revocation sequence = %d %v, want above 0", seq, err)
	}
	return seq
}

// A "sign out everywhere" that lands inside the booking's create, after the sweep's owner check and before the
// queue row, is caught by the check after the 201: the booked run is killed session_revoked and the schedule
// pauses. The booked row carries the schedule's revocation sequence, not 0. This test must fail if the post-201
// owner re-check (scheduleSweep.booked's live call) is removed.
func TestRunSchedule_RevokedBetweenCheckAndBook_PG(t *testing.T) {
	e := newSchedEnv(t)
	rev := e.revocations()
	seq := bumpedSeq(t, rev)
	v := e.createAs(personSession(t, nil, seq), "")
	capture := e.captureQueue()
	revoked := false
	e.srv.queueRace = func(point string) {
		if point == queueRaceEnqueueAfterFold && !revoked {
			revoked = true
			if err := rev.RevokeSub(context.Background(), e.personP); err != nil {
				t.Error(err)
			}
		}
	}
	e.dueIn(v.ID, time.Minute)
	e.tick()
	runs := e.bookedRuns()
	if !revoked || len(runs) != 1 {
		t.Fatalf("revoked in the window %v, booked runs %d; want the revocation inside one booking", revoked, len(runs))
	}
	if r := e.run(runs[0]); r.State != types.RunKilled {
		t.Errorf("the run booked across the revocation is %s, want KILLED", r.State)
	}
	if rows := e.audits(runs[0], "run.kill"); len(rows) != 1 || queueAuditData(t, rows[0])["reason"] != queueCancelSessionRevoked {
		t.Errorf("run.kill rows = %+v, want one naming %s", rows, queueCancelSessionRevoked)
	}
	e.wantPaused(v.ID, schedulePausedIdentity)
	if a := capture.only(t); a.Seq != seq {
		t.Errorf("the booked queue row's revocation sequence = %d, want the schedule's %d", a.Seq, seq)
	}
}

// A booking judges revocations from the schedule's stored issue: its queue row carries the stored sequence and
// epoch.
func TestRunSchedule_BookingCarriesStoredRevocationSeq_PG(t *testing.T) {
	e := newSchedEnv(t)
	rev := e.revocations()
	seq := bumpedSeq(t, rev)
	v := e.createAs(personSession(t, nil, seq), "")
	var stored queueAuthority
	if err := json.Unmarshal(e.schedule(v.ID).Authority, &stored); err != nil || stored.Seq != seq {
		t.Fatalf("stored authority sequence = %d (%v), want %d", stored.Seq, err, seq)
	}
	capture := e.captureQueue()
	e.dueIn(v.ID, time.Minute)
	e.tick()
	got := capture.only(t)
	sameEpoch := (got.Epoch == nil) == (stored.Epoch == nil) && (got.Epoch == nil || *got.Epoch == *stored.Epoch)
	if got.Seq != stored.Seq || !sameEpoch {
		t.Errorf("booked authority seq %d epoch %v, want the stored %d and %v", got.Seq, got.Epoch, stored.Seq, stored.Epoch)
	}
}

// A group the owner's latest sign-in dropped is gone from the booking.
func TestRunSchedule_GroupRefreshNarrows_PG(t *testing.T) {
	e := newSchedEnv(t)
	e.signIn([]string{"a", "b"})
	v := e.createAs(personSession(t, []string{"a", "b"}, 0), "")
	e.signIn([]string{"b"})
	capture := e.captureQueue()
	e.dueIn(v.ID, time.Minute)
	e.tick()
	if a := capture.only(t); strings.Join(a.Groups, ",") != "b" {
		t.Errorf("the booking's groups = %q, want [b]", a.Groups)
	}
}

// A group the owner gained since the schedule was made pauses it before it books, so a DENY on that group is
// never skipped: the owner re-confirms by resuming, and the DENY then applies. This test must fail if
// narrowScheduleGroups' gained result is ignored.
func TestRunSchedule_GainedGroupPausesBeforeBooking_PG(t *testing.T) {
	e := newSchedEnv(t)
	ctx := context.Background()
	e.signIn([]string{"a"})
	v := e.createAs(personSession(t, []string{"a"}, 0), "")
	deny := func() types.CapabilityGrant {
		g, err := e.pg.UpsertCapabilityGrant(ctx, types.CapabilityGrant{SubjectType: types.CapabilitySubjectGroup, Subject: "c",
			Capability: capAgent, Value: "claude-code", Effect: types.CapabilityDeny, CreatedBy: "seed"})
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	grant := deny()
	e.signIn([]string{"a", "c"})
	due := e.dueIn(v.ID, time.Minute)
	e.tick()
	sc := e.wantPaused(v.ID, schedulePausedGroupsChanged)
	if !sc.NextAt.Equal(due) || len(e.bookedRuns()) != 0 {
		t.Errorf("a gained group: next_at %s (want %s unchanged), %d runs (want none)", sc.NextAt, due, len(e.bookedRuns()))
	}
	if rows := e.scheduleAudits(v.ID, "run.schedule.pause"); len(rows) != 1 || queueAuditData(t, rows[0])["reason"] != schedulePausedGroupsChanged {
		t.Errorf("run.schedule.pause rows = %+v", rows)
	}
	withC := personSession(t, []string{"a", "c"}, 0)
	if code, body := e.routeAs(withC, http.MethodPost, v.ID, "resume"); code != http.StatusForbidden {
		t.Errorf("resume while the DENY on c stands = %d %s, want 403: the resume's own check applies it", code, body)
	}
	if _, err := e.pg.DeleteCapabilityGrant(ctx, grant.ID); err != nil {
		t.Fatal(err)
	}
	if code, body := e.routeAs(withC, http.MethodPost, v.ID, "resume"); code != http.StatusOK {
		t.Fatalf("resume = %d %s", code, body)
	}
	var stored queueAuthority
	if err := json.Unmarshal(e.schedule(v.ID).Authority, &stored); err != nil || strings.Join(stored.Groups, ",") != "a,c" {
		t.Fatalf("stored groups after the resume = %q (%v), want [a c]", stored.Groups, err)
	}
	deny()
	e.dueIn(v.ID, time.Minute)
	e.tick()
	if sc := e.schedule(v.ID); sc.PausedAt == nil || !strings.HasPrefix(sc.PausedReason, scheduleRefusedPrefix) || len(e.bookedRuns()) != 0 {
		t.Errorf("after the resume the DENY on c: schedule %+v, %d runs; want paused refused:<reason> and no run", sc, len(e.bookedRuns()))
	}
}

// A weekly schedule made just after its time starts the next day and repeats on that weekday; a resume whose
// next slot is six days out is not refused start_at_too_far, since each slot meets the cap at its own booking.
func TestRunSchedule_WeeklyCreatedJustAfterItsSlot_PG(t *testing.T) {
	e := newSchedEnv(t)
	slot := time.Now().UTC().Add(-time.Minute).Truncate(time.Minute)
	body := `{"name":"Weekly","every":"weekly","time":"` + slot.Format("15:04") + `","time_zone":"UTC","request":` +
		endBody("background", `"runner_pool_id":"`+e.runnerPool.ID.String()+`"`) + `}`
	w := e.call(e.person, http.MethodPost, "/api/v1/run-schedules", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	v := decodeJSON[runScheduleView](t, w)
	if want := int(slot.Add(24 * time.Hour).Weekday()); v.Weekday == nil || *v.Weekday != want || !v.NextAt.Equal(slot.Add(24*time.Hour)) {
		t.Errorf("weekly made after its time: weekday %v next %s; want %d and %s", v.Weekday, v.NextAt, want, slot.Add(24*time.Hour))
	}
	e.exec(`UPDATE run_schedules SET weekday = $2, paused_at = now(), paused_reason = 'by_owner' WHERE id = $1`, v.ID, int(slot.Weekday()))
	if code, body := e.routeAs(e.person, http.MethodPost, v.ID, "resume"); code != http.StatusOK {
		t.Fatalf("resume of a slot six days out = %d %s, want 200", code, body)
	}
	if sc := e.schedule(v.ID); !sc.NextAt.After(time.Now().Add(6 * 24 * time.Hour)) {
		t.Errorf("next_at after the resume = %s, want about seven days out", sc.NextAt)
	}
}

// No API token resumes a schedule, whatever its role: the schedule stays paused.
func TestRunSchedule_ResumeRefusesTokenCaller_PG(t *testing.T) {
	e := newSchedEnv(t)
	v := e.createAs(e.person, "")
	if code, body := e.routeAs(e.person, http.MethodPost, v.ID, "pause"); code != http.StatusOK {
		t.Fatalf("pause = %d %s", code, body)
	}
	member, _ := mintToken(t, e.srv, e.person, "ci-member")
	admin, _ := mintToken(t, e.srv, e.super, "ci-admin")
	for _, c := range []struct{ name, tok string }{{"a member's wdn_ token", member}, {"a cli_access token", e.cliAccess()}, {"an admin's wdn_ token", admin}} {
		name, tok := c.name, c.tok
		w := do(t, e.srv, http.MethodPost, "/api/v1/run-schedules/"+v.ID.String()+"/resume", tok, "")
		if w.Code != http.StatusUnprocessableEntity || errorReason(w) != reasonRunScheduleOwnerUnsupported {
			t.Errorf("resume with %s = %d %s, want 422 %s", name, w.Code, w.Body, reasonRunScheduleOwnerUnsupported)
		}
	}
	e.wantPaused(v.ID, schedulePausedByOwner)
}

// cliAccess is a cli_access token of the person's, with its CLI session.
func (e *schedEnv) cliAccess() string {
	e.t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		e.t.Fatal(err)
	}
	now, notTruncated := time.Now().UTC(), false
	exp := now.Add(time.Hour)
	sess := store.CLISessionRow{ID: uuid.New(), Identity: store.CLIIdentity{Principal: e.personP, Email: "person@corp.example", Role: oidc.RoleUser,
		UserType: types.UserTypeStandard, GroupsTruncated: &notTruncated, RevSeq: 1},
		IdentityStampedAt: now, Machine: "laptop", Store: types.CLIStoreKeychain, PublicKey: pub, CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour)}
	raw := apiTokenPrefix + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := e.pg.CreateCLISession(context.Background(), sess, "wdc_"+uuid.NewString(), types.APIToken{
		ID: uuid.New(), Principal: e.personP, Email: "person@corp.example", Role: oidc.RoleUser, UserType: types.UserTypeStandard,
		Name: "CLI: laptop", GroupsTruncated: &notTruncated, CreatedAt: now, ExpiresAt: &exp,
		Kind: types.APITokenKindCLIAccess, CLISessionID: &sess.ID,
	}, raw); err != nil {
		e.t.Fatalf("seed a CLI session with a cli_access token: %v", err)
	}
	return raw
}

// seedTextRun is a finished run of owner's with caller text in every text field.
func (e *schedEnv) seedTextRun(owner string) uuid.UUID {
	e.t.Helper()
	id, now := uuid.New(), time.Now().UTC()
	if _, err := e.pg.CreateRun(context.Background(), types.AgentRun{ID: id, CreatedAt: now, UpdatedAt: now, CreatedBy: owner, Agent: "claude-code",
		ConfinementClass: types.CC2, State: types.RunRunning, SPIFFEID: "spiffe://wardyn.local/agent-run/" + id.String(), RunnerTarget: "docker",
		Task: "secret task", Title: "secret title", Description: "secret description"}); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func blankText(r types.AgentRun) bool {
	return r.Task == "" && r.Title == "" && r.Description == "" && r.CorrelationID == ""
}

// replayAfterDelete books a slot, puts the schedule back as if its advance was lost, and ticks again with remove
// run right after the replayed create answers. The replayed run was booked before the change: this tick neither
// cancels nor blanks it.
func (e *schedEnv) replayAfterDelete(t *testing.T, remove func(id uuid.UUID)) {
	t.Helper()
	v := e.createAs(e.person, "")
	due := e.dueIn(v.ID, time.Minute)
	e.tick()
	runs := e.bookedRuns()
	if len(runs) != 1 {
		t.Fatalf("booked runs = %d, want 1", len(runs))
	}
	e.exec(`UPDATE run_schedules SET next_at = $2 WHERE id = $1`, v.ID, due)
	sw := e.srv.newScheduleSweep(e.pg)
	create := sw.create
	sw.create = func(w http.ResponseWriter, r *http.Request) {
		create(w, r)
		remove(v.ID)
	}
	if err := sw.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := e.run(runs[0]); r.State != types.RunQueued || r.Title == "" {
		t.Errorf("the replayed booking is %s with title %q; want QUEUED with its text", r.State, r.Title)
	}
	for _, ev := range e.scheduleAudits(v.ID, "run.schedule.fire") {
		if queueAuditData(t, ev)["reason"] == "schedule_gone" {
			t.Error("a replayed booking was treated as made after the delete")
		}
	}
}

// An erasure that lands inside the booking's create leaves no queued run of the owner: the post-201 re-read
// finds the schedule gone and blanks and cancels that one run. The owner's other run is as the erasure left it,
// and another person's keeps its text. This test must fail if the post-201 re-read (scheduleSweep.booked's
// GetRunSchedule) is removed.
func TestRunSchedule_ErasureDuringTickLeavesNoRun_PG(t *testing.T) {
	t.Run("fresh booking", func(t *testing.T) {
		e := newSchedEnv(t)
		ctx := context.Background()
		v := e.createAs(e.person, "")
		mine, theirs := e.seedTextRun(e.personP), e.seedTextRun(e.otherP)
		erased := false
		e.srv.queueRace = func(point string) {
			if point == queueRaceEnqueueAfterFold && !erased {
				erased = true
				if _, err := e.srv.eraseRunTasks(ctx, e.personP); err != nil {
					t.Error(err)
				}
			}
		}
		e.dueIn(v.ID, time.Minute)
		e.tick()
		runs := e.bookedRuns()
		if !erased || len(runs) != 1 {
			t.Fatalf("erased in the window %v, booked runs %d", erased, len(runs))
		}
		if n := e.count(`SELECT count(*) FROM agent_runs WHERE created_by = $1 AND state = 'QUEUED'`, e.personP); n != 0 {
			t.Errorf("the owner has %d QUEUED runs after the erasure, want none", n)
		}
		if r := e.run(runs[0]); r.State != types.RunKilled || !blankText(r) {
			t.Errorf("the booked run is %s with text %q/%q/%q/%q; want KILLED and blank", r.State, r.Task, r.Title, r.Description, r.CorrelationID)
		}
		if !e.gone(v.ID) {
			t.Error("the schedule survived the erasure")
		}
		if fires := e.scheduleAudits(v.ID, "run.schedule.fire"); len(fires) != 1 || queueAuditData(t, fires[0])["reason"] != "schedule_gone" {
			t.Errorf("run.schedule.fire rows = %+v, want one naming schedule_gone", fires)
		}
		if r := e.run(mine); r.State != types.RunRunning {
			t.Errorf("the owner's running run is %s after the tick, want RUNNING: the cancel is id-scoped", r.State)
		}
		if r := e.run(theirs); r.Task != "secret task" || r.Title != "secret title" {
			t.Errorf("another person's run lost its text: %q %q", r.Task, r.Title)
		}
	})
	t.Run("replayed booking", func(t *testing.T) {
		e := newSchedEnv(t)
		e.replayAfterDelete(t, func(uuid.UUID) {
			if _, err := e.pg.DeleteRunSchedulesOf(context.Background(), e.personP); err != nil {
				t.Error(err)
			}
		})
	})
}

// The owner's Delete landing inside the booking's create cancels and blanks that one run, with no advance; the
// owner's other run keeps its text. This test must fail if BlankRunTaskByID is replaced by BlankRunTasks(owner).
func TestRunSchedule_DeletedMidBookingCancelsAndBlanks_PG(t *testing.T) {
	t.Run("fresh booking", func(t *testing.T) {
		e := newSchedEnv(t)
		ctx := context.Background()
		v := e.createAs(e.person, "")
		mine := e.seedTextRun(e.personP)
		deleted := false
		e.srv.queueRace = func(point string) {
			if point == queueRaceEnqueueAfterFold && !deleted {
				deleted = true
				if _, err := e.pg.DeleteRunSchedule(ctx, v.ID); err != nil {
					t.Error(err)
				}
			}
		}
		e.dueIn(v.ID, time.Minute)
		e.tick()
		runs := e.bookedRuns()
		if !deleted || len(runs) != 1 {
			t.Fatalf("deleted in the window %v, booked runs %d", deleted, len(runs))
		}
		if r := e.run(runs[0]); r.State != types.RunKilled || r.Task != "" || r.Title != "" || r.Description != "" {
			t.Errorf("the booked run is %s with text %q/%q/%q; want KILLED and blank", r.State, r.Task, r.Title, r.Description)
		}
		if r := e.run(mine); r.Task != "secret task" || r.Title != "secret title" || r.Description != "secret description" {
			t.Errorf("the owner's other run lost its text: %q %q %q", r.Task, r.Title, r.Description)
		}
		if fires := e.scheduleAudits(v.ID, "run.schedule.fire"); len(fires) != 1 || queueAuditData(t, fires[0])["reason"] != "schedule_gone" {
			t.Errorf("run.schedule.fire rows = %+v, want one naming schedule_gone", fires)
		}
	})
	t.Run("replayed booking", func(t *testing.T) {
		e := newSchedEnv(t)
		e.replayAfterDelete(t, func(id uuid.UUID) {
			if _, err := e.pg.DeleteRunSchedule(context.Background(), id); err != nil {
				t.Error(err)
			}
		})
	})
}

// A resume stores the resuming session's groups. A session that lags the latest sign-in resumes, and the next tick
// pauses groups_changed again and books nothing: the owner signs in again first.
func TestRunSchedule_ResumeFromLaggingSessionPausesAgain_PG(t *testing.T) {
	e := newSchedEnv(t)
	e.signIn([]string{"a"})
	lagging := personSession(t, []string{"a"}, 0)
	v := e.createAs(lagging, "")
	e.signIn([]string{"a", "c"})
	e.dueIn(v.ID, time.Minute)
	e.tick()
	e.wantPaused(v.ID, schedulePausedGroupsChanged)
	if code, body := e.routeAs(lagging, http.MethodPost, v.ID, "resume"); code != http.StatusOK {
		t.Fatalf("resume from the lagging session = %d %s", code, body)
	}
	e.dueIn(v.ID, time.Minute)
	e.tick()
	e.wantPaused(v.ID, schedulePausedGroupsChanged)
	if n := len(e.bookedRuns()); n != 0 {
		t.Errorf("a lagging session's resume booked %d runs", n)
	}
}

// A stored authority that reads as an admin in the Admin view is refused 409 admin_view at its booking, which
// pauses the schedule.
func TestRunSchedule_PausesOnAdminViewRefusal_PG(t *testing.T) {
	e := newSchedEnv(t)
	v := e.createAs(e.person, "")
	e.exec(`UPDATE run_schedules SET authority = jsonb_set(authority, '{role}', to_jsonb($2::text)) WHERE id = $1`, v.ID, oidc.RoleAdmin)
	e.dueIn(v.ID, time.Minute)
	e.tick()
	e.wantPaused(v.ID, scheduleRefusedPrefix+string(authz.ReasonAdminView))
	if n := len(e.bookedRuns()); n != 0 {
		t.Errorf("an admin-view booking made %d runs", n)
	}
}

// A slot whose 201 was answered but whose advance was lost is replayed under its key: the same run is recorded
// booked and no second run exists.
func TestRunSchedule_MissedReplaysTheKey_PG(t *testing.T) {
	e := newSchedEnv(t)
	v := e.createAs(e.person, "")
	due := e.dueIn(v.ID, 2*time.Second)
	e.tick()
	runs := e.bookedRuns()
	if len(runs) != 1 {
		t.Fatalf("booked runs = %d, want 1", len(runs))
	}
	time.Sleep(time.Until(due) + time.Second)
	e.exec(`UPDATE run_schedules SET next_at = $2, last_run_id = NULL, last_outcome = '' WHERE id = $1`, v.ID, due)
	e.tick()
	sc := e.schedule(v.ID)
	if sc.LastRunID == nil || *sc.LastRunID != runs[0] || sc.LastOutcome != scheduleOutcomeBooked {
		t.Errorf("the replayed slot recorded %+v, want run %s booked", sc, runs[0])
	}
	if n := len(e.bookedRuns()); n != 1 {
		t.Errorf("runs after the replay = %d, want 1", n)
	}
}

// twoUnits gives the person a home unit that allows scheduled runs and a second unit of theirs, named by run_in,
// whose limits are deny.
func (e *schedEnv) twoUnits(deny bool) (home, pinned types.GovernanceUnit) {
	e.t.Helper()
	ctx := context.Background()
	ceiling := types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com", "github.com"}, MinConfinementClass: types.CC2}
	var err error
	if home, err = e.pg.UpsertGovernanceUnit(ctx, types.GovernanceUnit{Name: "Day team", CreatedBy: "seed", Ceiling: ceiling}); err != nil {
		e.t.Fatal(err)
	}
	pinned = e.setPinnedDeny(types.GovernanceUnit{Name: "Night team", CreatedBy: "seed", Ceiling: ceiling}, deny)
	for i, u := range []types.GovernanceUnit{home, pinned} {
		if _, err := e.pg.UpsertGovernanceMembership(ctx, types.GovernanceMembership{SubjectType: types.CapabilitySubjectUser,
			Subject: e.personP, UnitID: u.ID, Priority: i + 1, Home: i == 0, CreatedBy: "seed"}); err != nil {
			e.t.Fatal(err)
		}
	}
	return home, pinned
}

func (e *schedEnv) setPinnedDeny(u types.GovernanceUnit, deny bool) types.GovernanceUnit {
	e.t.Helper()
	u.Limits = types.GovernanceLimits{DenyScheduledRuns: deny}
	out, err := e.pg.UpsertGovernanceUnit(context.Background(), u)
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

// The create's preflight judges the unit the request pins (run_in), not only the caller's home unit: the context
// choice slot is installed as the create installs it. This test must fail if schedulePreflight's
// withContextChoiceSlot is removed.
func TestRunScheduleCreate_PinnedUnitDenyIsSeen(t *testing.T) {
	e := newSchedEnv(t)
	_, pinned := e.twoUnits(true)
	w := e.call(e.person, http.MethodPost, "/api/v1/run-schedules", e.scheduleBody(scheduleEveryDaily, `,"run_in":"`+pinned.ID.String()+`"`))
	if w.Code != http.StatusForbidden || errorReason(w) != reasonScheduledRunsDenied {
		t.Fatalf("a schedule pinned to a denying unit = %d %s, want 403 %s", w.Code, w.Body, reasonScheduledRunsDenied)
	}
	if n := e.count(`SELECT count(*) FROM run_schedules`); n != 0 {
		t.Errorf("a refused create stored %d schedules", n)
	}
	if w := e.call(e.person, http.MethodPost, "/api/v1/run-schedules", e.scheduleBody(scheduleEveryDaily, "")); w.Code != http.StatusCreated {
		t.Errorf("the same schedule in the home unit = %d %s, want 201", w.Code, w.Body)
	}
}

// A resume refused by the governance profile says why the schedule cannot resume, never "Start the run now", in
// the body and in its authz.denied row, with the door's policy; the schedule stays paused. The same holds when
// only the unit the stored request pins denies.
func TestRunScheduleResume_DeniedIsRefusedWithItsOwnSentence(t *testing.T) {
	check := func(t *testing.T, e *schedEnv, id uuid.UUID) {
		t.Helper()
		w := e.call(e.person, http.MethodPost, "/api/v1/run-schedules/"+id.String()+"/resume", "")
		if w.Code != http.StatusForbidden || errorReason(w) != reasonScheduledRunsDenied || !strings.Contains(w.Body.String(), runScheduleResumeDeniedMsg) {
			t.Fatalf("resume under a denying profile = %d %s, want 403 %s with the resume sentence", w.Code, w.Body, reasonScheduledRunsDenied)
		}
		if strings.Contains(w.Body.String(), "Start the run now") || !strings.Contains(w.Body.String(), `"policy"`) {
			t.Errorf("the resume refusal %s gives one-time advice or no policy", w.Body)
		}
		var rows int
		for _, ev := range e.h.audit.snapshot() {
			if ev.Action == "authz.denied" && ev.Target == "runs.schedule" {
				rows++
				if !strings.Contains(string(ev.Data), runScheduleResumeDeniedMsg) {
					t.Errorf("the authz.denied row carries %s, want the resume sentence", ev.Data)
				}
			}
		}
		if rows != 1 {
			t.Errorf("authz.denied rows for runs.schedule = %d, want 1", rows)
		}
		e.wantPaused(id, schedulePausedByOwner)
	}
	t.Run("the caller's profile", func(t *testing.T) {
		e := newSchedEnv(t)
		v := e.createAs(e.person, "")
		if code, body := e.routeAs(e.person, http.MethodPost, v.ID, "pause"); code != http.StatusOK {
			t.Fatalf("pause = %d %s", code, body)
		}
		e.seedAssignment(e.seedScheduleUnit(uuid.Nil, types.GovernanceLimits{DenyScheduledRuns: true}), e.personP)
		check(t, e, v.ID)
	})
	t.Run("the pinned unit only", func(t *testing.T) {
		e := newSchedEnv(t)
		_, pinned := e.twoUnits(false)
		v := e.createAs(e.person, `,"run_in":"`+pinned.ID.String()+`"`)
		if code, body := e.routeAs(e.person, http.MethodPost, v.ID, "pause"); code != http.StatusOK {
			t.Fatalf("pause = %d %s", code, body)
		}
		e.setPinnedDeny(pinned, true)
		check(t, e, v.ID)
	})
}
