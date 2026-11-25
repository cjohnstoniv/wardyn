// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"testing"

	"github.com/google/uuid"
)

// TestDriveClaimNamedByWardyn: a runner mounts only a claim named as Wardyn derives, whichever scheme the
// drive's name takes, and a portal's folder; a name near either prefix is not one.
func TestDriveClaimNamedByWardyn(t *testing.T) {
	byID := UserDrive{ID: uuid.New(), Name: "Team drive", Backend: DriveBackendK8sPVCStatic, ObjectScheme: DriveObjectSchemeID}
	bySlug := UserDrive{ID: uuid.New(), Name: "research", Backend: DriveBackendK8sPVCStatic}
	for name, want := range map[string]bool{
		DriveObjectName(byID, "d-9f3a1c"):                              true,
		DriveObjectName(bySlug, "d-9f3a1c"):                            true,
		DrivePortalObjectName(UserDrive{ID: uuid.New()}, "p-0a1b2c3d"): true,
		"data":             false,
		"wardyn-":          false,
		"wardyndrive-x":    false,
		"x-wardyn-drive-y": false,
	} {
		if got := DriveClaimNamedByWardyn(name); got != want {
			t.Errorf("DriveClaimNamedByWardyn(%q) = %v, want %v", name, got, want)
		}
	}
}
