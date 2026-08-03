// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Integration tests for the Workspace Composition store surface: sources[]/
// base_image/requirements CRUD round-trip, the derived-mirror behaviour
// (Kind/Source/Ref/DefaultTarget populate for a single-source workspace, stay
// zero for a multi-source one), and SetWorkspaceRequirements' anti-clobber
// discipline. Guarded by WARDYN_TEST_PG (see store_pg_test.go).
package store_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_WorkspaceCRUD_CompositionRoundTrip covers CRUD round-trip for
// sources[]/base_image/requirements and the derived-mirror rule: a
// single-source workspace's legacy Kind/Source/Ref/DefaultTarget mirror
// Sources[0] on read; a multi-source workspace leaves them at the zero value
// (there is no single "the" source to mirror).
func TestPG_WorkspaceCRUD_CompositionRoundTrip(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)

	t.Run("single_source_sets_derived_mirrors", func(t *testing.T) {
		now := time.Now().UTC()
		in := types.Workspace{
			ID:   uuid.New(),
			Name: "ws-single-" + uuid.NewString(),
			Sources: []types.WorkspaceSource{{
				Type: types.WorkspaceSourceTypeRepo, Source: "octocat/Hello-World", Ref: "main", Target: "/home/agent/work",
			}},
			BaseImage: &types.WorkspaceBaseImage{
				Kind: "registry", Image: "ghcr.io/acme/base:latest", Steps: []string{"RUN apt-get update"},
			},
			Requirements: map[string]types.WorkspaceRequirement{
				"egress:api.github.com": {Level: "required", Provenance: "scan_seeded"},
			},
			Status:    types.WorkspacePendingScan,
			CreatedAt: now, UpdatedAt: now,
		}
		created, err := pg.CreateWorkspace(ctx, in)
		if err != nil {
			t.Fatalf("create workspace: %v", err)
		}
		t.Cleanup(func() { _ = pg.DeleteWorkspace(context.Background(), created.ID) })

		got, err := pg.GetWorkspace(ctx, created.ID)
		if err != nil {
			t.Fatalf("get workspace: %v", err)
		}

		if len(got.Sources) != 1 || got.Sources[0] != in.Sources[0] {
			t.Errorf("sources = %+v, want %+v", got.Sources, in.Sources)
		}
		if got.BaseImage == nil || !reflect.DeepEqual(*got.BaseImage, *in.BaseImage) {
			t.Errorf("base_image = %+v, want %+v", got.BaseImage, in.BaseImage)
		}
		wantReq := types.WorkspaceRequirement{Level: "required", Provenance: "scan_seeded"}
		if len(got.Requirements) != 1 || got.Requirements["egress:api.github.com"] != wantReq {
			t.Errorf("requirements = %+v, want {egress:api.github.com: %+v}", got.Requirements, wantReq)
		}

		// Derived read-only mirrors: populated from the sole source.
		if got.Kind != types.WorkspaceKind(types.WorkspaceSourceTypeRepo) {
			t.Errorf("derived Kind = %q, want %q", got.Kind, types.WorkspaceSourceTypeRepo)
		}
		if got.Source != "octocat/Hello-World" {
			t.Errorf("derived Source = %q, want octocat/Hello-World", got.Source)
		}
		if got.Ref != "main" {
			t.Errorf("derived Ref = %q, want main", got.Ref)
		}
		if got.DefaultTarget != "/home/agent/work" {
			t.Errorf("derived DefaultTarget = %q, want /home/agent/work", got.DefaultTarget)
		}
	})

	t.Run("multi_source_leaves_mirrors_zero", func(t *testing.T) {
		now := time.Now().UTC()
		in := types.Workspace{
			ID:   uuid.New(),
			Name: "ws-multi-" + uuid.NewString(),
			Sources: []types.WorkspaceSource{
				{Type: types.WorkspaceSourceTypeLocalDir, Path: "/srv/one", Target: "/home/agent/work", Writable: true},
				{Type: types.WorkspaceSourceTypeRepo, Source: "octocat/Hello-World", Target: "/home/agent/extra"},
			},
			Status:    types.WorkspacePendingScan,
			CreatedAt: now, UpdatedAt: now,
		}
		created, err := pg.CreateWorkspace(ctx, in)
		if err != nil {
			t.Fatalf("create workspace: %v", err)
		}
		t.Cleanup(func() { _ = pg.DeleteWorkspace(context.Background(), created.ID) })

		got, err := pg.GetWorkspace(ctx, created.ID)
		if err != nil {
			t.Fatalf("get workspace: %v", err)
		}

		if len(got.Sources) != 2 {
			t.Fatalf("sources round-trip: got %d entries, want 2: %+v", len(got.Sources), got.Sources)
		}
		if got.Sources[0] != in.Sources[0] || got.Sources[1] != in.Sources[1] {
			t.Errorf("sources = %+v, want %+v", got.Sources, in.Sources)
		}
		if got.BaseImage != nil {
			t.Errorf("base_image = %+v, want nil (never set)", got.BaseImage)
		}
		if len(got.Requirements) != 0 {
			t.Errorf("requirements = %+v, want empty (never set)", got.Requirements)
		}

		// A multi-source workspace has no single "the" kind/source/ref/target:
		// every derived mirror must be left at its Go zero value.
		var zero types.Workspace
		if got.Kind != zero.Kind {
			t.Errorf("multi-source Kind = %q, want zero value", got.Kind)
		}
		if got.Source != zero.Source {
			t.Errorf("multi-source Source = %q, want zero value", got.Source)
		}
		if got.Ref != zero.Ref {
			t.Errorf("multi-source Ref = %q, want zero value", got.Ref)
		}
		if got.DefaultTarget != zero.DefaultTarget {
			t.Errorf("multi-source DefaultTarget = %q, want zero value", got.DefaultTarget)
		}
	})
}

