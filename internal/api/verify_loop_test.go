// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// verify_loop_test.go — the Wave-4 gates. The verify loop is: a CONFINED
// record session HOLDS an off-policy host at the door (wait_for_review); the
// operator's approve — at decide(), the one chokepoint — lands the host as an
// `egress:<host>` required/operator_set row on THAT workspace's contract; and
// the next confined replay honors the folded row. Three properties, three
// tests, plus the negative (a plain run's approval writes nothing durable).

func seedEgressApproval(h *harness, runID uuid.UUID, host string) uuid.UUID {
	id := uuid.New()
	h.approvals.byID[id] = types.ApprovalRequest{
		ID: id, RunID: runID, Kind: types.ApprovalEgressDomain,
		RequestedScope: mustJSON(map[string]string{"host": host, "mode": "wait_for_review"}),
	}
	return id
}

// TestApproveDecision_RecordRun_WritesContractRow: approving an egress_domain
// request raised by a workspace record/verify run writes the row IMMEDIATELY
// into that workspace's requirements overlay — required (a human wants the
// replay to pass) and operator_set (a human clicked), audited with the run as
// its provenance.
func TestApproveDecision_RecordRun_WritesContractRow(t *testing.T) {
	h := newHarness(t)
	runID, wsID := uuid.New(), uuid.New()
	fake := &recordStore{
		run:             types.AgentRun{ID: runID, WorkspaceID: &wsID, Task: "workspace record", State: types.RunRunning},
		importStateFake: importStateFake{ws: types.Workspace{ID: wsID}},
	}
	cfg := baseTestConfig(h, fake)
	cfg.Approvals = h.approvals
	srv := New(cfg)

	apID := seedEgressApproval(h, runID, "Registry.NPMJS.org")
	w := do(t, srv, http.MethodPost, "/api/v1/approvals/"+apID.String()+"/approve", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("approve: code = %d, body=%s", w.Code, w.Body.String())
	}
	row, ok := fake.ws.Requirements["egress:registry.npmjs.org"] // lowercased
	if !ok || row.Level != "required" || row.Provenance != "operator_set" {
		t.Fatalf("requirements = %+v, want egress:registry.npmjs.org required/operator_set", fake.ws.Requirements)
	}
	found := false
	for _, ev := range h.audit.events {
		if ev.Action == "workspace.requirement.write" && ev.Outcome == "success" {
			found = true
		}
	}
	if !found {
		t.Error("no workspace.requirement.write audit event recorded")
	}
}

// TestApproveDecision_NoDurableWriteOffTheVerifyPath: the negatives, each a
// broken gate in the fail-silent chain — a PLAIN run's approval, a DENY, and a
// junk host must all leave the contract untouched (the approval/denial itself
// still stands for the in-flight session; nothing durable lands).
func TestApproveDecision_NoDurableWriteOffTheVerifyPath(t *testing.T) {
	cases := []struct {
		name, task, host, verb string
	}{
		{"plain run's approval", "dev task", "api.stripe.com", "approve"},
		{"deny on a record run", "workspace record", "api.stripe.com", "deny"},
		{"junk host shape", "workspace record", "localhost", "approve"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			runID, wsID := uuid.New(), uuid.New()
			fake := &recordStore{
				run:             types.AgentRun{ID: runID, WorkspaceID: &wsID, Task: tc.task, State: types.RunRunning},
				importStateFake: importStateFake{ws: types.Workspace{ID: wsID}},
			}
			cfg := baseTestConfig(h, fake)
			cfg.Approvals = h.approvals
			srv := New(cfg)

			apID := seedEgressApproval(h, runID, tc.host)
			w := do(t, srv, http.MethodPost, "/api/v1/approvals/"+apID.String()+"/"+tc.verb, adminToken, "")
			if w.Code != http.StatusOK {
				t.Fatalf("%s: code = %d, body=%s", tc.verb, w.Code, w.Body.String())
			}
			if len(fake.ws.Requirements) != 0 {
				t.Fatalf("requirements = %+v, want NOTHING written", fake.ws.Requirements)
			}
			// W19-W19b-5: the junk-host-shape gate used to fail silent, unlike
			// every other give-up path in learnVerifyEgress — it must now audit
			// the miss too, so an operator can see why the contract wasn't
			// updated instead of wondering why the next replay still holds.
			if tc.name == "junk host shape" {
				found := false
				for _, ev := range h.audit.events {
					if ev.Action == "workspace.requirement.write" && ev.Outcome == "failure" {
						found = true
					}
				}
				if !found {
					t.Error("no workspace.requirement.write failure audit event recorded for an invalid host shape")
				}
			}
		})
	}
}

