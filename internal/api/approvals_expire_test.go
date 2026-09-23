// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// seedApproval writes a row straight into the fake store's map, bypassing
// Request — these tests are about handleInternalExpireApproval's own
// ownership/kind gating, not the raise path.
func seedApproval(h *harness, runID uuid.UUID, kind types.ApprovalKind) uuid.UUID {
	id := uuid.New()
	h.approvals.byID[id] = types.ApprovalRequest{
		ID: id, RunID: runID, Kind: kind, State: types.ApprovalPending,
		RequestedScope: json.RawMessage(`{}`),
	}
	return id
}

// TestHandleInternalExpireApproval_OwnToolCallExpires is #811's fix: the gate
// closing its OWN tool_call approval succeeds and actually moves the row.
func TestHandleInternalExpireApproval_OwnToolCallExpires(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	id := seedApproval(h, runID, types.ApprovalToolCall)
	tok := h.mintRunToken(t, runID)

	w := do(t, h.srv, http.MethodPost, "/api/v1/internal/approvals/"+id.String()+"/expire", tok, "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("code = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	calls := h.approvals.expiredCalls()
	if len(calls) != 1 || calls[0] != id {
		t.Fatalf("expiredCalls = %v, want exactly [%s]", calls, id)
	}
}

// TestHandleInternalExpireApproval_ForeignRunIs404 proves a run cannot expire
// another run's approval — the same existence-hiding 404 handleInternalGetApproval
// gives a cross-run read, never a 403 that would confirm the row exists.
func TestHandleInternalExpireApproval_ForeignRunIs404(t *testing.T) {
	h := newHarness(t)
	owner := uuid.New()
	id := seedApproval(h, owner, types.ApprovalToolCall)
	tok := h.mintRunToken(t, uuid.New()) // a DIFFERENT run's token

	w := do(t, h.srv, http.MethodPost, "/api/v1/internal/approvals/"+id.String()+"/expire", tok, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404; body=%s", w.Code, w.Body.String())
	}
	if calls := h.approvals.expiredCalls(); len(calls) != 0 {
		t.Fatalf("expiredCalls = %v, want none — a foreign run's approval must never move", calls)
	}
}

// TestHandleInternalExpireApproval_NonToolCallIs404 restricts this route to
// the one kind wardyn-toolgate ever raises (handleBrokerCreateApproval):
// an egress_domain approval of the SAME run is still refused.
func TestHandleInternalExpireApproval_NonToolCallIs404(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	id := seedApproval(h, runID, types.ApprovalEgressDomain)
	tok := h.mintRunToken(t, runID)

	w := do(t, h.srv, http.MethodPost, "/api/v1/internal/approvals/"+id.String()+"/expire", tok, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404; body=%s", w.Code, w.Body.String())
	}
	if calls := h.approvals.expiredCalls(); len(calls) != 0 {
		t.Fatalf("expiredCalls = %v, want none — a non-tool_call approval must never move via this route", calls)
	}
}

// TestHandleInternalExpireApproval_AlreadyDecidedIsIdempotent proves the CAS
// race with a human decision (or the periodic sweep) that beat the gate to it
// answers success, not an error — the row must not move, and no second
// approval.expire row (see approval.ExpireOne) should surprise the human who
// already decided it.
func TestHandleInternalExpireApproval_AlreadyDecidedIsIdempotent(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	id := seedApproval(h, runID, types.ApprovalToolCall)
	ap := h.approvals.byID[id]
	ap.State = types.ApprovalApproved
	h.approvals.byID[id] = ap
	tok := h.mintRunToken(t, runID)

	w := do(t, h.srv, http.MethodPost, "/api/v1/internal/approvals/"+id.String()+"/expire", tok, "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("code = %d, want 204 (idempotent no-op); body=%s", w.Code, w.Body.String())
	}
	if got := h.approvals.byID[id].State; got != types.ApprovalApproved {
		t.Fatalf("state = %s, want APPROVED left untouched", got)
	}
}
