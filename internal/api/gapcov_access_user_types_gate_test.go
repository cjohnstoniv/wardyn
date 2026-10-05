// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func gapCovWithParam(r *http.Request, key, val string) *http.Request {
	rc := chi.NewRouteContext()
	rc.URLParams.Add(key, val)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rc))
}

func gapCovHumanRequest(method, body string) *http.Request {
	r := httptest.NewRequest(method, "/x", strings.NewReader(body))
	return r.WithContext(withHumanIdentity(r.Context(), "sub-a", "a@corp.example", oidc.RoleAdmin, types.UserTypeStandard, nil, false))
}

// gapCovUserTypeStore fails the two update statements on demand.
type gapCovUserTypeStore struct {
	*userTypeStore
	updateErr error
}

func (s *gapCovUserTypeStore) UpdateUserType(ctx context.Context, t types.UserType) (types.UserType, error) {
	if s.updateErr != nil {
		return types.UserType{}, s.updateErr
	}
	return s.userTypeStore.UpdateUserType(ctx, t)
}

func (s *gapCovUserTypeStore) UpdateUserTypeMetadata(ctx context.Context, t types.UserType) (types.UserType, error) {
	if s.updateErr != nil {
		return types.UserType{}, s.updateErr
	}
	return s.userTypeStore.UpdateUserTypeMetadata(ctx, t)
}

// With the switch on, the admin token still changes a type's priority directly and leaves a
// break-glass row naming the type.
func TestGapCovUserTypePriorityChangeByTheAdminTokenLeavesABreakGlassRow(t *testing.T) {
	t.Setenv(envGovernanceSecondHuman, "true")
	st := newUserTypeStore()
	st.rows["pm"] = types.UserType{ID: "pm", Name: "Portfolio manager", Priority: 10}
	srv, h := userTypeServer(t, st, nil)

	w := do(t, srv, http.MethodPut, "/api/v1/user-types/pm", adminToken, `{"name":"Portfolio manager","priority":20}`)
	if w.Code != http.StatusOK || st.rows["pm"].Priority != 20 {
		t.Fatalf("PUT = %d %s, priority %d; want 200 and 20", w.Code, w.Body, st.rows["pm"].Priority)
	}
	if row := gapCovBypassRow(t, h, govKindUserType); row.Target != "pm" {
		t.Errorf("break-glass target = %q, want pm", row.Target)
	}
}

// In local mode a human's priority change is refused 503 with the local-mode reason and nothing is
// written; a metadata-only edit is not covered and still applies.
func TestGapCovUserTypePriorityChangeIsRefusedInLocalModeWithTheSwitchOn(t *testing.T) {
	t.Setenv(envGovernanceSecondHuman, "true")
	st := newUserTypeStore()
	st.rows["pm"] = types.UserType{ID: "pm", Name: "Portfolio manager", Priority: 10}
	h := newHarness(t)
	cfg := baseTestConfig(h, st)
	cfg.LocalMode = true
	srv := New(cfg)

	put := func(body string) *httptest.ResponseRecorder {
		r := gapCovHumanRequest(http.MethodPut, body)
		r = gapCovWithParam(r, "id", "pm")
		w := httptest.NewRecorder()
		srv.handleUpdateUserType(w, r)
		return w
	}
	w := put(`{"name":"Portfolio manager","priority":20}`)
	if w.Code != http.StatusServiceUnavailable || errorReason(w) != reasonGovernanceSecondHumanLocalMode || st.rows["pm"].Priority != 10 {
		t.Fatalf("priority change = %d %q, priority %d; want 503 %s and 10", w.Code, errorReason(w), st.rows["pm"].Priority, reasonGovernanceSecondHumanLocalMode)
	}
	w = put(`{"name":"Desk staff","priority":10}`)
	if w.Code != http.StatusOK || st.rows["pm"].Name != "Desk staff" {
		t.Fatalf("rename = %d %s, name %q; want 200 and the new name", w.Code, w.Body, st.rows["pm"].Name)
	}
}

// A store that refuses the update answers the matching status and reason, and audits nothing.
func TestGapCovUserTypeUpdateStoreRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		reason string
	}{
		{"the type vanished", store.ErrNotFound, http.StatusNotFound, reasonUserTypeNotFound},
		{"the name is taken", store.ErrConflict, http.StatusConflict, reasonUserTypeConflict},
		{"another failure", errors.New("gapcov: write refused"), http.StatusInternalServerError, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := newUserTypeStore()
			base.rows["pm"] = types.UserType{ID: "pm", Name: "Portfolio manager", Priority: 10}
			st := &gapCovUserTypeStore{userTypeStore: base, updateErr: tc.err}
			srv, h := userTypeServer(t, nil, nil)
			srv.cfg.Store = st
			w := do(t, srv, http.MethodPut, "/api/v1/user-types/pm", adminToken, `{"name":"Desk staff","priority":10}`)
			if w.Code != tc.status || (tc.reason != "" && errorReason(w) != tc.reason) {
				t.Fatalf("PUT = %d %q, want %d %q", w.Code, errorReason(w), tc.status, tc.reason)
			}
			if got := govCovAudits(h, "user_type.write"); len(got) != 0 {
				t.Errorf("%d user_type.write rows after a refused update", len(got))
			}
		})
	}
}

