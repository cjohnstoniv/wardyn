// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerio

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
)

// heldStart is a substrate whose first ExecStream blocks until its context ends, recording when it was
// called and when that context ended; later ones are the fake's.
type heldStart struct {
	contextSubstrate
	held   atomic.Bool
	called chan time.Time
	ended  chan time.Time
}

func (h *heldStart) ExecStream(ctx context.Context, ref string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	if !h.held.CompareAndSwap(false, true) {
		return h.contextSubstrate.ExecStream(ctx, ref, spec)
	}
	h.called <- time.Now()
	<-ctx.Done()
	h.ended <- time.Now()
	return nil, ctx.Err()
}

// The org gives up on a start the runner has received and is holding, on a link that stays healthy. The
// call's RESET ends the context the production Server called its substrate with.
func TestRunnerioCall_ResetCancelsHeldStart(t *testing.T) {
	h := &heldStart{called: make(chan time.Time, 1), ended: make(chan time.Time, 1)}
	_, sub, _, rp, ref := streamRigWith(t, func(f *runnertest.Fake) substrate.Substrate {
		h.contextSubstrate = contextSubstrate{f}
		return h
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sent := time.Now()
	started := make(chan error, 1)
	go func() { _, err := sub.ExecStream(ctx, ref, runner.ExecSpec{Argv: []string{"socat"}}); started <- err }()
	select {
	case at := <-h.called:
		// The runner's own start, which Wardyn does not bound: recorded, not asserted.
		t.Logf("the substrate was called %s after the org sent the start", at.Sub(sent))
	case <-time.After(10 * time.Second):
		t.Fatal("the start never reached the runner's substrate")
	}
	gaveUp := time.Now()
	cancel() // the org abandons the call; Call sends a RESET for it
	select {
	case err := <-started:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("abandoned start = %v, want the caller's cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the abandoned start never returned to the org")
	}
	select {
	case at := <-h.ended:
		t.Logf("the substrate's context ended %s after the org gave up", at.Sub(gaveUp))
	case <-time.After(5 * time.Second):
		t.Fatal("the substrate's context stayed live after the org abandoned the call")
	}
	if !rp.Online() {
		t.Fatalf("the link went down (%v); the RESET, not a link loss, must end the start", rp.Err())
	}
	// The link still serves: a later start reaches the substrate and opens.
	later, cancelLater := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancelLater()
	e, err := sub.ExecStream(later, ref, runner.ExecSpec{Argv: []string{"true"}})
	if err != nil {
		t.Fatalf("a later start on the same link: %v", err)
	}
	_ = e.Close()
}
