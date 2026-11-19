// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_ForceDelete_NamesItsDetachAndKeepsItOnAFailedDelete: a forced delete of a source or a base
// image returns the names of the workspaces its detach changed, a workspace that attached after the
// caller's listing among them, and a delete that fails after the detach leaves every workspace as it
// was. Guarded by WARDYN_TEST_PG through hydrateStore.
func TestPG_ForceDelete_NamesItsDetachAndKeepsItOnAFailedDelete(t *testing.T) {
	s := hydrateStore(t)
	ctx := context.Background()
	exec := func(q string) {
		t.Helper()
		if _, err := s.Pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	src := mkSource(t, s, types.SourceRepo, "acme/receipts", nil)
	img, err := s.UpsertBaseImage(ctx, types.BaseImageEntry{
		ID: uuid.New(), Kind: "custom", Name: "img", Image: "ubuntu:24.04",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	attach := func(name string) {
		t.Helper()
		if _, err := s.CreateWorkspace(ctx, types.Workspace{
			ID: uuid.New(), Name: name, Status: types.WorkspacePendingScan,
			Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral}},
			Attachments: []types.WorkspaceAttachment{
				{SourceID: &src.ID}, {Ephemeral: true, Target: "/home/agent/scratch"},
			},
			BaseImageID: &img.ID,
			CreatedAt:   time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	attach("ws-early")
	if listed, err := s.WorkspacesAttaching(ctx, src.ID); err != nil || len(listed) != 1 {
		t.Fatalf("listing = %v, %v; want ws-early alone", listed, err)
	}
	attach("ws-late")

	exec(`CREATE FUNCTION refuse_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'delete refused'; END $$`)
	exec(`CREATE TRIGGER refuse_source_delete BEFORE DELETE ON sources FOR EACH ROW EXECUTE FUNCTION refuse_delete()`)
	exec(`CREATE TRIGGER refuse_image_delete BEFORE DELETE ON base_images FOR EACH ROW EXECUTE FUNCTION refuse_delete()`)
	if _, err := s.DeleteSource(ctx, src.ID, true); err == nil {
		t.Fatal("the source delete the trigger refuses returned no error")
	}
	if _, err := s.DeleteBaseImage(ctx, img.ID, true); err == nil {
		t.Fatal("the base image delete the trigger refuses returned no error")
	}
	if still, err := s.WorkspacesAttaching(ctx, src.ID); err != nil || len(still) != 2 {
		t.Errorf("after a failed source delete the workspaces attaching it are %v (%v), want both", still, err)
	}
	if using, err := s.WorkspacesUsingBaseImage(ctx, img.ID); err != nil || len(using) != 2 {
		t.Errorf("after a failed base image delete the workspaces using it are %v (%v), want both", using, err)
	}
	exec(`DROP TRIGGER refuse_source_delete ON sources`)
	exec(`DROP TRIGGER refuse_image_delete ON base_images`)

	detached, err := s.DeleteSource(ctx, src.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(detached, ","); got != "ws-early,ws-late" {
		t.Errorf("source delete detached %q, want ws-early,ws-late", got)
	}
	detached, err = s.DeleteBaseImage(ctx, img.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(detached, ","); got != "ws-early,ws-late" {
		t.Errorf("base image delete detached %q, want ws-early,ws-late", got)
	}
}
