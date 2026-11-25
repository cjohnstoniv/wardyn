// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A move to Kubernetes keeps the run's work folder on a new drive of the run's person. The work folder is always
// carried into storage the move creates, never into a drive that existed before it. The drive meets every door
// keep_work_folder's drive meets, at admission and in the mover's request. Its row is made when the move runs, named
// after the run and the move and marked with the run (KeptFromRun). A failed move deletes it through the delete a
// person's own drive takes. A committed move leaves it the run's work drive, and the person's drive once the run ends.

// moveWorkDriveNS seeds moveWorkDriveID.
var moveWorkDriveNS = uuid.MustParse("8c3e2f61-4d0a-4b7e-9f25-6a1d3c9e5b70")

// moveWorkDriveID is the work drive a move makes, derived from the run and the move, so a failed move's cleanup and
// the move sweep find the row the move made.
func moveWorkDriveID(runID, moveID uuid.UUID) uuid.UUID {
	return uuid.NewSHA1(moveWorkDriveNS, append(runID[:], moveID[:]...))
}

// memberDriveByMoveFailed is the "by" of the drive.member.delete row of a failed move's work drive.
const memberDriveByMoveFailed = "move_failed"

// moveDestKubernetes reports a destination that runs the run on Kubernetes: a runner whose caps name the k8s driver,
// or the organisation's executors on a deployment that dispatches to Kubernetes.
func (s *Server) moveDestKubernetes(d moveDest, caps runner.Capabilities) bool {
	if d.local() {
		return caps.Driver == "k8s"
	}
	return s.cfg.RunnerTarget == "k8s"
}

// moveWorkDriveRefused is a move refused because its new work drive cannot be made. layer is the door's own reason.
func moveWorkDriveRefused(status int, layer, msg string) *moveRefusal {
	return &moveRefusal{status: status, reason: reasonMoveWorkDriveRefused, layer: layer,
		msg: "The destination keeps the run's work folder on a new drive of yours, and it cannot be made: " + msg}
}

// moveWorkDriveGates runs keep_work_folder's doors (moveWorkDriveDoors) for a destination that keeps the work folder
// on a new drive (plan.WorkDrive), over the run's stored policy. It answers nil, nil, nil when the destination needs
// no new drive, and an error for a read a door could not make.
func (s *Server) moveWorkDriveGates(ctx context.Context, run types.AgentRun, d moveDest, plan moveDestPlan) (*keptWorkFolder, *moveRefusal, error) {
	if !plan.WorkDrive {
		return nil, nil, nil
	}
	spec, err := s.moveStoredSpec(ctx, run.ID)
	if err != nil {
		return nil, nil, err
	}
	return s.moveWorkDriveDoors(ctx, run, d, spec.ProxyConfig.Policy)
}

// moveWorkDriveDoors runs, in the mover's request ctx: the mover is the run's person, then every door
// keepWorkFolderGates runs over policy, the drive is a cluster claim, and a runner destination can mount it.
func (s *Server) moveWorkDriveDoors(ctx context.Context, run types.AgentRun, d moveDest, policy types.RunPolicySpec) (*keptWorkFolder, *moveRefusal, error) {
	if human := types.CanonicalUserSubject(oidcHumanFromContext(ctx)); human == "" || human != types.CanonicalUserSubject(run.CreatedBy) {
		return nil, moveWorkDriveRefused(http.StatusForbidden, reasonMemberDriveRequiresPerson, memberDriveRequiresPersonMsg()), nil
	}
	// The doors read only the request's context, as every caller of a door without a request does.
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, "/", nil)
	if err != nil {
		return nil, nil, err
	}
	k, f := s.keepWorkFolderGates(r, policy, false, false)
	if f == nil && k.row.Backend != types.DriveBackendK8sPVC {
		return nil, moveWorkDriveRefused(http.StatusUnprocessableEntity, reasonMemberDriveUnsupported, moveWorkDriveNoClaimMsg()), nil
	}
	if f == nil && d.local() {
		if bf := s.driveBindFailureHere(ctx, types.ResolvedDrive{Drive: k.row}, drivePlace{runner: d.RunnerID}); bf != nil {
			f = &runRefusal{drive: bf}
		}
	}
	if f == nil {
		return k, nil, nil
	}
	ref, err := moveDoorRefusal(f)
	return nil, ref, err
}

// moveWorkDriveNoClaimMsg refuses a Kubernetes destination on a deployment whose own drives are not cluster claims.
func moveWorkDriveNoClaimMsg() string {
	return "this deployment does not make drives on Kubernetes."
}

// moveDoorRefusal is a keep_work_folder door's refusal as the move's: the door's reason is the layer and its sentence
// the message. A door that could not read what it needs is an error.
func moveDoorRefusal(f *runRefusal) (*moveRefusal, error) {
	switch {
	case f.err != nil:
		return nil, fmt.Errorf("%s: %w", f.operation, f.err)
	case f.drive != nil && f.drive.reason == "":
		return nil, errors.New(f.drive.member)
	case f.drive != nil:
		return moveWorkDriveRefused(f.drive.status, f.drive.reason, f.drive.member), nil
	}
	status := f.status
	if status == 0 {
		status = http.StatusForbidden
	}
	return moveWorkDriveRefused(status, f.body.Reason, f.body.Error), nil
}

