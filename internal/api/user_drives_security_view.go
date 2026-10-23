// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "github.com/cjohnstoniv/wardyn/internal/types"

// The security admin's read of GET /drives is an ALLOW-LIST: every value below
// is built fresh from named fields, so a field added to a drive, a grant or the
// response later is absent from this tier's answer until someone copies it here
// on purpose. user_drives_security_view_test.go walks the types and fails on a
// field that is in neither the copied set nor the super-only set.
//
// Withheld from a security admin: a drive's host_root and storage_class (the
// host and the cluster), its owner subject and its delete tombstone, and the
// two deployment facts about host roots and the runner target.

// securityAdminDrive copies the fields of a drive a security admin may read.
func securityAdminDrive(d types.UserDrive) types.UserDrive {
	return types.UserDrive{
		ID:           d.ID,
		Name:         d.Name,
		Backend:      d.Backend,
		HomeTemplate: d.HomeTemplate,
		ObjectScheme: d.ObjectScheme,
		SizeMiB:      d.SizeMiB,
		Writable:     d.Writable,
		Reclaim:      d.Reclaim,
		CreatedAt:    d.CreatedAt,
		UpdatedAt:    d.UpdatedAt,
		CreatedBy:    d.CreatedBy,
	}
}

// securityAdminGrant copies the fields of an allocation a security admin may read.
func securityAdminGrant(g types.UserDriveGrant) types.UserDriveGrant {
	var writable *bool
	if g.WritableOverride != nil {
		v := *g.WritableOverride
		writable = &v
	}
	return types.UserDriveGrant{
		ID:               g.ID,
		SubjectType:      g.SubjectType,
		Subject:          g.Subject,
		DriveID:          g.DriveID,
		Priority:         g.Priority,
		SizeMiBOverride:  g.SizeMiBOverride,
		WritableOverride: writable,
		HomeOverride:     g.HomeOverride,
		Enabled:          g.Enabled,
		CreatedAt:        g.CreatedAt,
		CreatedBy:        g.CreatedBy,
	}
}

// securityAdminDrivesView is GET /drives's body for a caller who is not a super
// admin.
func securityAdminDrivesView(in userDrivesResponse) userDrivesResponse {
	out := userDrivesResponse{
		Drives:     make([]types.UserDriveListItem, len(in.Drives)),
		Grants:     make([]types.UserDriveGrant, len(in.Grants)),
		Disabled:   in.Disabled,
		GrantTotal: in.GrantTotal,
	}
	for i, d := range in.Drives {
		out.Drives[i] = types.UserDriveListItem{UserDrive: securityAdminDrive(d.UserDrive), GrantCount: d.GrantCount}
	}
	for i, g := range in.Grants {
		out.Grants[i] = securityAdminGrant(g)
	}
	return out
}
