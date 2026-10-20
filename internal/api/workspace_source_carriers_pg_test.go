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
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_WorkspaceSourceIsHeldToACarrierTheCallerMayUse: nothing makes a source
// unique across workspaces, so a repo another member onboarded first does not
// refuse a member their own workspace or a shared one carrying it, at the
// onboarding gate or at the capability bound. Another person's private
// workspace never admits it: a source only they carry answers exactly as one
// nobody onboarded, and a grant on their workspace is not a grant on the source.
func TestPG_WorkspaceSourceIsHeldToACarrierTheCallerMayUse(t *testing.T) {
	pool := throwawayPGPool(t)
	pg := store.NewPG(pool)
	h := newHarness(t)
	h.srv.cfg.Store = pg
	h.srv.cfg.OIDC = &oidc.Authenticator{}
	ctx := context.Background()

	const subA, subB, subC = "sub-carrier-a", "sub-carrier-b", "sub-carrier-c"
	const shared, onlyA, nobody = "org/shared-app", "org/a-private", "org/never-onboarded"
	base := time.Now().UTC().Add(-time.Hour)
	put := func(i int, owner string, repos ...string) types.Workspace {
		t.Helper()
		var srcs []types.WorkspaceSource
		for _, r := range repos {
			srcs = append(srcs, types.WorkspaceSource{Type: types.WorkspaceSourceTypeRepo, Source: r})
		}
		ws, err := pg.CreateWorkspace(ctx, types.Workspace{
			ID: uuid.New(), Name: "ws-" + owner, OwnedBy: owner, Status: types.WorkspaceScanned, Sources: srcs,
			CreatedAt: base.Add(time.Duration(i) * time.Minute), UpdatedAt: base,
		})
		if err != nil {
			t.Fatal(err)
		}
		return ws
	}
	// A onboarded both repos first; B's own and the shared workspace came later.
	wsA := put(0, subA, shared, onlyA)
	wsB := put(1, subB, shared)
	wsO := put(2, "", shared)

	admit := func(sub string, attach *uuid.UUID, repos ...string) *httptest.ResponseRecorder {
		t.Helper()
		spec := types.RunPolicySpec{MinConfinementClass: types.CC2}
		for _, r := range repos {
			spec.WorkspaceRepos = append(spec.WorkspaceRepos, types.WorkspaceRepo{Repo: r})
		}
		req := createRunRequest{Agent: "claude-code", WorkspaceID: attach}
		r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(operatorCtx(sub, sub+"@corp.example", oidc.RoleUser))
		w := httptest.NewRecorder()
		if _, ok := h.srv.seedAndAdmitWorkspace(r.Context(), w, r, &spec, &req, true); ok {
			w.Code = http.StatusOK
		}
		return w
	}

	t.Run("gate: own and shared workspaces admit a source another member onboarded first", func(t *testing.T) {
		if w := admit(subB, &wsB.ID); w.Code != http.StatusOK {
			t.Errorf("B attaching own workspace: %d %s, want admitted", w.Code, w.Body.String())
		}
		if w := admit(subC, &wsO.ID); w.Code != http.StatusOK {
			t.Errorf("C attaching the shared workspace: %d %s, want admitted", w.Code, w.Body.String())
		}
		if w := admit(subC, nil, shared); w.Code != http.StatusOK {
			t.Errorf("C naming a repo the shared workspace carries: %d %s, want admitted", w.Code, w.Body.String())
		}
	})

	t.Run("gate: a source only another person carries answers as one nobody onboarded", func(t *testing.T) {
		foreign, never := admit(subC, nil, onlyA), admit(subC, nil, nobody)
		if foreign.Code != http.StatusUnprocessableEntity || never.Code != http.StatusUnprocessableEntity ||
			foreign.Body.String() != strings.ReplaceAll(never.Body.String(), nobody, onlyA) {
			t.Errorf("foreign-only: %d %s; never onboarded: %d %s; want the same 422 but for the repo name",
				foreign.Code, foreign.Body.String(), never.Code, never.Body.String())
		}
	})

	t.Run("capability bound: a grant on a carrier the caller may use admits; one on another person's workspace does not", func(t *testing.T) {
		if _, err := pg.PutCapabilityEnforcement(ctx, map[string]bool{capWorkspace: true}); err != nil {
			t.Fatal(err)
		}
		for sub, ws := range map[string]types.Workspace{subB: wsB, subC: wsA} {
			if _, err := pg.UpsertCapabilityGrant(ctx, types.CapabilityGrant{ID: uuid.New(), SubjectType: types.CapabilitySubjectUser,
				Subject: sub, Capability: capWorkspace, Value: ws.ID.String(), Effect: types.CapabilityAllow}); err != nil {
				t.Fatal(err)
			}
		}
		bound := func(sub string) error {
			spec := types.RunPolicySpec{MinConfinementClass: types.CC2, WorkspaceRepos: []types.WorkspaceRepo{{Repo: shared}}}
			_, _, err := h.srv.narrowUserInlinePolicy(memberCtxFor(sub), sub, &spec)
			return err
		}
		if err := bound(subB); err != nil {
			t.Errorf("B, granted own workspace: %v, want admitted", err)
		}
		var refused *errUngrantedWorkspaceRepo
		if err := bound(subC); !errors.As(err, &refused) || refused.wsID == wsA.ID.String() {
			t.Errorf("C, granted only A's private workspace: %v, want refused, never on A's grant nor naming A's workspace", err)
		}
	})
}

