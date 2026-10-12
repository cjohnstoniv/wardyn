// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import "github.com/cjohnstoniv/wardyn/internal/types"

// MeDriveCatalogue is GET /api/v1/me/drives: every drive the caller's own
// grants reach, the ones a run may attach through CreateRunRequest.Drives.
// Nothing here names a host path, a storage object or another person.
type MeDriveCatalogue struct {
	// Drives is never null. It is empty when nothing is granted, when a
	// profile shuts the door (DeniedByProfile) or when the catalogue could not
	// be answered (Unavailable).
	Drives []MeDrive `json:"drives"`
	// DeniedByProfile names the governance profile whose deny_user_drive
	// shuts the door for the caller.
	DeniedByProfile string `json:"denied_by_profile,omitempty"`
	// Unavailable is why the catalogue could not be answered:
	// groups_snapshot_stale, user_type_unknown, unavailable or
	// governance_unavailable (the /me.user_drive_unavailable tokens).
	Unavailable string `json:"unavailable,omitempty"`
	// PoolChecked is true when runner_pool_id named a pool the caller may use,
	// so UnavailableReason includes not_in_pool. False for no pool and for one
	// the caller cannot use; the pool is checked again at launch.
	PoolChecked bool `json:"pool_checked"`
}

// MeDrive is one drive of the catalogue.
type MeDrive struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Section is user (a managed backend) or shared (a share backend).
	Section string `json:"section"`
	// Source is the tier of the grant that wins this drive for the caller:
	// personal (a user grant), team (a group grant) or organisation (an all or
	// user type grant).
	Source  string             `json:"source"`
	Backend types.DriveBackend `json:"backend"`
	// Target is where the drive mounts, derived by the server.
	Target string `json:"target"`
	// Writable is the grant's resolved posture; a run may only narrow it.
	Writable    bool                     `json:"writable"`
	SizeMiB     int                      `json:"size_mib,omitempty"`
	Enforcement types.StorageEnforcement `json:"enforcement"`
	// Legacy marks the drive drive.enabled mounts at /home/agent/drive: the
	// caller's top-ranked grant.
	Legacy bool `json:"legacy"`
	// UnavailableReason is why a run cannot attach this drive now: paused,
	// unmountable or not_in_pool. "" when it can.
	UnavailableReason string `json:"unavailable_reason,omitempty"`
}

// The closed sets of MeDrive.
const (
	MeDriveSectionUser   = "user"
	MeDriveSectionShared = "shared"

	MeDriveSourcePersonal     = "personal"
	MeDriveSourceTeam         = "team"
	MeDriveSourceOrganisation = "organisation"

	MeDriveUnavailablePaused      = "paused"
	MeDriveUnavailableUnmountable = "unmountable"
	MeDriveUnavailableNotInPool   = "not_in_pool"
)
