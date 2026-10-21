// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Keeping a run's work folder: keep_work_folder makes the run's person a member
// drive at create and mounts it at the work folder instead of scratch, so the
// folder outlives the run as one of their own drives. It meets every door
// POST /me/drives meets, plus storage.user_drive.member_create.keep_main_drive,
// and the store's own count cap. Only the work folder is kept: home, agent
// settings and /tmp stay the sandbox's. Nothing happens at run end: no
// teardown reclaims a drive's volume or claim, only a drive delete does.
package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func keepWorkFolderDisabledMsg() string {
	return "Your organisation doesn't let runs keep their work folder as a drive. Ask an admin."
}

func keepWorkFolderLimitReachedMsg(n, limit int) string {
	return fmt.Sprintf("You have %d of %d drives. Delete one to keep this run's work folder.", n, limit)
}

func keepWorkFolderTargetTakenMsg(target string) string {
	return fmt.Sprintf("keep_work_folder: %s mounts over the work folder, so the folder can't be kept. Mount it somewhere else.", target)
}

// keptWorkFolderNS seeds keptWorkFolderDriveID.
var keptWorkFolderNS = uuid.MustParse("5d0f6f3e-8f3b-4f38-9a8e-2b6c1f4a7e10")

// keptWorkFolderDriveID is the drive a run keeps its work folder in, derived
// from the run id so the queue's start and dispatch find the row create made.
func keptWorkFolderDriveID(runID uuid.UUID) uuid.UUID {
	return uuid.NewSHA1(keptWorkFolderNS, runID[:])
}

// keptWorkFolder is what the fold decided for keep_work_folder: the row create
// inserts once the run id exists, its owner, and the switch's limit.
type keptWorkFolder struct {
	row   types.UserDrive
	owner string
	limit int
}

// keepWorkFolderGates runs keep_work_folder's doors for a request asking for
// it, in order: a person launching for themselves, member drives and the keep
// switch on, create's own doors (profile, backend, deletable), the runner can
// mount it, nothing else mounts over the work folder, and the person is under
// their drive limit. The store counts again at the insert. promoted skips the
// count: the queue's start replays this over the drive its create made.
func (s *Server) keepWorkFolderGates(r *http.Request, spec types.RunPolicySpec, promoted bool) (*keptWorkFolder, *runRefusal) {
	ctx := r.Context()
	owner := types.CanonicalUserSubject(oidcHumanFromContext(ctx))
	if _, actor := actorFromContext(ctx, ""); owner == "" || neverOperator(ctx) || types.CanonicalUserSubject(actor) != owner {
		return nil, runError(http.StatusForbidden, reasonMemberDriveRequiresPerson, memberDriveRequiresPersonMsg())
	}
	provider, err := s.userDriveProvider(ctx)
	if err != nil {
		return nil, runServerError("get site config", err)
	}
	if provider.Disabled || provider.MemberCreate == nil || !provider.MemberCreate.Enabled || !provider.MemberCreate.KeepMainDrive {
		return nil, runError(http.StatusUnprocessableEntity, reasonKeepWorkFolderDisabled, keepWorkFolderDisabledMsg())
	}
	row, policy, refusal := s.memberDriveCreateDoors(r)
	if refusal != nil {
		return nil, refusal
	}
	if f := s.driveBindFailureHere(ctx, types.ResolvedDrive{Drive: row}); f != nil {
		return nil, &runRefusal{drive: f}
	}
	for _, m := range spec.WorkspaceMounts {
		if t := path.Clean(m.Target); t == runner.ScratchWorkPath || strings.HasPrefix(runner.ScratchWorkPath, t+"/") {
			return nil, runError(http.StatusUnprocessableEntity, reasonWorkspaceTargetOverlap, keepWorkFolderTargetTakenMsg(t))
		}
	}
	ms, ok := s.cfg.Store.(store.MemberDriveStore)
	if !ok {
		return nil, runError(http.StatusUnprocessableEntity, reasonMemberDriveUnsupported, memberDriveUnsupportedMsg())
	}
	k := &keptWorkFolder{row: row, owner: owner, limit: policy.MaxPerPerson}
	if promoted {
		return k, nil
	}
	mine, err := ms.ListMemberDrivesOf(ctx, owner)
	if err != nil {
		return nil, runServerError("list member drives", err)
	}
	if len(mine) >= policy.MaxPerPerson {
		return nil, runError(http.StatusUnprocessableEntity, reasonMemberDriveLimitReached, keepWorkFolderLimitReachedMsg(len(mine), policy.MaxPerPerson))
	}
	return k, nil
}

// keptWorkFolderName names the drive after the run: its title, else "Run",
// then the run id's first block, so two runs of one title get two drives.
func keptWorkFolderName(run types.AgentRun) string {
	suffix := " " + run.ID.String()[:8]
	title := strings.TrimSpace(run.Title)
	if types.ValidateDriveName(title) != nil {
		title = "Run"
	}
	for len(title)+len(suffix) > 128 { // the drive name rule's byte limit
		_, size := utf8.DecodeLastRuneInString(title)
		title = title[:len(title)-size]
	}
	return strings.TrimSpace(title) + suffix
}

