// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

// Package spoolowner keeps every file in a run's spool directory with the directory's owner.
package spoolowner

import "os"

// Keep is a no-op where files carry no owning user to hand over.
func Keep(*os.Root, *os.File) error { return nil }
