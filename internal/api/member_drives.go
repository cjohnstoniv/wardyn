// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Member drives: a person creates and deletes their own drive. The row's
// owner_subject is the authority and no grant row is ever written, so no admin
// grant can widen a person's drive and no member drive can win the legacy
// resolver. Deleting destroys the substrate object before the row goes.
package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// One bucket per person for creates and deletes together: 6 a minute, burst 3.
// Each one is a Docker or apiserver write, so a loop must not churn them.
const (
	memberDriveRatePerSec = 6.0 / 60
	memberDriveBurst      = 3
)

// The member-drive sentences. The console uses the same bytes
// (ui/src/app/lib/drive-refusals.ts); TestDriveRefusalSentencesMatchGolden pins
// both sides to ui/src/app/lib/drive-refusals.golden.json.

func memberDriveRequiresPersonMsg() string {
	return "Sign in under your own account to create or delete a drive."
}

func memberDrivesDisabledMsg() string {
	return "Your organisation doesn't let people create drives. Ask an admin."
}

func memberDrivesDeniedByUnitMsg(profile string) string {
	return fmt.Sprintf("Your governance profile %q doesn't let you create drives.", profile)
}

func memberDriveLimitReachedMsg(n, limit int) string {
	return fmt.Sprintf("You have %d of %d drives. Delete one to create another.", n, limit)
}

func memberDriveUnsupportedMsg() string { return "This server can't create drives." }

func memberDriveUnsizedMsg() string {
	return "This server can't create drives until an admin sets their size."
}

func memberDriveDeleteUnavailableCreateMsg() string {
	return "This server can't delete drives, so it can't create them. Ask your admin."
}

func memberDriveDeleteUnavailableMsg() string {
	return "This server can't delete drives right now. Ask your admin."
}

func memberDriveNameInvalidMsg() string {
	return "Give the drive a name with at least one letter or digit, up to 128 characters."
}

func memberDriveNameTakenMsg(name string) string {
	return fmt.Sprintf("%q is already the name of a drive you can use. Pick another name.", name)
}

func memberDriveInUseMsg() string {
	return "A run is still using this drive. End that run, or delete a run kept after its end, then try again."
}

// memberDriveReleasingOnMacMsg is memberDriveInUseMsg for a disk drive: a macOS VM run records no
// sandbox, so the owner's last run counts until its lease is stale (watcherLeaseStaleAfter), up to 90
// seconds after it already shows as ended (#3899).
func memberDriveReleasingOnMacMsg(name string) string {
	return fmt.Sprintf("A run is still using %s, or a macOS VM run that just ended is still releasing it. Try again in two minutes, or end that run first.", name)
}

func memberDriveBusyMsg() string { return "This drive is already being deleted." }

func memberDriveDeleteFailedMsg() string {
	return "Wardyn could not confirm that deletion finished."
}

// memberDriveMacUnavailableMsg refuses a disk drive on a runner that is not one of the caller's own
// Macs keeping disk drives.
func memberDriveMacUnavailableMsg() string {
	return "runner_id: choose one of your own Macs. It must be online, with a runner that is up to date."
}

// memberDriveDeleteDeferredMsg, memberDriveInRunOnMacMsg and memberDriveRecordOnlyMsg are a disk
// drive's delete when its Mac is offline (MAC-D10), runs it now (MAC-D11) or is no longer registered
// (MAC-D12).
func memberDriveDeleteDeferredMsg(mac string) string {
	return fmt.Sprintf("Deleting. %s is offline, so the disk is removed when it reconnects.", mac)
}

func memberDriveInRunOnMacMsg(name, mac string) string {
	return fmt.Sprintf("%s is in a run on %s. Delete it after that run ends.", name, mac)
}

func memberDriveRecordOnlyMsg(mac string) string {
	return fmt.Sprintf("%s is no longer registered, so this drive's disk stays on that Mac. Deleting removes only the record.", mac)
}

func memberDriveRateLimitedMsg() string {
	return "Too many drive changes; wait a minute and try again."
}

// memberDriveOwner is the person a member-drive route acts for: the canonical
// subject of a session or of the person's own API token, never the email. A
// device, a delegated caller, SCIM, a service identity, the admin token and
// local mode have no person here and are refused.
func memberDriveOwner(w http.ResponseWriter, r *http.Request) (string, bool) {
	owner := types.CanonicalUserSubject(oidcHumanFromContext(r.Context()))
	if owner == "" || neverOperator(r.Context()) {
		writeErrorReason(w, http.StatusForbidden, reasonMemberDriveRequiresPerson, memberDriveRequiresPersonMsg())
		return "", false
	}
	return owner, true
}

