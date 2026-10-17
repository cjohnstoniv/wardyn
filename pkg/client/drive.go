// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// MemberDriveAdminItem is one person's drive in GET /api/v1/drives/member.
type MemberDriveAdminItem struct {
	ID           uuid.UUID          `json:"id"`
	Name         string             `json:"name"`
	Owner        string             `json:"owner"`
	Backend      types.DriveBackend `json:"backend"`
	SizeMiB      int                `json:"size_mib,omitempty"`
	StorageClass string             `json:"storage_class,omitempty"`
	// Object is the Docker volume or claim name the drive stores into.
	Object    string    `json:"object"`
	CreatedAt time.Time `json:"created_at"`
	// Deleting is true while a delete of the drive is in flight.
	Deleting bool `json:"deleting"`
}

// MemberDriveList is GET /api/v1/drives/member: one page of every person's
// drives, by owner then name, and how many there are in all.
type MemberDriveList struct {
	Drives []MemberDriveAdminItem `json:"drives"`
	Total  int                    `json:"total"`
}

// MemberDriveDeleteResult is DELETE /api/v1/drives/member/{id}'s answer: the
// same facts its drive.member.delete audit row carries. Outcome is deleted or
// already_absent.
type MemberDriveDeleteResult struct {
	Drive   string             `json:"drive"`
	DriveID uuid.UUID          `json:"drive_id"`
	Owner   string             `json:"owner"`
	Backend types.DriveBackend `json:"backend"`
	Object  string             `json:"object"`
	Outcome string             `json:"outcome"`
}

// ListMemberDrives returns one page of every person's drives
// (GET /api/v1/drives/member, super admin).
func (c *Client) ListMemberDrives(ctx context.Context, opts ListOpts) (MemberDriveList, error) {
	var out MemberDriveList
	err := c.do(ctx, http.MethodGet, appendListOpts("/api/v1/drives/member", []ListOpts{opts}), nil, &out)
	return out, err
}

// DeleteMemberDrive deletes one person's drive and its files
// (DELETE /api/v1/drives/member/{id}, super admin).
func (c *Client) DeleteMemberDrive(ctx context.Context, id uuid.UUID) (MemberDriveDeleteResult, error) {
	var out MemberDriveDeleteResult
	err := c.do(ctx, http.MethodDelete, "/api/v1/drives/member/"+id.String(), nil, &out)
	return out, err
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