// keptDrive is the row keep_work_folder makes for run: the fold's row, owned by
// the run's person, named after the run and marked with it.
func keptDrive(r *http.Request, k *keptWorkFolder, run types.AgentRun) types.UserDrive {
	d := k.row
	d.ID, d.Name, d.OwnerSubject, d.CreatedBy = keptWorkFolderDriveID(run.ID), keptWorkFolderName(run), k.owner, principalFromRequest(r)
	d.KeptFromRun = &run.ID
	return d
}

// keptDriveRefusal is the answer to the store refusing drive d at its insert:
// the count cap or the name, raced past the fold. nil for any other error.
func keptDriveRefusal(err error, k *keptWorkFolder, d types.UserDrive) *runRefusal {
	switch {
	case errors.Is(err, store.ErrMemberDriveLimit):
		return runError(http.StatusUnprocessableEntity, reasonMemberDriveLimitReached, keepWorkFolderLimitReachedMsg(k.limit, k.limit))
	case errors.Is(err, store.ErrMemberDriveNameTaken):
		return runError(http.StatusConflict, reasonMemberDriveNameTaken, memberDriveNameTakenMsg(d.Name))
	}
	return nil
}

// keepWorkFolder inserts the drive the fold admitted, owned by the run's
// person and counted at their limit, once the run row exists. A run that asked
// for nothing is a no-op. A queued run's drive is inserted with its queue row
// instead (queuedCreate.answer), so no promotion finds the run without it.
func (s *Server) keepWorkFolder(r *http.Request, k *keptWorkFolder, run types.AgentRun) *runRefusal {
	if k == nil {
		return nil
	}
	ms, ok := s.cfg.Store.(store.MemberDriveStore)
	if !ok {
		return runError(http.StatusUnprocessableEntity, reasonMemberDriveUnsupported, memberDriveUnsupportedMsg())
	}
	d := keptDrive(r, k, run)
	saved, err := ms.CreateMemberDrive(r.Context(), d, k.limit)
	if err != nil {
		if refusal := keptDriveRefusal(err, k, d); refusal != nil {
			return refusal
		}
		return runServerError("keep the work folder", err)
	}
	s.auditKeptDrive(r, k, run, saved)
	return nil
}

// auditKeptDrive is the drive.member.create row of a kept work folder.
func (s *Server) auditKeptDrive(r *http.Request, k *keptWorkFolder, run types.AgentRun, saved types.UserDrive) {
	data := map[string]any{
		"backend": saved.Backend, "drive": saved.Name, "drive_id": saved.ID, "limit": k.limit,
		"size_mib": saved.SizeMiB, "storage_class": saved.StorageClass,
		"from_run": run.ID, "target": runner.ScratchWorkPath, "enforcement": types.EnforcementFor(saved.Backend),
	}
	if ms, ok := s.cfg.Store.(store.MemberDriveStore); ok {
		if all, lerr := ms.ListMemberDrivesOf(r.Context(), k.owner); lerr == nil {
			data["count"] = len(all)
		}
	}
	s.recordAudit(r.Context(), s.auditEvent(&run.ID, actorTypeFromRequest(r), principalFromRequest(r),
		"drive.member.create", saved.ID.String(), "success", mustJSON(data)))
}

// keptWorkFolderMount is the mount of the drive run keeps its work folder in,
// read under the run's person, and that drive's id and owner for dispatch's
// re-read (memberDrivesStillThere). A drive that cannot be read fails the run
// as drive_not_found: the run never starts without the folder it asked to keep.
func (s *Server) keptWorkFolderMount(ctx context.Context, run types.AgentRun) (types.DriveMount, uuid.UUID, string, bool) {
	id, owner := keptWorkFolderDriveID(run.ID), types.CanonicalUserSubject(run.CreatedBy)
	var d types.UserDrive
	err := errors.New("api: the store holds no member drives")
	if ms, ok := s.cfg.Store.(store.MemberDriveStore); ok {
		d, err = ms.GetMemberDriveOf(ctx, owner, id)
	}
	var mount types.DriveMount
	if err == nil {
		mount, err = memberDriveMount(d)
	}
	if err != nil || d.DeleteStartedAt != nil {
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
			run.ID.String(), "failure", mustJSON(map[string]any{"reason": string(authz.ReasonDriveNotFound)})))
		s.failAndRevoke(ctx, run.ID, run.State, driveNotFoundMsg(id.String()))
		return types.DriveMount{}, id, owner, false
	}
	mount.Target, mount.SizeMiB, mount.Enforcement = runner.ScratchWorkPath, d.SizeMiB, types.EnforcementFor(d.Backend)
	return mount, id, owner, true
}
