// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The Windows VM reasons: the same closed set and convention as reasons.go, in a file of their own beside
// run_winvm.go and windows_licences.go. TestReasonDocsMatchReasonsGo reads this file too.
const (
	reasonWindowsVMsOff              = "windows_vms_off"               // features.windows_vm is not on: no Windows VM launch, and no pool, holder or image added
	reasonWindowsImageRequired       = "windows_image_required"        // a Windows VM run names no registered image in windows_image, or names a raw image
	reasonWindowsImageUnregistered   = "windows_image_unregistered"    // no active registered Windows image of that name, or its pool was deleted
	reasonWindowsLicenceNotHolder    = "windows_licence_not_holder"    // the caller holds no seat in the image's licence pool
	reasonWindowsLicenceInUse        = "windows_licence_in_use"        // the caller's seat in the pool is held by another live run
	reasonWindowsLicencePoolFull     = "windows_licence_pool_full"     // every seat in the pool already has a holder
	reasonWindowsLicenceSeatLive     = "windows_licence_seat_live"     // a live run holds a seat through the holder, pool or image a write would change
	reasonWindowsVMFeatureRefused    = "windows_vm_feature_refused"    // a Windows VM run is desktop-only: the named feature or door is refused
	reasonWindowsVMRunnerUnsupported = "windows_vm_runner_unsupported" // the run's runner is not an organisation runner that offers Windows VM sandboxes
	reasonWindowsVMFenced            = "windows_vm_fenced"             // the database's Windows licence fence refused a write to the run; no route should reach it
	// The licence pool and image routes.
	reasonWindowsLicencesUnavailable = "windows_licences_unavailable" // 501: the store keeps no Windows licence pools (not the Postgres store)
	reasonWindowsLicenceInvalid      = "windows_licence_invalid"      // a pool, holder or image body or id is malformed
	reasonWindowsLicenceNotFound     = "windows_licence_not_found"    // no active pool, holder or image with that id
	reasonWindowsLicenceConflict     = "windows_licence_conflict"     // a name, ref or holder already taken, a stale revision, or seats below the holder count
)
