// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// A person's own team drive (#3459): POST /me/drives with team. The other refusals are the member drive ones.
const (
	reasonMemberTeamDrivesDisabled   = "member_team_drives_disabled"   // storage.user_drive.team_drives or member_create.team_drives is off
	reasonMemberTeamDriveUnsupported = "member_team_drive_unsupported" // not a Docker deployment, or the request names a Mac
	reasonMemberTeamDriveNoUnit      = "member_team_drive_no_unit"     // the person is in no governance unit, or in Default
)
