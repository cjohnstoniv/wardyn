// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package cliutil

import "os"

// fileOwnerUID is always -1 on Windows: a file has no uid there.
func fileOwnerUID(os.FileInfo) int { return -1 }
