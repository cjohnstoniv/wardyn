// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Hosted-only: the portal person's lifecycle (suspend, reactivate, purge, sweep) over the real store. The people
// are made by real exchanges through a registered portal; the verifier is the in-memory fake the exchange tests
// use. SCIM is off unless a test turns it on.

const lifeWorkforceIssuer = "https://workforce.example.com/v2.0"

type portalLife struct {
	*exchPG
	run  *scimRunner
	d    types.Delegate
	cred string
}

func newPortalLife(t *testing.T) *portalLife {
	t.Helper()
	e := newExchPG(t)
	l := &portalLife{exchPG: e, run: &scimRunner{fakeRunner: &fakeRunner{}}}
	e.h.srv.cfg.Runner = l.run
	e.h.srv.cfg.SessionRevocations = &pgTestRevocations{pool: e.pool, st: e.st}
	// A portal step settles only once its pinned event is in the audit table, so every row the recorder takes is written
	// there too; each event is on the in-memory recorder once, however often it is replayed.
	e.h.srv.cfg.Audit = &tableTee{inner: e.h.srv.cfg.Audit, table: store.Recorder{Pool: e.pool}, seen: map[uuid.UUID]bool{}}
	l.d, l.cred = e.portal(t, portalTestIssuer, nil)
	if _, err := e.st.ActivatePortalIssuers(context.Background(), "test", nil); err != nil {
		t.Fatalf("switch issuer portals on: %v", err)
	}
	return l
}

// scimOn configures SCIM on the server for the workforce issuer (or issuer, when given) with a purge delay.
func (l *portalLife) scimOn(issuer string, purgeAfter time.Duration) {
	l.h.srv.cfg.SCIM = &SCIMConfig{Token: "scim-test-token", Issuer: issuer, Tenant: "tenant-a", PurgeAfter: purgeAfter}
}

// signIn makes person uid sign in through the portal: the identity row, one grant and its bearer.
func (l *portalLife) signIn(t *testing.T, uid string) (principal, bearer string) {
	t.Helper()
	l.ver.subj = l.person(uid, "")
	return l.ver.subj.Principal, l.token(t, l.d, l.cred)
}

