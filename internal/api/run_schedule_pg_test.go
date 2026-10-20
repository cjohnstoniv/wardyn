// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
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
