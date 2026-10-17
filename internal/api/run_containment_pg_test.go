// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The organisation's side of a Kubernetes runner's node-loss containment, against a
// real Postgres: the runner's containment-pending report is kept, held for leases and shown with its
// egress bound, and a node record is made by the people the rules name and carried to the runner.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type containmentEnv struct {
	*poolEnv
	runnerID uuid.UUID
	run      types.AgentRun
	gen      uuid.UUID
}

// newContainmentEnv is a RUNNING local run of sub-person's on their claimed runner, with an execution.
func newContainmentEnv(t *testing.T, secondHuman bool) *containmentEnv {
	t.Helper()
	e := newOrgRunnerEnv(t, secondHuman)
	e.cfg.RunnerAuditPrepare = func(_ context.Context, ev types.AuditEvent) (types.AuditEvent, error) { return ev, nil }
	e.cfg.RunConfigKey = bytes.Repeat([]byte{7}, kek.DEKSize)
	e.srv = New(e.cfg)
	ctx := context.Background()
	rid := e.claimedRunner(e.personP)
	run := newFinalizeRun()
	run.CreatedBy, run.Placement, run.RunnerID, run.EvidenceSource = e.personP, types.PlacementLocal, &rid, types.RunEvidenceRunnerAsserted
	run, err := e.pg.CreateRun(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	run.SandboxRef = "runner:" + rid.String() + "/sandbox"
	if err := e.pg.SetSandboxRef(ctx, run.ID, run.SandboxRef); err != nil {
		t.Fatal(err)
	}
	gen := uuid.New()
	if _, err := e.pg.BeginRunnerExecution(ctx, rid, run.ID, uuid.Nil, gen, runnerwire.MethodExec); err != nil {
		t.Fatal(err)
	}
	return &containmentEnv{poolEnv: e, runnerID: rid, run: run, gen: gen}
}

func (e *containmentEnv) tuple(reuses ...runner.NodeSample) *runner.NodeTuple {
	return &runner.NodeTuple{RunID: e.run.ID, Reuses: reuses, Pods: []runner.NodeTuplePod{{Component: "proxy", UID: "uid-proxy-1", NodeName: "n1",
		CausalUID: "uid-n1", PostBind: &runner.NodeSample{Name: "n1", UID: "uid-n1", MachineID: "m1", SystemUUID: "s1"}}}}
}

// page sends one Final state page for the run: the report p, or none, and the proxy-stopped stamp.
func (e *containmentEnv) page(p *runnerwire.ContainmentPending, stopped *time.Time) error {
	e.t.Helper()
	return finalStatePage(e.t, e.srv, e.runnerID, runnerwire.RunState{RunID: e.run.ID, SandboxPresent: true, ContainmentPending: p, ProxyStoppedAt: stopped,
		Execution: &runnerwire.ExecutionResult{Generation: e.gen, Operation: runnerwire.MethodExec, Attempted: true}})
}

func (e *containmentEnv) mustPage(p *runnerwire.ContainmentPending, stopped *time.Time) {
	e.t.Helper()
	if err := e.page(p, stopped); err != nil {
		e.t.Fatalf("state page: %v", err)
	}
}

func (e *containmentEnv) containment(c *http.Cookie) types.RunContainment {
	e.t.Helper()
	w := e.call(c, http.MethodGet, "/api/v1/runs/"+e.run.ID.String()+"/containment", "")
	if w.Code != http.StatusOK {
		e.t.Fatalf("GET containment = %d %s", w.Code, w.Body)
	}
	var out types.RunContainment
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		e.t.Fatal(err)
	}
	return out
}

// recordW posts a node record as c (nil: the admin token) naming tuple.
func (e *containmentEnv) recordW(c *http.Cookie, attestation string, tuple json.RawMessage) *httptest.ResponseRecorder {
	e.t.Helper()
	body, _ := json.Marshal(types.RunNodeRecordRequest{Attestation: attestation, Source: types.NodeRecordSourceProviderConsole, SourceID: "i-0abc123", Tuple: tuple})
	return e.as(c, http.MethodPost, "/api/v1/runs/"+e.run.ID.String()+"/node-record", string(body))
}

