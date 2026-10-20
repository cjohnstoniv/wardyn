// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// scheduleAt is the request fields of a start d from now, in UTC.
func scheduleAt(d time.Duration) string {
	return `,"start_at":"` + time.Now().Add(d).UTC().Format(time.RFC3339) + `","start_time_zone":"UTC"`
}

// due moves a scheduled run's start time into the past, as the clock would.
func (e *queueEnv) due(id any) {
	e.t.Helper()
	e.exec(`UPDATE run_queue SET start_at = now() - interval '1 second' WHERE run_id = $1`, id)
}

// With the switch off a start time is refused with its own reason and nothing is written.
func TestPG_ScheduledRunRefusedWhileTheSwitchIsOff(t *testing.T) {
	e := newQueueEnv(t)
	t.Setenv(envScheduledRuns, "")
	pool := e.cappedPool("Night", 5)
	_, w := e.launch(nil, pool, scheduleAt(time.Hour))
	if w.Code != http.StatusUnprocessableEntity || errorReason(w) != reasonScheduledRunsOff {
		t.Fatalf("scheduled launch with the switch off = %d %s; want 422 %s", w.Code, w.Body, reasonScheduledRunsOff)
	}
	if n := e.count(`SELECT count(*) FROM agent_runs WHERE runner_pool_id = $1`, pool.ID); n != 0 {
		t.Errorf("a refused scheduled launch wrote %d runs", n)
	}
}

// A scheduled run waits QUEUED as "scheduled" in a pool with room, holding nothing, with its deadline
// inside seven days of the request; no sweep starts it early; once due, the sweep starts it.
func TestPG_ScheduledRunWaitsForItsTime(t *testing.T) {
	e := newQueueEnv(t)
	t.Setenv(envScheduledRuns, "1")
	pool := e.cappedPool("Night", 5)
	out := e.queuedLaunch(nil, pool, scheduleAt(2*time.Hour))
	if !slices.ContainsFunc(out.Warnings, func(w string) bool { return strings.HasPrefix(w, "This run is scheduled to start at ") }) {
		t.Errorf("warnings %q carry no scheduled sentence", out.Warnings)
	}
	w := e.admin(http.MethodGet, "/api/v1/runs/"+out.ID.String(), "")
	got := decodeJSON[types.AgentRun](t, w)
	if got.Queue == nil || got.Queue.Reason != string(runnerpool.WaitScheduled) || got.Queue.StartAt == nil || got.Queue.TimeZone != "UTC" {
		t.Fatalf("GET queue = %+v; want reason scheduled with its start and zone", got.Queue)
	}
	if !got.Queue.DeadlineAt.After(*got.Queue.StartAt) || got.Queue.DeadlineAt.After(got.Queue.QueuedAt.Add(scheduleMaxAhead)) {
		t.Errorf("deadline %s: want after the start %s and within 7 days of %s", got.Queue.DeadlineAt, got.Queue.StartAt, got.Queue.QueuedAt)
	}
	if r := e.run(out.ID); r.SPIFFEID != "" {
		t.Errorf("a scheduled run was minted for: %q", r.SPIFFEID)
	}
	e.sweep()
	if r := e.run(out.ID); r.State != types.RunQueued || e.queueRows(out.ID) != 1 {
		t.Fatalf("a sweep before the start time moved the run: %s, %d queue rows", r.State, e.queueRows(out.ID))
	}
	e.due(out.ID)
	e.sweep()
	e.waitStarted(out.ID)
}

