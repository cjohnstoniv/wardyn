// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// With the second-human switch on, the admin token still writes an assignment directly and leaves a
// break-glass row naming it.
func TestGapCovAssignmentWritesByTheAdminTokenLeaveABreakGlassRow(t *testing.T) {
	t.Setenv(envGovernanceSecondHuman, "true")
	pid := uuid.MustParse("00000000-0000-0000-0000-00000000a001")

	t.Run("upsert", func(t *testing.T) {
		srv, h := govCrudServer(t, &govCrudStore{})
		body := `{"subject_type":"group","subject":"eng","profile_id":"` + pid.String() + `","priority":3}`
		w := do(t, srv, http.MethodPost, "/api/v1/governance/assignments", adminToken, body)
		if w.Code != http.StatusCreated {
			t.Fatalf("POST = %d %s, want 201", w.Code, w.Body)
		}
		rows := govCovAudits(h, "governance.assignment.write")
		if len(rows) != 1 {
			t.Fatalf("%d assignment audit rows, want 1", len(rows))
		}
		if row := gapCovBypassRow(t, h, govKindAssignment); row.Target != rows[0].Target {
			t.Errorf("break-glass target = %q, want the assignment's %q", row.Target, rows[0].Target)
		}
	})
	t.Run("delete", func(t *testing.T) {
		srv, h := govCrudServer(t, &govCrudStore{})
		id := uuid.MustParse("00000000-0000-0000-0000-00000000a002")
		w := do(t, srv, http.MethodDelete, "/api/v1/governance/assignments/"+id.String(), adminToken, "")
		if w.Code != http.StatusNoContent {
			t.Fatalf("DELETE = %d %s, want 204", w.Code, w.Body)
		}
		if row := gapCovBypassRow(t, h, govKindAssignment); row.Target != id.String() {
			t.Errorf("break-glass target = %q, want %s", row.Target, id)
		}
	})
}

// In local mode a human's assignment write is refused 503 with the local-mode reason before the request
// is read: the body sent is one the strict decoder refuses (and the delete has no id to parse), so any
// handler that read first would answer 400.
func TestGapCovAssignmentWritesAreRefusedInLocalModeWithTheSwitchOn(t *testing.T) {
	t.Setenv(envGovernanceSecondHuman, "true")
	for _, tc := range []struct {
		name, method string
		handler      func(*Server) http.HandlerFunc
	}{
		{"upsert", http.MethodPost, func(s *Server) http.HandlerFunc { return s.handleUpsertGovernanceAssignment }},
		{"delete", http.MethodDelete, func(s *Server) http.HandlerFunc { return s.handleDeleteGovernanceAssignment }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			cfg := baseTestConfig(h, &govCrudStore{})
			cfg.LocalMode = true
			srv := New(cfg)
			r := httptest.NewRequest(tc.method, "/x", strings.NewReader(`{"unknown":1}`))
			r = r.WithContext(withHumanIdentity(r.Context(), "sub-a", "a@corp.example", oidc.RoleAdmin, types.UserTypeStandard, nil, false))
			w := httptest.NewRecorder()
			tc.handler(srv)(w, r)
			if w.Code != http.StatusServiceUnavailable || errorReason(w) != reasonGovernanceSecondHumanLocalMode {
				t.Fatalf("%s = %d %q, want 503 %s", tc.name, w.Code, errorReason(w), reasonGovernanceSecondHumanLocalMode)
			}
			if got := govCovAuditActions(h); len(got) != 0 {
				t.Errorf("audit rows %v for a refused write", got)
			}
		})
	}
}
