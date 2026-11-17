// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// r3bGetRunErrStore is a recordStore whose run read FAILS — the one shape
// learnVerifyEgress's folded give-up made indistinguishable from "not a record
// run". Everything else routes to recordStore so the decide path is otherwise
// the normal one.
type r3bGetRunErrStore struct {
	*recordStore
	err error
	// afterDecide, when set, lets the run read succeed until approval apID is decided, so the
	// fault lands at the post-decision write-back and not at the pre-decision gates.
	afterDecide *fakeApprovals
	apID        uuid.UUID
}

func (s *r3bGetRunErrStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	if s.afterDecide != nil {
		s.afterDecide.mu.Lock()
		state := s.afterDecide.byID[s.apID].State
		s.afterDecide.mu.Unlock()
		if state != types.ApprovalApproved {
			return s.recordStore.GetRun(ctx, id)
		}
	}
	return types.AgentRun{}, s.err
}

// r3bRequirementWriteFailures returns the workspace.requirement.write failure
// rows recorded so far, newest last.
func r3bRequirementWriteFailures(events []types.AuditEvent) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range events {
		if ev.Action == "workspace.requirement.write" && ev.Outcome == "failure" {
			out = append(out, ev)
		}
	}
	return out
}

// r3bAuditDetail pulls the "detail" string out of an audit event's data blob.
func r3bAuditDetail(t *testing.T, ev types.AuditEvent) string {
	t.Helper()
	var data struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatalf("audit data %q is not a JSON object: %v", string(ev.Data), err)
	}
	return data.Detail
}