func (l *portalLife) ident(t *testing.T, principal string) store.PrincipalIdentity {
	t.Helper()
	var id uuid.UUID
	if err := l.pool.QueryRow(context.Background(), `SELECT id FROM principal_identities WHERE principal = $1`, principal).Scan(&id); err != nil {
		t.Fatalf("identity of %s: %v", principal, err)
	}
	row, err := l.st.GetIdentity(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

// post sends POST /people/{principal}/{action} as cookie.
func (l *portalLife) post(t *testing.T, cookie *http.Cookie, principal, action, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doSSO(t, l.h.srv, http.MethodPost, "/api/v1/people/"+url.PathEscape(principal)+"/"+action, cookie, body)
}

func (l *portalLife) get(t *testing.T, cookie *http.Cookie, path string) *httptest.ResponseRecorder {
	t.Helper()
	return doSSO(t, l.h.srv, http.MethodGet, path, cookie, "")
}

func (l *portalLife) meCode(t *testing.T, bearer string) int {
	t.Helper()
	return do(t, l.h.srv, http.MethodGet, "/api/v1/me", bearer, "").Code
}

// exchangeFor runs an exchange for uid and returns the HTTP status.
func (l *portalLife) exchangeFor(t *testing.T, uid string) int {
	t.Helper()
	l.ver.subj = l.person(uid, "")
	return l.exchange(t, l.d.ID, l.cred, "subject-token").Code
}

// deniedReasons are the reasons of every denied delegation.exchange row so far.
func (l *portalLife) deniedReasons() []string {
	var out []string
	for _, ev := range l.rows("delegation.exchange") {
		if ev.Outcome == "denied" {
			out = append(out, lifeData(ev)["reason"].(string))
		}
	}
	return out
}

func lifeData(ev types.AuditEvent) map[string]any {
	out := map[string]any{}
	_ = json.Unmarshal(ev.Data, &out)
	return out
}

func (l *portalLife) seedRun(t *testing.T, principal string, state types.RunState) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	run, err := l.st.CreateRun(ctx, types.AgentRun{
		ID: id, CreatedBy: principal, Agent: "claude-code", ConfinementClass: types.CC1, State: state, RunnerTarget: "docker",
		Task: "t", SPIFFEID: "spiffe://wardyn.local/agent-run/" + id.String(),
	})
	if err != nil {
		t.Fatalf("seed a run for %s: %v", principal, err)
	}
	if run.State != state {
		if ok, err := l.st.UpdateRunStateIf(ctx, id, run.State, state); err != nil || !ok {
			t.Fatalf("move the run to %s: %v %v", state, ok, err)
		}
	}
	if err := l.st.SetSandboxRef(ctx, id, "wardyn-agent-"+id.String()); err != nil {
		t.Fatal(err)
	}
	return id
}

func (l *portalLife) runState(t *testing.T, id uuid.UUID) types.RunState {
	t.Helper()
	run, err := l.st.GetRun(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return run.State
}

// ledger is every deprovision_jobs row of identity id as text: a row a sweep touched differs.
func ledgerText(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT kind, step, target, state, attempts, last_error, detail::text, updated_at
		FROM deprovision_jobs WHERE identity_id = $1 ORDER BY kind, step, target`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var kind, step, target, state, lastErr, detail string
		var attempts int
		var at time.Time
		if err := rows.Scan(&kind, &step, &target, &state, &attempts, &lastErr, &detail, &at); err != nil {
			t.Fatal(err)
		}
		b.WriteString(strings.Join([]string{kind, step, target, state, lastErr, detail, at.Format(time.RFC3339Nano)}, "|"))
		b.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// allDone reports whether every ledger row of id is done, and that there is at least one.
func (l *portalLife) allDone(t *testing.T, id uuid.UUID) bool {
	t.Helper()
	return l.count(t, `SELECT count(*) FROM deprovision_jobs WHERE identity_id = $1`, id) > 0 &&
		l.count(t, `SELECT count(*) FROM deprovision_jobs WHERE identity_id = $1 AND state <> 'done'`, id) == 0
}

// waitDue polls until the database says the person's purge deadline has passed. The deadline is checked
// under the row lock against the database clock, so an app clock cannot move it.
func (l *portalLife) waitDue(t *testing.T, id uuid.UUID) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if l.count(t, `SELECT count(*) FROM principal_identities WHERE id = $1 AND purge_after <= now()`, id) == 1 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the purge deadline never passed")
}

// seedWorkforceLeaver writes a suspended workforce person of the SCIM issuer with a running run and pending
// ledger rows, the work a portal sweep must leave alone.
func (l *portalLife) seedWorkforceLeaver(t *testing.T, name string, due bool) (store.PrincipalIdentity, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	row, err := l.st.UpsertLoginIdentity(ctx, store.LoginIdentity{
		Principal: name, Issuer: lifeWorkforceIssuer, TenantID: "tenant-a", ObjectID: "oid-" + name, Email: name + "@corp.example",
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	run := l.seedRun(t, name, types.RunRunning)
	plan := store.SuspendPlan{
		IdentityID: row.ID, Principal: name, Principals: []string{name}, PurgePrincipals: []string{name},
		PendingJobs: []store.JobKey{{Step: jobStepAuditDeact}, {Step: jobStepSweep, Target: name}},
	}
	if due {
		plan.PurgeAfter = time.Second
	}
	if _, err := l.st.SuspendIdentity(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if due {
		l.waitDue(t, row.ID)
	}
	return row, run
}

// lifeEventually polls cond until it holds, failing after ten seconds.
func lifeEventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// freshBearer is the bearer of an exchange whose subject token was issued after everything the test did so far
// (after any cutoff it wrote), as a person who signs in at the provider again would hold. It fails the test unless
// the exchange answers 200.
func (l *portalLife) freshBearer(t *testing.T, uid string) string {
	t.Helper()
	time.Sleep(50 * time.Millisecond)
	l.ver.subj = l.person(uid, "")
	l.ver.subj.IssuedAt = time.Now()
	tok, _ := portalToken(t, l.exchange(t, l.d.ID, l.cred, "subject-token"))
	return tok
}

// tableTee records on the audit table first and then, once per event id, on inner.
type tableTee struct {
	inner audit.Recorder
	table store.Recorder
	mu    sync.Mutex
	seen  map[uuid.UUID]bool
}

func (t *tableTee) Record(ctx context.Context, ev types.AuditEvent) error {
	if err := t.table.Record(ctx, ev); err != nil {
		return err
	}
	t.mu.Lock()
	first := !t.seen[ev.ID]
	t.seen[ev.ID] = true
	t.mu.Unlock()
	if !first || t.inner == nil {
		return nil
	}
	return t.inner.Record(ctx, ev)
}
