// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestGovernanceProposerWithdrawsOutsideApproverSet: the unit admin who proposed a change of an envelope's
// root, which only the security tier decides, may still reject it, and the reject row says the proposer
// withdrew it. Approving it is refused on scope, as it is for a peer admin of the same root, who did not
// propose it and is refused a reject on scope too (only an admin above a unit decides its own change), and
// an admin above it is refused one by the envelope. A security admin's reject of a later proposal is no
// withdrawal.
func TestGovernanceProposerWithdrawsOutsideApproverSet(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	e.seedEnvelope(e.a1)
	contact := e.restate(e.a1, map[string]any{"contact": map[string]any{"owner": "Platform A"}})
	ch := e.pending(e.call(e.adminA1, http.MethodPut, envUnitPath(e.a1), contact))

	if w := e.call(e.adminA1, http.MethodPost, approvePath(ch.ID), ""); !scopeRefused(t, w) {
		t.Errorf("the proposer approving = %d %s, want 403 unit_scope_denied", w.Code, w.Body)
	}
	if w := e.call(e.peerA1, http.MethodPost, rejectPath(ch.ID), ""); !scopeRefused(t, w) {
		t.Errorf("a peer admin rejecting = %d %s, want 403 unit_scope_denied", w.Code, w.Body)
	}
	if w := e.call(e.adminA, http.MethodPost, rejectPath(ch.ID), ""); !securitySurfaceRefused(t, w) {
		t.Errorf("an admin above the root rejecting = %d %s, want 403 security_admin_surface", w.Code, w.Body)
	}
	if got := e.changeState(ch.ID); got != types.GovernanceChangePending {
		t.Fatalf("after the refusals the change is %s, want pending", got)
	}

	if w := e.call(e.adminA1, http.MethodPost, rejectPath(ch.ID), `{"reason":"proposed by mistake"}`); w.Code != http.StatusOK {
		t.Fatalf("the proposer withdrawing = %d %s, want 200", w.Code, w.Body)
	}
	if got := e.changeState(ch.ID); got != types.GovernanceChangeRejected {
		t.Errorf("withdrawn, the change is %s, want rejected", got)
	}
	if got := withdrawnOf(t, e.h, ch.ID); got != true {
		t.Errorf("the proposer's reject row has withdrawn %v, want true", got)
	}

	again := e.pending(e.call(e.adminA1, http.MethodPut, envUnitPath(e.a1), contact))
	e.reject(again)
	if got := withdrawnOf(t, e.h, again.ID); got != nil {
		t.Errorf("a security admin's reject row has withdrawn %v, want none", got)
	}
}

// withdrawnOf is the withdrawn data key of change id's governance.change.reject row, nil when it has none.
func withdrawnOf(t *testing.T, h *harness, id uuid.UUID) any {
	t.Helper()
	for _, ev := range govCovAudits(h, "governance.change.reject") {
		if ev.Target != id.String() {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatalf("reject row data %s: %v", ev.Data, err)
		}
		return data["withdrawn"]
	}
	t.Fatalf("no governance.change.reject row for %s", id)
	return nil
}
