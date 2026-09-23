// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_WorkspaceMemberScope runs the member workspace list on the path
// production takes: store.PG is a WorkspacesByOwnerPager, so a member's
// GET /workspaces is answered by ListWorkspacesPageForOwner's SQL (own rows
// plus operator-owned ones), never by the in-Go filter every api fake exercises.
// A regression that dropped owned_by from wsCols/scanWorkspace would read every
// member-owned row back as "" — operator-owned, visible to every member — and
// only a real Postgres shows it.
func TestPG_WorkspaceMemberScope(t *testing.T) {
	pool := throwawayPGPool(t)
	pg := store.NewPG(pool)
	h := newHarness(t)
	h.srv.cfg.Store = pg
	h.srv.cfg.OIDC = &oidc.Authenticator{}
	h.srv.router = h.srv.routes()
	ctx := context.Background()

	const subA, subB = "sub-member-a", "sub-member-b"
	memberA := ssoSession(t, subA, "a@corp.example", oidc.RoleMember)
	memberB := ssoSession(t, subB, "b@corp.example", oidc.RoleMember)
	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)

	byOwner := map[string][]uuid.UUID{}
	base := time.Now().UTC().Add(-time.Hour)
	for i, owner := range []string{subA, subB, "", subA, subB, ""} {
		ws, err := pg.CreateWorkspace(ctx, types.Workspace{
			ID: uuid.New(), Name: fmt.Sprintf("ws-%d", i), OwnedBy: owner, Status: types.WorkspaceScanned,
			Sources:   []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/work"}},
			CreatedAt: base.Add(time.Duration(i) * time.Minute), UpdatedAt: base,
		})
		if err != nil {
			t.Fatalf("CreateWorkspace(%q): %v", owner, err)
		}
		if ws.OwnedBy != owner {
			t.Fatalf("CreateWorkspace returned owned_by %q, want %q", ws.OwnedBy, owner)
		}
		byOwner[owner] = append(byOwner[owner], ws.ID)
	}

	// listAll walks GET /workspaces one row per page, so the SQL's LIMIT/OFFSET
	// is part of what is proven: a scope applied after paging would still pass a
	// single unpaged read.
	listAll := func(t *testing.T, who *http.Cookie) map[uuid.UUID]bool {
		t.Helper()
		got := map[uuid.UUID]bool{}
		for offset := 0; offset < 20; offset++ {
			w := doSSO(t, h.srv, http.MethodGet, fmt.Sprintf("/api/v1/workspaces?limit=1&offset=%d", offset), who, "")
			if w.Code != http.StatusOK {
				t.Fatalf("GET /workspaces offset %d: %d %s", offset, w.Code, w.Body.String())
			}
			var page []types.Workspace
			if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
				t.Fatalf("decode offset %d: %v", offset, err)
			}
			if len(page) == 0 {
				return got
			}
			if len(page) > 1 {
				t.Fatalf("offset %d returned %d rows for limit=1", offset, len(page))
			}
			if got[page[0].ID] {
				t.Fatalf("offset %d repeated %s", offset, page[0].ID)
			}
			got[page[0].ID] = true
		}
		t.Fatal("paging never ended")
		return nil
	}
	want := func(owners ...string) map[uuid.UUID]bool {
		out := map[uuid.UUID]bool{}
		for _, o := range owners {
			for _, id := range byOwner[o] {
				out[id] = true
			}
		}
		return out
	}
	assertSees := func(t *testing.T, name string, who *http.Cookie, owners ...string) {
		t.Helper()
		got, exp := listAll(t, who), want(owners...)
		if !maps.Equal(got, exp) {
			t.Errorf("%s sees %v, want exactly the rows owned by %q: %v",
				name, slices.Collect(maps.Keys(got)), owners, slices.Collect(maps.Keys(exp)))
		}
	}

	assertSees(t, "member A", memberA, subA, "")
	assertSees(t, "member B", memberB, subB, "")
	assertSees(t, "admin", admin, subA, subB, "")

	// UpdateWorkspace is a full-column write that must never move ownership:
	// owned_by is absent from its SET list, and this is what pins that.
	a1 := byOwner[subA][0]
	ws, err := pg.GetWorkspace(ctx, a1)
	if err != nil {
		t.Fatal(err)
	}
	if ws.OwnedBy != subA {
		t.Fatalf("GetWorkspace owned_by = %q, want %q", ws.OwnedBy, subA)
	}
	ws.OwnedBy = subB
	ws.Name = "renamed"
	upd, err := pg.UpdateWorkspace(ctx, a1, ws, false)
	if err != nil {
		t.Fatalf("UpdateWorkspace: %v", err)
	}
	if got, _ := pg.GetWorkspace(ctx, a1); upd.OwnedBy != subA || got.OwnedBy != subA || got.Name != "renamed" {
		t.Fatalf("after UpdateWorkspace carrying OwnedBy=%q: returned %q, stored %q (name %q) — want owner %q kept, name updated",
			subB, upd.OwnedBy, got.OwnedBy, got.Name, subA)
	}

	// SetWorkspaceOwner is the one writer of the column: it round-trips, and
	// the member lists follow it.
	moved, err := pg.SetWorkspaceOwner(ctx, a1, subB)
	if err != nil {
		t.Fatalf("SetWorkspaceOwner: %v", err)
	}
	if got, _ := pg.GetWorkspace(ctx, a1); moved.OwnedBy != subB || got.OwnedBy != subB {
		t.Fatalf("SetWorkspaceOwner: returned %q, stored %q, want %q", moved.OwnedBy, got.OwnedBy, subB)
	}
	byOwner[subA] = byOwner[subA][1:]
	byOwner[subB] = append(byOwner[subB], a1)
	assertSees(t, "member A after the reassign", memberA, subA, "")
	assertSees(t, "member B after the reassign", memberB, subB, "")
}
