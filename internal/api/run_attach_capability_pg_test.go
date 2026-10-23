// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The workspace capability at the run doors, on a real Postgres: every
// workspace the request attaches is held to the caller's grant, workspace_id
// and each workspaces[] entry alike. Guarded by WARDYN_TEST_PG: skipped cleanly
// when unset, must PASS when set.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// attachCapMember is the member newPresetFixture signs in.
const attachCapMember = "sub-preset-member"

// attachCapWorkspaces stores two shared scratch-only workspaces a member may
// launch, apart from any capability: nothing to clone and nothing to mount.
func attachCapWorkspaces(t *testing.T, pg store.PG) (a, b uuid.UUID) {
	t.Helper()
	ids := make([]uuid.UUID, 2)
	for i, name := range []string{"cap-a", "cap-b"} {
		ws, err := pg.CreateWorkspace(context.Background(), types.Workspace{ID: uuid.New(), Name: name, Status: types.WorkspaceScanned,
			Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/" + name}}})
		if err != nil {
			t.Fatalf("CreateWorkspace: %v", err)
		}
		ids[i] = ws.ID
	}
	return ids[0], ids[1]
}

// attachCapBody is a background run attaching ids in order, with workspace_id
// set to primary when it is not nil.
func attachCapBody(primary *uuid.UUID, ids ...uuid.UUID) string {
	sels := make([]string, len(ids))
	for i, id := range ids {
		sels[i] = `{"workspace_id":"` + id.String() + `"}`
	}
	alias := ""
	if primary != nil {
		alias = `"workspace_id":"` + primary.String() + `",`
	}
	return `{"agent":"claude-code","task":"t",` + alias + `"workspaces":[` + strings.Join(sels, ",") + `]}`
}

func setWorkspaceEnforcement(t *testing.T, pg store.PG, on bool) {
	t.Helper()
	if _, err := pg.PutCapabilityEnforcement(context.Background(), map[string]bool{capWorkspace: on}); err != nil {
		t.Fatalf("PutCapabilityEnforcement: %v", err)
	}
}

func grantWorkspace(t *testing.T, pg store.PG, id uuid.UUID) types.CapabilityGrant {
	t.Helper()
	g, err := pg.UpsertCapabilityGrant(context.Background(),
		grant(types.CapabilitySubjectUser, attachCapMember, capWorkspace, id.String(), types.CapabilityAllow))
	if err != nil {
		t.Fatalf("UpsertCapabilityGrant: %v", err)
	}
	return g
}

// TestPG_RunAttachedWorkspacesHeldToTheGrant: a member granted A only, with
// the workspace kind enforced, is refused B however the request attaches it,
// at create, preflight and preview, with the refusal workspace_id gets. Granted
// both, unenforced, or as the operator, the same attach launches; a primary
// that is not workspaces[0] stays malformed.
func TestPG_RunAttachedWorkspacesHeldToTheGrant(t *testing.T) {
	f := newPresetFixture(t)
	a, b := attachCapWorkspaces(t, f.pg)
	setWorkspaceEnforcement(t, f.pg, true)
	grantWorkspace(t, f.pg, a)

	refusedNamingB := func(t *testing.T, w *httptest.ResponseRecorder) {
		t.Helper()
		if w.Code != http.StatusForbidden || reasonOf(t, w) != string(authz.ReasonCapabilityWorkspace) ||
			!strings.Contains(w.Body.String(), "you are not granted workspace "+b.String()+" — ask an admin for access, or launch without a workspace") {
			t.Fatalf("%d %s, want 403 %s naming %s", w.Code, w.Body.String(), authz.ReasonCapabilityWorkspace, b)
		}
	}
	launched := func(t *testing.T, w *httptest.ResponseRecorder) {
		t.Helper()
		f.dispatched(t, mustCreated(t, w).ID)
	}
	member := func(path, body string) *httptest.ResponseRecorder {
		return doSSO(t, f.srv, http.MethodPost, path, f.member, body)
	}

	t.Run("workspaces [B] is refused naming B", func(t *testing.T) {
		refusedNamingB(t, member("/api/v1/runs", attachCapBody(nil, b)))
	})
	t.Run("workspaces [A, B] is refused naming B", func(t *testing.T) {
		refusedNamingB(t, member("/api/v1/runs", attachCapBody(nil, a, b)))
	})
	t.Run("workspace_id A with workspaces [A, B] is refused naming B", func(t *testing.T) {
		refusedNamingB(t, member("/api/v1/runs", attachCapBody(&a, a, b)))
	})
	t.Run("preflight of workspaces [B] is refused naming B", func(t *testing.T) {
		refusedNamingB(t, member("/api/v1/runs/preflight", attachCapBody(nil, b)))
	})
	t.Run("preview of workspaces [B] is refused naming B", func(t *testing.T) {
		refusedNamingB(t, member(policyPreviewPath, attachCapBody(nil, b)))
	})
	t.Run("control: the operator attaches B", func(t *testing.T) {
		launched(t, do(t, f.srv, http.MethodPost, "/api/v1/runs", adminToken, attachCapBody(nil, b)))
	})

	bGrant := grantWorkspace(t, f.pg, b)
	t.Run("control: granted both, the member attaches both", func(t *testing.T) {
		launched(t, member("/api/v1/runs", attachCapBody(&a, a, b)))
	})
	t.Run("control: a primary that is not workspaces[0] stays malformed", func(t *testing.T) {
		w := member("/api/v1/runs", attachCapBody(&a, b))
		if w.Code != http.StatusBadRequest || reasonOf(t, w) != reasonWorkspaceSelectionInvalid {
			t.Fatalf("%d %s, want 400 %s", w.Code, w.Body.String(), reasonWorkspaceSelectionInvalid)
		}
	})

	if _, err := f.pg.DeleteCapabilityGrant(context.Background(), bGrant.ID); err != nil {
		t.Fatalf("DeleteCapabilityGrant: %v", err)
	}
	setWorkspaceEnforcement(t, f.pg, false)
	t.Run("control: unenforced, the member attaches B ungranted", func(t *testing.T) {
		launched(t, member("/api/v1/runs", attachCapBody(&a, a, b)))
	})
}

// capRestrictionsUnreadable fails the one capability read the workspace check
// makes on an unenforced kind.
type capRestrictionsUnreadable struct{ store.PG }

func (capRestrictionsUnreadable) ListCapabilityRestrictions(context.Context) (map[string]map[string]bool, error) {
	return nil, errors.New("restrictions unreadable")
}

// TestPG_RunAttachedWorkspaceCapabilityReadFailure: a capability read that
// fails while an attached workspace is checked answers what workspace_id's
// check answers, never a launch.
func TestPG_RunAttachedWorkspaceCapabilityReadFailure(t *testing.T) {
	f := newPresetFixture(t)
	_, b := attachCapWorkspaces(t, f.pg)
	f.srv.cfg.Store = capRestrictionsUnreadable{f.pg}
	alias := doSSO(t, f.srv, http.MethodPost, "/api/v1/runs", f.member, `{"agent":"claude-code","task":"t","workspace_id":"`+b.String()+`"}`)
	attached := doSSO(t, f.srv, http.MethodPost, "/api/v1/runs", f.member, attachCapBody(nil, b))
	if alias.Code != http.StatusInternalServerError || attached.Code != alias.Code || attached.Body.String() != alias.Body.String() {
		t.Fatalf("workspaces [B]: %d %s; workspace_id B: %d %s; want the same 500",
			attached.Code, attached.Body.String(), alias.Code, alias.Body.String())
	}
}