func (e *containmentEnv) record(c *http.Cookie, attestation string, tuple json.RawMessage) (int, string) {
	e.t.Helper()
	w := e.recordW(c, attestation, tuple)
	return w.Code, w.Body.String()
}

// renew asks for a new run token over the run's runner's relay, as its proxy does.
func (e *containmentEnv) renew() *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/token/renew", nil)
	req.Header.Set("Authorization", "Bearer "+e.h.mintRunToken(e.t, e.run.ID))
	req = req.WithContext(relayConnContext(req.Context(), relayedConn{runner: e.runnerID}))
	w := httptest.NewRecorder()
	e.srv.relayRoutes(e.srv.InternalHandler()).ServeHTTP(w, req)
	return w
}

func (e *containmentEnv) held() bool {
	e.t.Helper()
	held, err := e.pg.RunHeldContainedOnRunner(context.Background(), e.run.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return held
}

// recordActions is the run's PENDING stop_proxy actions that carry a node record.
func (e *containmentEnv) recordActions() []runnerwire.PendingAction {
	e.t.Helper()
	page, err := e.srv.PendingFromRunner(context.Background(), e.runnerID)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []runnerwire.PendingAction
	for _, a := range page {
		if a.RunID == e.run.ID && a.Kind == types.RunnerActionStopProxy && a.NodeRecord != nil {
			out = append(out, a)
		}
	}
	return out
}

func (e *containmentEnv) answer(actionID uuid.UUID, outcome, reason string) {
	e.t.Helper()
	res := runnerwire.ActionResult{ID: actionID, Outcome: outcome, Reason: reason, ObservedAt: time.Now().Add(-time.Second)}
	if err := e.srv.EventFromRunner(context.Background(), e.runnerID, runnerwire.Event{Kind: runnerwire.EventActionResult, RunID: &e.run.ID, Data: mustJSON(res)}); err != nil {
		e.t.Fatalf("action result: %v", err)
	}
}

// TestPG_RunContainmentPendingIsKeptHeldAndBounded: the report is kept until the runner reports the proxy
// stopped, never cleared by a page that merely omits it; its flags only latch on; the run is held for
// leases meanwhile; and run detail shows the egress bound from the last renewal, the machines to
// security operators only.
func TestPG_RunContainmentPendingIsKeptHeldAndBounded(t *testing.T) {
	e := newContainmentEnv(t, false)
	ctx := context.Background()
	since := time.Now().Add(-10 * time.Minute).UTC().Truncate(time.Microsecond)
	report := &runnerwire.ContainmentPending{Kind: runner.ContainmentAgentOnly, Since: since, Tuple: e.tuple()}
	if e.held() {
		t.Fatal("a run with no report is held")
	}
	e.mustPage(report, nil)
	if rows := e.auditData(auditRunContainmentPending, e.run.ID.String()); len(rows) != 1 || rows[0]["state"] != store.ContainmentPendingEntered {
		t.Fatalf("containment audit rows = %v", rows)
	}
	if !e.held() {
		t.Fatal("a containment-pending run is not held for leases")
	}
	// The second layer behind the runner's relay fence: a renewal over the relay gets no token, audited.
	if w := e.renew(); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), reasonRunContainmentPending) || strings.Contains(w.Body.String(), `"token"`) {
		t.Fatalf("renew of a containment-pending run = %d %s, want 403 %s", w.Code, w.Body, reasonRunContainmentPending)
	}
	if !hasAudit(e.h, "identity.renew", "denied") {
		t.Fatal("no identity.renew denied row for a containment-pending run")
	}

	var renewed time.Time
	if err := e.pool.QueryRow(ctx, `SELECT token_renewed_at FROM agent_runs WHERE id=$1`, e.run.ID).Scan(&renewed); err != nil {
		t.Fatal(err)
	}
	got := e.containment(e.person)
	if p := got.Pending; p == nil || p.Kind != runner.ContainmentAgentOnly || !p.Since.Equal(since) || !p.BoundUntil.Equal(renewed.Add(identity.RunTokenTTL)) ||
		p.BoundPassed || p.NameReused || p.TwoPeople || len(p.Tuple) != 0 || got.Records == nil || len(got.Records) != 0 {
		t.Fatalf("the owner's containment = %+v", got)
	}
	if p := e.containment(e.alice).Pending; p == nil || !store.SameJSON(p.Tuple, mustJSON(e.tuple())) {
		t.Fatalf("a security operator's containment = %+v, want the tuple", p)
	}
	if w := e.call(e.other, http.MethodGet, "/api/v1/runs/"+e.run.ID.String()+"/containment", ""); w.Code != http.StatusNotFound {
		t.Fatalf("another person's GET = %d, want 404", w.Code)
	}

	// The reuse latches on, is audited once, and a later report without it does not clear it.
	reused := *report
	reused.NameReused, reused.Since = true, since.Add(time.Minute)
	e.mustPage(&reused, nil)
	e.mustPage(report, nil)
	if rows := e.auditData(auditRunContainmentPending, e.run.ID.String()); len(rows) != 2 || rows[1]["state"] != store.ContainmentPendingNameReused {
		t.Fatalf("containment audit rows after a reuse = %v", rows)
	}
	if p := e.containment(e.alice).Pending; p == nil || !p.NameReused || !p.TwoPeople || !p.Since.Equal(since) {
		t.Fatalf("after a reuse and a report without it = %+v, want it latched with the first since", p)
	}

	// A page that omits the report keeps it; a report beside a stopped proxy is refused.
	e.mustPage(nil, nil)
	if e.containment(e.alice).Pending == nil || !e.held() {
		t.Fatal("a page without the report cleared it")
	}
	stopped := time.Now()
	if err := e.page(report, &stopped); err == nil {
		t.Fatal("a containment-pending report beside a stopped proxy was accepted")
	}

	// Past the bound, egress is bounded but termination unknown.
	if _, err := e.pool.Exec(ctx, `UPDATE agent_runs SET token_renewed_at = now() - interval '61 minutes' WHERE id=$1`, e.run.ID); err != nil {
		t.Fatal(err)
	}
	if p := e.containment(e.person).Pending; p == nil || !p.BoundPassed {
		t.Fatalf("past the bound = %+v, want bound_passed", p)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE agent_runs SET token_renewed_at = now() WHERE id=$1`, e.run.ID); err != nil {
		t.Fatal(err)
	}

	// Only the proxy-stopped stamp, sent on completion evidence, clears it.
	e.mustPage(nil, &stopped)
	if rows := e.auditData(auditRunContainmentPending, e.run.ID.String()); len(rows) != 3 || rows[2]["state"] != store.ContainmentPendingCleared {
		t.Fatalf("containment audit rows after the stop = %v", rows)
	}
	if e.containment(e.person).Pending != nil || e.held() {
		t.Fatal("a stopped proxy left the run containment pending")
	}
}

// TestPG_NodeRecordOnePerson: a causal run's record is one security operator's, is never the admin
// token's, must name the tuple shown and fit the run, and is carried to the runner on a stop_proxy whose
// answer the record keeps.
func TestPG_NodeRecordOnePerson(t *testing.T) {
	e := newContainmentEnv(t, false)
	e.mustPage(&runnerwire.ContainmentPending{Kind: runner.ContainmentPendingDelete, Since: time.Now().Add(-time.Hour), Tuple: e.tuple()}, nil)
	tuple := e.containment(e.alice).Pending.Tuple

	for name, c := range map[string]struct {
		cookie      *http.Cookie
		attestation string
		tuple       json.RawMessage
		code        int
		says        string
	}{
		"the admin token":                 {nil, types.NodeRecordPoweredOff, tuple, http.StatusForbidden, reasonNodeRecordHumanOnly},
		"the run's owner":                 {e.person, types.NodeRecordPoweredOff, tuple, http.StatusForbidden, ""},
		"no prior holder on a causal run": {e.alice, types.NodeRecordNoPriorHolder, tuple, http.StatusConflict, "only for a run with no causal association"},
		"a tuple that moved":              {e.alice, types.NodeRecordPoweredOff, mustJSON(e.tuple(runner.NodeSample{Name: "n1", UID: "x"})), http.StatusConflict, "changed since you loaded it"},
		"no tuple":                        {e.alice, types.NodeRecordPoweredOff, nil, http.StatusBadRequest, reasonNodeRecordInvalid},
		"an unknown attestation":          {e.alice, "node_gone", tuple, http.StatusBadRequest, reasonNodeRecordInvalid},
	} {
		if code, body := e.record(c.cookie, c.attestation, c.tuple); code != c.code || !strings.Contains(body, c.says) {
			t.Errorf("%s = %d %s, want %d %q", name, code, body, c.code, c.says)
		}
	}
	if n := len(e.recordActions()); n != 0 {
		t.Fatalf("a refused record queued %d actions", n)
	}

	code, body := e.record(e.alice, types.NodeRecordPoweredOff, tuple)
	if code != http.StatusCreated {
		t.Fatalf("a causal run's record = %d %s", code, body)
	}
	if code, body := e.record(e.bob, types.NodeRecordPoweredOff, tuple); code != http.StatusConflict || !strings.Contains(body, "already waiting") {
		t.Fatalf("a second record while one waits = %d %s", code, body)
	}
	actions := e.recordActions()
	if len(actions) != 1 {
		t.Fatalf("PENDING carries %d records, want one", len(actions))
	}
	rec := actions[0].NodeRecord
	if rec.Attestation != runner.AttestNodePoweredOff || rec.Source != types.NodeRecordSourceProviderConsole || rec.SourceID != "i-0abc123" ||
		len(rec.Approvers) != 1 || !rec.Approvers[0].Human || !strings.HasPrefix(rec.Approvers[0].Principal, "hmac-sha256:") || rec.FourEyes ||
		!store.SameJSON(mustJSON(rec.Tuple), tuple) || actions[0].Ref != e.run.SandboxRef {
		t.Fatalf("the record on PENDING = %+v", actions[0])
	}
	if rows := e.auditData(auditRunNodeRecord, e.run.ID.String()); len(rows) != 1 || rows[0]["source_id"] != "i-0abc123" ||
		rows[0]["source"] != types.NodeRecordSourceProviderConsole || rows[0]["people"] != float64(1) {
		t.Fatalf("run.node.record rows = %v", rows)
	}

	// The runner refuses it: the record says so, and the run stays pending.
	e.answer(actions[0].ID, types.RunnerActionRefused, "a Node still holds proxy's node name n1 on the acceptance list")
	got := e.containment(e.alice)
	if got.Pending == nil || len(got.Records) != 1 || got.Records[0].State != types.NodeRecordRefused || !strings.Contains(got.Records[0].Reason, "still holds") {
		t.Fatalf("after a refusal = %+v", got)
	}

	// A new record goes out, the runner accepts it, and its stop report clears the run.
	if code, body := e.record(e.alice, types.NodeRecordPoweredOff, tuple); code != http.StatusCreated {
		t.Fatalf("a record after a refusal = %d %s", code, body)
	}
	actions = e.recordActions()
	if len(actions) != 1 {
		t.Fatalf("PENDING carries %d records after the refusal, want one", len(actions))
	}
	e.answer(actions[0].ID, types.RunnerActionSuccess, "")
	stopped := time.Now()
	e.mustPage(nil, &stopped)
	got = e.containment(e.alice)
	if got.Pending != nil || len(got.Records) != 2 || got.Records[0].State != types.NodeRecordAccepted || got.Records[0].DecidedAt == nil {
		t.Fatalf("after acceptance = %+v", got)
	}
}

// TestPG_NodeRecordTwoPeople: after a recorded reuse only "node powered off" is taken and it always needs
// two people, as does any record on the no-association path, whatever the four-eyes setting. The second
// person is a different human; the admin token is neither; and a runner record that moves meanwhile
// lapses the approvals.
func TestPG_NodeRecordTwoPeople(t *testing.T) {
	e := newContainmentEnv(t, false)
	reuse := runner.NodeSample{Name: "n1", UID: "uid-n1-new", MachineID: "m7", SystemUUID: "s7"}
	e.mustPage(&runnerwire.ContainmentPending{Kind: runner.ContainmentPendingDelete, Since: time.Now().Add(-time.Hour), NameReused: true, Tuple: e.tuple(reuse)}, nil)
	tuple := e.containment(e.alice).Pending.Tuple

	if code, body := e.record(e.alice, types.NodeRecordNoPriorHolder, tuple); code != http.StatusConflict || !strings.Contains(body, "node name was reused") {
		t.Fatalf("no prior holder after a reuse = %d %s", code, body)
	}
	ch := e.pending(e.recordW(e.alice, types.NodeRecordPoweredOff, tuple))
	if ch.TargetKind != govKindRunNodeRecord || len(e.recordActions()) != 0 {
		t.Fatalf("held change %+v, or a record went out before approval", ch)
	}
	approve := func(c *http.Cookie) (int, string) {
		w := e.as(c, http.MethodPost, "/api/v1/governance/changes/"+ch.ID.String()+"/approve", "")
		return w.Code, w.Body.String()
	}
	for name, c := range map[string]*http.Cookie{"the proposer": e.alice, "the proposer's mailbox": e.twin, "the admin token": nil} {
		if code, body := approve(c); code != http.StatusForbidden {
			t.Errorf("%s approved = %d %s, want 403", name, code, body)
		}
	}
	if len(e.recordActions()) != 0 || e.changeState(ch.ID) != string(types.GovernanceChangePending) {
		t.Fatal("a refused approval applied the record")
	}
	e.approve(ch.ID)
	actions := e.recordActions()
	if len(actions) != 1 || len(actions[0].NodeRecord.Approvers) != 2 || actions[0].NodeRecord.Approvers[0] == actions[0].NodeRecord.Approvers[1] {
		t.Fatalf("PENDING after approval = %+v, want one record by two people", actions)
	}
	if rows := e.auditData(auditRunNodeRecord, e.run.ID.String()); len(rows) != 1 || rows[0]["people"] != float64(2) || rows[0]["name_reused"] != true {
		t.Fatalf("run.node.record rows = %v", rows)
	}
}

// TestPG_NodeRecordNoAssociationNeedsTwoAndLapses: "no prior holder" is taken on the no-association path,
// always held for a second person, and a runner record that moves before approval makes it stale.
func TestPG_NodeRecordNoAssociationNeedsTwoAndLapses(t *testing.T) {
	e := newContainmentEnv(t, false)
	report := &runnerwire.ContainmentPending{Kind: runner.ContainmentPendingDelete, Since: time.Now().Add(-time.Hour), NoCausalAssociation: true, Tuple: e.tuple()}
	e.mustPage(report, nil)
	tuple := e.containment(e.alice).Pending.Tuple
	ch := e.pending(e.recordW(e.alice, types.NodeRecordNoPriorHolder, tuple))

	moved := *report
	moved.Tuple = e.tuple(runner.NodeSample{Name: "n1", UID: "uid-n1-new", MachineID: "m7", SystemUUID: "s7"})
	e.mustPage(&moved, nil)
	if w := e.as(e.bob, http.MethodPost, "/api/v1/governance/changes/"+ch.ID.String()+"/approve", ""); w.Code != http.StatusConflict {
		t.Fatalf("approving a record whose runner record moved = %d %s, want 409 stale", w.Code, w.Body)
	}
	if len(e.recordActions()) != 0 {
		t.Fatal("a stale record went out")
	}
}