// TestPG_WorkspaceMountIsJudgedByTheCarrierTheRunUses: a directory a member's own workspace and an
// operator workspace both carry is judged by the carrier the run uses (the attached one), and the
// member's own mount gate runs whichever carrier judged it. Here the member's roots no longer
// admit the directory, and the operator's workspace, onboarded later, is the last carrier.
func TestPG_WorkspaceMountIsJudgedByTheCarrierTheRunUses(t *testing.T) {
	pool := throwawayPGPool(t)
	pg := store.NewPG(pool)
	h := newHarness(t)
	h.srv.cfg.Store = pg
	h.srv.cfg.OIDC = &oidc.Authenticator{}
	h.srv.cfg.UserMounts = runner.UserMountPolicy{Roots: []string{t.TempDir()}}
	ctx := context.Background()
	dir := t.TempDir() // outside every member root

	const subM, subN = "sub-mount-m", "sub-mount-n"
	base := time.Now().UTC().Add(-time.Hour)
	put := func(i int, owner string) types.Workspace {
		t.Helper()
		ws, err := pg.CreateWorkspace(ctx, types.Workspace{
			ID: uuid.New(), Name: "ws-mount-" + owner, OwnedBy: owner, Status: types.WorkspaceScanned,
			Sources:   []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: dir}},
			CreatedAt: base.Add(time.Duration(i) * time.Minute), UpdatedAt: base,
		})
		if err != nil {
			t.Fatal(err)
		}
		return ws
	}
	wsM := put(0, subM)
	wsO := put(1, "")

	admit := func(sub string, attach uuid.UUID) *httptest.ResponseRecorder {
		t.Helper()
		spec := types.RunPolicySpec{MinConfinementClass: types.CC2}
		req := createRunRequest{Agent: "claude-code", WorkspaceID: &attach}
		r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(operatorCtx(sub, sub+"@corp.example", oidc.RoleUser))
		w := httptest.NewRecorder()
		if _, ok := h.srv.seedAndAdmitWorkspace(r.Context(), w, r, &spec, &req, true); ok {
			w.Code = http.StatusOK
		}
		return w
	}
	refused := func(w *httptest.ResponseRecorder) bool {
		return w.Code == http.StatusUnprocessableEntity && strings.Contains(w.Body.String(), reasonWorkspaceSourceMountNotAllowed)
	}
	if w := admit(subM, wsM.ID); !refused(w) {
		t.Errorf("member attaching their own workspace: %d %s, want the member mount gate's refusal", w.Code, w.Body.String())
	}
	if w := admit(subM, wsO.ID); !refused(w) {
		t.Errorf("member attaching the operator-owned carrier of their own directory: %d %s, want their own mount gate's refusal", w.Code, w.Body.String())
	}
	if w := admit(subN, wsO.ID); w.Code != http.StatusOK {
		t.Errorf("another member attaching the operator-owned workspace: %d %s, want admitted (an operator mount)", w.Code, w.Body.String())
	}
}
