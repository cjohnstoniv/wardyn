// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The hydrate pass is the three-tier split's compat engine: a workspace whose
// composition lives in ATTACHMENTS reads back with the same derived
// Sources/Profile/Status/BaseImage view every pre-split consumer expects, plus
// the folded EffectiveRequirements the run path consumes.

func hydrateStore(t *testing.T) store.PG {
	t.Helper()
	pool := databaseBefore(t, "zzz") // every migration applied
	return store.PG{Pool: pool}
}

func mkSource(t *testing.T, s store.PG, kind types.SourceKind, locator string, reqs map[string]types.WorkspaceRequirement) types.Source {
	t.Helper()
	src, err := s.UpsertSource(context.Background(), types.Source{
		ID: uuid.New(), Kind: kind, Locator: locator, Name: "src",
		Requirements: reqs, Status: types.WorkspacePendingScan,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("upsert source: %v", err)
	}
	return src
}

func TestPG_HydrateAttachments_DerivedViewAndFold(t *testing.T) {
	s := hydrateStore(t)
	ctx := context.Background()

	src := mkSource(t, s, types.SourceLocalDir, "/home/me/payments", map[string]types.WorkspaceRequirement{
		"egress:api.stripe.com":     {Level: "required", Provenance: "scan_seeded"},
		"secret:stripe-key":         {Level: "required", Provenance: "scan_seeded"},
		"write:/home/me/payments":   {Level: "optional", Provenance: "operator_set"},
		"write:/home/me/other-repo": {Level: "required", Provenance: "operator_set"}, // cross-path: must drop
	})
	prof, _ := json.Marshal(map[string]any{"languages": []string{"go"}, "confidence": "high"})
	if _, err := s.SetSourceScanResultUnfenced(ctx, src.ID, prof, types.WorkspaceScanned); err != nil {
		t.Fatalf("set source scan: %v", err)
	}

	img, err := s.UpsertBaseImage(ctx, types.BaseImageEntry{
		ID: uuid.New(), Kind: "registry", Name: "go", Image: "docker.io/library/golang:1.22",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("upsert image: %v", err)
	}

	ws, err := s.CreateWorkspace(ctx, types.Workspace{
		ID: uuid.New(), Name: "pay", Status: types.WorkspacePendingScan,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral}}, // legacy column: ignored once attachments exist
		Attachments: []types.WorkspaceAttachment{
			{Ephemeral: true, Target: "/scratch"},
			{SourceID: &src.ID, Target: "/work/pay", Writable: true,
				Overrides: map[string]string{"secret:stripe-key": types.OverrideOff}},
		},
		BaseImageID: &img.ID,
		Requirements: map[string]types.WorkspaceRequirement{
			"integration:corp-artifactory": {Level: "required", Provenance: "operator_set"},
		},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Derived Sources view, in attachment order, per-attachment knobs applied.
	if len(ws.Sources) != 2 || ws.Sources[0].Type != types.WorkspaceSourceTypeEphemeral {
		t.Fatalf("Sources = %+v, want [ephemeral, local_dir]", ws.Sources)
	}
	if ws.Sources[1].Path != "/home/me/payments" || !ws.Sources[1].Writable || ws.Sources[1].Target != "/work/pay" {
		t.Errorf("Sources[1] = %+v, want the attached dir with writable+target", ws.Sources[1])
	}
	// Status derives worst-of (one scanned source + ephemeral = scanned).
	if ws.Status != types.WorkspaceScanned {
		t.Errorf("Status = %s, want scanned (derived)", ws.Status)
	}
	// Profile derives from the source's scan.
	if ws.Profile == nil {
		t.Error("Profile must derive from the attached source's scan")
	}
	// BaseImage derives from the catalog ref.
	if ws.BaseImage == nil || ws.BaseImage.Image != "docker.io/library/golang:1.22" {
		t.Errorf("BaseImage = %+v, want the catalog row's view", ws.BaseImage)
	}
	// The fold: source rows minus the off-override minus the cross-path write,
	// plus the workspace overlay.
	eff := ws.EffectiveRequirements
	if _, present := eff["secret:stripe-key"]; present {
		t.Error("off-overridden source row must not fold in")
	}
	if _, present := eff["write:/home/me/other-repo"]; present {
		t.Error("cross-path write claim must be dropped by the fold")
	}
	if eff["egress:api.stripe.com"].Level != "required" {
		t.Errorf("source egress row missing from fold: %+v", eff)
	}
	if eff["integration:corp-artifactory"].Level != "required" {
		t.Errorf("overlay row missing from fold: %+v", eff)
	}
	// The overlay itself stays JUST the overlay.
	if len(ws.Requirements) != 1 {
		t.Errorf("Requirements (overlay) = %+v, want only the workspace's own row", ws.Requirements)
	}
}

// Pre-split rows (empty attachments) pass through byte-identically: embedded
// sources stay the truth, and EffectiveRequirements IS the overlay.
func TestPG_HydrateLegacyRow_PassthroughIdentity(t *testing.T) {
	s := hydrateStore(t)
	ctx := context.Background()
	overlay := map[string]types.WorkspaceRequirement{
		"secret:acme-key": {Level: "required", Provenance: "operator_set"},
	}
	ws, err := s.CreateWorkspace(ctx, types.Workspace{
		ID: uuid.New(), Name: "legacy", Status: types.WorkspacePendingScan,
		Sources:      []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: "/x", Writable: true}},
		Requirements: overlay,
		CreatedAt:    time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(ws.Sources) != 1 || ws.Sources[0].Path != "/x" {
		t.Errorf("legacy Sources must pass through: %+v", ws.Sources)
	}
	if ws.Status != types.WorkspacePendingScan {
		t.Errorf("legacy Status must pass through, got %s", ws.Status)
	}
	a, _ := json.Marshal(ws.EffectiveRequirements)
	b, _ := json.Marshal(overlay)
	if string(a) != string(b) {
		t.Errorf("EffectiveRequirements = %s, want the overlay identity %s", a, b)
	}
}

func TestPG_MergeWorkspaceRequirements_AtomicAndCapped(t *testing.T) {
	s := hydrateStore(t)
	ctx := context.Background()
	ws, err := s.CreateWorkspace(ctx, types.Workspace{
		ID: uuid.New(), Name: "m", Status: types.WorkspacePendingScan,
		Sources:   []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral}},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Merge adds without clobbering.
	if _, err := s.MergeWorkspaceRequirements(ctx, ws.ID, map[string]types.WorkspaceRequirement{
		"egress:a.example": {Level: "required", Provenance: "operator_set"},
	}); err != nil {
		t.Fatalf("merge 1: %v", err)
	}
	got, err := s.MergeWorkspaceRequirements(ctx, ws.ID, map[string]types.WorkspaceRequirement{
		"egress:b.example": {Level: "required", Provenance: "operator_set"},
	})
	if err != nil {
		t.Fatalf("merge 2: %v", err)
	}
	if len(got.Requirements) != 2 {
		t.Errorf("overlay after two merges = %+v, want both rows (|| is additive)", got.Requirements)
	}

	// The cap refuses with ErrConflict, never silently dropping.
	big := map[string]types.WorkspaceRequirement{}
	for i := 0; i < 256; i++ {
		big["egress:h"+uuid.NewString()[:8]+".example"] = types.WorkspaceRequirement{Level: "optional", Provenance: "operator_set"}
	}
	if _, err := s.MergeWorkspaceRequirements(ctx, ws.ID, big); err != nil {
		t.Fatalf("bulk merge under cap: %v", err)
	}
	_, err = s.MergeWorkspaceRequirements(ctx, ws.ID, map[string]types.WorkspaceRequirement{
		"egress:one-more.example": {Level: "required", Provenance: "operator_set"},
	})
	if !errors.Is(err, store.ErrConflict) {
		t.Errorf("merge past the cap = %v, want ErrConflict", err)
	}

	// Unknown workspace is NotFound, not Conflict.
	if _, err := s.MergeWorkspaceRequirements(ctx, uuid.New(), map[string]types.WorkspaceRequirement{
		"egress:x.example": {Level: "required", Provenance: "operator_set"},
	}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("merge on missing workspace = %v, want ErrNotFound", err)
	}
}

// Delete-in-use is loud: names, 409-shaped; force detaches.
func TestPG_DeleteSourceInUse(t *testing.T) {
	s := hydrateStore(t)
	ctx := context.Background()
	src := mkSource(t, s, types.SourceRepo, "acme/widgets", nil)
	for _, name := range []string{"ws-a", "ws-b"} {
		if _, err := s.CreateWorkspace(ctx, types.Workspace{
			ID: uuid.New(), Name: name, Status: types.WorkspacePendingScan,
			Sources:     []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral}},
			Attachments: []types.WorkspaceAttachment{{SourceID: &src.ID}},
			CreatedAt:   time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	names, err := s.WorkspacesAttaching(ctx, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "ws-a" || names[1] != "ws-b" {
		t.Fatalf("attaching = %v, want both names, sorted", names)
	}
	// Force-detach then delete: attachments shrink, library row gone.
	if err := s.DeleteSource(ctx, src.ID, true); err != nil {
		t.Fatalf("force delete: %v", err)
	}
	left, err := s.WorkspacesAttaching(ctx, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("attachments must be stripped on force delete, still: %v", left)
	}
}
