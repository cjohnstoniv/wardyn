// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// keepPoolRunner is the queue env's runner, able to delete drives, so member
// drives (and keep_work_folder) are open.
type keepPoolRunner struct{ *poolRunner }

func (*keepPoolRunner) ReclaimDrive(context.Context, types.DriveMount) (runner.DriveReclaimOutcome, error) {
	return runner.DriveReclaimAlreadyAbsent, nil
}

func (*keepPoolRunner) CanManageDrives(context.Context) error { return nil }

// A queued keep_work_folder run promoted the moment its row commits, before the
// rest of its answer, finds its drive: the drive commits with the queue row, so
// the start mounts it rather than failing the run as drive_not_found.
func TestPG_QueuedKeepRunPromotedAtOnceFindsItsDrive(t *testing.T) {
	e := newQueueEnv(t)
	ctx := context.Background()
	e.srv.cfg.RunnerTarget = "docker"
	e.srv.cfg.Runner = &keepPoolRunner{e.runner}
	site, err := e.pg.GetSiteConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	site.WorkspaceProviders = keepSite(true, true, 3, 0).WorkspaceProviders
	if _, err := e.pg.PutSiteConfig(ctx, site); err != nil {
		t.Fatal(err)
	}
	pool := e.cappedPool("Shared", 1)
	occupant := e.occupy(pool)

	var promoted types.AgentRun
	prev := queuedRunCommitted
	t.Cleanup(func() { queuedRunCommitted = prev })
	queuedRunCommitted = func(_ context.Context, id uuid.UUID) {
		e.free(occupant)
		e.sweep()
		e.eventually("the promoted run to start or fail", func() bool {
			r := e.run(id)
			return r.State == types.RunFailed || r.AdmittedAt != nil && r.SPIFFEID != "" && r.State != types.RunPending && r.State != types.RunQueued
		})
		promoted = e.run(id)
	}
	if _, w := e.launch(e.person, e.reread(pool), `,"keep_work_folder":true`); w.Code != 201 {
		t.Fatalf("a queued keep launch = %d %s, want 201", w.Code, w.Body)
	}
	if promoted.State == types.RunFailed {
		t.Fatalf("promoted in the window, the run failed: %s", promoted.StatusDetail)
	}
	if _, err := e.pg.GetMemberDriveOf(ctx, types.CanonicalUserSubject(promoted.CreatedBy), keptWorkFolderDriveID(promoted.ID)); err != nil {
		t.Errorf("the kept drive: %v", err)
	}
	var created map[string]any
	for _, ev := range e.audits(promoted.ID, "run.create") {
		if ev.Outcome == "success" {
			created = queueAuditData(t, ev)
		}
	}
	if created["keep_work_folder"] != true || created["kept_drive_id"] != keptWorkFolderDriveID(promoted.ID).String() {
		t.Errorf("run.create success = %v, want keep_work_folder true and kept_drive_id %s", created, keptWorkFolderDriveID(promoted.ID))
	}
	spec := e.specFor(promoted.ID)
	var kept bool
	for _, d := range spec.Drives {
		kept = kept || d.Target == runner.ScratchWorkPath
	}
	if !kept {
		t.Errorf("dispatched drives %+v, want one at %s", spec.Drives, runner.ScratchWorkPath)
	}
}
