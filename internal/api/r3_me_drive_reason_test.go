// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestMeUnavailableReasonPrecedence is F273.
//
// PF-26's own motivating case is a TRUNCATED group snapshot, and it fails BOTH
// resolves: the drive resolver names it groups_snapshot_stale — the same token
// the launch path's 403 carries, whose remedy the member can perform — and the
// ceiling resolve then fails for the identical reason. /me overwrote the
// specific token with governance_unavailable unconditionally, so it told the
// member "governance is unavailable" (wait, or ask an operator) while POST /runs
// told them "sign in again (or re-mint your API token)". The member was shown
// the one remedy that is not theirs.
func TestMeUnavailableReasonPrecedence(t *testing.T) {

	t.Run("a truncated snapshot keeps the launch path's own token", func(t *testing.T) {
		// The shape TestSeedRequestDriveTruncatedGroupsIs403 uses for "nothing
		// matches on user subjects alone": the group snapshot is unreadable AND
		// a group-tier grant exists, so the drive resolver refuses rather than
		// serving an answer that was decided by which groups happened to fit.
		// hasGroupTierAssignments makes the CEILING resolve fail for the same
		// reason, which is what makes this the one state where the two failures
		// carry different tokens.
		st := &driveStore{
			hasGroupTier: true, userTierOnly: true, hasGroupTierAssignments: true,
		}
		srv, _ := driveRunServer(st, "docker")
		ud, denied, reason := meDriveBody(t, srv, driveMemberCtx(nil, true))

		if reason != driveUnavailableGroups {
			t.Errorf("user_drive_unavailable = %q, want %q — the launch refuses this member with "+
				"groups_snapshot_stale, whose remedy is theirs (sign in again / re-mint the token); "+
				"governance_unavailable tells them to wait for an operator instead", reason, driveUnavailableGroups)
		}
		// The suppression is unconditional and stays: an unknown door must never
		// ship beside an allocation, whichever reason names it.
		if ud != nil {
			t.Errorf("user_drive = %v, want null — an unknown door must not ship beside an allocation", ud)
		}
		if denied != "" {
			t.Errorf("user_drive_denied_by_profile = %q, want empty — no profile was read", denied)
		}
	})
}

// TestMeBlankNamedDenyProfile is F274.
//
// governance_profiles.name is TEXT NOT NULL UNIQUE with no non-empty CHECK, so a
// profile with DenyUserDrive set and a blank name reports ("", true) from
// driveDoorProfile. userDriveDeniedByProfile discarded the bool, so the name key
// shipped "" — which the documented contract reads as "no profile denies you" —
// beside a fully populated, WRITABLE user_drive, while POST /runs with
// drive.enabled answered 403 'mounting a user drive is not allowed by your
// governance profile ""'. That is the same fail-open driveDoorShut's bool was
// introduced to close, left standing at the sibling call site.
func TestMeBlankNamedDenyProfile(t *testing.T) {
	d := driveFixture(func(d *types.UserDrive) { d.Writable = true })

	t.Run("a shut door with no name to quote does not read as open", func(t *testing.T) {
		st := &driveStore{
			drive: d, grant: grantFixture(d.ID, nil), tier: types.CapabilitySubjectUser,
			profile: &types.GovernanceProfile{Name: "", Limits: types.GovernanceLimits{DenyUserDrive: true}},
		}
		srv, _ := driveRunServer(st, "docker")
		ud, denied, _ := meDriveBody(t, srv, driveMemberCtx([]string{"eng"}, false))

		if ud != nil {
			t.Errorf("user_drive = %v — a WRITABLE allocation shipped beside a door the launch path refuses; "+
				"the card draws a checkbox for a mount POST /runs answers 403 for", ud)
		}
		if denied != "" {
			t.Errorf("user_drive_denied_by_profile = %q, want empty — there is no name to quote, which is "+
				"exactly why this state takes the door-unknown path instead", denied)
		}
	})

	// A NAMED deny is untouched: it keeps shipping the profile name beside the
	// allocation, which is the four-state doctrine working as designed — an
	// allocation and a door are different facts, and a member with a drive and a
	// shut door needs to see both.
	t.Run("a named deny still names the profile and keeps the allocation", func(t *testing.T) {
		st := &driveStore{
			drive: d, grant: grantFixture(d.ID, nil), tier: types.CapabilitySubjectUser,
			profile: &types.GovernanceProfile{Name: "contractors", Limits: types.GovernanceLimits{DenyUserDrive: true}},
		}
		srv, _ := driveRunServer(st, "docker")
		ud, denied, _ := meDriveBody(t, srv, driveMemberCtx([]string{"eng"}, false))
		if denied != "contractors" {
			t.Errorf("user_drive_denied_by_profile = %q, want %q", denied, "contractors")
		}
		if ud == nil {
			t.Error("a named deny suppressed the allocation — the two keys are deliberately independent, and a " +
				"member with a drive AND a shut door needs to see both")
		}
	})
}
