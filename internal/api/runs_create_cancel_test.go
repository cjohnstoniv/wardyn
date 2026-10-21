// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"

	"github.com/google/uuid"
)

// A run tracked again while an earlier track of it is still unwinding (a queue start whose run went back
// to its queue and was promoted again) keeps the newer registration: the older one's unregister removes
// only its own, so a kill still cancels the newer create.
func TestInflightCreates_AnOlderUntrackLeavesTheNewerSlot(t *testing.T) {
	var c inflightCreates
	run := uuid.New()
	older, untrackOlder := c.track(t.Context(), run)
	newer, untrackNewer := c.track(t.Context(), run)
	defer untrackNewer()
	untrackOlder()
	if older.Err() == nil {
		t.Error("the older create was not cancelled by its own unregister")
	}
	c.cancel(run)
	if newer.Err() == nil {
		t.Fatal("a cancel after the older unregister did not reach the newer create: its slot was removed")
	}
}
