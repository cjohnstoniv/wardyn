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
var CastSaveSlots = streamSaveSlots.size

// CastSaveSlotsHeld reports how many stream save slots are held right now.
func CastSaveSlotsHeld() int { return streamSaveSlots.Held() }

// CastReadSlots is the number of opened casts a PGStore allows at once.
var CastReadSlots = readSlots.size

// CastReadSlotsHeld reports how many read slots are held right now.
func CastReadSlotsHeld() int { return readSlots.Held() }

// InMemorySaveSlotHeld reports whether the in-memory save slot is held.
func InMemorySaveSlotHeld() bool { return inMemorySaveSlot.Held() > 0 }

// SlotQueue is the queue saves wait in for a slot.
type SlotQueue = slotQueue

// NewSlotQueue returns a queue of size slots, apart from the stores' own.
func NewSlotQueue(size int) *SlotQueue { return &slotQueue{size: size} }

// Take is take.
func (q *slotQueue) Take(ctx context.Context) (func(), error) { return q.take(ctx) }

// Held reports how many of q's slots are held.
func (q *slotQueue) Held() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.held
}

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

// HoldGroup is holdGroup.
func HoldGroup(ctx context.Context, group string) (func(), error) { return holdGroup(ctx, group) }

// SetCastWriteChunk shrinks the size one statement of a save writes, restoring it when t ends.
func SetCastWriteChunk(t testing.TB, n int) {
	old := castWriteChunk
	castWriteChunk = n
	t.Cleanup(func() { castWriteChunk = old })
}

// SetReplayWriteTimeout sets the per-write bound of a replay and returns a restore func.
func SetReplayWriteTimeout(d time.Duration) (restore func()) {
	old := replayWriteTimeout
	replayWriteTimeout = d
	return func() { replayWriteTimeout = old }
}
