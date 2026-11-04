// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

// The sentences of an attended window onto a macOS VM run that both ends of the grant say in the same
// words: the control plane refuses a second window on a run from its registry, and the run's runner refuses a
// second grant's open from its own. A client shows whichever it gets, so they must not differ.

// MacWindowOpenMsg is the refusal of a window opened on a run that already has one, on the computer named.
func MacWindowOpenMsg(runner string) string {
	return "This run's window is already open on " + runner + "."
}

// MacWindowNotAtMacMsg is the refusal of a window the runner could not put on the person's screen: they are
// not signed in at that computer's screen, or it is locked.
func MacWindowNotAtMacMsg(runner string) string {
	return "The window didn't open: you aren't signed in at " + runner + "'s screen, or it is locked. Unlock it and open the window again."
}