// refuseMemberDriveRate spends one of owner's tokens, keyed on the proven
// person, so the bucket can't be split across spellings of one identity.
func (s *Server) refuseMemberDriveRate(w http.ResponseWriter, owner string) bool {
	if s.memberDriveLimiter.allow(owner, s.cfg.Now()) {
		return false
	}
	w.Header().Set("Retry-After", "10")
	writeErrorReason(w, http.StatusTooManyRequests, reasonMemberDriveRateLimited, memberDriveRateLimitedMsg())
	return true
}

// handleCreateMemberDrive is POST /me/drives: 201 with the new drive. It
// creates the row only; the first run that mounts the drive creates the volume
// or claim. Never a drive this deployment could not later delete.
func (s *Server) handleCreateMemberDrive(w http.ResponseWriter, r *http.Request) {
	owner, ok := memberDriveOwner(w, r)
	if !ok || s.refuseMemberDriveRate(w, owner) {
		return
	}
	var req client.MemberDriveCreateRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if types.ValidateDriveName(req.Name) != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonMemberDriveNameInvalid, memberDriveNameInvalidMsg())
		return
	}
	ms, ok := s.cfg.Store.(store.MemberDriveStore)
	if !ok {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonMemberDriveUnsupported, memberDriveUnsupportedMsg())
		return
	}
	d, policy, refusal := s.memberDriveCreateGates(r, strings.TrimSpace(req.Name), req.RunnerID != "")
	if refusal.write(s, w, r) {
		return
	}
	if req.RunnerID != "" {
		if d.HomeRunnerID = s.memberDriveMac(r.Context(), owner, req.RunnerID); d.HomeRunnerID == nil {
			writeErrorReason(w, http.StatusUnprocessableEntity, reasonMemberDriveUnsupported, memberDriveMacUnavailableMsg())
			return
		}
	}
	d.OwnerSubject, d.CreatedBy = owner, principalFromRequest(r)
	saved, err := ms.CreateMemberDrive(r.Context(), d, policy.MaxPerPerson)
	switch {
	case errors.Is(err, store.ErrMemberDriveLimit):
		n := policy.MaxPerPerson
		if all, lerr := ms.ListMemberDrivesOf(r.Context(), owner); lerr == nil {
			n = len(all)
		}
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonMemberDriveLimitReached, memberDriveLimitReachedMsg(n, policy.MaxPerPerson))
		return
	case errors.Is(err, store.ErrMemberDriveNameTaken):
		writeErrorReason(w, http.StatusConflict, reasonMemberDriveNameTaken, memberDriveNameTakenMsg(d.Name))
		return
	case err != nil:
		writeServerError(w, r, "create member drive", err)
		return
	}
	s.auditMemberDriveCreate(r, ms, saved, policy.MaxPerPerson)
	out := memberMeDrive(saved)
	out.Mac = s.macDiskName(r.Context(), saved)
	writeJSON(w, http.StatusCreated, out)
}

// memberDriveMac is the Mac a disk drive is created on: one of owner's own claimed runners, not revoked,
// online here, whose caps keep disk drives. nil for anything else, all alike, so the refusal says nothing
// about a runner the caller does not own.
func (s *Server) memberDriveMac(ctx context.Context, owner, raw string) *uuid.UUID {
	id, err := uuid.Parse(raw)
	st, ok := s.cfg.Store.(store.RunnerStore)
	if err != nil || !ok {
		return nil
	}
	rn, err := st.GetRunner(ctx, id)
	if err != nil || rn.OwnerKind != types.RunnerOwnerPerson || rn.State != types.RunnerClaimed || rn.RevokedAt != nil ||
		types.CanonicalUserSubject(rn.Owner) != owner {
		return nil
	}
	caps, ok := s.runnerLastCaps(id)
	if !ok || caps.MacOSVM == nil || !caps.MacOSVM.DiskDrives || !s.runnerOnline(id) {
		return nil
	}
	return &id
}

// memberDriveCreateGates runs create's gates after the name rule: the doors
// (memberDriveCreateDoors), then the caller's own view of drive names. It
// returns the row to insert and the switch's policy, or the refusal.
func (s *Server) memberDriveCreateGates(r *http.Request, name string, mac bool) (types.UserDrive, types.MemberDrivePolicy, *runRefusal) {
	d, policy, f := s.memberDriveCreateDoors(r, mac)
	if f == nil {
		f = s.memberDriveNameRefusal(r.Context(), name)
	}
	if f != nil {
		return types.UserDrive{}, policy, f
	}
	d.Name = name
	return d, policy, nil
}

