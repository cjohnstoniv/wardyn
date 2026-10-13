// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording

import "testing"

// CastSaveSlots is the number of PGStore saves admitted at once.
var CastSaveSlots = cap(castSaveSlots)

// CastSaveSlotsHeld reports how many admission slots are held right now.
func CastSaveSlotsHeld() int { return len(castSaveSlots) }

// OnCastSaveWait calls f each time a save starts waiting for a slot.
func OnCastSaveWait(t testing.TB, f func()) {
	old := castSaveWait
	castSaveWait = f
	t.Cleanup(func() { castSaveWait = old })
}
