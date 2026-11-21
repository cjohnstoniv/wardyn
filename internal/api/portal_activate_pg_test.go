// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// activateEnv is a server over a real database with a fake clock the activation and its alarm read.
type activateEnv struct {
	t     *testing.T
	srv   *Server
	st    store.PG
	pool  *pgxpool.Pool
	audit *recRecorder
	now   time.Time
}

func newActivateEnv(t *testing.T) *activateEnv {
	t.Helper()
	// The server's own sessions carry a 0.10.2 label, as a booted wardynd's do, so the sample sees only the
	// clients a test opens. The throwaway pool's unlabelled connections are closed first.
	raw := throwawayPGPool(t)
	cfg := raw.Config().Copy()
	cfg.ConnConfig.RuntimeParams["application_name"] = "wardynd/0.10.2/test"
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	h := newHarness(t)
	e := &activateEnv{t: t, pool: pool, st: store.NewPG(pool), audit: h.audit, now: time.Date(2026, 12, 1, 9, 0, 0, 0, time.UTC)}
	e.srv = New(baseTestConfig(h, e.st))
	e.srv.cfg.Now = func() time.Time { return e.now }
	return e
}

// client opens another session on the database with application_name label, closed when the test ends.
func (e *activateEnv) client(label string) *pgx.Conn {
	e.t.Helper()
	cfg := e.pool.Config().ConnConfig.Copy()
	cfg.RuntimeParams["application_name"] = label
	c, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		e.t.Fatalf("connect as %q: %v", label, err)
	}
	e.t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

// sample takes one sample at the clock, as the replica's monitor would.
func (e *activateEnv) sample() {
	e.t.Helper()
	if _, err := e.srv.portalMonitorStep(context.Background(), e.now); err != nil {
		e.t.Fatalf("sample: %v", err)
	}
}

// warm samples every ten seconds for two minutes, so the window is full.
func (e *activateEnv) warm() {
	e.t.Helper()
	for i := 0; i <= int(portalSampleWindow/portalSampleEvery); i++ {
		e.sample()
		e.now = e.now.Add(portalSampleEvery)
	}
}

func (e *activateEnv) activate(body string) (int, string) {
	e.t.Helper()
	w := do(e.t, e.srv, http.MethodPost, "/api/v1/admin/portal-issuers/activate", adminToken, body)
	return w.Code, w.Body.String()
}

