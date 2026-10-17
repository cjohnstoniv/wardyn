// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const gapCovGrantBody = `{"subject_type":"group","subject":"eng","capability":"egress_host","value":"*.example.com","effect":"allow"}`

func gapCovPermServer(t *testing.T, localMode bool) (*Server, *permStore, *harness) {
	t.Helper()
	st := &permStore{capStore: &capStore{}}
	h := newHarness(t)
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.LocalMode = localMode
	return New(cfg), st, h
}

// gapCovBypassRow is the one break-glass row an admin-token write left, with its target and the kind
// of thing it wrote.
func gapCovBypassRow(t *testing.T, h *harness, wantKind string) types.AuditEvent {
	t.Helper()
	rows := govCovAudits(h, "governance.change.bypass")
	if len(rows) != 1 {
		t.Fatalf("%d break-glass rows, want 1", len(rows))
	}
	d := govCovAuditData(t, rows[0])
	if d["target_kind"] != wantKind || d["reason"] != "admin_token_break_glass" || rows[0].Outcome != "success" {
		t.Fatalf("break-glass row = outcome %q data %v, want a success for kind %s", rows[0].Outcome, d, wantKind)
	}
	return rows[0]
}

// With the second-human switch on, the admin token still writes a permission directly, and each
// such write leaves a break-glass row naming what it wrote.
func TestGapCovPermissionWritesByTheAdminTokenLeaveABreakGlassRow(t *testing.T) {
	t.Setenv(envGovernanceSecondHuman, "true")

	t.Run("grant upsert", func(t *testing.T) {
		srv, st, h := gapCovPermServer(t, false)
		w := do(t, srv, http.MethodPost, "/api/v1/permissions/grants", adminToken, gapCovGrantBody)
		if w.Code != http.StatusCreated || len(st.grants) != 1 {
			t.Fatalf("POST = %d %s, %d grants stored; want 201 and one grant", w.Code, w.Body, len(st.grants))
		}
		if row := gapCovBypassRow(t, h, govKindGrant); row.Target != st.grants[0].ID.String() {
			t.Errorf("break-glass target = %q, want the grant's id %s", row.Target, st.grants[0].ID)
		}
	})
	t.Run("grant delete", func(t *testing.T) {
		srv, st, h := gapCovPermServer(t, false)
		id := uuid.MustParse("00000000-0000-0000-0000-00000000d001")
		st.grants = []types.CapabilityGrant{{ID: id, SubjectType: types.CapabilitySubjectAll, Capability: capSecret, Value: "prod-db", Effect: types.CapabilityAllow}}
		w := do(t, srv, http.MethodDelete, "/api/v1/permissions/grants/"+id.String(), adminToken, "")
		if w.Code != http.StatusNoContent || len(st.grants) != 0 {
			t.Fatalf("DELETE = %d, %d grants left; want 204 and none", w.Code, len(st.grants))
		}
		if row := gapCovBypassRow(t, h, govKindGrant); row.Target != id.String() {
			t.Errorf("break-glass target = %q, want %s", row.Target, id)
		}
	})
	t.Run("enforcement replace", func(t *testing.T) {
		srv, st, h := gapCovPermServer(t, false)
		w := do(t, srv, http.MethodPut, "/api/v1/permissions/enforcement", adminToken, `{"secret":true}`)
		if w.Code != http.StatusOK || !st.enf[capSecret] {
			t.Fatalf("PUT = %d %s, enforcement %v; want 200 with secret on", w.Code, w.Body, st.enf)
		}
		if row := gapCovBypassRow(t, h, govKindEnforcement); row.Target != govEnforcementKey {
			t.Errorf("break-glass target = %q, want %q", row.Target, govEnforcementKey)
		}
	})
}

// In local mode, which authenticates nobody, a human's permission write is refused 503 with the
// local-mode reason before the request is read: each request below would otherwise be answered 400
// (a body the strict decoder refuses, or no id to parse). The handlers are called directly: the
// router's own local-mode guard answers a non-loopback peer first.
func TestGapCovPermissionWritesAreRefusedInLocalModeWithTheSwitchOn(t *testing.T) {
	t.Setenv(envGovernanceSecondHuman, "true")
	id := uuid.MustParse("00000000-0000-0000-0000-00000000d002")
	for _, tc := range []struct {
		name, method, body string
		handler            func(*Server) http.HandlerFunc
	}{
		{"grant upsert", http.MethodPost, `{"unknown":1}`, func(s *Server) http.HandlerFunc { return s.handleUpsertCapabilityGrant }},
		{"grant delete", http.MethodDelete, "", func(s *Server) http.HandlerFunc { return s.handleDeleteCapabilityGrant }},
		{"enforcement replace", http.MethodPut, `{"secret":"not a bool"}`, func(s *Server) http.HandlerFunc { return s.handlePutCapabilityEnforcement }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, st, h := gapCovPermServer(t, true)
			st.grants = []types.CapabilityGrant{{ID: id, SubjectType: types.CapabilitySubjectAll, Capability: capSecret, Value: "prod-db", Effect: types.CapabilityAllow}}
			r := httptest.NewRequest(tc.method, "/x", strings.NewReader(tc.body))
			r = r.WithContext(withHumanIdentity(r.Context(), "sub-a", "a@corp.example", oidc.RoleAdmin, types.UserTypeStandard, nil, false))
			w := httptest.NewRecorder()
			tc.handler(srv)(w, r)
			if w.Code != http.StatusServiceUnavailable || errorReason(w) != reasonGovernanceSecondHumanLocalMode {
				t.Fatalf("%s = %d %q, want 503 %s", tc.name, w.Code, errorReason(w), reasonGovernanceSecondHumanLocalMode)
			}
			if len(st.grants) != 1 || len(st.enf) != 0 {
				t.Errorf("store changed: %d grants, enforcement %v", len(st.grants), st.enf)
			}
			if n := len(govCovAudits(h, "governance.change.bypass")); n != 0 {
				t.Errorf("%d break-glass rows for a refused write", n)
			}
		})
	}
}

// With the switch on and a human caller, an enforcement replacement is held for a second human, not
// written.
func TestGapCovEnforcementReplacementByAHumanIsHeld(t *testing.T) {
	t.Setenv(envGovernanceSecondHuman, "true")
	st := &govCovStore{proposeSaved: types.GovernanceChange{ID: govCovChangeID}}
	srv, _ := govCovServer(t, st)
	super := govCovSession(t, "sub-super", "super@corp.example", oidc.RoleAdmin)

	w := doSSO(t, srv, http.MethodPut, "/api/v1/permissions/enforcement", super, `{"secret":true}`)
	ch := govCovHoldAnswer(t, w, st)
	if ch.TargetKind != govKindEnforcement || ch.Op != "replace" || ch.TargetKey != govEnforcementKey {
		t.Fatalf("stored change = %+v", ch)
	}
	var payload map[string]bool
	if err := json.Unmarshal(ch.Payload, &payload); err != nil || len(payload) != 1 || !payload[capSecret] {
		t.Fatalf("payload = %s (%v), want the requested map", ch.Payload, err)
	}
}