// memberDriveCreateDoors runs every create gate that does not need the name,
// in order: the org switch, the profile doors, the backend and size, and the
// substrate's lifecycle. The catalogue's create block reads the same answer.
// mac makes the row a disk drive, which the Mac it is kept on deletes
// (reclaim_drive), so the deployment's own lifecycle does not decide it.
func (s *Server) memberDriveCreateDoors(r *http.Request, mac bool) (types.UserDrive, types.MemberDrivePolicy, *runRefusal) {
	ctx := r.Context()
	provider, err := s.userDriveProvider(ctx)
	if err != nil {
		return types.UserDrive{}, types.MemberDrivePolicy{}, runServerError("get site config", err)
	}
	if provider.Disabled || provider.MemberCreate == nil || !provider.MemberCreate.Enabled {
		return types.UserDrive{}, types.MemberDrivePolicy{}, runError(http.StatusUnprocessableEntity, reasonMemberDrivesDisabled, memberDrivesDisabledMsg())
	}
	policy := *provider.MemberCreate
	ceiling, err := s.effectiveCeiling(ctx)
	if err != nil {
		return types.UserDrive{}, policy, ceilingRunRefusal(err)
	}
	if f := s.memberDriveDoorRefusal(r, ceiling); f != nil {
		return types.UserDrive{}, policy, f
	}
	d, f := s.memberDriveRow(policy, provider.MaxSizeMiB, ceiling.Limits.MaxDriveSizeMiB, mac)
	if f != nil {
		return types.UserDrive{}, policy, f
	}
	if !mac && !s.memberDrivesDeletable(ctx) {
		return types.UserDrive{}, policy, runError(http.StatusUnprocessableEntity, reasonMemberDriveDeleteUnavailable, memberDriveDeleteUnavailableCreateMsg())
	}
	return d, policy, nil
}

// memberDriveDoorRefusal is create's profile door: deny_user_drive (a person
// who may mount no drive may make none) or deny_member_drives. Operators are
// exempt, as from the mount door; never from the org switch. The decision is
// the flags, never the name, so a blank-named denying profile still refuses.
func (s *Server) memberDriveDoorRefusal(r *http.Request, ceiling governanceCeiling) *runRefusal {
	if s.runUngoverned(r.Context()) || ceiling.Unit == nil ||
		!ceiling.Limits.DenyUserDrive && !ceiling.Limits.DenyMemberDrives {
		return nil
	}
	msg := memberDrivesDeniedByUnitMsg(ceiling.Unit.Name)
	d := authz.Deny(authz.ReasonGovernanceUnit, "drives.member.create", msg).WithPolicy(s.ceilingPolicy(r.Context(), ceiling))
	return &runRefusal{status: http.StatusForbidden, decision: &d,
		body: errorBody{Error: msg, Reason: reasonMemberDrivesDeniedByUnit}}
}

// memberDriveRow is the row create inserts: this deployment's own managed
// backend, and on Kubernetes the switch's claim size clamped by the deployment
// and profile ceilings. A Docker volume has no size, so it gets none. A stored
// Kubernetes switch with no size is refused rather than made into an unsized
// claim. mac makes it a disk drive, sized as a claim is, or macDiskDefaultMiB
// when the switch sets no size; its home Mac is the caller's to set.
func (s *Server) memberDriveRow(policy types.MemberDrivePolicy, deploymentMiB, profileMiB int, mac bool) (types.UserDrive, *runRefusal) {
	var d types.UserDrive
	switch {
	case mac:
		d.Backend, d.SizeMiB = types.DriveBackendMacDisk, cmp.Or(policy.SizeMiB, macDiskDefaultMiB)
	case s.cfg.RunnerTarget == "docker":
		d.Backend = types.DriveBackendDockerVolume
		return d, nil
	case s.cfg.RunnerTarget == "k8s":
		if policy.SizeMiB <= 0 {
			return d, runError(http.StatusUnprocessableEntity, reasonMemberDriveUnsupported, memberDriveUnsizedMsg())
		}
		d.Backend, d.SizeMiB, d.StorageClass = types.DriveBackendK8sPVC, policy.SizeMiB, policy.StorageClass
	default:
		return d, runError(http.StatusUnprocessableEntity, reasonMemberDriveUnsupported, memberDriveUnsupportedMsg())
	}
	if bound, _ := (driveSizeCeiling{DeploymentMiB: deploymentMiB, ProfileMiB: profileMiB}).bound(); bound > 0 && bound < d.SizeMiB {
		d.SizeMiB = bound
	}
	return d, nil
}

// macDiskDefaultMiB is a disk drive's size when the switch sets none: 64 GiB. The image is sparse, so it
// takes room on the Mac only as the drive fills.
const macDiskDefaultMiB = 64 << 10

