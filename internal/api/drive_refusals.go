// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "fmt"

// The sentences of the drives[] refusals. The console uses the same bytes
// (ui/src/app/lib/drive-refusals.ts); TestDriveRefusalSentencesMatchGolden pins
// both sides to ui/src/app/lib/drive-refusals.golden.json. Change a sentence in
// both files and the table together.

func driveSelectionConflictMsg() string { return "Send drive or drives, not both." }

func driveInvalidMsg(i int) string {
	return fmt.Sprintf("drives[%d].drive_id: give a drive id from your drive list, once.", i)
}

func driveTooManyMsg(max int) string { return fmt.Sprintf("drives: attach at most %d drives.", max) }

// driveNotFoundMsg echoes the id as sent: the same bytes for another person's
// drive and for an id that names nothing.
func driveNotFoundMsg(id string) string { return fmt.Sprintf("No drive %s is available to you.", id) }

func driveNotInPoolMsg(name string) string {
	return fmt.Sprintf("%s: not available in this pool's runners.", name)
}

func driveTargetConflictMsg(a, b, target string) string {
	return fmt.Sprintf("%s and %s both mount at %s. Attach one of them.", a, b, target)
}

func driveShareBesideManagedMsg(share, managed string) string {
	return fmt.Sprintf("%s is a shared drive and %s a managed one; a Kubernetes run mounts one kind or the other. Attach one of them.", share, managed)
}
