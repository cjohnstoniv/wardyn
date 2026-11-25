// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// counts=skip leaves both hidden-count headers out and makes no count read; the same request without it sends both
// headers, from one read. The rows are the same either way.
func TestListRunsFiltered_CountsSkipLeavesTheHiddenHeadersOut(t *testing.T) {
	srv, ast, _, rec := recordingFixture(t)
	memberSess := ssoSession(t, "sub-counts", "counts@corp.example", oidc.RoleUser)
	run := uuid.New()
	ast.mu.Lock()
	ast.runs[run] = types.AgentRun{ID: run, CreatedBy: "sub-counts", State: types.RunRunning}
	ast.mu.Unlock()

	skipped := doSSO(t, srv, http.MethodGet, "/api/v1/runs?view=user&counts=skip", memberSess, "")
	if skipped.Code != http.StatusOK {
		t.Fatalf("counts=skip: status = %d, body=%s", skipped.Code, skipped.Body.String())
	}
	for _, h := range []string{"X-Wardyn-Hidden-Older", "X-Wardyn-Hidden-Killed"} {
		if _, ok := skipped.Header()[h]; ok {
			t.Errorf("counts=skip sent %s", h)
		}
	}
	if rec.counts != 0 {
		t.Errorf("counts=skip made %d hidden-count reads, want 0", rec.counts)
	}

	full := doSSO(t, srv, http.MethodGet, "/api/v1/runs?view=user", memberSess, "")
	if full.Code != http.StatusOK {
		t.Fatalf("no counts param: status = %d, body=%s", full.Code, full.Body.String())
	}
	for _, h := range []string{"X-Wardyn-Hidden-Older", "X-Wardyn-Hidden-Killed"} {
		if full.Header().Get(h) != "0" {
			t.Errorf("without counts=skip %s = %q, want \"0\"", h, full.Header().Get(h))
		}
	}
	if rec.counts != 1 {
		t.Errorf("without counts=skip made %d hidden-count reads, want 1", rec.counts)
	}
	if skipped.Body.String() != full.Body.String() {
		t.Errorf("counts=skip changed the rows:\n skip %s\n full %s", skipped.Body.String(), full.Body.String())
	}
}

// A counts value other than skip is refused with its own reason, not read as "no counts param".
func TestListRunsFiltered_CountsRejectsAnUnknownValue(t *testing.T) {
	h := newHarness(t)
	for _, q := range []string{"view=user&counts=1", "view=user&counts=SKIP", "status=active&counts=none"} {
		w := do(t, h.srv, http.MethodGet, "/api/v1/runs?"+q, adminToken, "")
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400; body=%s", q, w.Code, w.Body.String())
		}
		if got := errorReason(w); got != reasonInvalidCountsParam {
			t.Errorf("%s: reason = %q, want %q", q, got, reasonInvalidCountsParam)
		}
	}
}