// memberDrivesDeletable reports whether the wired runner can destroy what it
// would create: it must be a DriveReclaimer, and its lifecycle check must say
// yes. Anything else is no, so create never makes an undeletable drive.
func (s *Server) memberDrivesDeletable(ctx context.Context) bool {
	if _, ok := s.cfg.Runner.(runner.DriveReclaimer); !ok {
		return false
	}
	c, ok := s.cfg.Runner.(runner.DriveLifecycleChecker)
	return ok && c.CanManageDrives(ctx) == nil
}

// memberDriveNameRefusal refuses a name whose slug equals one in the caller's
// own view: a managed drive their grants reach. Only those are compared, so the
// refusal says nothing about anyone else's drives. The caller's own member
// drives are the store's per-owner unique index.
func (s *Server) memberDriveNameRefusal(ctx context.Context, name string) *runRefusal {
	reach, err := s.reachableDrives(withDisplayRead(ctx), 0)
	if err != nil {
		return driveRunError(err)
	}
	slug := types.DriveSlug(name)
	for _, d := range reach {
		if d.winner.Drive.Backend.Kind() == types.DriveKindManaged && types.DriveSlug(d.winner.Drive.Name) == slug {
			return runError(http.StatusConflict, reasonMemberDriveNameTaken, memberDriveNameTakenMsg(name))
		}
	}
	return nil
}

// memberMeDrive is a member drive as the catalogue shows it to its owner.
func memberMeDrive(d types.UserDrive) client.MeDrive {
	return client.MeDrive{
		ID: d.ID.String(), Name: d.Name, Section: client.MeDriveSectionUser, Source: client.MeDriveSourcePersonal,
		Backend: d.Backend, Target: runner.DriveTargetFor(types.ResolvedDrive{Drive: d}, false), Writable: true,
		SizeMiB: d.SizeMiB, Enforcement: types.EnforcementFor(d.Backend), Owned: true, KeptFromRun: keptFromRunOf(d),
	}
}

func keptFromRunOf(d types.UserDrive) string {
	if d.KeptFromRun == nil {
		return ""
	}
	return d.KeptFromRun.String()
}

// keptFromRunTitle is the title of the run a member drive was kept from, read
// now so a renamed run shows its current title. "" when the drive was made by
// hand, the run is gone or untitled, or the run is not its owner's.
func (s *Server) keptFromRunTitle(ctx context.Context, d types.UserDrive) string {
	if d.KeptFromRun == nil {
		return ""
	}
	run, err := s.cfg.Store.GetRun(ctx, *d.KeptFromRun)
	if err != nil || run.CreatedBy != d.OwnerSubject {
		return ""
	}
	return run.Title
}

// auditMemberDriveCreate writes drive.member.create. count is the owner's
// drives after the insert; a failed read leaves it out rather than guessing.
func (s *Server) auditMemberDriveCreate(r *http.Request, ms store.MemberDriveStore, d types.UserDrive, limit int) {
	data := map[string]any{
		"backend": d.Backend, "drive": d.Name, "drive_id": d.ID, "limit": limit,
		"size_mib": d.SizeMiB, "storage_class": d.StorageClass,
	}
	if all, err := ms.ListMemberDrivesOf(r.Context(), d.OwnerSubject); err == nil {
		data["count"] = len(all)
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"drive.member.create", d.ID.String(), "success", mustJSON(data)))
}

