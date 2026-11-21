// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Keeping a run's work folder: keep_work_folder makes the run's person a member
// drive at create and mounts it at the work folder instead of scratch, so the
// folder outlives the run as one of their own drives. It meets every door
// POST /me/drives meets, plus storage.user_drive.member_create.keep_main_drive,
// and the store's own count cap. Only the work folder is kept: home, agent
// settings and /tmp stay the sandbox's. Nothing happens at run end: no
// teardown reclaims a drive's volume or claim, only a drive delete does. The one
// exception is a run that ends without ever starting: nothing was mounted by it, so
// dropUnstartedKeptDrive deletes its drive, through the delete a person's own drive takes, and frees the person's count.
// A macOS VM run its Mac refused when it started is one (dropUncreatedMacKeptDrive).
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"
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

// dropUnstartedKeptDrive deletes the drive run kept its work folder in, for a run that ended without ever
// starting (cancelled or timed out in the queue, aborted by the component fence, or aborted after its
// insert on the direct path). The drive was made with the run and nothing was mounted by it, so it would
// only count against the person's max_per_person. It goes through the same delete as the person's own:
// the owner-starting check, then the substrate's reclaim. The drive is a member drive like any other and
// the owner's other runs can attach it, so a drive one of them holds is left in place, and the refusal is
// audited by that delete as it audits its own, never as an already-absent object. A run that has no kept
// drive, a drive some other run kept, and a drive another delete holds are left alone.
//
// A run that left the queue owes the reclaim in its own transition (store.KeptDriveReclaimStore), so this
// is the first attempt and sweepKeptDriveReclaims the retry: a failure here, or a process that stops
// before it, leaves the drive for the sweep. A drive that is held stays and its debt is cleared.
func (s *Server) dropUnstartedKeptDrive(ctx context.Context, runID uuid.UUID) {
	ms, ok := s.cfg.Store.(store.MemberDriveStore)
	if !ok {
		return
	}
	ctx = context.WithoutCancel(ctx)
	id := keptWorkFolderDriveID(runID)
	d, err := ms.GetMemberDrive(ctx, id)
	if err != nil || d.KeptFromRun == nil || *d.KeptFromRun != runID {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			slog.WarnContext(ctx, "api: read the kept drive of a run that never started", slog.String("run_id", runID.String()), slog.Any("err", err))
		}
		return
	}
	s.settleKeptDriveReclaim(ctx, d, s.reclaimUnstartedKeptDrive(ctx, ms, d))
}

// keptDriveReclaimRetry is how long a failed reclaim waits for its next attempt.
const keptDriveReclaimRetry = 10 * time.Minute

// keptDriveReclaimBatch bounds one sweep pass.
const keptDriveReclaimBatch = 50

// keptDriveVerdict is how one reclaim attempt of a never-started run's kept drive ended.
type keptDriveVerdict int

const (
	keptDriveGone  keptDriveVerdict = iota // the drive is deleted, or already was
	keptDriveHeld                          // the drive stays: another run holds or may have it, or its Mac finishes the delete
	keptDriveRetry                         // not done: a read or the substrate failed
)

// reclaimUnstartedKeptDrive is one attempt at deleting d, the drive a run that never started kept, through the
// delete a person's own takes: the owner-starting check, the tombstone, the substrate's reclaim.
func (s *Server) reclaimUnstartedKeptDrive(ctx context.Context, ms store.MemberDriveStore, d types.UserDrive) keptDriveVerdict {
	runID := d.ID.String()
	if d.KeptFromRun != nil {
		runID = d.KeptFromRun.String()
	}
	// A run of the owner's that is starting may attach the drive, and once one has the drive is hers to keep, so
	// the refusal is final, not a wait.
	if refused, serr := s.memberDriveOwnerStarting(ctx, ms, d, memberDriveByRunEnd); serr != nil || refused {
		if serr != nil {
			slog.WarnContext(ctx, "api: read the owner's starting runs for a kept drive", slog.String("run_id", runID), slog.Any("err", serr))
			return keptDriveRetry
		}
		return keptDriveHeld
	}
	d, started, err := ms.BeginMemberDriveDeleteAdmin(ctx, d.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return keptDriveGone
	case errors.Is(err, store.ErrMemberDriveBusy):
		return keptDriveRetry
	case err != nil:
		slog.WarnContext(ctx, "api: delete the kept drive of a run that never started", slog.String("run_id", runID), slog.Any("err", err))
		return keptDriveRetry
	}
	res := s.deleteMemberDrive(ctx, ms, d, started, memberDriveByRunEnd)
	switch {
	case res.err != nil:
		slog.WarnContext(ctx, "api: finish the delete of the kept drive of a run that never started", slog.String("run_id", runID), slog.Any("err", res.err))
		return keptDriveRetry
	case res.reason == "" || res.reason == reasonMemberDriveRecordOnly:
		return keptDriveGone
	case res.reason == reasonMemberDriveInUse || res.reason == reasonMemberDriveDeleteDeferred:
		return keptDriveHeld
	}
	return keptDriveRetry
}

