// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build darwin && arm64 && cgo

package vzengine

import (
	"testing"
	"time"

	"github.com/Code-Hex/vz/v3"
)

// A failed Start closes done. The framework never closes states, so watch must end on done alone.
func TestWatchEndsWhenAFailedStartClosesDone(t *testing.T) {
	m := &machine{done: make(chan struct{})}
	states := make(chan vz.VirtualMachineState)
	ended := make(chan struct{})
	go func() { m.watch(states); close(ended) }()
	m.once.Do(func() { close(m.done) })
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("watch still running after a failed Start closed done")
	}
}

func TestWatchClosesDoneWhenAStartedVMStops(t *testing.T) {
	m := &machine{done: make(chan struct{})}
	states := make(chan vz.VirtualMachineState)
	go m.watch(states)
	states <- vz.VirtualMachineStateRunning
	states <- vz.VirtualMachineStateStopped
	select {
	case <-m.done:
	case <-time.After(5 * time.Second):
		t.Fatal("done not closed after the started VM stopped")
	}
}
