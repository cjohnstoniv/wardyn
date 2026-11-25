// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestPortalTeamDrive_FolderPerPortalAndDrive: a portal's folder is named from the portal and the drive, so two
// portals on one drive, one portal on two drives and a group on the same drive all name different objects; the
// same portal and drive name the same one; and a drive named with another drive's id still names its own.
func TestPortalTeamDrive_FolderPerPortalAndDrive(t *testing.T) {
	portalA, portalB := uuid.New(), uuid.New()
	d1 := UserDrive{ID: uuid.New(), Name: "shared", Backend: DriveBackendDockerVolume, ObjectScheme: DriveObjectSchemeID, Team: true}
	d2 := UserDrive{ID: uuid.New(), Name: "other", Backend: DriveBackendDockerVolume, ObjectScheme: DriveObjectSchemeID, Team: true}
	object := func(d UserDrive, p uuid.UUID) string { return DrivePortalObjectName(d, DrivePortalHome(d, p)) }

	if first, again := DrivePortalHome(d1, portalA), DrivePortalHome(d1, portalA); first != again {
		t.Fatal("DrivePortalHome is not deterministic")
	}
	home := DrivePortalHome(d1, portalA)
	if !strings.HasPrefix(home, "p-") || len(home) != 2+driveHomeHashLen {
		t.Fatalf("home = %q, want p- and %d hex", home, driveHomeHashLen)
	}
	for _, be := range []DriveBackend{DriveBackendDockerVolume, DriveBackendK8sPVCStatic} {
		if !driveHomeSegmentOK(be, home) {
			t.Errorf("%s refuses the portal home %q", be, home)
		}
	}
	group := DriveObjectName(d1, DriveTeamHome(d1))
	objects := map[string]string{
		"portal A, drive 1":        object(d1, portalA),
		"portal B, drive 1":        object(d1, portalB),
		"portal A, drive 2":        object(d2, portalA),
		"group team, drive 1":      group,
		"group team, drive 2":      DriveObjectName(d2, DriveTeamHome(d2)),
		"portal B, drive 2":        object(d2, portalB),
		"per-person home, drive 1": DriveObjectName(d1, "d-"+strings.Repeat("0", driveHomeHashLen)),
		"per-person home, drive 2": DriveObjectName(d2, "d-"+strings.Repeat("1", driveHomeHashLen)),
	}
	seen := map[string]string{}
	for what, name := range objects {
		if other, dup := seen[name]; dup {
			t.Errorf("%s and %s name one object, %q", what, other, name)
		}
		seen[name] = what
	}
	if !strings.HasPrefix(object(d1, portalA), "wardyn-portal-"+DriveObjectID(d1.ID)+"-p-") {
		t.Errorf("portal object %q is not wardyn-portal-<drive id>-p-<hash>", object(d1, portalA))
	}
	for what, name := range objects {
		if strings.HasPrefix(what, "portal") && strings.HasPrefix(name, driveObjectPrefix) {
			t.Errorf("%s: %q begins with the prefix every other drive object has", what, name)
		}
		if !strings.HasPrefix(what, "portal") && strings.HasPrefix(name, drivePortalObjectPrefix) {
			t.Errorf("%s: %q begins with the portal prefix", what, name)
		}
	}
	// The slug scheme is not what a portal's object follows: a drive on it names the same portal object as one
	// on the id scheme, and a drive named with another drive's id never shifts it.
	slug := d1
	slug.ObjectScheme = ""
	if object(slug, portalA) != object(d1, portalA) {
		t.Error("the portal object depends on the drive's object scheme")
	}
	impostor := UserDrive{ID: uuid.New(), Name: DriveObjectID(d1.ID), Backend: DriveBackendDockerVolume, Team: true}
	if object(impostor, portalA) == object(d1, portalA) {
		t.Error("a drive named with another drive's id names that drive's portal folder")
	}
}

// TestValidateUserDriveGrant_PortalSubject: a portal is named by its id in canonical lowercase form and never
// carries a directory name; the other subject types keep their rules.
func TestValidateUserDriveGrant_PortalSubject(t *testing.T) {
	id := uuid.New()
	grant := func(subject, override string) UserDriveGrant {
		return UserDriveGrant{SubjectType: DriveSubjectPortal, Subject: subject, DriveID: uuid.New(), HomeOverride: override}
	}
	g := grant(id.String(), "")
	if err := ValidateUserDriveGrant(&g); err != nil {
		t.Fatalf("a portal allocation is refused: %v", err)
	}
	if g.Subject != id.String() {
		t.Errorf("subject = %q, want %q", g.Subject, id)
	}
	for name, c := range map[string]UserDriveGrant{
		"upper case":      grant(strings.ToUpper(id.String()), ""),
		"no dashes":       grant(strings.ReplaceAll(id.String(), "-", ""), ""),
		"braces":          grant("{"+id.String()+"}", ""),
		"urn":             grant("urn:uuid:"+id.String(), ""),
		"a name":          grant("portal-one", ""),
		"empty":           grant("", ""),
		"a home override": grant(id.String(), "team"),
	} {
		if err := ValidateUserDriveGrant(&c); err == nil {
			t.Errorf("%s: accepted, want a refusal", name)
		}
	}
	if CapabilitySubjectType(DriveSubjectPortal).Valid() {
		t.Error("CapabilitySubjectType.Valid accepts portal: a capability grant, a governance row or a pool rule would learn it")
	}
}

// TestValidateTeamDriveGrant_Portal: a portal allocation is for a team drive only, may carry members_can_write
// there on a writable drive, and never a directory name; the group rules are unchanged.
func TestValidateTeamDriveGrant_Portal(t *testing.T) {
	portal := UserDriveGrant{SubjectType: DriveSubjectPortal, Subject: uuid.NewString()}
	team := UserDrive{ID: uuid.New(), Team: true, Writable: true}
	if err := ValidateTeamDriveGrant(team, portal); err != nil {
		t.Fatalf("a portal allocation of a team drive is refused: %v", err)
	}
	write := portal
	write.MembersCanWrite = true
	if err := ValidateTeamDriveGrant(team, write); err != nil {
		t.Errorf("members_can_write on a writable team drive is refused: %v", err)
	}
	for name, c := range map[string]struct {
		d UserDrive
		g UserDriveGrant
	}{
		"a per-person drive":    {UserDrive{ID: uuid.New(), Writable: true}, portal},
		"a read-only team":      {UserDrive{ID: uuid.New(), Team: true}, write},
		"a home override":       {team, UserDriveGrant{SubjectType: DriveSubjectPortal, Subject: portal.Subject, HomeOverride: "x"}},
		"a user on a team":      {team, UserDriveGrant{SubjectType: CapabilitySubjectUser, Subject: "alice"}},
		"all on a team":         {team, UserDriveGrant{SubjectType: CapabilitySubjectAll}},
		"user type on a team":   {team, UserDriveGrant{SubjectType: CapabilitySubjectUserType, Subject: "contractor"}},
		"write, per-person one": {UserDrive{ID: uuid.New(), Writable: true}, UserDriveGrant{SubjectType: DriveSubjectPortal, Subject: portal.Subject, MembersCanWrite: true}},
	} {
		if err := ValidateTeamDriveGrant(c.d, c.g); err == nil {
			t.Errorf("%s: allowed, want a refusal", name)
		}
	}
}