// moveExcludeReattached leaves every drive the destination re-attaches out of set: its storage is the same at the
// destination, so nothing of it is carried or written.
func moveExcludeReattached(set runner.CarriedSet, drives []types.DriveMount) runner.CarriedSet {
	set.Exclude = slices.Clone(set.Exclude)
	for _, dr := range drives {
		set.Exclude = append(set.Exclude, strings.TrimPrefix(dr.Target, "/"))
	}
	return set
}

// moveSourceWorkMiB is the size of the drive the source keeps its work folder on, 0 when it has none.
func moveSourceWorkMiB(rec runSandboxSpecRecord) int {
	for _, dr := range rec.Drives {
		if dr.Target == runner.ScratchWorkPath {
			return dr.SizeMiB
		}
	}
	return 0
}

// makeWorkDrive makes the move's work drive (step 5, before the destination's spec is built) from the row its doors
// admitted: its id from the run and the move, owned by the run's person and counted at their limit again, marked with
// the run, and at least as large as the source's work drive, else the switch's size. Its mount, at the work folder,
// goes into the destination's spec. A move with no new drive does nothing.
func (x *moveExec) makeWorkDrive(ctx context.Context) *moveEnd {
	if x.work == nil {
		return nil
	}
	ms, ok := x.s.cfg.Store.(store.MemberDriveStore)
	if !ok {
		return failMove(reasonMoveWorkDriveRefused, "The destination's work drive could not be made: "+memberDriveUnsupportedMsg())
	}
	d := x.work.row
	d.ID, d.OwnerSubject, d.CreatedBy = moveWorkDriveID(x.run.ID, x.mv.ID), x.work.owner, x.principal
	d.Name = keptDriveName(x.run, " "+x.run.ID.String()[:8]+" moved "+x.mv.ID.String()[:8])
	runID := x.run.ID
	d.KeptFromRun = &runID
	d.SizeMiB = max(d.SizeMiB, moveSourceWorkMiB(x.rec))
	x.workMade = true
	saved, err := ms.CreateMemberDrive(ctx, d, x.work.limit)
	if err != nil {
		msg := "the store did not take it."
		if f := keptDriveRefusal(err, x.work, d); f != nil {
			msg = f.body.Error
		}
		return failMove(reasonMoveWorkDriveRefused, "The destination's work drive could not be made: "+msg)
	}
	mount, err := memberDriveMount(saved)
	if err != nil {
		return failMove(reasonMoveWorkDriveRefused, "The destination's work drive could not be mounted: "+err.Error())
	}
	mount.Target, mount.SizeMiB, mount.Enforcement = runner.ScratchWorkPath, saved.SizeMiB, types.EnforcementFor(saved.Backend)
	x.workMount = &mount
	x.s.recordAudit(ctx, x.s.auditEvent(&x.run.ID, x.actorType, x.principal, "drive.member.create", saved.ID.String(), "success",
		mustJSON(map[string]any{
			"backend": saved.Backend, "drive": saved.Name, "drive_id": saved.ID, "limit": x.work.limit,
			"size_mib": saved.SizeMiB, "storage_class": saved.StorageClass,
			"from_run": x.run.ID, "target": runner.ScratchWorkPath, "enforcement": types.EnforcementFor(saved.Backend),
		})))
	return nil
}

// dropMoveWorkDrive deletes the work drive a failed move made, through the delete a person's own drive takes: the
// owner-starting check, the tombstone, then deleteMemberDrive. True once the drive is gone, or there is none; false
// leaves it for the move sweep's next pass. A drive whose delete another caller holds is that caller's to finish.
func (s *Server) dropMoveWorkDrive(ctx context.Context, runID, moveID uuid.UUID) bool {
	ms, ok := s.cfg.Store.(store.MemberDriveStore)
	if !ok {
		return true
	}
	ctx = context.WithoutCancel(ctx)
	id := moveWorkDriveID(runID, moveID)
	d, err := ms.GetMemberDrive(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return true
	case err != nil:
		slog.WarnContext(ctx, "api: read a failed move's work drive", slog.String("move_id", moveID.String()), slog.Any("err", err))
		return false
	case d.KeptFromRun == nil || *d.KeptFromRun != runID:
		return true
	}
	if refused, err := s.memberDriveOwnerStarting(ctx, ms, d, memberDriveByMoveFailed); err != nil || refused {
		return false
	}
	d, started, err := ms.BeginMemberDriveDelete(ctx, d.OwnerSubject, id)
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrMemberDriveBusy):
		return true
	case err != nil:
		slog.WarnContext(ctx, "api: begin the delete of a failed move's work drive", slog.String("move_id", moveID.String()), slog.Any("err", err))
		return false
	}
	res := s.deleteMemberDrive(ctx, ms, d, started, memberDriveByMoveFailed)
	return res.err == nil && (res.reason == "" || res.reason == reasonMemberDriveRecordOnly)
}
