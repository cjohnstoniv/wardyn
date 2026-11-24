// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Keeping a run's work folder: keep_work_folder makes the run's person a member
// drive at create and mounts it at the work folder instead of scratch, so the
// folder outlives the run as one of their own drives. It meets every door
// POST /me/drives meets, plus storage.user_drive.member_create.keep_main_drive,
// and the store's own count cap. Only the work folder is kept: home, agent
// settings and /tmp stay the sandbox's. Nothing happens at run end: no
// teardown reclaims a drive's volume or claim, only a drive delete does. The one
// exception is a run that ends without ever starting: nothing was mounted by it, so the statement that ends it
// claims its drive and finishQueuedKeptDrive deletes it, through the delete a person's own drive takes, and frees
// the person's count. A macOS VM run that ended before its Mac was asked to create the sandbox is one
// (dropUncreatedMacKeptDrive); one whose create was asked and failed keeps its drive.
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// keepWorkFolderOneMacMsg refuses keeping a macOS VM's work folder in a pool of several Macs: the
// folder is a disk on the Mac the run used (MAC-D07).
func keepWorkFolderOneMacMsg() string {
	return "The kept work folder stays on the Mac the run used, so keeping it needs a pool with one Mac. Choose one, or don't keep the folder."
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

// onMac reports a macOS VM's kept work folder: a disk on the Mac the run uses.
func (k *keptWorkFolder) onMac() bool { return k != nil && k.row.Backend == types.DriveBackendMacDisk }

// keepWorkFolderGates runs keep_work_folder's doors for a request asking for
// it, in order: a person launching for themselves, member drives and the keep
// switch on, create's own doors (profile, backend, deletable), the runner can
// mount it, nothing else mounts over the work folder, and the person is under
// their drive limit. The store counts again at the insert. promoted skips the
// count: the queue's start replays this over the drive its create made. mac
// keeps a macOS VM's work folder as a disk on the Mac the run is placed on
// (stepDriveEligibility names it), which that Mac's runner mounts and deletes.
func (s *Server) keepWorkFolderGates(r *http.Request, spec types.RunPolicySpec, promoted, mac bool) (*keptWorkFolder, *runRefusal) {
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
	row, policy, refusal := s.memberDriveCreateDoors(r, mac)
	if refusal != nil {
		return nil, refusal
	}
	if !mac {
		if f := s.driveBindFailureHere(ctx, types.ResolvedDrive{Drive: row}, drivePlace{}); f != nil {
			return nil, &runRefusal{drive: f}
		}
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

// keepWorkFolderOrAbort is keepWorkFolder for create: a refusal aborts the
// half-built run and is written, and false tells the caller to stop.
func (s *Server) keepWorkFolderOrAbort(w http.ResponseWriter, r *http.Request, k *keptWorkFolder, run types.AgentRun, abort func(string)) bool {
	refusal := s.keepWorkFolder(r, k, run)
	if refusal == nil {
		return true
	}
	abort(createRunKeepAbortHint)
	refusal.write(s, w, r)
	return false
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
	target := runner.ScratchWorkPath
	if run.MachineKind.IsMacOSVM() {
		target = runner.MacGuestPath(target)
	}
	data := map[string]any{
		"backend": saved.Backend, "drive": saved.Name, "drive_id": saved.ID, "limit": k.limit,
		"size_mib": saved.SizeMiB, "storage_class": saved.StorageClass,
		"from_run": run.ID, "target": target, "enforcement": types.EnforcementFor(saved.Backend),
	}
	if ms, ok := s.cfg.Store.(store.MemberDriveStore); ok {
		if all, lerr := ms.ListMemberDrivesOf(r.Context(), k.owner); lerr == nil {
			data["count"] = len(all)
		}
	}
	s.recordAudit(r.Context(), s.auditEvent(&run.ID, actorTypeFromRequest(r), principalFromRequest(r),
		"drive.member.create", saved.ID.String(), "success", mustJSON(data)))
}

// memberDriveByRunEnd is the "by" of the drive.member.delete row of a kept drive whose run never started.
const memberDriveByRunEnd = "run_never_started"

// keptDriveOfRun is the drive run kept its work folder in, false when it has none or the drive is some other run's.
func (s *Server) keptDriveOfRun(ctx context.Context, ms store.MemberDriveStore, runID uuid.UUID) (types.UserDrive, bool) {
	d, err := ms.GetMemberDrive(ctx, keptWorkFolderDriveID(runID))
	if err != nil || d.KeptFromRun == nil || *d.KeptFromRun != runID {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			slog.WarnContext(ctx, "api: read the kept drive of a run that never started", slog.String("run_id", runID.String()), slog.Any("err", err))
		}
		return types.UserDrive{}, false
	}
	return d, true
}

// finishQueuedKeptDrive is the cleanup of the drive a run kept when it ended without starting: it left the queue
// (cancelled, timed out, aborted by the component fence, ended by its pool's removal), or was ended before any
// sandbox was asked for (the pending abort, a macOS VM dispatch's returns). It can run long after the run ended: a
// pool removal ends every waiting run at once and cleans them up one at a time. The statement that ended the run
// took the claim on the drive in the same statement (store.keptDriveHoldSQL, EndUnstartedRun), and the claim is
// the run's: it carries the instant the run ended, which no other delete of the drive can, and the issue count
// the drive had, which must be even (nothing issued). Only that initial claim is finished, past the
// owner-starting check deleteMemberDrive makes, and a drive it keeps has its claim cleared, which is when the drive
// is first offered to the owner's other runs. After the first issue the count is odd and a second call finds
// nothing to act on: only an owner's, an administrator's or the sweep's takeover issues again.
//
// A drive with no such claim is kept: the statement found the owner's own delete already holding the drive and
// left it, or no claim was taken (a failed create, a run that ran). When the owner's delete fails and clears its
// claim the drive is the owner's, and a later run may have attached it and written to it. The owner's request
// is what deletes it, or keeps it.
func (s *Server) finishQueuedKeptDrive(ctx context.Context, runID uuid.UUID) {
	ms, ok := s.cfg.Store.(store.MemberDriveStore)
	if !ok {
		return
	}
	ctx = context.WithoutCancel(ctx)
	d, ok := s.keptDriveOfRun(ctx, ms, runID)
	if !ok || d.DeleteStartedAt == nil || d.DeleteIssues%2 != 0 {
		return
	}
	run, err := s.cfg.Store.GetRun(ctx, runID)
	if err != nil {
		slog.WarnContext(ctx, "api: read the run a kept drive was kept from", slog.String("run_id", runID.String()), slog.Any("err", err))
		return
	}
	if run.EndedAt == nil || !d.DeleteStartedAt.Equal(*run.EndedAt) {
		return
	}
	if res := s.deleteMemberDrive(ctx, ms, d, *d.DeleteStartedAt, memberDriveByRunEnd); res.err != nil {
		slog.WarnContext(ctx, "api: finish the delete of the kept drive of a run that never started", slog.String("run_id", runID.String()), slog.Any("err", res.err))
	}
}

// dropUncreatedMacKeptDrive, deferred by a dispatch so it runs after the dispatch's lease is released, finishes
// the claim a macOS VM run's end took on its kept drive when the dispatch created no sandbox: a run its Mac
// refused before it was asked, or that failed first (#3900's cases that left no disk). Until the lease is gone the
// delete would count the run itself as one still starting, and leave the drive. A run whose create was asked and
// failed took no claim, and its drive stays.
func (s *Server) dropUncreatedMacKeptDrive(ctx context.Context, run types.AgentRun, created *bool) {
	if *created || !run.MachineKind.IsMacOSVM() || !s.keptRunEnded(ctx, run.ID) {
		return
	}
	s.finishQueuedKeptDrive(ctx, run.ID)
}

// keptRunEnded reports whether runID, the run a kept drive was kept from, has ended or is gone. Until then
// the drive is that run's alone: another run cannot attach it (reachableDrive.held). A run that cannot
// be read counts as not ended, so the drive stays out of reach rather than into it.
func (s *Server) keptRunEnded(ctx context.Context, runID uuid.UUID) bool {
	run, err := s.cfg.Store.GetRun(ctx, runID)
	if errors.Is(err, store.ErrNotFound) {
		return true
	}
	if err != nil {
		slog.WarnContext(ctx, "api: read the run a kept drive was kept from", slog.String("run_id", runID.String()), slog.Any("err", err))
		return false
	}
	return run.State.IsTerminal()
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
	if err != nil || d.Deleting() {
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
			run.ID.String(), "failure", mustJSON(map[string]any{"reason": string(authz.ReasonDriveNotFound)})))
		s.failAndRevoke(ctx, run.ID, run.State, driveNotFoundMsg(id.String()))
		return types.DriveMount{}, id, owner, false
	}
	mount.Target, mount.SizeMiB, mount.Enforcement = runner.ScratchWorkPath, d.SizeMiB, types.EnforcementFor(d.Backend)
	return mount, id, owner, true
}