// settleKeptDriveReclaim records how an attempt ended: a deleted drive's debt went with its row, a held one is
// cleared, and one to retry is deferred, so a drive the substrate keeps refusing is tried and audited every
// keptDriveReclaimRetry and not on every sweep.
func (s *Server) settleKeptDriveReclaim(ctx context.Context, d types.UserDrive, v keptDriveVerdict) {
	rs, ok := s.cfg.Store.(store.KeptDriveReclaimStore)
	if !ok || v == keptDriveGone {
		return
	}
	var err error
	if v == keptDriveHeld {
		err = rs.ClearKeptDriveReclaim(ctx, d.ID)
	} else {
		err = rs.DeferKeptDriveReclaim(ctx, d.ID, keptDriveReclaimRetry)
	}
	if err != nil {
		slog.WarnContext(ctx, "api: record the outcome of a kept drive reclaim", slog.String("drive_id", d.ID.String()), slog.Any("err", err))
	}
}

// sweepKeptDriveReclaims retries the reclaim of each kept drive whose run left the queue without starting and
// whose first attempt did not finish it, against the same owner-starting and in-use checks as the first.
// A drive whose run is not known to have ended is left alone for a later pass: the debt is written only with
// that run's end, so this is a guard, not a path.
func (s *Server) sweepKeptDriveReclaims(ctx context.Context) error {
	rs, ok := s.cfg.Store.(store.KeptDriveReclaimStore)
	ms, mok := s.cfg.Store.(store.MemberDriveStore)
	if !ok || !mok {
		return nil
	}
	due, err := rs.ListDueKeptDriveReclaims(ctx, keptDriveReclaimBatch)
	if err != nil {
		return err
	}
	ctx = context.WithoutCancel(ctx)
	for _, d := range due {
		if d.KeptFromRun == nil || !s.keptRunEnded(ctx, *d.KeptFromRun) {
			s.settleKeptDriveReclaim(ctx, d, keptDriveRetry)
			continue
		}
		// A drive offered to the owner's other runs once its run ended, so a retry made after one of them was
		// created may find a drive that run wrote to: that is the owner's now, and stays.
		attached, aerr := rs.KeptDriveMayBeAttached(ctx, d.ID)
		switch {
		case aerr != nil:
			slog.WarnContext(ctx, "api: read whether a kept drive may have been attached", slog.String("drive_id", d.ID.String()), slog.Any("err", aerr))
			s.settleKeptDriveReclaim(ctx, d, keptDriveRetry)
		case attached:
			s.settleKeptDriveReclaim(ctx, d, keptDriveHeld)
		default:
			s.settleKeptDriveReclaim(ctx, d, s.reclaimUnstartedKeptDrive(ctx, ms, d))
		}
	}
	return nil
}

// dropUncreatedMacKeptDrive, deferred by a dispatch so it runs after the dispatch's lease is released, is
// dropUnstartedKeptDrive for a macOS VM run the dispatch created no sandbox for and that has ended: a run
// its Mac refused when it started (#3900). Until the lease is gone the drop would count the run itself as
// one still starting, and leave the drive.
func (s *Server) dropUncreatedMacKeptDrive(ctx context.Context, run types.AgentRun, created *bool) {
	if *created || !run.MachineKind.IsMacOSVM() || !s.keptRunEnded(ctx, run.ID) {
		return
	}
	s.dropUnstartedKeptDrive(ctx, run.ID)
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
	if err != nil || d.DeleteStartedAt != nil {
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
			run.ID.String(), "failure", mustJSON(map[string]any{"reason": string(authz.ReasonDriveNotFound)})))
		s.failAndRevoke(ctx, run.ID, run.State, driveNotFoundMsg(id.String()))
		return types.DriveMount{}, id, owner, false
	}
	mount.Target, mount.SizeMiB, mount.Enforcement = runner.ScratchWorkPath, d.SizeMiB, types.EnforcementFor(d.Backend)
	return mount, id, owner, true
}
