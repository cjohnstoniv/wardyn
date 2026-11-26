// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The seed step's reasons (POST /me/drives/{id}/seed).
const (
	reasonDriveSeedInvalid               = "drive_seed_invalid"                 // the body names no workspace or repository, or a bad ref
	reasonDriveSeedIntoInvalid           = "drive_seed_into_invalid"            // into is not one relative sub-folder
	reasonDriveSeedUnsupported           = "drive_seed_unsupported"             // a deleting, unsettled, team, disk, host or share drive, or a repository not on GitHub over HTTPS
	reasonDriveSeedOwnCredentialRequired = "drive_seed_own_credential_required" // no own token or no ceiling pairing for github.com, a grant the run would drop, or a run that did not get the own token
	reasonDriveSeedInUse                 = "drive_seed_in_use"                  // a sandbox mounts the drive, or a run of the owner is starting
	reasonDriveSeedUnavailable           = "drive_seed_unavailable"             // the substrate cannot say whether a sandbox holds the drive
	reasonDriveSeedNoCapacity            = "drive_seed_no_capacity"             // the run would wait in a pool queue, and a fill never waits
)
