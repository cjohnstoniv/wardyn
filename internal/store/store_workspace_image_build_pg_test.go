// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

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

func imageBuildWorkspace(t *testing.T, pg store.PG, imageRef string) types.Workspace {
	t.Helper()
	now := time.Now().UTC()
	ws, err := pg.CreateWorkspace(context.Background(), types.Workspace{
		ID: uuid.New(), Name: "ws-image-build-" + uuid.NewString(), ImageRef: imageRef,
		Status: types.WorkspacePendingScan, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	t.Cleanup(func() { _ = pg.DeleteWorkspace(context.Background(), ws.ID) })
	return ws
}

func sampleImageBuild() *types.ImageBuild {
	return &types.ImageBuild{
		Schema: types.ImageBuildSchema, Lane: types.ImageBuildLaneGenerated, CacheKey: "key", BuildID: "build-1",
		Reason: types.ImageBuildReasonCacheMiss, BuiltAt: time.Now().UTC().Truncate(time.Millisecond),
		WardynVersion: "0.9.0", Resolved: true,
		Inputs: types.ImageBuildInputs{
			Base:       types.ImageBuildRef{Ref: "base:ubuntu", Digest: "sha256:aa"},
			Features:   []types.ImageBuildFeature{{Ref: "feat:1", Digest: "sha256:bb", Options: map[string]any{"version": "1.22"}}},
			Installers: []types.ImageBuildInstaller{{Tool: "claude-code", Channel: "stable", Version: "2.0.0", Platform: "linux-x64", SHA256: "cc"}},
			Generator:  &types.ImageBuildGenerator{Salt: "v2", FilesSHA256: map[string]string{"a": "1"}},
			Repo:       &types.ImageBuildRepo{URL: "https://example.test/r.git", Ref: "main"},
		},
		Image: types.ImageBuildImage{Ref: "wardyn-workspace/x:tag", ID: "sha256:dd"},
	}
}

func TestPG_SetWorkspaceBuiltImage_WritesRefHashAndRecordTogether(t *testing.T) {
	pg := store.NewPG(runsPGPool(t))
	ctx := context.Background()
	ws := imageBuildWorkspace(t, pg, "old:tag")
	rec := sampleImageBuild()

	applied, err := pg.SetWorkspaceBuiltImage(ctx, ws.ID, "old:tag", "new:tag", "hash-1", rec)
	if err != nil || !applied {
		t.Fatalf("applied=%v err=%v, want applied", applied, err)
	}
	got, err := pg.GetWorkspace(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ImageRef != "new:tag" || got.BuiltProfileHash != "hash-1" {
		t.Fatalf("image_ref/hash = %q/%q", got.ImageRef, got.BuiltProfileHash)
	}
	if got.ImageBuild == nil {
		t.Fatal("record not read back")
	}
	if !got.ImageBuild.BuiltAt.Equal(rec.BuiltAt) || got.ImageBuild.BuiltAt.Location() != time.UTC {
		t.Errorf("built_at = %v, want %v in UTC", got.ImageBuild.BuiltAt, rec.BuiltAt)
	}
	g, w := *got.ImageBuild, *rec
	g.BuiltAt, w.BuiltAt = time.Time{}, time.Time{}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("record = %+v\nwant     %+v", g, w)
	}
}

func TestPG_SetWorkspaceBuiltImage_StaleObservedRefChangesNothing(t *testing.T) {
	pg := store.NewPG(runsPGPool(t))
	ctx := context.Background()
	ws := imageBuildWorkspace(t, pg, "current:tag")

	applied, err := pg.SetWorkspaceBuiltImage(ctx, ws.ID, "stale:tag", "new:tag", "hash-2", sampleImageBuild())
	if err != nil || applied {
		t.Fatalf("applied=%v err=%v, want a clean refusal", applied, err)
	}
	got, err := pg.GetWorkspace(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ImageRef != "current:tag" || got.BuiltProfileHash != "" || got.ImageBuild != nil {
		t.Errorf("row changed: ref=%q hash=%q record=%v", got.ImageRef, got.BuiltProfileHash, got.ImageBuild)
	}
	if applied, err := pg.SetWorkspaceBuiltImage(ctx, uuid.New(), "", "x", "y", nil); err != nil || applied {
		t.Errorf("unknown id: applied=%v err=%v, want false, nil", applied, err)
	}
}

func TestPG_UpdateWorkspace_LeavesImageBuildAlone(t *testing.T) {
	pg := store.NewPG(runsPGPool(t))
	ctx := context.Background()
	ws := imageBuildWorkspace(t, pg, "")
	stale, err := pg.GetWorkspace(ctx, ws.ID) // snapshot taken before the record exists
	if err != nil {
		t.Fatal(err)
	}
	if applied, err := pg.SetWorkspaceBuiltImage(ctx, ws.ID, "", "new:tag", "h", sampleImageBuild()); err != nil || !applied {
		t.Fatalf("applied=%v err=%v", applied, err)
	}
	stale.Name += "-edited"
	if _, err := pg.UpdateWorkspace(ctx, ws.ID, stale, false); err != nil {
		t.Fatal(err)
	}
	got, err := pg.GetWorkspace(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ImageBuild == nil || got.ImageBuild.BuildID != "build-1" {
		t.Fatalf("record lost by a full-row update: %+v", got.ImageBuild)
	}
}

func TestPG_WorkspaceImageBuild_PreexistingRowReadsNil(t *testing.T) {
	pool := runsPGPool(t)
	pg := store.NewPG(pool)
	ctx := context.Background()
	id := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, attachments) VALUES ($1, $2, '[]'::jsonb)`,
		id, "ws-preexisting-"+id.String()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.DeleteWorkspace(context.Background(), id) })

	got, err := pg.GetWorkspace(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.ImageBuild != nil {
		t.Errorf("GetWorkspace: ImageBuild = %+v, want nil", got.ImageBuild)
	}
	all, err := pg.ListWorkspaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range all {
		if w.ID == id {
			found = true
			if w.ImageBuild != nil {
				t.Errorf("ListWorkspaces: ImageBuild = %+v, want nil", w.ImageBuild)
			}
		}
	}
	if !found {
		t.Error("preexisting row missing from ListWorkspaces")
	}
}
