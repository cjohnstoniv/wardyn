// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Member drives: a person creates and deletes their own drive. The row's
// owner_subject is the authority and no grant row is ever written, so no admin
// grant can widen a person's drive and no member drive can win the legacy
// resolver. Deleting destroys the substrate object before the row goes.
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

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

func memberDrivesDeniedByProfileMsg(profile string) string {
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

func memberDriveBusyMsg() string { return "This drive is already being deleted." }

func memberDriveDeleteFailedMsg() string {
	return "The drive's files couldn't be deleted. Nothing was removed; try again."
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
	d, policy, refusal := s.memberDriveCreateGates(r, strings.TrimSpace(req.Name))
	if refusal.write(s, w, r) {
		return
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
	writeJSON(w, http.StatusCreated, memberMeDrive(saved))
}

// memberDriveCreateGates runs create's gates after the name rule: the doors
// (memberDriveCreateDoors), then the caller's own view of drive names. It
// returns the row to insert and the switch's policy, or the refusal.
func (s *Server) memberDriveCreateGates(r *http.Request, name string) (types.UserDrive, types.MemberDrivePolicy, *runRefusal) {
	d, policy, f := s.memberDriveCreateDoors(r)
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
func (s *Server) memberDriveCreateDoors(r *http.Request) (types.UserDrive, types.MemberDrivePolicy, *runRefusal) {
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
	d, f := s.memberDriveRow(policy, provider.MaxSizeMiB, ceiling.Limits.MaxDriveSizeMiB)
	if f != nil {
		return types.UserDrive{}, policy, f
	}
	if !s.memberDrivesDeletable(ctx) {
		return types.UserDrive{}, policy, runError(http.StatusUnprocessableEntity, reasonMemberDriveDeleteUnavailable, memberDriveDeleteUnavailableCreateMsg())
	}
	return d, policy, nil
}

// memberDriveDoorRefusal is create's profile door: deny_user_drive (a person
// who may mount no drive may make none) or deny_member_drives. Operators are
// exempt, as from the mount door; never from the org switch. The decision is
// the flags, never the name, so a blank-named denying profile still refuses.
func (s *Server) memberDriveDoorRefusal(r *http.Request, ceiling governanceCeiling) *runRefusal {
	if s.runUngoverned(r.Context()) || ceiling.Profile == nil ||
		!ceiling.Limits.DenyUserDrive && !ceiling.Limits.DenyMemberDrives {
		return nil
	}
	msg := memberDrivesDeniedByProfileMsg(ceiling.Profile.Name)
	d := authz.Deny(authz.ReasonGovernanceProfile, "drives.member.create", msg)
	return &runRefusal{status: http.StatusForbidden, decision: &d,
		body: errorBody{Error: msg, Reason: reasonMemberDrivesDeniedByProfile}}
}

// memberDriveRow is the row create inserts: this deployment's own managed
// backend, and on Kubernetes the switch's claim size clamped by the deployment
// and profile ceilings. A Docker volume has no size, so it gets none. A stored
// Kubernetes switch with no size is refused rather than made into an unsized
// claim.
func (s *Server) memberDriveRow(policy types.MemberDrivePolicy, deploymentMiB, profileMiB int) (types.UserDrive, *runRefusal) {
	var d types.UserDrive
	switch s.cfg.RunnerTarget {
	case "docker":
		d.Backend = types.DriveBackendDockerVolume
	case "k8s":
		if policy.SizeMiB <= 0 {
			return d, runError(http.StatusUnprocessableEntity, reasonMemberDriveUnsupported, memberDriveUnsizedMsg())
		}
		d.Backend, d.SizeMiB, d.StorageClass = types.DriveBackendK8sPVC, policy.SizeMiB, policy.StorageClass
		if bound, _ := (driveSizeCeiling{DeploymentMiB: deploymentMiB, ProfileMiB: profileMiB}).bound(); bound > 0 && bound < d.SizeMiB {
			d.SizeMiB = bound
		}
	default:
		return d, runError(http.StatusUnprocessableEntity, reasonMemberDriveUnsupported, memberDriveUnsupportedMsg())
	}
	return d, nil
}

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
		SizeMiB: d.SizeMiB, Enforcement: types.EnforcementFor(d.Backend), Owned: true,
	}
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
	res := s.deleteMemberDrive(r, ms, d, started, memberDriveByOwner)
	switch {
	case res.err != nil:
		writeServerError(w, r, "finish member drive delete", res.err)
	case res.reason == "":
		w.WriteHeader(http.StatusNoContent)
	case res.reason == reasonMemberDriveInUse:
		writeErrorReason(w, http.StatusConflict, reasonMemberDriveInUse, memberDriveInUseMsg())
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
}

// deleteMemberDrive destroys a tombstoned member drive's substrate object and
// then removes its row. The mount is built from the stored row and its owner
// only, so the object is the one dispatch mounted. The row goes only when the
// substrate said deleted or already_absent; every other outcome clears the
// tombstone and keeps the drive. Every attempt is audited.
func (s *Server) deleteMemberDrive(r *http.Request, ms store.MemberDriveStore, d types.UserDrive, started time.Time, by string) memberDriveDeletion {
	// Past the tombstone the delete finishes even if the caller goes away, so
	// the row is always removed or restored and the attempt audited.
	// The substrate work after it (the lifecycle check and the reclaim) is
	// bounded by driveReclaimTimeout; running out aborts and keeps the drive.
	r = r.WithContext(context.WithoutCancel(r.Context()))
	ctx := r.Context()
	sctx, cancel := context.WithTimeout(ctx, driveReclaimTimeout)
	defer cancel()
	mount, err := memberDriveMount(d)
	res := memberDriveDeletion{object: mount.ObjectName}
	rc, ok := s.cfg.Runner.(runner.DriveReclaimer)
	switch {
	case err != nil:
		res.outcome, res.reason = driveReclaimOutcomeFailed, reasonMemberDriveDeleteFailed
	case !ok || d.Backend.RunnerTarget() != s.cfg.RunnerTarget || !s.memberDrivesDeletable(sctx):
		res.outcome, res.reason = driveReclaimOutcomeFailed, reasonMemberDriveDeleteUnavailable
	default:
		outcome, rerr := rc.ReclaimDrive(sctx, mount)
		switch {
		case rerr == nil:
			res.outcome = string(outcome)
		case errors.Is(rerr, runner.ErrDriveInUse) || errors.Is(rerr, runner.ErrDriveNotReclaimable):
			res.outcome, res.reason, res.driverErr = driveReclaimOutcomeRefused, reasonMemberDriveInUse, rerr
		default:
			slog.WarnContext(ctx, "api: member drive delete failed on the substrate",
				slog.String("drive_id", d.ID.String()), slog.String("error", rerr.Error()))
			res.outcome, res.reason = driveReclaimOutcomeFailed, reasonMemberDriveDeleteFailed
		}
	}
	if res.reason == "" {
		if ferr := ms.FinishMemberDriveDelete(ctx, d.ID, started); ferr != nil && !errors.Is(ferr, store.ErrNotFound) {
			res.err = ferr
		}
	} else if aerr := ms.AbortMemberDriveDelete(ctx, d.ID, started); aerr != nil && !errors.Is(aerr, store.ErrNotFound) {
		slog.ErrorContext(ctx, "api: clear a member drive's delete tombstone",
			slog.String("drive_id", d.ID.String()), slog.String("error", aerr.Error()))
	}
	s.auditMemberDriveDelete(r, d, res, by)
	return res
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
		Target: runner.DriveTarget,
	}, nil
}

// auditMemberDriveDelete writes drive.member.delete for one attempt: deleted
// and already_absent are successes; refused and failed are failures.
func (s *Server) auditMemberDriveDelete(r *http.Request, d types.UserDrive, res memberDriveDeletion, by string) {
	result := "failure"
	if res.reason == "" {
		result = "success"
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"drive.member.delete", d.ID.String(), result, mustJSON(map[string]any{
			"backend": d.Backend, "by": by, "drive": d.Name, "drive_id": d.ID,
			"object": res.object, "outcome": res.outcome, "owner": d.OwnerSubject,
		})))
}
