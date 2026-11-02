// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package main

import (
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// waitFullPass follows the pass, not a deadline: a pass that takes longer than any fixed wait a test could
// pick, as one on a loaded hosted runner does, is still waited for. It is parallel so its wait does not
// add to the package's serial time.
func TestWaitFullPassFollowsThePassNotADeadline(t *testing.T) {
	t.Parallel()
	h := newSinkHarness(t)
	e := &endCalls{isVM: true, hold: make(chan struct{})}
	endVMsOnFullSpool(h.sink, &runnerwire.VMCaps{}, e.end)
	h.fillSpool(false)
	d := &dockerSink{spoolSink: h.sink}
	d.extras(nil, nil).options.Tick(t.Context())
	time.AfterFunc(6*time.Second, func() { close(e.hold) })
	waitFullPass(t, d)
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.runs) != 1 {
		t.Fatalf("the pass ended %d runs once it finished, want 1", len(e.runs))
	}
}
