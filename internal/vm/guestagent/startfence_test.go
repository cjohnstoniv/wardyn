// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"errors"
	"testing"
	"time"
)

// The stop scrub's close waits for a start the fence already let through, and refuses every start after
// it. Catches a scrub that runs while an admitted exec is still starting its process, and an exec that
// starts on the scrubbed guest.
func TestStartFenceWaitsForAnAdmittedStartAndRefusesLaterOnes(t *testing.T) {
	var f startFence
	stopping := errors.New("stopping")
	if err := f.enter(); err != nil {
		t.Fatalf("enter before close = %v", err)
	}
	closed := make(chan struct{})
	go func() { f.close(stopping); close(closed) }()
	select {
	case <-closed:
		t.Fatal("close returned while a start it let through was still under way")
	case <-time.After(100 * time.Millisecond):
	}
	f.leave()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("close did not return after the start left")
	}
	if err := f.enter(); !errors.Is(err, stopping) {
		t.Fatalf("enter after close = %v, want the close error", err)
	}
}
