// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Approvals from more than one person on a held governance change: the minimum, the record each approver
// is judged on, what a recorded approval leaves pending, the last approval's re-check, and the take-back.
// Guarded by WARDYN_TEST_PG (throwawayPGPool): skipped cleanly when unset, must PASS when set. The test
// server's authenticator has no role map, so every signed-in record derives the admin role and the request
// tier (security admin) is the lower of the two.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func approvalPath(id uuid.UUID) string {
	return "/api/v1/governance/changes/" + id.String() + "/approval"
}

// minApprovals is the configuration mutation that sets WARDYN_GOVERNANCE_MIN_APPROVALS.
func minApprovals(n int) func(*Config) { return func(c *Config) { c.GovernanceMinApprovals = n } }

// signIn records the latest sign-in of a person, as the identity gate does at sign-in.
func (e *govEnv) signIn(sub, email string) {
	e.t.Helper()
	if err := e.pg.PutPrincipalSignIn(context.Background(), sub, email, []string{}, nil, false); err != nil {
		e.t.Fatalf("record sign-in of %s: %v", sub, err)
	}
}

// held is an update of profile p proposed by alice, held for approval.
func (e *govEnv) held() types.GovernanceChange {
	e.t.Helper()
	name := "approvals-" + uuid.NewString()[:8]
	p := e.seedProfile(name, "pypi.org")
	return e.pending(e.call(e.alice, http.MethodPut, "/api/v1/governance/units/"+p.ID.String(), profileBody(name, "pypi.org", "github.com")))
}