// TestConfinedEgressDomains_HonorsFoldedRequiredRows: the replay allowlist
// unions the folded contract's REQUIRED egress: rows (workspace overlay ∪
// attached sources — EffectiveRequirements) beside the legacy ApprovedEgress
// lane. Without this, the host an operator just approved is denied again on
// the very next confined session. Optional rows stay out: a replay has no
// enabled-optional wire.
func TestConfinedEgressDomains_HonorsFoldedRequiredRows(t *testing.T) {
	ws := types.Workspace{
		ApprovedEgress: []string{"legacy.example.com"},
		EffectiveRequirements: map[string]types.WorkspaceRequirement{
			"egress:registry.npmjs.org":   {Level: "required", Provenance: "operator_set"},
			"egress:optional.example.com": {Level: "optional", Provenance: "operator_set"},
			"secret:STRIPE_KEY":           {Level: "required", Provenance: "operator_set"}, // not an egress row
		},
	}
	got := confinedEgressDomains(ws)
	if !slices.Contains(got, "registry.npmjs.org") {
		t.Errorf("allowlist %v missing the folded required egress row", got)
	}
	if !slices.Contains(got, "legacy.example.com") {
		t.Errorf("allowlist %v dropped the legacy approved lane", got)
	}
	if slices.Contains(got, "optional.example.com") {
		t.Errorf("allowlist %v must not include an optional row", got)
	}
}

// TestRecordVerify_ConfinedHoldsAtDoor pins the flip: a CONFINED verify
// session dispatches with FirstUseApproval=wait_for_review — the off-policy
// host parks at the door while the operator (present by construction: every
// session is interactive) decides in the live strip. The old deny_with_review
// rationale ("unattended probe must fail fast") described a session shape
// that no longer exists. An OPEN learning session stays always_deny (inert
// under allow-all).
func TestRecordVerify_ConfinedHoldsAtDoor(t *testing.T) {
	fr := &fakeRunner{}
	srv, _ := pgHarnessWithRunner(t, fr)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o600); err != nil {
		t.Fatalf("seed dir: %v", err)
	}
	for _, tc := range []struct {
		confined bool
		want     types.FirstUseMode
	}{
		{confined: true, want: types.FirstUseWaitForReview},
		{confined: false, want: types.FirstUseAlwaysDeny},
	} {
		wsName := "verify-hold-" + uuid.NewString()[:8]
		body := fmt.Sprintf(`{"name":%q,"sources":[{"type":"local_dir","path":%q}]}`, wsName, dir)
		w := do(t, srv, http.MethodPost, "/api/v1/workspaces", adminToken, body)
		if w.Code != http.StatusCreated {
			t.Fatalf("create workspace: code = %d, body=%s", w.Code, w.Body.String())
		}
		var ws types.Workspace
		if err := json.Unmarshal(w.Body.Bytes(), &ws); err != nil {
			t.Fatalf("decode workspace: %v", err)
		}

		recBody := fmt.Sprintf(`{"name":"probe","confined":%v}`, tc.confined)
		w = do(t, srv, http.MethodPost, "/api/v1/workspaces/"+ws.ID.String()+"/record", adminToken, recBody)
		if w.Code != http.StatusAccepted {
			t.Fatalf("record confined=%v: code = %d, body=%s", tc.confined, w.Code, w.Body.String())
		}
		if got := fr.lastSpec.ProxyConfig.Policy.FirstUseApproval; got != tc.want {
			t.Errorf("confined=%v dispatched FirstUseApproval=%q, want %q", tc.confined, got, tc.want)
		}
	}
}