func gapCovRoleMapServer(t *testing.T, st store.Store, localMode bool) (*Server, *harness) {
	t.Helper()
	h := newHarness(t)
	cfg := baseTestConfig(h, st)
	cfg.OIDC = newAccessAuth(t, nil, "", nil, nil)
	cfg.LocalMode = localMode
	return New(cfg), h
}

// With the switch on, the admin token still writes a role mapping directly and leaves a break-glass
// row naming it, on both the create and the delete.
func TestGapCovRoleMappingWritesByTheAdminTokenLeaveABreakGlassRow(t *testing.T) {
	t.Setenv(envGovernanceSecondHuman, "true")
	t.Run("upsert", func(t *testing.T) {
		st := &roleMapStore{}
		srv, h := gapCovRoleMapServer(t, st, false)
		w := do(t, srv, http.MethodPost, "/api/v1/access/mappings", adminToken, `{"value":"eng-team","role":"user","acknowledge_access_change":true}`)
		if w.Code != http.StatusCreated || len(st.rows) != 1 {
			t.Fatalf("POST = %d %s, %d rows; want 201 and one row", w.Code, w.Body, len(st.rows))
		}
		if row := gapCovBypassRow(t, h, govKindRoleMapping); row.Target != st.rows[0].ID.String() {
			t.Errorf("break-glass target = %q, want the mapping's id %s", row.Target, st.rows[0].ID)
		}
	})
	t.Run("delete", func(t *testing.T) {
		id := uuid.MustParse("00000000-0000-0000-0000-00000000e001")
		st := &roleMapStore{rows: []types.RoleMapping{
			{ID: id, Value: "eng-team", Role: oidc.RoleUser},
			{ID: uuid.MustParse("00000000-0000-0000-0000-00000000e002"), Value: "ops", Role: oidc.RoleAdmin},
		}}
		srv, h := gapCovRoleMapServer(t, st, false)
		w := do(t, srv, http.MethodDelete, "/api/v1/access/mappings/"+id.String(), adminToken, "")
		if w.Code != http.StatusNoContent || len(st.rows) != 1 {
			t.Fatalf("DELETE = %d %s, %d rows left; want 204 and one", w.Code, w.Body, len(st.rows))
		}
		if row := gapCovBypassRow(t, h, govKindRoleMapping); row.Target != id.String() {
			t.Errorf("break-glass target = %q, want %s", row.Target, id)
		}
	})
}

// gapCovCountingRoleStore counts the reads a role-mapping write makes.
type gapCovCountingRoleStore struct {
	*roleMapStore
	reads int
}

func (s *gapCovCountingRoleStore) ListUserTypes(ctx context.Context) ([]types.UserType, error) {
	s.reads++
	return s.roleMapStore.ListUserTypes(ctx)
}

func (s *gapCovCountingRoleStore) ListRoleMappings(ctx context.Context) ([]types.RoleMapping, error) {
	s.reads++
	return s.roleMapStore.ListRoleMappings(ctx)
}

// In local mode a human's role-mapping write is refused 503 with the local-mode reason before the
// store is read.
func TestGapCovRoleMappingWritesAreRefusedInLocalModeWithTheSwitchOn(t *testing.T) {
	t.Setenv(envGovernanceSecondHuman, "true")
	for _, tc := range []struct {
		name, method string
		handler      func(*Server) http.HandlerFunc
	}{
		{"upsert", http.MethodPost, func(s *Server) http.HandlerFunc { return s.handleUpsertRoleMapping }},
		{"delete", http.MethodDelete, func(s *Server) http.HandlerFunc { return s.handleDeleteRoleMapping }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &gapCovCountingRoleStore{roleMapStore: &roleMapStore{rows: []types.RoleMapping{{ID: uuid.New(), Value: "eng-team", Role: oidc.RoleUser}}}}
			srv, h := gapCovRoleMapServer(t, st, true)
			w := httptest.NewRecorder()
			tc.handler(srv)(w, gapCovHumanRequest(tc.method, `{"value":"x","role":"user"}`))
			if w.Code != http.StatusServiceUnavailable || errorReason(w) != reasonGovernanceSecondHumanLocalMode {
				t.Fatalf("%s = %d %q, want 503 %s", tc.name, w.Code, errorReason(w), reasonGovernanceSecondHumanLocalMode)
			}
			if st.reads != 0 || len(st.rows) != 1 || len(govCovAuditActions(h)) != 0 {
				t.Errorf("%d store read(s), %d rows, audit %v; want nothing read, changed or recorded", st.reads, len(st.rows), govCovAuditActions(h))
			}
		})
	}
}
