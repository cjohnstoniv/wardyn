// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func (s *Server) authorizeRequestDrive(r *http.Request,
	req createRunRequest, ceiling governanceCeiling) (*types.ResolvedDrive, *runRefusal) {
	if req.Drive == nil || !req.Drive.Enabled {
		return nil, nil
	}
	// The org switch first, and the order is the argument: storage.user_drive's
	// `disabled` and a profile's DenyUserDrive answer two different questions, and
	// only the second one is about this member. With drives off deployment-wide
	// nobody was DENIED — there is nothing here to mount — so it is a 422 in the
	// REFUSED_BACKEND family and never the door's 403 with its authz.denied row.
	provider, perr := s.userDriveProvider(r.Context())
	if perr != nil {
		return nil, runServerError("get site config", perr)
	}
	if provider.Disabled {
		return nil, runDriveRefusal(http.StatusUnprocessableEntity, driveRefusalDrivesDisabled,
			fmt.Sprintf(driveRefusedBackendMsg, driveDisabledMsg))
	}
	if refusal := s.userDriveDoorRefusal(r, ceiling); refusal != nil {
		return nil, refusal
	}
	// The SAME resolver /me and the admin preview run. A store failure is an
	// ERROR here and not "no drive" — writeDriveError's 500 arm — because
	// mounting nothing where an admin allocated something loses a member's work
	// silently, while a 500 tells them to try again.
	resolved, err := s.resolveUserDrive(r.Context(), ceiling.Limits.MaxDriveSizeMiB)
	if err != nil {
		return nil, driveRunError(err)
	}
	if resolved == nil {
		return nil, runDriveRefusal(http.StatusUnprocessableEntity, driveRefusalNoAllocation,
			"no user drive is allocated to you — ask an admin for an allocation")
	}
	if resolved.Paused {
		return nil, runDriveRefusal(http.StatusUnprocessableEntity, driveRefusalPaused,
			"your allocation is paused by an admin",
			slog.String("drive", resolved.Drive.Name))
	}
	return resolved, nil
}

func (s *Server) userDriveDoorRefusal(r *http.Request, ceiling governanceCeiling) *runRefusal {
	profile, shut := s.driveDoorProfile(r.Context(), ceiling)
	if !shut {
		return nil
	}
	// The mock round's frozen member copy, reproduced byte-exact: the console
	// never rewords a server refusal, so this line is where that string ships.
	return runDenied(authz.Deny(authz.ReasonGovernanceProfile, "runs.drive", driveDeniedByProfileMsg(profile)).WithPolicy(s.ceilingPolicy(r.Context(), ceiling)))
}

func runDriveRefusal(status int, reason, member string, attrs ...any) *runRefusal {
	return &runRefusal{drive: &driveBindFailure{status: status, reason: reason, member: member, attrs: attrs}}
}

func driveRunError(err error) *runRefusal {
	switch {
	case errors.Is(err, errDrivesDisabled):
		return runError(http.StatusUnprocessableEntity, driveRefusalDrivesDisabled, fmt.Sprintf(driveRefusedBackendMsg, driveDisabledMsg))
	case errors.Is(err, errGroupsSnapshotStale):
		return runError(http.StatusForbidden, reasonGroupsSnapshotStale, groupsSnapshotStaleMsg)
	case errors.Is(err, errUserTypeUnknown):
		return runError(http.StatusForbidden, driveUnavailableUserType, userTypeUnknownMsg)
	case errors.Is(err, errDriveUnmountable):
		return runError(http.StatusUnprocessableEntity, driveUnavailableUnmountable, strings.TrimPrefix(err.Error(), errDriveUnmountable.Error()+": "))
	default:
		return runServerError("resolve user drive", err)
	}
}

func driveReadOnlyRefusal(req createRunRequest, resolved types.ResolvedDrive) *runRefusal {
	if req.Drive.ReadOnly != nil && !*req.Drive.ReadOnly && !resolved.Writable {
		return runDriveRefusal(http.StatusUnprocessableEntity, driveRefusalReadOnly,
			"your allocation is read-only; read_only:false cannot widen it", slog.String("drive", resolved.Drive.Name))
	}
	return nil
}
