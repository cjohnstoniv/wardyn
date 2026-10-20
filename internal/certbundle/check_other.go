// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package certbundle

import (
	"fmt"
	"os"
)

// openChecked on a platform without Unix modes checks only that the target is
// a regular file: Windows guards a per-user profile directory with its ACL,
// which os.FileMode does not report.
func openChecked(path string) (*os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err == nil && !fi.Mode().IsRegular() {
		err = fmt.Errorf("refusing the CA file %s: not a regular file", path)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
