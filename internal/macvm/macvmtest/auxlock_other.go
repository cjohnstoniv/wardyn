// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package macvmtest

import "os"

// lockAux opens path; this OS has no flock, so it locks nothing.
func lockAux(path string) (*os.File, error) { return os.OpenFile(path, os.O_RDWR, 0) }
