// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// driveRehomedAllocations is how many of the drive's allocations an edit moves, and the identity changes that move
// them, by what each kind of allocation's storage object is named from. A person's or group's on an ordinary drive is
// named by all five fields. A group's on a team drive is the team folder, named by the drive's id: the home template
// never reaches it, the host root only on a host path, and the name only where the drive's object scheme puts it in the
// object name of a placement Wardyn names (driveNamesAnObject: not a host path, which is the root and the home). A portal's is named by the portal and drive ids alone, so only a backend change (a Docker volume
// becoming a claim) moves it. The store asserts the same rule under its row lock (driveMovesAnAllocationPred in internal/store).
func driveRehomedAllocations(before types.UserDriveListItem, after types.UserDrive, changes []string) (int, []string) {
	members := before.GrantCount - before.PortalGrantCount
	backendMoved := before.Backend != after.Backend
	relevant := func(change string) bool { return true }
	if before.Team {
		named := driveNamesAnObject(before.UserDrive) || driveNamesAnObject(after)
		relevant = func(change string) bool {
			switch {
			case strings.HasPrefix(change, "home_template "):
				return false
			case strings.HasPrefix(change, "host_root "):
				return after.Backend == types.DriveBackendHostPath
			case strings.HasPrefix(change, "name "), strings.HasPrefix(change, "object_scheme "):
				return named
			}
			return true
		}
	}
	var fields []string
	for _, c := range changes {
		if (members > 0 && relevant(c)) || (before.PortalGrantCount > 0 && backendMoved && strings.HasPrefix(c, "backend ")) {
			fields = append(fields, c)
		}
	}
	moved := 0
	if members > 0 {
		for _, c := range fields {
			if relevant(c) {
				moved += members
				break
			}
		}
	}
	if before.PortalGrantCount > 0 && backendMoved {
		moved += before.PortalGrantCount
	}
	return moved, fields
}

// driveNamesAnObject reports whether any place of d, the first or the second, is an object Wardyn names (a Docker
// volume or a claim), whose name carries the drive's slug under the slug scheme. A host path is the root and the home,
// so on a drive that is only host paths a rename moves nothing. The store's form is driveMovesAnAllocationPred.
func driveNamesAnObject(d types.UserDrive) bool {
	return types.DriveObjectNamedByWardyn(d.Backend) || (d.AltBackend != "" && types.DriveObjectNamedByWardyn(d.AltBackend))
}
