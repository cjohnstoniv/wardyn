// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/approval"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// r3bBypassRows returns the approval.second_human.bypass rows recorded so far.
func r3bBypassRows(events []types.AuditEvent) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range events {
		if ev.Action == "approval.second_human.bypass" {
			out = append(out, ev)
		}
	}
	return out
}

// r3bSecondHumanFixture is an admin-token server holding one run and whatever
// approvals the caller seeds. Deliberately NOT LocalMode: the local-mode
// refusal answers before the gate's comparison and would pin the wrong branch.
func r3bSecondHumanFixture(t *testing.T) (*harness, *Server, uuid.UUID) {
	t.Helper()
	h := newHarness(t)
	runID, wsID := uuid.New(), uuid.New()
	fake := &recordStore{
		run:             types.AgentRun{ID: runID, WorkspaceID: &wsID, Task: "dev task", State: types.RunRunning, CreatedBy: "someone@corp.example"},
		importStateFake: importStateFake{ws: types.Workspace{ID: wsID}},
	}
	cfg := baseTestConfig(h, fake)
	cfg.Approvals = h.approvals
	srv := New(cfg)
	if srv.cfg.LocalMode {
		t.Fatal("fixture is in LocalMode — the local-mode refusal would answer before the break-glass branch")
	}
	return h, srv, runID
}

