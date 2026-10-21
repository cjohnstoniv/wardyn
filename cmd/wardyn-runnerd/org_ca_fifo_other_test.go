// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package main

import "testing"

func makeAnchorFIFO(t *testing.T, _ string) {
	t.Helper()
	t.Skip("no FIFOs on this platform")
}
