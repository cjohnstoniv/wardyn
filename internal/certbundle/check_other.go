// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package certbundle

import "os"

// CheckFile on a platform without Unix modes checks only that the file
// exists: Windows guards a per-user profile directory with its ACL, which
// os.FileMode does not report.
func CheckFile(path string) error {
	_, err := os.Stat(path)
	return err
}
