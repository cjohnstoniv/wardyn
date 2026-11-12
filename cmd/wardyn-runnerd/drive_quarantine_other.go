// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix && !darwin

package main

import "os"

// driveOpenMark is the quarantine flag where there is none: only a Mac reads it, and macOS VM drives
// exist only there. This build is what the tests on other systems run.
func driveOpenMark(*os.File) error { return nil }