// TestR3BSecondHumanBypassIsScopedToDecisionsTheGateGoverns is F147's pin.
//
// approval.second_human.bypass is the record that a four-eyes rule WAS bypassed
// — that is how docs/ENV.md, docs/OPERATIONS.md and threatmodel/THREAT-MODEL.md
// all describe it, and ENV.md adds "Scoped to egress_domain only". The row was
// written at the TOP of requireSecondHuman for any admin-token caller while the
// switch was on: before the kind check, before the approval was known to exist,
// and before the decision. So it recorded break-glass for approvals the gate
// never governs, for ids that do not exist, and for requests that then failed.
//
// Each arm below is one of those three, and the control is the real break-glass
// — without it the whole thing passes by never writing the row at all.
func TestR3BSecondHumanBypassIsScopedToDecisionsTheGateGoverns(t *testing.T) {
	t.Run("control: a real egress break-glass is still recorded", func(t *testing.T) {
		t.Setenv(envEgressSecondHuman, "1")
		h, srv, runID := r3bSecondHumanFixture(t)
		apID := seedEgressApproval(h, runID, "registry.npmjs.org")

		w := do(t, srv, http.MethodPost, "/api/v1/approvals/"+apID.String()+"/approve", adminToken, "")
		if w.Code != http.StatusOK {
			t.Fatalf("admin-token break-glass: code = %d, body=%s", w.Code, w.Body.String())
		}
		rows := r3bBypassRows(h.audit.events)
		if len(rows) != 1 {
			t.Fatalf("approval.second_human.bypass rows = %d, want exactly 1 — the break-glass must not go silent", len(rows))
		}
		if rows[0].ActorType != types.ActorSystem || rows[0].Actor != adminTokenPrincipal {
			t.Errorf("bypass actor = %s/%s, want %s/%s", rows[0].ActorType, rows[0].Actor, types.ActorSystem, adminTokenPrincipal)
		}
		if rows[0].Target != apID.String() {
			t.Errorf("bypass target = %q, want the approval id %q", rows[0].Target, apID.String())
		}
		// Correlation: the row now carries the run, so the approval.decide it
		// "sits beside" is findable from it rather than only by timestamp.
		if rows[0].RunID == nil || *rows[0].RunID != runID {
			t.Errorf("bypass run_id = %v, want the decided run %v", rows[0].RunID, runID)
		}
	})

	t.Run("a credential approval writes no bypass row", func(t *testing.T) {
		t.Setenv(envEgressSecondHuman, "1")
		h, srv, runID := r3bSecondHumanFixture(t)
		apID := uuid.New()
		h.approvals.byID[apID] = types.ApprovalRequest{
			ID: apID, RunID: runID, Kind: types.ApprovalCredential,
			RequestedScope: json.RawMessage(`{"host":"dev.azure.com","secret_name":"ado-pat"}`),
			State:          types.ApprovalPending,
		}

		w := do(t, srv, http.MethodPost, "/api/v1/approvals/"+apID.String()+"/approve", adminToken, "")
		if w.Code != http.StatusOK {
			t.Fatalf("credential approve: code = %d, body=%s", w.Code, w.Body.String())
		}
		if rows := r3bBypassRows(h.audit.events); len(rows) != 0 {
			t.Errorf("approval.second_human.bypass rows = %d, want 0 — docs/ENV.md scopes this switch to "+
				"egress_domain, so a kind the gate never governs cannot have been bypassed", len(rows))
		}
	})

	t.Run("a non-existent approval writes no bypass row", func(t *testing.T) {
		t.Setenv(envEgressSecondHuman, "1")
		h, srv, _ := r3bSecondHumanFixture(t)

		w := do(t, srv, http.MethodPost, "/api/v1/approvals/"+uuid.New().String()+"/approve", adminToken, "")
		if w.Code != http.StatusNotFound {
			t.Fatalf("unknown approval: code = %d, want 404; body=%s", w.Code, w.Body.String())
		}
		if rows := r3bBypassRows(h.audit.events); len(rows) != 0 {
			t.Errorf("approval.second_human.bypass rows = %d, want 0 — there is no approval here to have "+
				"bypassed a gate on", len(rows))
		}
	})

	// F318. This subtest used to require ZERO rows here, and the concern behind
	// that is kept verbatim below: nothing was decided, so no row may CLAIM a
	// decision. But the bypass itself did happen — the gate was passed, at the
	// gate, before Decide was ever called — and docs/ENV.md promises the
	// operator that each admin-token bypass writes approval.second_human.bypass.
	// An emit that fired only on success made the count of break-glass uses
	// depend on whether the store answered, so a caller who never completes a
	// decision left nothing behind at all. The OUTCOME is what tells the two
	// apart, which is why the emit stays below Decide rather than moving back
	// into the gate.
	for _, tc := range []struct {
		name      string
		decideErr error
		wantCode  int
		wantClass string
	}{
		{"a store failure", errors.New("decide approval: connection reset by peer"), http.StatusInternalServerError, "error"},
		{"an already-decided approval", approval.ErrAlreadyDecided, http.StatusConflict, "already_decided"},
	} {
		t.Run("a decision that FAILS ("+tc.name+") writes a failure bypass row", func(t *testing.T) {
			t.Setenv(envEgressSecondHuman, "1")
			h, srv, runID := r3bSecondHumanFixture(t)
			apID := seedEgressApproval(h, runID, "registry.npmjs.org")
			h.approvals.decideErr = tc.decideErr

			w := do(t, srv, http.MethodPost, "/api/v1/approvals/"+apID.String()+"/approve", adminToken, "")
			if w.Code != tc.wantCode {
				t.Fatalf("failed decide: code = %d, want %d; body=%s", w.Code, tc.wantCode, w.Body.String())
			}
			rows := r3bBypassRows(h.audit.events)
			if len(rows) != 1 {
				t.Fatalf("approval.second_human.bypass rows = %d, want 1 — the admin-token caller got past the "+
					"four-eyes gate, and docs/ENV.md says each one writes a row; a break-glass that leaves nothing "+
					"behind when the store errors is a hole in the count an operator audits", len(rows))
			}
			// THE ORIGINAL ASSERTION, kept exactly: no row may say a four-eyes
			// rule was bypassed on a decision that WAS made, because none was.
			if rows[0].Outcome == "success" {
				t.Errorf("the bypass row for a FAILED decision has outcome=success — it names a break-glass on a " +
					"decision that never happened, which is the record this emit was moved below Decide to avoid")
			}
			if rows[0].Outcome != "failure" {
				t.Errorf("bypass row outcome = %q, want \"failure\"", rows[0].Outcome)
			}
			var data map[string]any
			if err := json.Unmarshal(rows[0].Data, &data); err != nil {
				t.Fatal(err)
			}
			if data["error"] != tc.wantClass {
				t.Errorf("bypass row data = %v, want error %q — a reader has to be able to tell a store outage from "+
					"a race on an already-decided approval without the raw error", data, tc.wantClass)
			}
			if data["reason"] != "admin_token_break_glass" {
				t.Errorf("bypass row data = %v, want reason admin_token_break_glass (unchanged from the success row)", data)
			}
		})
	}

	t.Run("switch off: no bypass row at all", func(t *testing.T) {
		h, srv, runID := r3bSecondHumanFixture(t)
		apID := seedEgressApproval(h, runID, "registry.npmjs.org")

		w := do(t, srv, http.MethodPost, "/api/v1/approvals/"+apID.String()+"/approve", adminToken, "")
		if w.Code != http.StatusOK {
			t.Fatalf("switch off: code = %d, body=%s", w.Code, w.Body.String())
		}
		if rows := r3bBypassRows(h.audit.events); len(rows) != 0 {
			t.Errorf("approval.second_human.bypass rows = %d with the switch unset, want 0", len(rows))
		}
	})
}
