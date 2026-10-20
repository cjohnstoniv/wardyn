// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestDriveTargetFor: the legacy drive keeps /home/agent/drive; every other
// drive is one folder below the root of its kind, named by its slug; a name
// with no slug has no path.
func TestDriveTargetFor(t *testing.T) {
	for _, c := range []struct {
		name    string
		backend types.DriveBackend
		legacy  bool
		team    bool
		want    string
	}{
		{"Team Data", types.DriveBackendDockerVolume, true, false, DriveTarget},
		{"Team Data", types.DriveBackendHostPath, true, false, DriveTarget},
		{"Team Data", types.DriveBackendDockerVolume, false, false, DrivesRoot + "/team-data"},
		{"Team Data", types.DriveBackendK8sPVC, false, false, DrivesRoot + "/team-data"},
		{"Team Data", types.DriveBackendHostPath, false, false, SharedRoot + "/team-data"},
		{"Team Data", types.DriveBackendK8sPVCStatic, false, false, SharedRoot + "/team-data"},
		{"../..", types.DriveBackendHostPath, false, false, ""},
		// A team drive is the group's folder: shared/<slug> on every backend, never the legacy path.
		{"Team Data", types.DriveBackendDockerVolume, false, true, SharedRoot + "/team-data"},
		{"Team Data", types.DriveBackendDockerVolume, true, true, SharedRoot + "/team-data"},
		{"Team Data", types.DriveBackendHostPath, true, true, SharedRoot + "/team-data"},
		{"Team Data", types.DriveBackendK8sPVCStatic, false, true, SharedRoot + "/team-data"},
	} {
		d := types.ResolvedDrive{Drive: types.UserDrive{Name: c.name, Backend: c.backend}, Team: c.team}
		got := DriveTargetFor(d, c.legacy)
		if got != c.want {
			t.Errorf("DriveTargetFor(%q, %s, legacy=%v, team=%v) = %q, want %q", c.name, c.backend, c.legacy, c.team, got, c.want)
		}
		if got != "" {
			if err := ValidateDriveTarget(&types.DriveMount{Target: got, Backend: c.backend, Team: c.team}); err != nil {
				t.Errorf("ValidateDriveTarget refuses its own derivation %q: %v", got, err)
			}
			if err := ValidateAuthoredTarget(got); err == nil {
				t.Errorf("an authored target may name the drive path %q", got)
			}
		}
	}
}

// TestValidateDriveTarget_RefusesWhatIsNotDerived: anything DriveTargetFor
// cannot produce for the backend is refused.
func TestValidateDriveTarget_RefusesWhatIsNotDerived(t *testing.T) {
	for _, c := range []struct {
		target  string
		backend types.DriveBackend
		team    bool
	}{
		// A team drive never lands on the person's own paths.
		{DriveTarget, types.DriveBackendDockerVolume, true},
		{DrivesRoot + "/team", types.DriveBackendDockerVolume, true},
		{DriveTarget, types.DriveBackendHostPath, true},
		{DrivesRoot, types.DriveBackendDockerVolume, false},
		{DrivesRoot + "/", types.DriveBackendDockerVolume, false},
		{DrivesRoot + "/a/b", types.DriveBackendDockerVolume, false},
		{DrivesRoot + "/Team", types.DriveBackendDockerVolume, false},
		{DrivesRoot + "/../.claude", types.DriveBackendDockerVolume, false},
		{SharedRoot + "/team", types.DriveBackendDockerVolume, false},
		{DrivesRoot + "/team", types.DriveBackendHostPath, false},
		{"/home/agent", types.DriveBackendHostPath, false},
		{"/work", types.DriveBackendK8sPVC, false},
	} {
		if err := ValidateDriveTarget(&types.DriveMount{Target: c.target, Backend: c.backend, Team: c.team}); err == nil {
			t.Errorf("ValidateDriveTarget(%q, %s, team=%v) = nil, want a refusal", c.target, c.backend, c.team)
		}
	}
}