func (e *activateEnv) on() bool {
	e.t.Helper()
	on, err := e.st.PortalIssuersActivated(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return on
}

func (e *activateEnv) rows(action string) int {
	n := 0
	for _, ev := range e.audit.snapshot() {
		if ev.Action == action {
			n++
		}
	}
	return n
}

const stoppedBody = `{"old_servers_stopped":true}`

func vetoed(t *testing.T, e *activateEnv, label string) {
	t.Helper()
	e.client(label)
	e.warm()
	code, body := e.activate(stoppedBody)
	if code != http.StatusConflict || !strings.Contains(body, reasonPortalIssuersOldServerSuspected) {
		t.Fatalf("activate beside a client labelled %q = %d %s, want 409 %s", label, code, body, reasonPortalIssuersOldServerSuspected)
	}
	if e.on() {
		t.Fatalf("a vetoed activation switched issuer portals on (label %q)", label)
	}
}

// A session with no application name is an untagged client: the activation is refused and lists it.
func TestPortalIssuersActivate_RefusedWhileUntaggedSession_PG(t *testing.T) {
	vetoed(t, newActivateEnv(t), "")
}

// A label that is not a 0.10.2 wardynd's (monitoring, a job) vetoes too.
func TestPortalIssuersActivate_CustomLabelRefused_PG(t *testing.T) {
	vetoed(t, newActivateEnv(t), "billing-job")
}

// An old server running with WARDYN_HA beside a new one does not tag itself 0.10.2, so it is vetoed.
func TestPortalIssuersActivate_HAOldReplicaBlocksNonHANewReplica_PG(t *testing.T) {
	vetoed(t, newActivateEnv(t), "wardynd/0.10.1/replica-a")
}

// An old server that holds the single-instance lock beside a new HA replica is seen by the sample as well.
func TestPortalIssuersActivate_NonHAOldReplicaBesideHANewReplicaVetoed_PG(t *testing.T) {
	vetoed(t, newActivateEnv(t), "wardynd/0.10.0/replica-b")
}

// The confirmation overrides the veto and is audited: one activate row, and one override row naming the session.
func TestPortalIssuersActivate_OverrideAudited_PG(t *testing.T) {
	e := newActivateEnv(t)
	e.client("monitoring-agent")
	e.warm()
	if code, body := e.activate(`{"old_servers_stopped":true,"confirm_listed_sessions":true}`); code != http.StatusOK {
		t.Fatalf("activate with the confirmation = %d %s", code, body)
	}
	if !e.on() {
		t.Fatal("the override did not switch issuer portals on")
	}
	if e.rows("portal_issuers.activate") != 1 || e.rows("portal_issuers.activate_override") != 1 {
		t.Fatalf("audit rows: activate %d, override %d, want 1 each", e.rows("portal_issuers.activate"), e.rows("portal_issuers.activate_override"))
	}
	for _, ev := range e.audit.snapshot() {
		if ev.Action == "portal_issuers.activate_override" && !strings.Contains(string(ev.Data), "monitoring-agent") {
			t.Errorf("the override row = %s, want the session named", ev.Data)
		}
	}
}

// Two hours of clean samples on a fake clock never switch the portals on by themselves.
func TestPortalIssuersActivate_NeverAutomatic_PG(t *testing.T) {
	e := newActivateEnv(t)
	for i := 0; i < int(time.Hour/portalSampleEvery); i++ {
		e.sample()
		e.now = e.now.Add(portalSampleEvery)
	}
	if e.on() {
		t.Fatal("issuer portals switched on without an operator")
	}
}

// sampleFor samples at the monitor's own cadence for d, leaving the clock at the end of it.
func (e *activateEnv) sampleFor(d time.Duration) {
	e.t.Helper()
	for end := e.now.Add(d); e.now.Before(end); e.now = e.now.Add(portalSampleEvery) {
		e.sample()
	}
}

// The call answers at once from the rolling sample: a replica up 30 s is warming (409 with Retry-After), and a
// session seen about 110 s ago and gone since still vetoes.
func TestPortalIssuersActivate_ReadsRollingSampleWithoutBlocking_PG(t *testing.T) {
	e := newActivateEnv(t)
	e.sampleFor(30 * time.Second)
	t0 := time.Now()
	w := do(t, e.srv, http.MethodPost, "/api/v1/admin/portal-issuers/activate", adminToken, stoppedBody)
	if w.Code != http.StatusConflict || errorReason(w) != reasonPortalIssuersSampleWarming || w.Header().Get("Retry-After") == "" {
		t.Fatalf("a replica up 30 s = %d %s retry-after %q, want 409 %s", w.Code, w.Body, w.Header().Get("Retry-After"), reasonPortalIssuersSampleWarming)
	}
	if _, err := strconv.Atoi(w.Header().Get("Retry-After")); err != nil {
		t.Errorf("Retry-After %q is not seconds", w.Header().Get("Retry-After"))
	}
	c := e.client("billing-job")
	e.sampleFor(10 * time.Second) // seen here
	_ = c.Close(context.Background())
	e.sampleFor(100 * time.Second) // gone by now
	if code, body := e.activate(stoppedBody); code != http.StatusConflict || !strings.Contains(body, reasonPortalIssuersOldServerSuspected) {
		t.Fatalf("a session seen 100 s ago = %d %s, want the veto", code, body)
	}
	if took := time.Since(t0); took > 5*time.Second {
		t.Errorf("the calls took %s, want an answer from the sample at once", took)
	}
}

// flakySampleStore is the store whose session read fails while fail is set, as a database that cannot be asked.
type flakySampleStore struct {
	store.PG
	fail *bool
}

func (f flakySampleStore) UntaggedClientSessions(ctx context.Context) ([]types.ClientSession, error) {
	if *f.fail {
		return nil, errors.New("the session read failed")
	}
	return f.PG.UntaggedClientSessions(ctx)
}

func (e *activateEnv) failReads() *bool {
	failing := new(bool)
	e.srv.cfg.Store = flakySampleStore{PG: e.st, fail: failing}
	return failing
}

// failFor runs the monitor for d with every session read failing: each step errors and records nothing.
func (e *activateEnv) failFor(d time.Duration) {
	e.t.Helper()
	for end := e.now.Add(d); e.now.Before(end); e.now = e.now.Add(portalSampleEvery) {
		if _, err := e.srv.portalMonitorStep(context.Background(), e.now); err == nil {
			e.t.Fatal("the failing read reported no error")
		}
	}
}

// A window whose reads stopped working is never clean: one good sample with an old client, then reads failing
// past the window, is answered with the retryable warming refusal and never switches the portals on; the same
// call after the reads recover and a full clean window passes.
func TestPortalIssuersActivate_StaleSampleRefusedNeverClean_PG(t *testing.T) {
	e := newActivateEnv(t)
	failing := e.failReads()
	c := e.client("old-wardynd")
	e.warm() // a full window that saw the old client
	_ = c.Close(context.Background())
	*failing = true
	e.failFor(3 * portalSampleWindow)
	w := do(t, e.srv, http.MethodPost, "/api/v1/admin/portal-issuers/activate", adminToken, stoppedBody)
	if w.Code != http.StatusConflict || errorReason(w) != reasonPortalIssuersSampleWarming || w.Header().Get("Retry-After") == "" {
		t.Fatalf("activate on a sample that expired = %d %s, want 409 %s with Retry-After", w.Code, w.Body, reasonPortalIssuersSampleWarming)
	}
	if e.on() {
		t.Fatal("an expired sample switched issuer portals on")
	}
	// A sample that is only a little late (inside the allowed gap) still counts as continuous.
	*failing = false
	e.sample()
	e.now = e.now.Add(portalSampleEvery)
	if code, body := e.activate(stoppedBody); code != http.StatusConflict || !strings.Contains(body, reasonPortalIssuersSampleWarming) {
		t.Fatalf("activate one sample after the reads recovered = %d %s, want warming (the window restarts)", code, body)
	}
	e.warm()
	if code, body := e.activate(stoppedBody); code != http.StatusOK || !e.on() {
		t.Fatalf("activate after a full clean window = %d %s, want 200 and the portals on", code, body)
	}
}

// A gap inside the window restarts it: samples before the gap do not count toward the two minutes, so a replica
// that failed for a minute is warming again, and an old client seen only before the gap is no longer held against
// the operator (the next full window decides).
func TestPortalIssuersActivate_GapRestartsWindow_PG(t *testing.T) {
	e := newActivateEnv(t)
	failing := e.failReads()
	e.sampleFor(90 * time.Second)
	*failing = true
	e.failFor(time.Minute)
	*failing = false
	e.sampleFor(30 * time.Second)
	if code, body := e.activate(stoppedBody); code != http.StatusConflict || !strings.Contains(body, reasonPortalIssuersSampleWarming) {
		t.Fatalf("activate 30 s after a gap = %d %s, want warming", code, body)
	}
}

// The truly clean window still switches the portals on, and the operator's override still works over a sample
// that is warm and lists a session.
func TestPortalIssuersActivate_CleanWindowActivatesOverrideStillWorks_PG(t *testing.T) {
	e := newActivateEnv(t)
	e.warm()
	if code, body := e.activate(stoppedBody); code != http.StatusOK || !e.on() {
		t.Fatalf("activate over a clean window = %d %s, want 200 and the portals on", code, body)
	}
	o := newActivateEnv(t)
	o.client("old-wardynd")
	o.warm()
	if code, body := o.activate(`{"old_servers_stopped":true,"confirm_listed_sessions":true}`); code != http.StatusOK || !o.on() {
		t.Fatalf("activate with the override over a listed session = %d %s, want 200", code, body)
	}
}

// After the switch-on an unlabelled client and one with a 0.10.1 label each write one old_client_seen row,
// and the portals stay on.
func TestPortalIssuers_OldClientAfterActivationAlarms_PG(t *testing.T) {
	e := newActivateEnv(t)
	if _, err := e.st.ActivatePortalIssuers(context.Background(), "test", nil); err != nil {
		t.Fatal(err)
	}
	e.client("")
	e.client("wardynd/0.10.1/x")
	e.sample()
	if n := e.rows("portal_issuers.old_client_seen"); n != 2 {
		t.Fatalf("old_client_seen rows = %d, want 2", n)
	}
	if !e.on() {
		t.Fatal("the alarm switched issuer portals off")
	}
}

// The same session seen in three hourly samples writes one row that day, and one more the next UTC day.
func TestPortalIssuers_OldClientAlarmDedupedPerDay_PG(t *testing.T) {
	e := newActivateEnv(t)
	if _, err := e.st.ActivatePortalIssuers(context.Background(), "test", nil); err != nil {
		t.Fatal(err)
	}
	e.client("billing-job")
	for i := 0; i < 3; i++ {
		e.sample()
		e.now = e.now.Add(portalAlarmEvery)
	}
	if n := e.rows("portal_issuers.old_client_seen"); n != 1 {
		t.Fatalf("rows after three samples in a day = %d, want 1", n)
	}
	e.now = time.Date(2026, 12, 2, 0, 5, 0, 0, time.UTC)
	e.sample()
	if n := e.rows("portal_issuers.old_client_seen"); n != 2 {
		t.Fatalf("rows after the next UTC day = %d, want 2", n)
	}
}
