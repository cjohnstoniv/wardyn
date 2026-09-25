// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// The per-run approval cap. The SANDBOX picks the hosts and tools it
// asks about, and the dedup guard only collapses repeats of the SAME scope, so a
// run walking a thousand different unknown hosts wrote a thousand approvals and
// put a thousand entries in front of a human. The bound did not exist for a
// mechanical reason: api.ApprovalService exposed Request/Decide/Get/List(state)
// and no way to ask how many rows a run already had.

// TestInternalApprovalRequest_PerRunCapAnswers429 drives the cap through the real
// route, with the count forced rather than 4096 rows seeded — the cap's job is to
// refuse past a number, and the number is not what could break.
func TestInternalApprovalRequest_PerRunCapAnswers429(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	tok := h.mintRunToken(t, runID)
	body := `{"kind":"egress_domain","requested_scope":{"host":"pkg.example.com"}}`

	// One under the cap: accepted, so the refusal below is the cap and not
	// something else in the handler.
	h.approvals.countForRun = maxApprovalsPerRun - 1
	if w := do(t, h.srv, http.MethodPost, "/api/v1/internal/approvals", tok, body); w.Code != http.StatusCreated {
		t.Fatalf("one under the cap: code = %d, want 201; body=%s", w.Code, w.Body.String())
	}

	h.approvals.countForRun = maxApprovalsPerRun
	before := len(h.audit.events)
	w := do(t, h.srv, http.MethodPost, "/api/v1/internal/approvals", tok,
		`{"kind":"egress_domain","requested_scope":{"host":"other.example.com"}}`)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("at the cap: code = %d, want 429; body=%s", w.Code, w.Body.String())
	}
	// The refusal writes no audit action of its own: a run that hits the cap must
	// not flood the trail with the refusal instead of the rows (B5's lesson one
	// wave over). The rows it already raised ARE the trail.
	for _, ev := range h.audit.events[before:] {
		t.Errorf("the cap refusal emitted a %q audit row; it is a rate bound, not a security event", ev.Action)
	}
}

// TestInternalApprovalRequest_CapFailsClosedOnACountError: "we could not tell how
// many this run has" must never read as "allow another one" — the unbounded raise
// path is the thing being bounded.
func TestInternalApprovalRequest_CapFailsClosedOnACountError(t *testing.T) {
	h := newHarness(t)
	tok := h.mintRunToken(t, uuid.New())
	h.approvals.countErr = errStoreNotFound

	w := do(t, h.srv, http.MethodPost, "/api/v1/internal/approvals", tok,
		`{"kind":"egress_domain","requested_scope":{"host":"pkg.example.com"}}`)
	if w.Code == http.StatusCreated {
		t.Fatalf("a failed count ADMITTED the raise (code %d); the cap must fail closed", w.Code)
	}
	if len(h.approvals.requested) != 0 {
		t.Errorf("the raise reached the FSM anyway (%d requests recorded)", len(h.approvals.requested))
	}
}
