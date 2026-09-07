// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// collisionRequest is the shared-credential caller (admin token or local mode):
// no verified human on the context, so isSecurityOperator answers true the way
// it does for every other tier question.
func collisionRequest() *http.Request {
	return httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil)
}

// memberRequest is a plain member SSO session, the caller F336 is about.
func collisionMemberRequest(sub string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil)
	return r.WithContext(withOIDCRole(withOIDCHuman(r.Context(), sub), string(oidc.RoleMember)))
}

// fallbackCollisionStore has NO ActiveRunsAtWorkspacePath, so it takes the in-Go
// fallback branch — the path every non-PG store takes, and the second half of
// the gap (both branches appended ids unfiltered).
type fallbackCollisionStore struct {
	store.Store
	runs []types.AgentRun
}

func (s *fallbackCollisionStore) ListRuns(context.Context) ([]types.AgentRun, error) {
	return s.runs, nil
}

// TestWorkspaceCollisionNamesOnlyRunsTheCallerMaySee is F336.
//
// The advisory 201 warning enumerated every active run id at the workspace
// path, unfiltered by ownership — so a member creating a run in a shared
// directory learned another principal's run ids, and by repeating the create
// could watch them come and go. handleObservedEgress already applies
// ownsRunOrAdmin to exactly this class of cross-user run telemetry.
//
// The AUDIT row is deliberately not filtered: it is written by wardynd for
// operators, and an operator investigating two agents that fought over a
// directory needs all of them.
func TestWorkspaceCollisionNamesOnlyRunsTheCallerMaySee(t *testing.T) {
	const path = "/srv/shared-workspace"
	const bob, alice = "sub-bob", "sub-alice"

	newSrv := func(t *testing.T, runs []types.AgentRun) (*Server, *recRecorder) {
		t.Helper()
		rec := &recRecorder{}
		return New(Config{Store: &collisionStore{runs: runs}, Audit: rec, RunnerTarget: "docker"}), rec
	}

	t.Run("a member is not told another principal's run id", func(t *testing.T) {
		mine, hers := uuid.New(), uuid.New()
		srv, rec := newSrv(t, []types.AgentRun{
			{ID: hers, WorkspacePath: path, State: types.RunRunning, CreatedBy: alice},
			{ID: mine, WorkspacePath: path, State: types.RunPending, CreatedBy: bob},
		})

		warnings := srv.warnWorkspaceCollision(collisionMemberRequest(bob), mine, path)

		for _, w := range warnings {
			if strings.Contains(w, hers.String()) {
				t.Errorf("the collision warning named %s, a run created by another principal: %q\n"+
					"a member learns foreign run ids from an ADVISORY sentence, and can enumerate them by "+
					"repeating the create", hers, w)
			}
		}
		if len(warnings) != 0 {
			t.Errorf("warnings = %v, want none — the only colliding run is one this caller may not see, and the "+
				"sentence names its ids", warnings)
		}
		// The operator's record is complete regardless.
		ev := lastAuditEvent(t, rec.events, "run.workspace.collision")
		var data map[string]any
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatal(err)
		}
		ids, _ := data["other_runs"].([]any)
		if len(ids) != 1 || ids[0] != hers.String() {
			t.Errorf("audit other_runs = %v, want [%s] — the operator's record of a collision must not shrink "+
				"because the caller owns none of the runs it collided with", data["other_runs"], hers)
		}
	})

	t.Run("a member is still told about their OWN colliding run", func(t *testing.T) {
		mine, alsoMine := uuid.New(), uuid.New()
		srv, _ := newSrv(t, []types.AgentRun{
			{ID: alsoMine, WorkspacePath: path, State: types.RunRunning, CreatedBy: bob},
			{ID: mine, WorkspacePath: path, State: types.RunPending, CreatedBy: bob},
		})

		warnings := srv.warnWorkspaceCollision(collisionMemberRequest(bob), mine, path)
		if len(warnings) != 1 || !strings.Contains(warnings[0], alsoMine.String()) {
			t.Errorf("warnings = %v, want the caller's own colliding run %s named — this is the signal the warning "+
				"exists to give, and the fix must not cost it", warnings, alsoMine)
		}
	})

	t.Run("an operator sees every colliding run", func(t *testing.T) {
		mine, hers := uuid.New(), uuid.New()
		srv, _ := newSrv(t, []types.AgentRun{
			{ID: hers, WorkspacePath: path, State: types.RunRunning, CreatedBy: alice},
			{ID: mine, WorkspacePath: path, State: types.RunPending, CreatedBy: "sub-super"},
		})
		r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil)
		r = r.WithContext(withOIDCRole(withOIDCHuman(r.Context(), "sub-super"), string(oidc.RoleAdmin)))

		warnings := srv.warnWorkspaceCollision(r, mine, path)
		if len(warnings) != 1 || !strings.Contains(warnings[0], hers.String()) {
			t.Errorf("warnings = %v, want the operator to be told about %s — ownsRunOrAdmin, not owner-only",
				warnings, hers)
		}
	})

	// The security tier reads run telemetry by the same rule everywhere else.
	t.Run("a security admin sees every colliding run", func(t *testing.T) {
		mine, hers := uuid.New(), uuid.New()
		srv, _ := newSrv(t, []types.AgentRun{
			{ID: hers, WorkspacePath: path, State: types.RunRunning, CreatedBy: alice},
			{ID: mine, WorkspacePath: path, State: types.RunPending, CreatedBy: "sub-sec"},
		})
		r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil)
		r = r.WithContext(withOIDCRole(withOIDCHuman(r.Context(), "sub-sec"), string(oidc.RoleSecurityAdmin)))

		if warnings := srv.warnWorkspaceCollision(r, mine, path); len(warnings) != 1 {
			t.Errorf("warnings = %v, want the security tier told — ownsRunOrAdmin is isSecurityOperator OR owner",
				warnings)
		}
	})

	// THE SAME GAP EXISTED ON BOTH BRANCHES, so the pin covers both: a store
	// without ActiveRunsAtWorkspacePath falls back to ListRuns + an in-Go
	// filter, which appended ids just as unfiltered.
	t.Run("the in-Go fallback branch filters too", func(t *testing.T) {
		mine, hers := uuid.New(), uuid.New()
		srv := New(Config{Store: &fallbackCollisionStore{runs: []types.AgentRun{
			{ID: hers, WorkspacePath: path, State: types.RunRunning, CreatedBy: alice},
			{ID: mine, WorkspacePath: path, State: types.RunPending, CreatedBy: bob},
		}}, Audit: &recRecorder{}, RunnerTarget: "docker"})

		for _, w := range srv.warnWorkspaceCollision(collisionMemberRequest(bob), mine, path) {
			if strings.Contains(w, hers.String()) {
				t.Errorf("the fallback branch named %s to a member: %q", hers, w)
			}
		}
	})
}