// TestPG_SetWorkspaceRequirements_AntiClobber proves SetWorkspaceRequirements
// is a scoped single-column write: it must not disturb a concurrently-set
// operator-owned column (approved_egress), and — the other direction — a later
// write to that OTHER column must not revert the requirements this call just
// set. Mirrors the anti-clobber proof style of
// TestPG_ClaimAndClearActiveRunCAS / TestPG_RecordResultUpsertAndStatusCAS.
func TestPG_SetWorkspaceRequirements_AntiClobber(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)

	now := time.Now().UTC()
	created, err := pg.CreateWorkspace(ctx, types.Workspace{
		ID:      uuid.New(),
		Name:    "ws-reqs-" + uuid.NewString(),
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/work"}},
		Status:  types.WorkspacePendingScan, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	t.Cleanup(func() { _ = pg.DeleteWorkspace(context.Background(), created.ID) })

	// A different scoped writer sets approved_egress first.
	if _, err := pg.SetWorkspaceApprovedEgress(ctx, created.ID, []string{"api.github.com"}); err != nil {
		t.Fatalf("set approved egress: %v", err)
	}

	wantReqs := map[string]types.WorkspaceRequirement{
		"secret:acme-anthropic-key": {Level: "required", Provenance: "operator_set"},
	}
	got, err := pg.SetWorkspaceRequirements(ctx, created.ID, wantReqs)
	if err != nil {
		t.Fatalf("set requirements: %v", err)
	}
	if len(got.ApprovedEgress) != 1 || got.ApprovedEgress[0] != "api.github.com" {
		t.Errorf("SetWorkspaceRequirements clobbered approved_egress: %v", got.ApprovedEgress)
	}
	if len(got.Requirements) != 1 || got.Requirements["secret:acme-anthropic-key"] != wantReqs["secret:acme-anthropic-key"] {
		t.Errorf("requirements = %+v, want %+v", got.Requirements, wantReqs)
	}

	// Reverse direction: a later write to the OTHER scoped column must not
	// revert the requirements just set.
	if _, err := pg.SetWorkspaceApprovedEgress(ctx, created.ID, []string{"api.github.com", "pypi.org"}); err != nil {
		t.Fatalf("second set approved egress: %v", err)
	}
	final, err := pg.GetWorkspace(ctx, created.ID)
	if err != nil {
		t.Fatalf("get workspace: %v", err)
	}
	if len(final.Requirements) != 1 || final.Requirements["secret:acme-anthropic-key"] != wantReqs["secret:acme-anthropic-key"] {
		t.Errorf("a later SetWorkspaceApprovedEgress clobbered requirements: %+v", final.Requirements)
	}

	// Missing workspace: scanWorkspace's ErrNoRows mapping still applies to
	// this scoped writer.
	if _, err := pg.SetWorkspaceRequirements(ctx, uuid.New(), wantReqs); err != store.ErrNotFound {
		t.Errorf("set requirements on unknown id: err = %v, want ErrNotFound", err)
	}
}
