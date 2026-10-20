// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestDriveTeamHome: one folder per drive, the same for every member, never a
// `d-` hash home, and a valid home on every backend a team drive may use.
func TestDriveTeamHome(t *testing.T) {
	a, b := UserDrive{ID: uuid.New()}, UserDrive{ID: uuid.New()}
	if DriveTeamHome(a) != DriveTeamHome(a) {
		t.Fatal("DriveTeamHome is not deterministic")
	}
	if DriveTeamHome(a) == DriveTeamHome(b) {
		t.Fatal("two team drives share one folder")
	}
	home := DriveTeamHome(a)
	if !strings.HasPrefix(home, "t-") || len(home) != 2+driveHomeHashLen {
		t.Fatalf("home = %q, want t- and %d hex", home, driveHomeHashLen)
	}
	for _, be := range []DriveBackend{DriveBackendHostPath, DriveBackendK8sPVCStatic, DriveBackendDockerVolume} {
		if !driveHomeSegmentOK(be, home) {
			t.Errorf("%s refuses the team home %q", be, home)
		}
	}
}

// TestValidateTeamDriveGrant: a team drive is allocated to groups only, pins no
// directory, and is read-only unless members_can_write is set on a writable
// drive; members_can_write means nothing on any other drive.
func TestValidateTeamDriveGrant(t *testing.T) {
	team := UserDrive{ID: uuid.New(), Team: true, Writable: true}
	group := UserDriveGrant{SubjectType: CapabilitySubjectGroup, Subject: "platform"}
	if err := ValidateTeamDriveGrant(team, group); err != nil {
		t.Fatalf("a group allocation of a team drive is refused: %v", err)
	}
	if group.MembersCanWrite {
		t.Fatal("an allocation's zero value can write")
	}
	for _, c := range []struct {
		name string
		d    UserDrive
		g    UserDriveGrant
	}{
		{"user", team, UserDriveGrant{SubjectType: CapabilitySubjectUser, Subject: "alice"}},
		{"all", team, UserDriveGrant{SubjectType: CapabilitySubjectAll}},
		{"user type", team, UserDriveGrant{SubjectType: CapabilitySubjectUserType, Subject: "contractor"}},
		{"home override", team, UserDriveGrant{SubjectType: CapabilitySubjectGroup, Subject: "platform", HomeOverride: "x"}},
		{"write on a read-only team drive", UserDrive{ID: uuid.New(), Team: true},
			UserDriveGrant{SubjectType: CapabilitySubjectGroup, Subject: "platform", MembersCanWrite: true}},
		{"write on a per-person drive", UserDrive{ID: uuid.New(), Writable: true},
			UserDriveGrant{SubjectType: CapabilitySubjectGroup, Subject: "platform", MembersCanWrite: true}},
	} {
		if err := ValidateTeamDriveGrant(c.d, c.g); err == nil {
			t.Errorf("%s: allowed, want a refusal", c.name)
		}
	}
	// A per-person drive keeps every allocation it had.
	if err := ValidateTeamDriveGrant(UserDrive{ID: uuid.New()}, UserDriveGrant{SubjectType: CapabilitySubjectAll}); err != nil {
		t.Errorf("a per-person drive's allocation is refused: %v", err)
	}
}

// TestValidateUserDriveTeamBackends: a team drive needs storage a group can
// share, so a ReadWriteOnce k8s_pvc claim is refused.
func TestValidateUserDriveTeamBackends(t *testing.T) {
	d := UserDrive{Name: "data", Backend: DriveBackendK8sPVC, SizeMiB: 1, Team: true}
	if err := ValidateUserDrive(&d, "k8s"); err == nil {
		t.Error("a team k8s_pvc drive validated")
	}
	d = UserDrive{Name: "data", Backend: DriveBackendK8sPVCStatic, HomeTemplate: HomeTemplateSub, Team: true}
	if err := ValidateUserDrive(&d, "k8s"); err != nil {
		t.Errorf("a team k8s_pvc_static drive is refused: %v", err)
	}
	d = UserDrive{Name: "data", Backend: DriveBackendDockerVolume, Team: true}
	if err := ValidateUserDrive(&d, "docker"); err != nil {
		t.Errorf("a team docker_volume drive is refused: %v", err)
	}
}
