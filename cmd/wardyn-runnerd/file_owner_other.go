// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package main

import "io/fs"

// fileOwner has no numeric owner to report off unix, so the owner warning never fires there.
func fileOwner(fs.FileInfo) (int, bool) { return 0, false }