// handleDeleteMyDrive is DELETE /me/drives/{id}: 204 once the drive's object is
// gone and its row with it. Never gated by the org switch or a profile door:
// deleting only subtracts. Another person's drive, an admin drive and an id
// that names nothing answer the same 404.
func (s *Server) handleDeleteMyDrive(w http.ResponseWriter, r *http.Request) {
	owner, ok := memberDriveOwner(w, r)
	if !ok || s.refuseMemberDriveRate(w, owner) {
		return
	}
	id, ok := parseIDParam(w, r, "id", "drive")
	if !ok {
		return
	}
	msg := driveNotFoundMsg(id.String())
	ms, ok := s.cfg.Store.(store.MemberDriveStore)
	if !ok {
		writeErrorReason(w, http.StatusNotFound, string(authz.ReasonDriveNotFound), msg)
		return
	}
	if d, gerr := ms.GetMemberDriveOf(r.Context(), owner, id); gerr == nil {
		refused, serr := s.memberDriveOwnerStarting(withRequestActor(r), ms, d, memberDriveByOwner)
		if serr != nil {
			writeServerError(w, r, "read the owner's starting runs", serr)
			return
		}
		if refused {
			msg := memberDriveInUseMsg()
			if d.Backend == types.DriveBackendMacDisk {
				msg = memberDriveReleasingOnMacMsg(d.Name)
			}
			writeErrorReason(w, http.StatusConflict, reasonMemberDriveInUse, msg)
			return
		}
	}
	d, started, err := ms.BeginMemberDriveDelete(r.Context(), owner, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.recordRefusal(r.Context(), r, authz.Deny(authz.ReasonDriveNotFound, "drives.member.delete", msg))
		writeErrorReason(w, http.StatusNotFound, string(authz.ReasonDriveNotFound), msg)
		return
	case errors.Is(err, store.ErrMemberDriveBusy):
		writeErrorReason(w, http.StatusConflict, reasonMemberDriveBusy, memberDriveBusyMsg())
		return
	case err != nil:
		writeServerError(w, r, "begin member drive delete", err)
		return
	}
	res := s.deleteMemberDrive(withRequestActor(r), ms, d, started, memberDriveByOwner)
	switch {
	case res.err != nil:
		writeServerError(w, r, "finish member drive delete", res.err)
	case res.reason == "":
		w.WriteHeader(http.StatusNoContent)
	case res.reason == reasonMemberDriveDeleteDeferred:
		_, mac, _ := s.macDiskHome(r.Context(), d)
		writeJSON(w, http.StatusAccepted, client.MyDriveDeleteResult{Reason: res.reason, Message: memberDriveDeleteDeferredMsg(mac)})
	case res.reason == reasonMemberDriveRecordOnly:
		_, mac, _ := s.macDiskHome(r.Context(), d)
		writeJSON(w, http.StatusOK, client.MyDriveDeleteResult{Reason: res.reason, Message: memberDriveRecordOnlyMsg(mac)})
	case res.reason == reasonMemberDriveInUse && d.Backend == types.DriveBackendMacDisk && res.driverErr != nil:
		_, mac, _ := s.macDiskHome(r.Context(), d)
		writeErrorReason(w, http.StatusConflict, reasonMemberDriveInUse, memberDriveInRunOnMacMsg(d.Name, mac))
	case res.reason == reasonMemberDriveInUse:
		writeErrorReason(w, http.StatusConflict, reasonMemberDriveInUse, memberDriveInUseMsg())
	case res.reason == reasonMemberDriveBusy:
		writeErrorReason(w, http.StatusConflict, reasonMemberDriveBusy, memberDriveBusyMsg())
	case res.reason == reasonMemberDriveDeleteUnavailable:
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonMemberDriveDeleteUnavailable, memberDriveDeleteUnavailableMsg())
	default:
		writeErrorReason(w, http.StatusServiceUnavailable, reasonMemberDriveDeleteFailed, memberDriveDeleteFailedMsg())
	}
}

// Who asked for a member drive's delete, as drive.member.delete records it.
const (
	memberDriveByOwner = "owner"
	memberDriveByAdmin = "admin"
)

// memberDriveDeletion is what deleteMemberDrive did. reason is "" on success,
// else the member reason the outcome maps to; driverErr is the substrate's own
// refusal for the admin route's 409; err is a row that could not be finished.
type memberDriveDeletion struct {
	object, outcome, reason string
	driverErr, err          error
	// issued is the count the issue statement returned (migration 0299): 0 when no physical call was made under
	// the claim. untouched is the substrate's definitive refusal, made before it changed anything.
	issued    int
	untouched bool
}

// deleteMemberDrive destroys a tombstoned member drive's substrate object and
// then removes its row. The mount is built from the stored row and its owner
// only, so the object is the one dispatch mounted. The row goes only when the
// substrate said deleted or already_absent; every other outcome clears the
// tombstone and keeps the drive. Every attempt is audited.
// memberDriveOwnerStarting refuses a delete before its tombstone, writing no row, while the
// drive's owner has a run in STARTING: that run may attach the drive, and would meet the tombstone
// as drive_not_found. The refusal is audited as deleteMemberDrive audits its own. deleteMemberDrive
// reads the same thing again past the tombstone, for a run that starts in between.
func (s *Server) memberDriveOwnerStarting(ctx context.Context, ms store.MemberDriveStore, d types.UserDrive, by string) (bool, error) {
	starting, err := ms.MemberDriveOwnerStarting(ctx, d.OwnerSubject, watcherLeaseStaleAfter)
	if err != nil || !starting {
		return false, err
	}
	mount, _ := memberDriveMount(d)
	s.auditMemberDriveDelete(ctx, d, memberDriveDeletion{object: mount.ObjectName, outcome: driveReclaimOutcomeRefused, reason: reasonMemberDriveInUse}, by)
	return true, nil
}

