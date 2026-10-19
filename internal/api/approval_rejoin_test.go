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

// A raise naming rejoin_id stamps that PENDING row of the run, so the console
// reads the re-entered hold as held, and creates nothing: it is answered even
// at the per-run cap. Another run's row, or a decided one, is not stamped.
func TestInternalApprovalRequest_RejoinStampsThePendingRow(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	tok := h.mintRunToken(t, runID)
	const scope = `"requested_scope":{"host":"pkg.example.com","mode":"wait_for_review"}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/internal/approvals", tok, `{"kind":"egress_domain",`+scope+`}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("raise: code = %d; body=%s", w.Code, w.Body.String())
	}
	var raised types.ApprovalRequest
	if err := json.Unmarshal(w.Body.Bytes(), &raised); err != nil {
		t.Fatal(err)
	}
	rejoin := `{"kind":"egress_domain",` + scope + `,"rejoin_id":"` + raised.ID.String() + `"}`

	h.approvals.countForRun = maxApprovalsPerRun
	if w := do(t, h.srv, http.MethodPost, "/api/v1/internal/approvals", tok, rejoin); w.Code != http.StatusOK {
		t.Fatalf("rejoin: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := h.approvals.byID[raised.ID]; got.LastRejoinedAt == nil {
		t.Fatal("the rejoin did not stamp the row")
	}
	if n := len(h.approvals.byID); n != 1 {
		t.Fatalf("approvals = %d, want 1: a rejoin creates nothing", n)
	}

	other := h.mintRunToken(t, uuid.New())
	if w := do(t, h.srv, http.MethodPost, "/api/v1/internal/approvals", other, rejoin); w.Code != http.StatusNotFound {
		t.Fatalf("another run's rejoin: code = %d, want 404; body=%s", w.Code, w.Body.String())
	}
	decided := h.approvals.byID[raised.ID]
	decided.State = types.ApprovalApproved
	h.approvals.byID[raised.ID] = decided
	if w := do(t, h.srv, http.MethodPost, "/api/v1/internal/approvals", tok, rejoin); w.Code != http.StatusNotFound {
		t.Fatalf("rejoin of a decided row: code = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}
