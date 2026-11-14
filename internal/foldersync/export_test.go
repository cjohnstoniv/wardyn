// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package foldersync

// SwapChildLooked sets the hook openChild runs between its look at an entry and its open, for a
// test outside this package, and returns the call that puts the old one back.
func SwapChildLooked(f func()) (restore func()) {
	old := childLooked
	childLooked = f
	return func() { childLooked = old }
}
