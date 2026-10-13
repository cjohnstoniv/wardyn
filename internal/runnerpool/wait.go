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

// WaitNoRunnerFree: the pool has room, but none of the person's runners in it can take the run right
// now (each is offline or cannot hold it), so the run keeps its place. It is a wait for a run that was
// already queued: a pool whose candidates are all offline at create is refused, never queued.
const WaitNoRunnerFree WaitReason = "runner_pool_no_runner_free"

// WaitRunnersAtCapacity: the pool has room and the person's runners in it are online, but each already
// holds as many runs as its "Runs at once" (max_runs) allows, so the run keeps its place until one has room.
const WaitRunnersAtCapacity WaitReason = "runner_pool_runners_at_capacity"

// Valid reports whether w is a wait reason.
func (w WaitReason) Valid() bool {
	return w == WaitAtCapacity || w == WaitNoRunnerFree || w == WaitRunnersAtCapacity
}

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
	return fmt.Sprintf("%s is running its limit of %d. This run starts when there's room.", pool, max)
}

// WaitRunnersAtCapacityMsg is the sentence of a queued run whose owner's runners are all at their limit.
func WaitRunnersAtCapacityMsg(pool string) string {
	return fmt.Sprintf("Your runners in %s are all running as many runs as they allow at once. This run starts when one has room.", pool)
}

// WaitNoRunnerFreeMsg is the sentence of a queued run no runner of its owner can take yet.
func WaitNoRunnerFreeMsg(pool string) string {
	return fmt.Sprintf("None of your runners in %s is online. This run starts when one is.", pool)
}