// TestLearnVerifyEgressAuditsEveryGiveUp pins the write-back
// contract.
//
// approvals_writeback.go's file header states the contract these functions
// share and the rest of the package does not: FAIL SILENT BUT AUDITED. The
// decision already stands by the time the write-back runs, so no give-up may
// fail the request — which means every give-up has to leave an audit row
// instead, "or the operator gets a green UI and a workspace that learned
// nothing". Two arms must not return bare:
//
//   - the GetRun error, folded in with the two NOT-APPLICABLE conditions beside
//     it (no workspace link, not a record run) so an unreadable run answered
//     exactly like an ordinary dev-task approval;
//   - the nil Store, folded in with the kind check at the top.
//
// The control is the junk-host arm, which audits its miss and is what
// makes this a contract rather than one branch's taste. The
// not-applicable conditions must STILL stay silent — a failure row on every
// plain-run approval would drown the two that mean something — so this pins
// both directions.
func TestLearnVerifyEgressAuditsEveryGiveUp(t *testing.T) {
	// ticket: R3B
	// The two give-ups are driven on the write-back itself. Through the handler neither is reached any
	// more: an approve whose run cannot be read, or on a server with no run store, is refused before
	// any decision (the network-preset check reads the run, 503 run_unreadable), which the third arm
	// pins. The write-back still owes its audited miss to any caller that reaches it with a decision.
	decided := func(runID uuid.UUID) types.ApprovalRequest {
		return types.ApprovalRequest{ID: uuid.New(), RunID: runID, Kind: types.ApprovalEgressDomain, State: types.ApprovalApproved,
			RequestedScope: mustJSON(map[string]string{"host": "registry.npmjs.org", "mode": "wait_for_review"})}
	}
	unreadable := func(t *testing.T) (*harness, *r3bGetRunErrStore, *Server, uuid.UUID) {
		h := newHarness(t)
		runID, wsID := uuid.New(), uuid.New()
		fake := &r3bGetRunErrStore{
			recordStore: &recordStore{
				run:             types.AgentRun{ID: runID, WorkspaceID: &wsID, Task: "workspace record", State: types.RunRunning},
				importStateFake: importStateFake{ws: types.Workspace{ID: wsID}},
			},
			err: errors.New("read run: connection reset by peer"),
		}
		cfg := baseTestConfig(h, fake)
		cfg.Approvals = h.approvals
		return h, fake, New(cfg), runID
	}

	t.Run("GetRun failure audits the miss", func(t *testing.T) {
		h, fake, srv, runID := unreadable(t)
		srv.learnVerifyEgress(context.Background(), decided(runID), types.ActorHuman, "admin@corp.example")
		if len(fake.ws.Requirements) != 0 {
			t.Fatalf("requirements = %+v, want NOTHING written", fake.ws.Requirements)
		}
		fails := r3bRequirementWriteFailures(h.audit.snapshot())
		if len(fails) != 1 {
			t.Fatalf("workspace.requirement.write failure rows = %d, want exactly 1 naming the run-read step", len(fails))
		}
		// The row has to say which step failed — "the contract was not written" is the
		// symptom the operator can already see; the step is the part only this row
		// carries.
		if detail := r3bAuditDetail(t, fails[0]); detail == "" || detail == "invalid or empty host in requested_scope" {
			t.Errorf("failure row detail = %q, want the run-read step", detail)
		}
	})

	t.Run("a run read that fails after the decision audits the miss through the handler", func(t *testing.T) {
		h, fake, srv, runID := unreadable(t)
		apID := seedEgressApproval(h, runID, "registry.npmjs.org")
		fake.afterDecide, fake.apID = h.approvals, apID
		w := do(t, srv, http.MethodPost, "/api/v1/approvals/"+apID.String()+"/approve", adminToken, "")
		// The decision itself must still stand: this write-back may never fail
		// the request, which is exactly why it owes an audit row.
		if w.Code != http.StatusOK {
			t.Fatalf("approve: code = %d, body=%s", w.Code, w.Body.String())
		}
		if len(fake.ws.Requirements) != 0 {
			t.Fatalf("requirements = %+v, want NOTHING written", fake.ws.Requirements)
		}
		fails := r3bRequirementWriteFailures(h.audit.snapshot())
		if len(fails) != 1 {
			t.Fatalf("workspace.requirement.write failure rows = %d, want exactly 1 naming the run-read step", len(fails))
		}
		if detail := r3bAuditDetail(t, fails[0]); detail != writebackAuditRunUnread {
			t.Errorf("failure row detail = %q, want %q: the step, never the store's text", detail, writebackAuditRunUnread)
		}
	})

	t.Run("nil store audits the miss", func(t *testing.T) {
		h := newHarness(t)
		cfg := baseTestConfig(h, nil) // no store at all
		cfg.Approvals = h.approvals
		srv := New(cfg)

		srv.learnVerifyEgress(context.Background(), decided(uuid.New()), types.ActorHuman, "admin@corp.example")
		fails := r3bRequirementWriteFailures(h.audit.snapshot())
		if len(fails) != 1 {
			t.Fatalf("workspace.requirement.write failure rows = %d, want exactly 1 naming the missing store", len(fails))
		}
		// Same wording persistWorkspaceEgressDecision's nil-Store arm uses, so
		// one audit query answers "why did this decision leave nothing behind"
		// across both write-backs.
		if detail := r3bAuditDetail(t, fails[0]); detail != "no store configured" {
			t.Errorf("failure row detail = %q, want %q", detail, "no store configured")
		}
	})

	t.Run("an approve of a run that cannot be read decides nothing", func(t *testing.T) {
		h, fake, srv, runID := unreadable(t)
		apID := seedEgressApproval(h, runID, "registry.npmjs.org")
		w := do(t, srv, http.MethodPost, "/api/v1/approvals/"+apID.String()+"/approve", adminToken, "")
		if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"reason":"run_unreadable"`) {
			t.Fatalf("approve: code = %d, body=%s, want 503 run_unreadable", w.Code, w.Body.String())
		}
		h.approvals.mu.Lock()
		state := h.approvals.byID[apID].State
		h.approvals.mu.Unlock()
		if state == types.ApprovalApproved {
			t.Errorf("approval state = %s, want it still undecided", state)
		}
		if len(fake.ws.Requirements) != 0 {
			t.Errorf("requirements = %+v, want NOTHING written", fake.ws.Requirements)
		}
		if fails := r3bRequirementWriteFailures(h.audit.snapshot()); len(fails) != 0 {
			t.Errorf("workspace.requirement.write failure rows = %d, want 0: no decision, so no write-back", len(fails))
		}
	})

	// The other half of the contract: the two conditions that are NOT give-ups
	// must stay silent. Splitting the folded return is only correct if it did
	// not turn every ordinary approval into a recorded miss.
	for _, tc := range []struct {
		name string
		run  types.AgentRun
	}{
		{name: "plain run is not applicable", run: types.AgentRun{Task: "dev task", State: types.RunRunning}},
		{name: "no workspace link is not applicable", run: types.AgentRun{Task: "workspace record", State: types.RunRunning}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			runID, wsID := uuid.New(), uuid.New()
			run := tc.run
			run.ID = runID
			if run.Task == "dev task" {
				run.WorkspaceID = &wsID
			}
			fake := &recordStore{run: run, importStateFake: importStateFake{ws: types.Workspace{ID: wsID}}}
			cfg := baseTestConfig(h, fake)
			cfg.Approvals = h.approvals
			srv := New(cfg)

			apID := seedEgressApproval(h, runID, "registry.npmjs.org")
			w := do(t, srv, http.MethodPost, "/api/v1/approvals/"+apID.String()+"/approve", adminToken, "")
			if w.Code != http.StatusOK {
				t.Fatalf("approve: code = %d, body=%s", w.Code, w.Body.String())
			}
			if fails := r3bRequirementWriteFailures(h.audit.snapshot()); len(fails) != 0 {
				t.Errorf("workspace.requirement.write failure rows = %d, want 0 — a not-applicable "+
					"condition must not be recorded as a miss (detail=%q)", len(fails), r3bAuditDetail(t, fails[0]))
			}
		})
	}
}

// TestLearnVerifyEgressMergeFailureNamesItsClass: a requirements write the store fails leaves one failure row
// whose detail is the class the store's sentinel tells apart, or "store error" for any other failure, and never
// the store's text, which only the server log keeps.
func TestLearnVerifyEgressMergeFailureNamesItsClass(t *testing.T) {
	raw := errors.New("write to db.internal:5432 refused")
	for name, c := range map[string]struct {
		err  error
		want string
	}{
		"the key cap":             {fmt.Errorf("merge: %w", store.ErrConflict), writebackAuditRequirementsFull},
		"a workspace not there":   {fmt.Errorf("merge: %w", store.ErrNotFound), writebackAuditWorkspaceGone},
		"any other store failure": {raw, writebackAuditStoreError},
	} {
		t.Run(name, func(t *testing.T) {
			logs := captureSlog(t)
			h := newHarness(t)
			runID, wsID := uuid.New(), uuid.New()
			fake := &recordStore{
				run:             types.AgentRun{ID: runID, WorkspaceID: &wsID, Task: "workspace record", State: types.RunRunning},
				importStateFake: importStateFake{ws: types.Workspace{ID: wsID}},
				mergeErr:        c.err,
			}
			cfg := baseTestConfig(h, fake)
			cfg.Approvals = h.approvals
			ap := types.ApprovalRequest{ID: uuid.New(), RunID: runID, Kind: types.ApprovalEgressDomain, State: types.ApprovalApproved,
				RequestedScope: mustJSON(map[string]string{"host": "registry.npmjs.org", "mode": "wait_for_review"})}
			New(cfg).learnVerifyEgress(context.Background(), ap, types.ActorHuman, "admin@corp.example")
			fails := r3bRequirementWriteFailures(h.audit.snapshot())
			if len(fails) != 1 {
				t.Fatalf("workspace.requirement.write failure rows = %d, want 1", len(fails))
			}
			if detail := r3bAuditDetail(t, fails[0]); detail != c.want {
				t.Errorf("failure row detail = %q, want %q", detail, c.want)
			}
			if d := string(fails[0].Data); strings.Contains(d, "db.internal") || strings.Contains(d, "store: ") {
				t.Errorf("failure row data = %s, want no store text", d)
			}
			if c.err == raw && !strings.Contains(logs.String(), "db.internal") {
				t.Errorf("log = %q, want the store's error", logs.String())
			}
		})
	}
}

// r3bEgressWriteErrStore is the fixture store whose egress list write fails with err.
type r3bEgressWriteErrStore struct {
	*authzStore
	err error
}

func (s r3bEgressWriteErrStore) AddWorkspaceEgressDecision(context.Context, uuid.UUID, string, bool, int) (types.Workspace, error) {
	return types.Workspace{}, s.err
}

// TestEgressDecisionWriteFailureNamesItsClass: an `always` decision whose egress list write the store fails leaves
// one failure row whose detail is the class the store's sentinel tells apart, or "store error" for any other
// failure, and never the store's text, which only the server log keeps.
func TestEgressDecisionWriteFailureNamesItsClass(t *testing.T) {
	raw := errors.New("write to db.internal:5432 refused")
	for name, c := range map[string]struct {
		err  error
		want string
	}{
		"the list at its cap or edited": {fmt.Errorf("add: %w", store.ErrConflict), writebackAuditEgressRefused},
		"a workspace not there":         {fmt.Errorf("add: %w", store.ErrNotFound), writebackAuditWorkspaceGone},
		"any other store failure":       {raw, writebackAuditStoreError},
	} {
		t.Run(name, func(t *testing.T) {
			logs := captureSlog(t)
			h := newHarness(t)
			runID, wsID := uuid.New(), uuid.New()
			cfg := baseTestConfig(h, r3bEgressWriteErrStore{authzStore: newAuthzStore(), err: c.err})
			cfg.Approvals = h.approvals
			ap := types.ApprovalRequest{ID: uuid.New(), RunID: runID, Kind: types.ApprovalEgressDomain, State: types.ApprovalApproved,
				RequestedScope: mustJSON(map[string]string{"host": "registry.npmjs.org", "mode": "wait_for_review"})}
			New(cfg).persistWorkspaceEgressDecision(context.Background(), ap, wsID, true, types.ActorHuman, "admin@corp.example")
			var fails []types.AuditEvent
			for _, ev := range h.audit.snapshot() {
				if ev.Action == "workspace.egress.approve" && ev.Outcome == "failure" {
					fails = append(fails, ev)
				}
			}
			if len(fails) != 1 {
				t.Fatalf("workspace.egress.approve failure rows = %d, want 1", len(fails))
			}
			if detail := r3bAuditDetail(t, fails[0]); detail != c.want {
				t.Errorf("failure row detail = %q, want %q", detail, c.want)
			}
			if d := string(fails[0].Data); strings.Contains(d, "db.internal") || strings.Contains(d, "store: ") {
				t.Errorf("failure row data = %s, want no store text", d)
			}
			if c.err == raw && !strings.Contains(logs.String(), "db.internal") {
				t.Errorf("log = %q, want the store's error", logs.String())
			}
		})
	}
}
