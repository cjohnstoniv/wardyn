// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording

import (
	"context"
	"io"
	"testing"
	"time"
)

// CastSaveSlots is the number of PGStore stream saves admitted at once.
var CastSaveSlots = cap(streamSaveSlots)

// CastSaveSlotsHeld reports how many stream save slots are held right now.
func CastSaveSlotsHeld() int { return len(streamSaveSlots) }

// InMemorySaveSlotHeld reports whether the in-memory save slot is held.
func InMemorySaveSlotHeld() bool { return len(inMemorySaveSlot) > 0 }

// OnCastSaveWait calls f each time a save starts waiting for a slot.
func OnCastSaveWait(t testing.TB, f func()) {
	old := castSaveWait
	castSaveWait = f
	t.Cleanup(func() { castSaveWait = old })
}

// ShortenCastSaveAdmitWait sets the admission wait to d for one test.
func ShortenCastSaveAdmitWait(t testing.TB, d time.Duration) {
	old := castSaveAdmitWait
	castSaveAdmitWait = d
	t.Cleanup(func() { castSaveAdmitWait = old })
}

// ReadCast reads r the way SaveCast buffers a cast.
func ReadCast(r io.Reader) ([]byte, error) {
	return readCapped(r, maxCastBytes, castBufferStart(r))
}

// HoldRun is holdRun.
func HoldRun(ctx context.Context, run string) (func(), error) { return holdRun(ctx, run) }
