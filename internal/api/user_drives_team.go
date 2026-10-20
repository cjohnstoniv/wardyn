// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Team drives (#1988): an admin drive whose group allocations share ONE
// folder. The admin write rules live here; the resolution is newResolvedDrive's.

// driveTeamOffMsg refuses marking a drive a team drive while the switch is off.
const driveTeamOffMsg = "team drives are off for this deployment: turn on storage.user_drive.team_drives first"

// driveTeamWriteRefusal refuses a team drive while storage.user_drive.team_drives
// is off. Empty msg is yes.
func (s *Server) driveTeamWriteRefusal(ctx context.Context, d types.UserDrive) (code int, reason, msg string) {
	if !d.Team {
		return 0, "", ""
	}
	provider, err := s.userDriveProvider(ctx)
	if err != nil {
		return http.StatusInternalServerError, reasonUserDriveCeilingUnavailable, loggedMsg(ctx, "get site config", err)
	}
	if !provider.TeamDrives {
		return http.StatusUnprocessableEntity, reasonUserDriveRequestInvalid, driveTeamOffMsg
	}
	return 0, "", ""
}

// driveTeamFlipRefusal answers a write that turns a drive into one folder for a
// group, or back into a folder per person. That changes who sees whose files,
// so it is refused while anyone is allocated the drive. Empty msg is yes, and
// the write is then guarded on the drive still being unallocated.
func driveTeamFlipRefusal(grants int) (code int, reason, msg string) {
	if grants == 0 {
		return 0, "", ""
	}
	return http.StatusConflict, reasonUserDriveRehomeInvalid, fmt.Sprintf(
		"team cannot change while the drive is allocated to %s: remove its allocations first", pluralDriveSubjects(grants))
}

// teamDriveGrantAllowed holds an allocation to its drive's team rule
// (types.ValidateTeamDriveGrant), answering 400 itself when it is refused. A
// drive that does not exist is left to the write, which answers 404.
func (s *Server) teamDriveGrantAllowed(w http.ResponseWriter, r *http.Request, g types.UserDriveGrant) bool {
	d, err := s.cfg.Store.GetUserDrive(r.Context(), g.DriveID)
	if errors.Is(err, store.ErrNotFound) {
		return true
	}
	if err != nil {
		writeServerError(w, r, "get user drive", err)
		return false
	}
	if err := types.ValidateTeamDriveGrant(d, g); err != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonUserDriveGrantInvalid, "invalid allocation: "+err.Error())
		return false
	}
	return true
}

// driveTeamReclaimMsg is the 422 a person's reclaim of a team drive answers.
const driveTeamReclaimMsg = "this is a team drive: its files belong to its groups and are not reclaimed per person; " +
	"remove the folder on the substrate (docs/OPERATIONS.md) once the groups no longer need it"

// driveTeamReclaimRefusal keeps a person's reclaim (the leaver's cleanup) off a
// team drive while team drives are on: a team drive's files belong to its groups
// and are not reclaimed per person. Reclaim derives the person's own object
// (DriveHomeName), never the group folder (DriveTeamHome), so the refusal also
// keeps it off a per-person object left from while the switch was off. Off, the
// drive resolves per person and reclaims as one. Empty msg is yes.
func (s *Server) driveTeamReclaimRefusal(ctx context.Context, d types.UserDrive) (code int, msg string) {
	if !d.Team {
		return 0, ""
	}
	provider, err := s.userDriveProvider(ctx)
	if err != nil {
		return http.StatusInternalServerError, loggedMsg(ctx, "get site config", err)
	}
	if !provider.TeamDrives {
		return 0, ""
	}
	return http.StatusUnprocessableEntity, driveTeamReclaimMsg
}

// teamSharedWith maps each team drive in reach to the caller's own groups that
// carry an allocation of it, sorted. Only groups the caller holds are named. A
// display read: any failure answers nothing rather than an error.
func (s *Server) teamSharedWith(ctx context.Context, reach []reachableDrive) map[uuid.UUID][]string {
	if !slices.ContainsFunc(reach, func(d reachableDrive) bool { return d.resolved != nil && d.resolved.Team }) {
		return nil
	}
	subj, err := s.callerSubjects(ctx)
	if err != nil || subj.stale {
		return nil
	}
	grants, err := s.cfg.Store.ListUserDriveGrants(ctx)
	if err != nil {
		return nil
	}
	out := map[uuid.UUID][]string{}
	for _, g := range grants {
		if g.SubjectType == types.CapabilitySubjectGroup && slices.Contains(subj.groups, g.Subject) &&
			!slices.Contains(out[g.DriveID], g.Subject) {
			out[g.DriveID] = append(out[g.DriveID], g.Subject)
		}
	}
	for _, groups := range out {
		slices.Sort(groups)
	}
	return out
}
