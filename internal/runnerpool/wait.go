// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerpool

import "fmt"

// WaitReason says why a run is waiting for a slot rather than starting. It is a
// run's state, never a refusal: it has no HTTP status, is not in Reasons, and a
// request is never answered with it.
type WaitReason string

// WaitAtCapacity: the pool is running its concurrent-run cap, so the run waits
// for one of its slots.
const WaitAtCapacity WaitReason = "runner_pool_at_capacity"

// Wait is the answer when a new run must queue behind a pool's cap. Whoever owns
// the queue records Reason on the queued run and shows Sentence; this package
// does not queue.
type Wait struct {
	Pool string
	Max  int
}

func (w *Wait) Reason() WaitReason { return WaitAtCapacity }

// Sentence is what the run says while it waits.
func (w *Wait) Sentence() string { return WaitAtCapacityMsg(w.Pool, w.Max) }

// WaitAtCapacityMsg is the sentence of a run waiting behind the pool's cap.
func WaitAtCapacityMsg(pool string, max int) string {
	return fmt.Sprintf("%s is running its limit of %d. This run starts when one finishes.", pool, max)
}
