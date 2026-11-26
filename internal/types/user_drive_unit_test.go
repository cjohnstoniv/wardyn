// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"testing"

	"github.com/google/uuid"
)

// TestValidateTeamDriveGrant_UnitTierOnMemberRowsOnly: the unit tier is a person's own team drive's allocation, so
// it passes on a team drive with an owner and is refused on an admin team drive; a group allocation is unchanged.
func TestValidateTeamDriveGrant_UnitTierOnMemberRowsOnly(t *testing.T) {
	unit := UserDriveGrant{SubjectType: DriveSubjectUnit, Subject: uuid.NewString()}
	member := UserDrive{ID: uuid.New(), Team: true, Writable: true, OwnerSubject: "alice"}
	admin := UserDrive{ID: uuid.New(), Team: true, Writable: true}
	if err := ValidateTeamDriveGrant(member, unit); err != nil {
		t.Fatalf("the unit tier on a person's own team drive is refused: %v", err)
	}
	write := unit
	write.MembersCanWrite = true
	if err := ValidateTeamDriveGrant(member, write); err != nil {
		t.Errorf("members_can_write on a person's own writable team drive is refused: %v", err)
	}
	if err := ValidateTeamDriveGrant(admin, unit); err == nil {
		t.Error("the unit tier on an admin team drive is allowed, want a refusal")
	}
	if err := ValidateTeamDriveGrant(admin, UserDriveGrant{SubjectType: CapabilitySubjectGroup, Subject: "eng"}); err != nil {
		t.Errorf("a group allocation of an admin team drive is refused: %v", err)
	}
	if DriveSubjectUnit.Valid() {
		t.Error("CapabilitySubjectType.Valid knows the unit tier; it is never stored")
	}
}
