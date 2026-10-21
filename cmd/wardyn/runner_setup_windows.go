// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import "os"

// fileOwnerUID says nothing on Windows, where `wardyn runner setup` refuses before it asks.
func fileOwnerUID(os.FileInfo) int { return -1 }