// The claim d carries (started, d.DeleteIssues) is settled by exactly one of: the row's removal (deleted, already
// absent, record-only), the claim's clearing (a refusal before any call, or a definitive refusal after it that
// touched nothing), the claim left for the Mac's sweep, or nothing, which leaves the drive deleting: a call was
// made and its outcome is not known. The one physical call is made only after IssueMemberDriveDelete committed
// for this exact pair, so a retry, a duplicate or a claim taken over never makes a second call.
func (s *Server) deleteMemberDrive(ctx context.Context, ms store.MemberDriveStore, d types.UserDrive, started time.Time, by string) memberDriveDeletion {
	// Past the claim the delete finishes even if the caller goes away, so
	// the row is always removed or settled and the attempt audited.
	// The substrate work after it (the lifecycle check and the reclaim) is
	// bounded by driveReclaimTimeout; running out leaves the drive held.
	ctx = context.WithoutCancel(ctx)
	sctx, cancel := context.WithTimeout(ctx, driveReclaimTimeout)
	defer cancel()
	mount, err := memberDriveMount(d)
	res := memberDriveDeletion{object: mount.ObjectName}
	rc, ok := s.cfg.Runner.(runner.DriveReclaimer)
	switch {
	case err != nil:
		res.outcome, res.reason = driveReclaimOutcomeFailed, reasonMemberDriveDeleteFailed
	case d.Backend == types.DriveBackendMacDisk:
		res = s.reclaimMacDisk(sctx, ms, d, started, mount)
		res.object = mount.ObjectName
	case !ok || d.Backend.RunnerTarget() != s.cfg.RunnerTarget || !s.memberDrivesDeletable(sctx):
		res.outcome, res.reason = driveReclaimOutcomeFailed, reasonMemberDriveDeleteUnavailable
	default:
		// Past the claim, a run of the owner's still STARTING may have re-read this drive before it
		// (memberDrivesStillThere) and create the drive's object after the reclaim, so the drive counts
		// as in use until that run leaves STARTING, and until a create whose run was ended on another
		// replica stops holding its lease. A run that re-reads later meets the claim.
		starting, serr := ms.MemberDriveOwnerStarting(sctx, d.OwnerSubject, watcherLeaseStaleAfter)
		if serr != nil {
			slog.WarnContext(ctx, "api: member drive delete could not read the owner's starting runs",
				slog.String("drive_id", d.ID.String()), slog.String("error", serr.Error()))
			res.outcome, res.reason = driveReclaimOutcomeFailed, reasonMemberDriveDeleteFailed
			break
		}
		if starting {
			res.outcome, res.reason = driveReclaimOutcomeRefused, reasonMemberDriveInUse
			break
		}
		issued, refusal, issuedOK := s.issueMemberDriveDelete(sctx, ms, d, started)
		if !issuedOK {
			refusal.object = mount.ObjectName
			res = refusal
			break
		}
		res.issued = issued
		outcome, rerr := rc.ReclaimDrive(sctx, mount)
		switch {
		case rerr == nil:
			res.outcome = string(outcome)
		case errors.Is(rerr, runner.ErrDriveInUse) || errors.Is(rerr, runner.ErrDriveUntouched):
			// Only the typed refusals are definitive. A bare ErrDriveNotReclaimable (a k8s claim already
			// Terminating) does not say nothing was touched, so it is the ambiguous failure below.
			res.outcome, res.reason, res.driverErr, res.untouched = driveReclaimOutcomeRefused, reasonMemberDriveInUse, rerr, true
		default:
			slog.WarnContext(ctx, "api: member drive delete failed on the substrate",
				slog.String("drive_id", d.ID.String()), slog.String("error", rerr.Error()))
			res.outcome, res.reason = driveReclaimOutcomeFailed, reasonMemberDriveDeleteFailed
		}
	}
	s.settleMemberDriveDelete(ctx, ms, d, started, by, &res)
	s.auditMemberDriveDelete(ctx, d, res, by)
	return res
}

