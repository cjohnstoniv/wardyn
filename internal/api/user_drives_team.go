// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

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

// driveTeamReclaimRefusal keeps a person's reclaim (the leaver's cleanup) off a
// team drive while team drives are on: its folder is the whole group's, not one
// person's storage. Off, the drive resolves per person and reclaims as one.
// Empty msg is yes.
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
	return http.StatusUnprocessableEntity, "this is a team drive: its folder is the whole group's, not one person's storage, " +
		"so a person's reclaim skips it; remove the folder on the substrate (docs/OPERATIONS.md) once the group no longer needs it"
}