// read is GET /governance/changes/{id} as bob, decoded to a generic map.
func (e *govEnv) read(id uuid.UUID) map[string]any {
	e.t.Helper()
	w := e.call(e.bob, http.MethodGet, "/api/v1/governance/changes/"+id.String(), "")
	if w.Code != http.StatusOK {
		e.t.Fatalf("read change %s = %d %s", id, w.Code, w.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		e.t.Fatalf("decode change: %v", err)
	}
	return out
}

// approvalsOf is the approvals block of a decoded change, nil when it has none.
func approvalsOf(m map[string]any) map[string]any {
	a, _ := m["approvals"].(map[string]any)
	return a
}

// TestPG_Approvals_UnsetMinimumIsToday: with no minimum, an approval applies on the one decider's word with no
// sign-in record, no approval row is written, the reject reason stays optional, and neither GET nor the list
// carries an approvals block.
func TestPG_Approvals_UnsetMinimumIsToday(t *testing.T) {
	e := newGovEnv(t)
	ch := e.held()
	if a := approvalsOf(e.read(ch.ID)); a != nil {
		t.Errorf("GET carries an approvals block with the minimum unset: %v", a)
	}
	if w := e.call(e.bob, http.MethodGet, "/api/v1/governance/changes", ""); w.Code != http.StatusOK || strings.Contains(w.Body.String(), `"approvals"`) {
		t.Errorf("the list = %d %s, want no approvals key", w.Code, w.Body)
	}
	if w := e.call(e.bob, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("approve = %d %s, want 200", w.Code, w.Body)
	}
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM governance_change_approvals`).Scan(&n); err != nil || n != 0 {
		t.Errorf("approval rows = %d (%v), want none", n, err)
	}
	other := e.held()
	if w := e.call(e.bob, http.MethodPost, rejectPath(other.ID), ""); w.Code != http.StatusOK {
		t.Errorf("a reject with no reason = %d %s, want 200 with the minimum unset", w.Code, w.Body)
	}
}

// TestPG_Approvals_TwoPeopleApply: with a minimum of two, the first approval is recorded and the change stays
// pending, the same person cannot count twice, the proposer cannot approve, and the second person's approval
// applies it with the keys the approve row gains.
func TestPG_Approvals_TwoPeopleApply(t *testing.T) {
	e := newGovEnv(t, minApprovals(2))
	ch := e.held()
	if w := e.call(e.bob, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusForbidden {
		t.Fatalf("approve with no sign-in record = %d %s, want 403", w.Code, w.Body)
	}
	e.signIn(govSubBob, "bob@corp.example")
	e.signIn(govSubCarol, "carol@corp.example")

	w := e.call(e.bob, http.MethodPost, approvePath(ch.ID), `{"reason":"checked the diff"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("first approval = %d %s, want 200", w.Code, w.Body)
	}
	if got := e.changeState(ch.ID); got != types.GovernanceChangePending {
		t.Fatalf("state after one of two = %q, want pending", got)
	}
	a := approvalsOf(e.read(ch.ID))
	if a == nil || a["have"] != float64(1) || a["required"] != float64(2) {
		t.Fatalf("approvals block = %v, want have 1 of 2", a)
	}
	if n := len(e.audits("governance.change.approval")); n != 1 {
		t.Errorf("%d governance.change.approval rows, want 1", n)
	}
	if n := len(e.audits("governance.change.approve")); n != 0 {
		t.Errorf("%d governance.change.approve rows after a recorded approval, want 0", n)
	}
	if w := e.call(e.bob, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceChangeAlreadyApproved {
		t.Errorf("a repeat approval = %d %s, want 409 %s", w.Code, w.Body, reasonGovernanceChangeAlreadyApproved)
	}
	if w := e.call(e.alice, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusForbidden {
		t.Errorf("the proposer approving = %d %s, want 403", w.Code, w.Body)
	}

	if w := e.call(e.carol, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("second approval = %d %s, want 200", w.Code, w.Body)
	}
	if got := e.changeState(ch.ID); got != types.GovernanceChangeApplied {
		t.Fatalf("state after two of two = %q, want applied", got)
	}
	rows := e.audits("governance.change.approve")
	if len(rows) != 1 {
		t.Fatalf("%d governance.change.approve rows, want 1", len(rows))
	}
	d := auditData(t, rows[0])
	if d["have"] != float64(2) || d["required"] != float64(2) {
		t.Errorf("approve data = %v, want have 2 and required 2", d)
	}
}

// TestPG_Approvals_TakeBack: a person takes their approval back, once; the second take-back is a 404.
func TestPG_Approvals_TakeBack(t *testing.T) {
	e := newGovEnv(t, minApprovals(2))
	ch := e.held()
	e.signIn(govSubBob, "bob@corp.example")
	if w := e.call(e.bob, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("approve = %d %s, want 200", w.Code, w.Body)
	}
	if w := e.call(e.bob, http.MethodDelete, approvalPath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("take back = %d %s, want 200", w.Code, w.Body)
	}
	if a := approvalsOf(e.read(ch.ID)); a == nil || a["have"] != float64(0) {
		t.Errorf("approvals after the take-back = %v, want have 0", a)
	}
	if w := e.call(e.bob, http.MethodDelete, approvalPath(ch.ID), ""); w.Code != http.StatusNotFound || wireReason(t, w) != reasonGovernanceChangeApprovalNotFound {
		t.Errorf("a second take-back = %d %s, want 404 %s", w.Code, w.Body, reasonGovernanceChangeApprovalNotFound)
	}
	if n := len(e.audits("governance.change.retract")); n != 1 {
		t.Errorf("%d governance.change.retract rows, want 1", n)
	}
}

// TestPG_Approvals_RejectNeedsAReason: under the rules a reject by someone other than the proposer needs a
// reason; the proposer withdraws without one.
func TestPG_Approvals_RejectNeedsAReason(t *testing.T) {
	e := newGovEnv(t, minApprovals(2))
	ch := e.held()
	if w := e.call(e.bob, http.MethodPost, rejectPath(ch.ID), ""); w.Code != http.StatusBadRequest || wireReason(t, w) != reasonGovernanceChangeReasonRequired {
		t.Errorf("a reject with no reason = %d %s, want 400 %s", w.Code, w.Body, reasonGovernanceChangeReasonRequired)
	}
	if w := e.call(e.alice, http.MethodPost, rejectPath(ch.ID), ""); w.Code != http.StatusOK {
		t.Errorf("the proposer withdrawing = %d %s, want 200 with no reason", w.Code, w.Body)
	}
}

// TestPG_Approvals_AnEarlierApprovalThatCannotBeConfirmedIsDropped: the last approval re-checks the earlier one
// on the person's latest record; with no record left it drops as cant_confirm, so the change stays pending.
func TestPG_Approvals_AnEarlierApprovalThatCannotBeConfirmedIsDropped(t *testing.T) {
	e := newGovEnv(t, minApprovals(2))
	ch := e.held()
	e.signIn(govSubBob, "bob@corp.example")
	e.signIn(govSubCarol, "carol@corp.example")
	if w := e.call(e.bob, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("approve = %d %s, want 200", w.Code, w.Body)
	}
	if _, err := e.pool.Exec(context.Background(), `DELETE FROM principal_groups WHERE principal = $1`, govSubBob); err != nil {
		t.Fatalf("remove the record: %v", err)
	}
	if w := e.call(e.carol, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("second approval = %d %s, want 200", w.Code, w.Body)
	}
	if got := e.changeState(ch.ID); got != types.GovernanceChangePending {
		t.Fatalf("state = %q, want pending: the earlier approval no longer counts", got)
	}
	var why string
	if err := e.pool.QueryRow(context.Background(), `SELECT dropped FROM governance_change_approvals WHERE approver = $1`, govSubBob).Scan(&why); err != nil || why != droppedCannotConfirmed {
		t.Errorf("bob's approval dropped = %q (%v), want %s", why, err, droppedCannotConfirmed)
	}
}