// The start of a scheduled run is the queue's own: a stored policy edited after scheduling ends it
// queue_policy_changed instead of starting what nobody reviewed.
func TestPG_ScheduledRunRechecksPolicyAtTheStart(t *testing.T) {
	e := newQueueEnv(t)
	t.Setenv(envScheduledRuns, "1")
	pool := e.cappedPool("Night", 5)
	w := e.admin(http.MethodPost, "/api/v1/policies", `{"name":"reviewed","spec":{"min_confinement_class":"CC2","allowed_domains":["api.anthropic.com"]}}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("store policy = %d %s", w.Code, w.Body)
	}
	policy := decodeJSON[types.RunPolicy](t, w)
	out := e.queuedLaunch(nil, pool, `,"policy_id":"`+policy.ID.String()+`"`+scheduleAt(time.Hour))
	if _, err := e.pg.UpdatePolicy(context.Background(), policy.ID, "reviewed", types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: []string{"pypi.org"}}); err != nil {
		t.Fatal(err)
	}
	e.due(out.ID)
	e.sweep()
	got := e.waitFailed(out.ID)
	if got.StatusDetail != "queue: queue_policy_changed" || got.FailureHint != types.RunEndQueuePolicyChanged.Sentence() {
		t.Errorf("a scheduled run whose policy changed = %s %q %q", got.State, got.StatusDetail, got.FailureHint)
	}
}

// awsSSOSchedule is a person's scheduled run on an AWS sign-in provider, with a live sign-in stored.
func (e *queueEnv) awsSSOSchedule() (types.RunnerPool, queuedLaunchAnswer) {
	e.t.Helper()
	e.srv.cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	e.srv.cfg.MaskRegistry = secretmask.NewRegistry()
	storeSSOBlobFor(e.t, e.srv, e.personP, putAWSSSOBlob(e.t, e.srv, time.Now().Add(3*time.Hour)))
	if _, err := e.srv.cfg.Store.PutSiteConfig(context.Background(), types.SiteConfig{
		Runners: &types.RunnerSettings{Enabled: true}, ModelProviders: providerBlock(awsSSOTestProvider()),
	}); err != nil {
		e.t.Fatal(err)
	}
	pool := e.reread(e.cappedPool("Night", 5))
	return pool, e.queuedLaunch(e.person, pool, `,"confinement_class":"CC2"`+scheduleAt(time.Hour))
}

// The stored request holds no secret: the queue row and the run row carry none of the owner's sign-in.
func TestPG_ScheduledRunStoresNoSecret(t *testing.T) {
	e := newQueueEnv(t)
	t.Setenv(envScheduledRuns, "1")
	_, out := e.awsSSOSchedule()
	var row string
	if err := e.pool.QueryRow(context.Background(),
		`SELECT (SELECT row_to_json(q)::text FROM run_queue q WHERE q.run_id = $1) || (SELECT row_to_json(r)::text FROM agent_runs r WHERE r.id = $1)`,
		out.ID).Scan(&row); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"sso-access-token-1234567890", "sso-refresh-token-1234567890", "sso-client-secret-1234567890"} {
		if strings.Contains(row, secret) {
			t.Errorf("the scheduled run's rows hold %q", secret)
		}
	}
}

// A due scheduled run whose owner's model sign-in expired with nothing to renew it waits for them,
// saying so, instead of failing at the start.
func TestPG_ScheduledRunWaitsForTheOwnersSignIn(t *testing.T) {
	e := newQueueEnv(t)
	t.Setenv(envScheduledRuns, "1")
	_, out := e.awsSSOSchedule()
	scope := chosenProvider{provider: awsSSOTestProvider(), owner: e.personP}.awsScope()
	if err := e.srv.cfg.Secrets.For(scope.owner).Delete(context.Background(), scope.ssoSecret()); err != nil {
		t.Fatal(err)
	}
	e.due(out.ID)
	e.sweep()
	r := e.run(out.ID)
	if r.State != types.RunQueued || r.StatusDetail != statusDetailQueuePrefix+string(runnerpool.WaitOwnerSignIn) {
		t.Fatalf("a due run with no sign-in = %s %q; want QUEUED %s", r.State, r.StatusDetail, runnerpool.WaitOwnerSignIn)
	}
	storeSSOBlobFor(t, e.srv, e.personP, putAWSSSOBlob(t, e.srv, time.Now().Add(3*time.Hour)))
	e.sweep()
	e.waitStarted(out.ID)
}

// A scheduled run still waiting for its owner's sign-in at its deadline ends saying so, not as a run
// that waited for a slot.
func TestPG_ScheduledRunSignInWaitEndsAtTheDeadline(t *testing.T) {
	e := newQueueEnv(t)
	t.Setenv(envScheduledRuns, "1")
	_, out := e.awsSSOSchedule()
	scope := chosenProvider{provider: awsSSOTestProvider(), owner: e.personP}.awsScope()
	if err := e.srv.cfg.Secrets.For(scope.owner).Delete(context.Background(), scope.ssoSecret()); err != nil {
		t.Fatal(err)
	}
	e.due(out.ID)
	e.sweep()
	e.exec(`UPDATE run_queue SET deadline_at = now() - interval '1 second' WHERE run_id = $1`, out.ID)
	e.sweep()
	r := e.run(out.ID)
	if r.State != types.RunFailed || r.StatusDetail != statusDetailQueuePrefix+string(types.RunEndQueueSignInTimeout) ||
		r.FailureHint != types.RunEndQueueSignInTimeout.Sentence() {
		t.Fatalf("a run whose sign-in wait passed its deadline = %s %q %q", r.State, r.StatusDetail, r.FailureHint)
	}
	rows := e.audits(out.ID, "run.queue.expire")
	if len(rows) != 1 || queueAuditData(t, rows[0])["reason"] != string(types.RunEndQueueSignInTimeout) {
		t.Errorf("run.queue.expire rows = %+v; want one with reason %s", rows, types.RunEndQueueSignInTimeout)
	}
}

// A scheduled start is the queue's own start: every re-check of section 7 that ends a waiting run ends a
// scheduled one at its time with the same reason, sentence and run.queue.start failure row.
func TestPG_ScheduledStartRunsTheQueueStartsChecks(t *testing.T) {
	for _, tc := range queueStartRechecks {
		t.Run(tc.name, func(t *testing.T) {
			e := newQueueEnv(t)
			t.Setenv(envScheduledRuns, "1")
			pool := e.cappedPool("Night", 5)
			out := e.queuedLaunch(e.person, pool, tc.extra+scheduleAt(time.Hour))
			tc.change(e, pool, e.run(out.ID))
			e.due(out.ID)
			e.sweep()
			e.ended(out.ID, tc)
		})
	}
}

// Switching scheduled runs off binds the runs already booked: at its time a booked run ends FAILED
// scheduled_runs_off, with a sentence that names the switch and a run.queue.start failure row, and the
// person's own run list shows it failed.
func TestPG_ScheduledRunEndsAtItsStartOnceTheSwitchIsOff(t *testing.T) {
	e := newQueueEnv(t)
	t.Setenv(envScheduledRuns, "1")
	pool := e.cappedPool("Night", 5)
	out := e.queuedLaunch(e.person, pool, scheduleAt(time.Hour))
	t.Setenv(envScheduledRuns, "")
	e.due(out.ID)
	e.sweep()
	reason := string(types.RunEndScheduledRunsOff)
	e.ended(out.ID, queueStartRecheck{reason: reason, sent: types.RunEndScheduledRunsOff.Sentence()})
	if !strings.Contains(types.RunEndScheduledRunsOff.Sentence(), envScheduledRuns) {
		t.Errorf("the end sentence %q does not name %s", types.RunEndScheduledRunsOff.Sentence(), envScheduledRuns)
	}
	var listed []types.AgentRun
	if err := json.Unmarshal(e.call(e.person, http.MethodGet, "/api/v1/runs", "").Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(listed, func(r types.AgentRun) bool { return r.ID == out.ID })
	if i < 0 || listed[i].State != types.RunFailed || listed[i].StatusDetail != statusDetailQueuePrefix+reason || listed[i].Queue != nil {
		t.Fatalf("the person's run list = %+v; want the booked run FAILED %s with no queue", listed, reason)
	}
}

// signInRefusedAtTheStart books a scheduled run whose owner's AWS session has expired but can be renewed,
// so promotion's status read admits it, and the renewal at the start is refused for good (invalid_grant).
// It returns the booking once it is back in its queue row, which it returns as promoted.
func (e *queueEnv) signInRefusedAtTheStart() (queuedLaunchAnswer, store.RunQueueEntry) {
	e.t.Helper()
	_, out := e.awsSSOSchedule()
	expired := putAWSSSOBlob(e.t, e.srv, time.Now().Add(-time.Minute))
	expired.RegistrationExpiresAt = time.Now().Add(90 * 24 * time.Hour)
	storeSSOBlobFor(e.t, e.srv, e.personP, expired)
	calls := fakeOIDC(e.t, func(w http.ResponseWriter, _ map[string]string, _ int) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant"})
	})
	e.due(out.ID)
	booked, err := e.pg.GetRunQueueEntry(context.Background(), out.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	e.sweep()
	waiting := statusDetailQueuePrefix + string(runnerpool.WaitOwnerSignIn)
	e.eventually("the refused start to return the booking to its queue", func() bool {
		r := e.run(out.ID)
		return calls.Load() >= 1 && (r.State != types.RunQueued || r.StatusDetail == waiting) && r.State != types.RunPending
	})
	if r := e.run(out.ID); r.State != types.RunQueued || r.StatusDetail != waiting || r.AdmittedAt != nil || r.SPIFFEID != "" {
		e.t.Fatalf("a start whose renewal was refused = %s %q admitted %v; want QUEUED %s, unadmitted, nothing minted", r.State, r.StatusDetail, r.AdmittedAt, waiting)
	}
	back, err := e.pg.GetRunQueueEntry(context.Background(), out.ID)
	if err != nil {
		e.t.Fatalf("the booking has no queue row: %v", err)
	}
	if back.Owner != booked.Owner || back.PoolID != booked.PoolID || !back.QueuedAt.Equal(booked.QueuedAt) || !back.DeadlineAt.Equal(booked.DeadlineAt) ||
		back.StartAt == nil || !back.StartAt.Equal(*booked.StartAt) || !bytes.Equal(back.Authority, booked.Authority) ||
		!bytes.Equal(back.Launch, booked.Launch) || !bytes.Equal(back.PolicyRef, booked.PolicyRef) {
		e.t.Fatalf("the booking came back as %+v; want the row it was promoted with, %+v", back, booked)
	}
	if rows := e.audits(out.ID, "run.queue.start"); len(rows) != 0 {
		e.t.Errorf("a booking returned to its queue wrote run.queue.start rows %+v", rows)
	}
	e.sweep()
	if r := e.run(out.ID); r.State != types.RunQueued || r.StatusDetail != waiting {
		e.t.Fatalf("a sweep after the refusal moved the booking: %s %q", r.State, r.StatusDetail)
	}
	return out, back
}

// A due booking whose expired sign-in is refused at its first renewal waits for its owner instead of
// failing, in the same queue row and deadline: a fresh sign-in starts it, a kill cancels it, and its
// deadline ends it queue_sign_in_timeout, as any sign-in wait.
func TestPG_ScheduledRunRefusedAtItsFirstRenewalWaitsForTheOwner(t *testing.T) {
	t.Run("a fresh sign-in starts it", func(t *testing.T) {
		e := newQueueEnv(t)
		t.Setenv(envScheduledRuns, "1")
		out, _ := e.signInRefusedAtTheStart()
		fresh := putAWSSSOBlob(t, e.srv, time.Now().Add(3*time.Hour))
		fresh.RefreshToken, fresh.RegistrationExpiresAt = "fresh-refresh-token-1234567890", time.Now().Add(90*24*time.Hour)
		storeSSOBlobFor(t, e.srv, e.personP, fresh)
		e.sweep()
		e.waitStarted(out.ID)
	})
	t.Run("a kill cancels it", func(t *testing.T) {
		e := newQueueEnv(t)
		t.Setenv(envScheduledRuns, "1")
		out, _ := e.signInRefusedAtTheStart()
		if w := e.call(e.person, http.MethodPost, "/api/v1/runs/"+out.ID.String()+"/kill", ""); w.Code != http.StatusAccepted {
			t.Fatalf("kill = %d %s", w.Code, w.Body)
		}
		if r := e.run(out.ID); r.State != types.RunKilled || e.queueRows(out.ID) != 0 {
			t.Fatalf("a killed booking = %s with %d queue rows; want KILLED with none", r.State, e.queueRows(out.ID))
		}
	})
	t.Run("its deadline ends it", func(t *testing.T) {
		e := newQueueEnv(t)
		t.Setenv(envScheduledRuns, "1")
		out, _ := e.signInRefusedAtTheStart()
		e.exec(`UPDATE run_queue SET deadline_at = now() - interval '1 second' WHERE run_id = $1`, out.ID)
		e.sweep()
		r := e.run(out.ID)
		if r.State != types.RunFailed || r.StatusDetail != statusDetailQueuePrefix+string(types.RunEndQueueSignInTimeout) ||
			r.FailureHint != types.RunEndQueueSignInTimeout.Sentence() || e.queueRows(out.ID) != 0 {
			t.Fatalf("a booking past its deadline = %s %q %q", r.State, r.StatusDetail, r.FailureHint)
		}
		if rows := e.audits(out.ID, "run.queue.expire"); len(rows) != 1 || queueAuditData(t, rows[0])["reason"] != string(types.RunEndQueueSignInTimeout) {
			t.Errorf("run.queue.expire rows = %+v; want one with reason %s", rows, types.RunEndQueueSignInTimeout)
		}
	})
}
