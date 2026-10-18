// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

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
	// groups_snapshot_stale, user_type_unknown, drive_subject_unsupported,
	// unavailable or governance_unavailable (the /me.user_drive_unavailable tokens).
	Unavailable string `json:"unavailable,omitempty"`
	// PoolChecked is true when runner_pool_id named a pool the caller may use,
	// so UnavailableReason includes not_in_pool. False for no pool and for one
	// the caller cannot use; the pool is checked again at launch.
	PoolChecked bool `json:"pool_checked"`
	// Create is whether the caller may create a drive now (POST /me/drives).
	// Absent when the caller is not a person: a device, a delegated caller,
	// SCIM, a service identity, the admin token or local mode.
	Create *MemberDriveCreate `json:"create,omitempty"`
}

// MemberDriveCreate is the catalogue's create availability, decided by the
// same gates as POST /me/drives except the name.
type MemberDriveCreate struct {
	Allowed bool `json:"allowed"`
	// Reason is set exactly when Allowed is false.
	Reason MemberDriveCreateReason `json:"reason,omitempty"`
	// DeniedByProfile names the profile, with reason denied_by_profile.
	DeniedByProfile string `json:"denied_by_profile,omitempty"`
	// Count is the caller's drives, one being deleted included.
	Count int `json:"count"`
	// Limit is the switch's drives per person; 0 when the switch is off.
	Limit int `json:"limit"`
}

// MemberDriveCreateReason is why a person cannot create a drive now. Closed;
// the TS twin is MemberDriveCreateReason in ui/src/app/lib/api/me-drives.ts.
type MemberDriveCreateReason string

const (
	// MemberDriveCreateDisabled: the org switch is off, or drives are off.
	MemberDriveCreateDisabled MemberDriveCreateReason = "disabled"
	// MemberDriveCreateDeniedByProfile: deny_member_drives or deny_user_drive.
	MemberDriveCreateDeniedByProfile MemberDriveCreateReason = "denied_by_profile"
	// MemberDriveCreateLimitReached: Count has reached Limit.
	MemberDriveCreateLimitReached MemberDriveCreateReason = "limit_reached"
	// MemberDriveCreateUnsupported: no managed backend for this deployment.
	MemberDriveCreateUnsupported MemberDriveCreateReason = "unsupported"
	// MemberDriveCreateDeleteUnavailable: the substrate could not delete one.
	MemberDriveCreateDeleteUnavailable MemberDriveCreateReason = "delete_unavailable"
	// MemberDriveCreateGovernance: the caller's governance could not be read.
	MemberDriveCreateGovernance MemberDriveCreateReason = "governance_unavailable"
	// MemberDriveCreateGroupsStale: the caller's group snapshot is stale.
	MemberDriveCreateGroupsStale MemberDriveCreateReason = "groups_snapshot_stale"
)

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
	// unmountable, placement_denied or not_in_pool. "" when it can.
	UnavailableReason string `json:"unavailable_reason,omitempty"`
	// Owned is true for a drive the caller created (POST /me/drives). Only an
	// owned drive takes DELETE /me/drives/{id}.
	Owned bool `json:"owned"`
}

// MemberDriveCreateRequest is POST /api/v1/me/drives: a person creates their
// own drive. The server picks the backend, the size and the mount path.
type MemberDriveCreateRequest struct {
	// Name is 1 to 128 characters with at least one letter or digit, unique
	// among the drives the caller can use.
	Name string `json:"name"`
}

// CreateMyDrive creates the caller's own drive (POST /api/v1/me/drives).
func (c *Client) CreateMyDrive(ctx context.Context, name string) (MeDrive, error) {
	var out MeDrive
	err := c.do(ctx, http.MethodPost, "/api/v1/me/drives", MemberDriveCreateRequest{Name: name}, &out)
	return out, err
}

// DeleteMyDrive deletes the caller's own drive and its files
// (DELETE /api/v1/me/drives/{id}).
func (c *Client) DeleteMyDrive(ctx context.Context, id uuid.UUID) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/me/drives/"+id.String(), nil, nil)
}