// settleMemberDriveDelete applies what deleteMemberDrive found to the claim (started, d.DeleteIssues).
func (s *Server) settleMemberDriveDelete(ctx context.Context, ms store.MemberDriveStore, d types.UserDrive, started time.Time, by string, res *memberDriveDeletion) {
	claimN := d.DeleteIssues
	sweep := by == memberDriveByMacReconnect
	var err error
	if res.reason == "" && res.outcome != string(runner.DriveReclaimDeleted) && res.outcome != string(runner.DriveReclaimAlreadyAbsent) {
		// Both reclaim paths end here. A reply with no outcome, or one this server does not know (a Mac's
		// successful reply that omits it), is not affirmative evidence that the storage is gone: the call
		// was issued, so the row stays held at the issued count and the answer is the ambiguous failure.
		slog.WarnContext(ctx, "api: a member drive's delete came back with an outcome that is not deleted or already_absent",
			slog.String("drive_id", d.ID.String()), slog.String("outcome", res.outcome))
		res.outcome, res.reason = driveReclaimOutcomeFailed, reasonMemberDriveDeleteFailed
		return
	}
	switch {
	case res.reason == "" || res.reason == reasonMemberDriveRecordOnly:
		n := claimN // a record-only delete issued nothing
		if res.issued != 0 {
			n = res.issued
		}
		if ferr := ms.FinishMemberDriveDelete(ctx, d.ID, started, n); ferr != nil && !errors.Is(ferr, store.ErrNotFound) {
			res.err = ferr
		}
		return
	case res.reason == reasonMemberDriveBusy, res.reason == reasonMemberDriveDeleteDeferred:
		// The claim was consumed or taken over (nothing to settle), or it stays for the Mac's sweep: the
		// drive reads as deleting, no run attaches it, and the Mac's reconnect finishes the delete.
		return
	case res.issued == 0:
		// Nothing was issued. The sweep keeps the mark for every such failure: the owner already asked for
		// the delete, and the next sweep meets it again.
		if !sweep {
			err = ms.ClearMemberDriveClaim(ctx, d.ID, started, claimN)
		}
	case res.untouched:
		// Refused after the issue, before anything changed. Only a claim over an even count clears; the
		// sweep keeps the mark and its next pass issues again.
		if !sweep && claimN%2 == 0 {
			err = ms.ClearMemberDriveIssued(ctx, d.ID, started, claimN, res.issued)
		}
	}
	// Anything else made a call whose outcome is not known: the drive stays deleting.
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.ErrorContext(ctx, "api: clear a member drive's delete claim",
			slog.String("drive_id", d.ID.String()), slog.String("error", err.Error()))
	}
}

// issueMemberDriveDelete makes the claim's one physical call outstanding. When it cannot, the deletion to answer
// with: the claim was consumed or taken over (busy, no call), or the write itself failed (failed, nothing
// issued as far as is known).
func (s *Server) issueMemberDriveDelete(ctx context.Context, ms store.MemberDriveStore, d types.UserDrive, started time.Time) (int, memberDriveDeletion, bool) {
	n, err := ms.IssueMemberDriveDelete(ctx, d.ID, started, d.DeleteIssues)
	switch {
	case err == nil:
		return n, memberDriveDeletion{}, true
	case errors.Is(err, store.ErrNotFound):
		return 0, memberDriveDeletion{outcome: driveReclaimOutcomeRefused, reason: reasonMemberDriveBusy}, false
	}
	slog.WarnContext(ctx, "api: record a member drive delete's issue", slog.String("drive_id", d.ID.String()), slog.String("error", err.Error()))
	return 0, memberDriveDeletion{outcome: driveReclaimOutcomeFailed, reason: reasonMemberDriveDeleteFailed}, false
}

// Outcomes of a disk drive's delete that are not its Mac's (drive.member.delete's outcome).
const (
	driveReclaimOutcomeDeferred   = "deferred"    // the Mac is offline: the tombstone stays until it reconnects
	driveReclaimOutcomeRecordOnly = "record_only" // the Mac is no longer registered: the image stays on it
)

// reclaimMacDisk is deleteMemberDrive's substrate step for a disk drive: the Mac
// it is kept on deletes its image (reclaim_drive). A Mac no longer registered
// keeps the image and the row goes, with no call and so no issue; an offline one, or one this replica
// does not hold, keeps the claim for its reconnect; one whose run has the disk refuses. A run
// of the owner's still starting may attach the disk, so it refuses too. The issue is made just before the
// call. The Mac answers a refusal typed only as in use, which is definitive.
func (s *Server) reclaimMacDisk(ctx context.Context, ms store.MemberDriveStore, d types.UserDrive, started time.Time, mount types.DriveMount) memberDriveDeletion {
	rn, _, err := s.macDiskHome(ctx, d)
	switch {
	case errors.Is(err, store.ErrNotFound) || err == nil && rn.RevokedAt != nil:
		return memberDriveDeletion{outcome: driveReclaimOutcomeRecordOnly, reason: reasonMemberDriveRecordOnly}
	case err != nil:
		return memberDriveDeletion{outcome: driveReclaimOutcomeFailed, reason: reasonMemberDriveDeleteFailed}
	case !s.macDiskHeldHere(rn.ID):
		return memberDriveDeletion{outcome: driveReclaimOutcomeDeferred, reason: reasonMemberDriveDeleteDeferred}
	}
	if starting, serr := ms.MemberDriveOwnerStarting(ctx, d.OwnerSubject, watcherLeaseStaleAfter); serr != nil || starting {
		if serr != nil {
			return memberDriveDeletion{outcome: driveReclaimOutcomeFailed, reason: reasonMemberDriveDeleteFailed}
		}
		return memberDriveDeletion{outcome: driveReclaimOutcomeRefused, reason: reasonMemberDriveInUse}
	}
	issued, refusal, ok := s.issueMemberDriveDelete(ctx, ms, d, started)
	if !ok {
		return refusal
	}
	outcome, rerr := s.runnerEndpoint(rn.ID).sub.ReclaimMacDisk(ctx, mount)
	switch {
	case rerr == nil:
		return memberDriveDeletion{outcome: string(outcome), issued: issued}
	case errors.Is(rerr, runner.ErrDriveInUse):
		return memberDriveDeletion{outcome: driveReclaimOutcomeRefused, reason: reasonMemberDriveInUse, driverErr: rerr, issued: issued, untouched: true}
	}
	slog.WarnContext(ctx, "api: a disk drive's delete failed on its Mac",
		slog.String("drive_id", d.ID.String()), slog.String("runner_id", rn.ID.String()), slog.String("error", rerr.Error()))
	return memberDriveDeletion{outcome: driveReclaimOutcomeFailed, reason: reasonMemberDriveDeleteFailed, issued: issued}
}

