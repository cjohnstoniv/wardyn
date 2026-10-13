// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

// SetMemberDriveCounted installs f between CreateMemberDrive's count and its
// insert for the length of one test.
func SetMemberDriveCounted(f func()) (restore func()) {
	prev := memberDriveCounted
	memberDriveCounted = f
	return func() { memberDriveCounted = prev }
}
