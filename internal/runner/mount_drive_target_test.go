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
		want    string
	}{
		{"Team Data", types.DriveBackendDockerVolume, true, DriveTarget},
		{"Team Data", types.DriveBackendHostPath, true, DriveTarget},
		{"Team Data", types.DriveBackendDockerVolume, false, DrivesRoot + "/team-data"},
		{"Team Data", types.DriveBackendK8sPVC, false, DrivesRoot + "/team-data"},
		{"Team Data", types.DriveBackendHostPath, false, SharedRoot + "/team-data"},
		{"Team Data", types.DriveBackendK8sPVCStatic, false, SharedRoot + "/team-data"},
		{"../..", types.DriveBackendHostPath, false, ""},
	} {
		d := types.ResolvedDrive{Drive: types.UserDrive{Name: c.name, Backend: c.backend}}
		got := DriveTargetFor(d, c.legacy)
		if got != c.want {
			t.Errorf("DriveTargetFor(%q, %s, legacy=%v) = %q, want %q", c.name, c.backend, c.legacy, got, c.want)
		}
		if got != "" {
			if err := ValidateDriveTarget(got, c.backend); err != nil {
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
	}{
		{DrivesRoot, types.DriveBackendDockerVolume},
		{DrivesRoot + "/", types.DriveBackendDockerVolume},
		{DrivesRoot + "/a/b", types.DriveBackendDockerVolume},
		{DrivesRoot + "/Team", types.DriveBackendDockerVolume},
		{DrivesRoot + "/../.claude", types.DriveBackendDockerVolume},
		{SharedRoot + "/team", types.DriveBackendDockerVolume},
		{DrivesRoot + "/team", types.DriveBackendHostPath},
		{"/home/agent", types.DriveBackendHostPath},
		{"/work", types.DriveBackendK8sPVC},
	} {
		if err := ValidateDriveTarget(c.target, c.backend); err == nil {
			t.Errorf("ValidateDriveTarget(%q, %s) = nil, want a refusal", c.target, c.backend)
		}
	}
}

// TestValidateDriveTarget_WorkFolderManagedOnly: a managed drive may mount at
// the work folder (keep_work_folder); a share never may, nor anywhere above it.
func TestValidateDriveTarget_WorkFolderManagedOnly(t *testing.T) {
	for _, b := range []types.DriveBackend{types.DriveBackendDockerVolume, types.DriveBackendK8sPVC} {
		if err := ValidateDriveTarget("/home/agent/work", b); err != nil {
			t.Errorf("ValidateDriveTarget(/home/agent/work, %s) = %v, want nil", b, err)
		}
	}
	for _, c := range []struct {
		target  string
		backend types.DriveBackend
	}{
		{"/home/agent/work", types.DriveBackendHostPath},
		{"/home/agent/work", types.DriveBackendK8sPVCStatic},
		{"/home/agent", types.DriveBackendDockerVolume},
		{"/tmp", types.DriveBackendDockerVolume},
		{"/home/agent/.claude", types.DriveBackendK8sPVC},
	} {
		if err := ValidateDriveTarget(c.target, c.backend); err == nil {
			t.Errorf("ValidateDriveTarget(%q, %s) = nil, want a refusal", c.target, c.backend)
		}
	}
}