// macDiskHeldHere reports whether this replica holds runnerID's session, online
// with caps that keep disk drives, so reclaim_drive reaches the Mac from here. A
// Mac held by another replica counts as away: that replica's sweep finishes the
// delete (finishMacDiskDeletes).
func (s *Server) macDiskHeldHere(runnerID uuid.UUID) bool {
	caps, ok := s.runnerLastCaps(runnerID)
	return ok && caps.MacOSVM != nil && caps.MacOSVM.DiskDrives && s.runnerOnline(runnerID)
}

// macDiskDeleteStore lists the disk drives whose delete waits for their Mac (store.PG).
type macDiskDeleteStore interface {
	ListMacDiskDeletes(ctx context.Context) ([]types.UserDrive, error)
}

// memberDriveByMacReconnect is the "by" of a disk drive delete its Mac's return finishes.
const memberDriveByMacReconnect = "mac_reconnect"

// finishMacDiskDeletes finishes each disk drive delete that waited for its Mac
// (MAC-D10) once this replica holds that Mac, or the Mac is no longer registered
// (MAC-D12): the tombstone is taken over once it is older than a minute, and the
// delete runs as its owner's did. The runner-ready kick and the run watcher's
// sweep call it; a drive skipped now is met again on the next sweep.
func (s *Server) finishMacDiskDeletes(ctx context.Context) error {
	ms, ok := s.cfg.Store.(store.MemberDriveStore)
	ls, lok := s.cfg.Store.(macDiskDeleteStore)
	if !ok || !lok {
		return nil
	}
	waiting, err := ls.ListMacDiskDeletes(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, w := range waiting {
		rn, _, herr := s.macDiskHome(ctx, w)
		gone := errors.Is(herr, store.ErrNotFound) || herr == nil && rn.RevokedAt != nil
		if !gone && (herr != nil || !s.macDiskHeldHere(rn.ID)) {
			continue
		}
		d, started, err := ms.BeginMemberDriveTakeover(ctx, w.ID)
		switch {
		case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrMemberDriveBusy):
			continue
		case err != nil:
			errs = append(errs, err)
			continue
		}
		if res := s.deleteMemberDrive(ctx, ms, d, started, memberDriveByMacReconnect); res.err != nil {
			errs = append(errs, res.err)
		}
	}
	return errors.Join(errs...)
}

// memberDriveMount is the DriveMount a member drive's object is addressed by:
// the hash home of the drive and its owner, as the resolver derives it.
func memberDriveMount(d types.UserDrive) (types.DriveMount, error) {
	home, err := types.DriveHomeName(d, d.OwnerSubject, "")
	if err != nil {
		return types.DriveMount{}, err
	}
	return types.DriveMount{
		DriveID: d.ID, Backend: d.Backend, ObjectName: types.DriveObjectName(d, home), DriveName: d.Name,
		StorageClass: d.StorageClass, HomeName: home, SubjectHash: types.DriveSubjectHash(d.OwnerSubject),
		Target: runner.DriveTarget, SizeMiB: d.SizeMiB, RunnerID: d.HomeRunnerID,
	}, nil
}

// auditMemberDriveDelete writes drive.member.delete for one attempt: deleted
// and already_absent are successes; refused and failed are failures.
func (s *Server) auditMemberDriveDelete(ctx context.Context, d types.UserDrive, res memberDriveDeletion, by string) {
	result := "failure"
	switch res.reason {
	case "", reasonMemberDriveRecordOnly, reasonMemberDriveDeleteDeferred:
		result = "success"
	}
	actorType, actor := auditActorFromContext(ctx)
	s.recordAudit(ctx, s.auditEvent(nil, actorType, actor,
		"drive.member.delete", d.ID.String(), result, mustJSON(map[string]any{
			"backend": d.Backend, "by": by, "drive": d.Name, "drive_id": d.ID,
			"object": res.object, "outcome": res.outcome, "owner": d.OwnerSubject,
		})))
}
