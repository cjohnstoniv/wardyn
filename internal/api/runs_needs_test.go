// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// #1197: GET /runs?status=needs (the attention-filtered list) and the
// attention projection on the plain GET /runs?view= path.
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// needsTestRun is a live run plus which of the three shapes its own held
// approval takes: "you" (a decidable egress), "admin" (a plain tool_call,
// admin-only), or "" (no held approval at all — a clean run that must never
// appear in a needs list).
func setupNeedsFixture(t *testing.T) (srv *Server, ast *authzStore, aap *authzApprovals, youRuns, adminRuns []uuid.UUID, foreignRun uuid.UUID) {
	t.Helper()
	srv, ast, aap = meAttentionFixture(t)
	const member = "sub-needs-member"

	you1, you2 := uuid.New(), uuid.New()
	admin1 := uuid.New()
	clean := uuid.New()
	foreignRun = uuid.New()
	ast.mu.Lock()
	ast.runs[you1] = types.AgentRun{ID: you1, CreatedBy: member, State: types.RunRunning, Task: "you-1"}
	ast.runs[you2] = types.AgentRun{ID: you2, CreatedBy: member, State: types.RunRunning, Task: "you-2"}
	ast.runs[admin1] = types.AgentRun{ID: admin1, CreatedBy: member, State: types.RunRunning, Task: "admin-only"}
	ast.runs[clean] = types.AgentRun{ID: clean, CreatedBy: member, State: types.RunRunning, Task: "clean"}
	ast.runs[foreignRun] = types.AgentRun{ID: foreignRun, CreatedBy: "sub-someone-else", State: types.RunRunning, Task: "foreign"}
	ast.mu.Unlock()
	aap.mu.Lock()
	aap.byID[uuid.New()] = pendingRow(you1, types.ApprovalEgressDomain, `{"host":"h1","mode":"wait_for_review"}`, nil)
	aap.byID[uuid.New()] = pendingRow(you2, types.ApprovalEgressDomain, `{"host":"h2","mode":"wait_for_review"}`, nil)
	aap.byID[uuid.New()] = pendingRow(admin1, types.ApprovalToolCall, `{}`, nil)
	aap.byID[uuid.New()] = pendingRow(foreignRun, types.ApprovalEgressDomain, `{"host":"h3","mode":"wait_for_review"}`, nil)
	aap.mu.Unlock()
	return srv, ast, aap, []uuid.UUID{you1, you2}, []uuid.UUID{admin1}, foreignRun
}

func decodeRuns(t *testing.T, w *httptest.ResponseRecorder) []types.AgentRun {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var runs []types.AgentRun
	if err := json.Unmarshal(w.Body.Bytes(), &runs); err != nil {
		t.Fatalf("decode: %v; body=%s", err, w.Body.String())
	}
	return runs
}

func idsOf(runs []types.AgentRun) map[uuid.UUID]bool {
	out := map[uuid.UUID]bool{}
	for _, r := range runs {
		out[r.ID] = true
	}
	return out
}

// TestRunsStatusNeeds pins the by=="you" filter (mutation MB made it always
// true), the header values (both hidden-count headers forced to 0), a
// foreign run never appearing, and paging/truncation on this branch.
func TestRunsStatusNeeds(t *testing.T) {
	srv, _, _, youRuns, adminRuns, foreignRun := setupNeedsFixture(t)
	memberSess := ssoSession(t, "sub-needs-member", "needs@corp.example", oidc.RoleUser)

	t.Run("only by==you runs come back; the admin-only and foreign runs never do", func(t *testing.T) {
		w := doSSO(t, srv, http.MethodGet, "/api/v1/runs?view=user&status=needs", memberSess, "")
		if h := w.Header().Get("X-Wardyn-Hidden-Older"); h != "0" {
			t.Errorf("X-Wardyn-Hidden-Older = %q, want \"0\" (needs implies live-only, nothing is ever hidden by age)", h)
		}
		if h := w.Header().Get("X-Wardyn-Hidden-Killed"); h != "0" {
			t.Errorf("X-Wardyn-Hidden-Killed = %q, want \"0\"", h)
		}
		got := idsOf(decodeRuns(t, w))
		for _, id := range youRuns {
			if !got[id] {
				t.Errorf("missing a by==you run %s", id)
			}
		}
		for _, id := range adminRuns {
			if got[id] {
				t.Errorf("an admin-only run %s leaked into status=needs", id)
			}
		}
		if got[foreignRun] {
			t.Error("a foreign run leaked into a member's status=needs list")
		}
		if len(got) != len(youRuns) {
			t.Errorf("returned %d runs, want exactly %d (the by==you set)", len(got), len(youRuns))
		}
	})

	t.Run("paging and X-Wardyn-Truncated are honoured on this branch", func(t *testing.T) {
		w := doSSO(t, srv, http.MethodGet, "/api/v1/runs?view=user&status=needs&limit=1", memberSess, "")
		runs := decodeRuns(t, w)
		if len(runs) != 1 {
			t.Fatalf("limit=1: got %d rows, want 1", len(runs))
		}
		if got := idsOf(runs); !got[youRuns[0]] && !got[youRuns[1]] {
			t.Errorf("windowed row %s is not one of the by==you runs", runs[0].ID)
		}
		if h := w.Header().Get("X-Wardyn-Truncated"); h != "true" {
			t.Errorf("X-Wardyn-Truncated = %q, want \"true\" (2 by==you rows exist, limit=1)", h)
		}

		w2 := doSSO(t, srv, http.MethodGet, "/api/v1/runs?view=user&status=needs&limit=200", memberSess, "")
		if h := w2.Header().Get("X-Wardyn-Truncated"); h != "" {
			t.Errorf("X-Wardyn-Truncated = %q, want unset when nothing is truncated", h)
		}
		if runs2 := decodeRuns(t, w2); len(runs2) != len(youRuns) {
			t.Errorf("limit=200: got %d rows, want %d (the whole by==you set)", len(runs2), len(youRuns))
		}
	})
}

// TestRunsViewProjectsAttention pins that the PLAIN GET /runs?view= path
// (no status=needs) also carries the attention projection on each live row —
// the same field status=needs filters on, just unfiltered here.
func TestRunsViewProjectsAttention(t *testing.T) {
	srv, _, _, youRuns, adminRuns, _ := setupNeedsFixture(t)
	memberSess := ssoSession(t, "sub-needs-member", "needs@corp.example", oidc.RoleUser)

	w := doSSO(t, srv, http.MethodGet, "/api/v1/runs?view=user", memberSess, "")
	runs := decodeRuns(t, w)
	byID := map[uuid.UUID]types.AgentRun{}
	for _, r := range runs {
		byID[r.ID] = r
	}
	for _, id := range youRuns {
		got, ok := byID[id]
		if !ok {
			t.Fatalf("missing run %s in the plain view= list", id)
		}
		if got.Attention == nil || got.Attention.By != types.AttentionYou {
			t.Errorf("run %s: attention = %+v, want by=you", id, got.Attention)
		}
	}
	for _, id := range adminRuns {
		got, ok := byID[id]
		if !ok {
			t.Fatalf("missing run %s in the plain view= list", id)
		}
		if got.Attention == nil || got.Attention.By != types.AttentionAdmin {
			t.Errorf("run %s: attention = %+v, want by=admin", id, got.Attention)
		}
	}
}
