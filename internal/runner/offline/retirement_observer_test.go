// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

type heldExitSub struct {
	*localSub
	entered, release chan struct{}
}

func (s heldExitSub) AgentStatus(context.Context, string, string) (runner.Status, error) {
	close(s.entered)
	<-s.release
	code := 0
	return runner.Status{State: types.RunStopped, ExitCode: &code}, nil
}

func TestRetirementWaitsForObserverThroughDurableExit(t *testing.T) {
	m, sub, id, _, a := retirementFixture(t)
	generation := uuid.New()
	r := m.state.Runs[id]
	r.Generation = generation
	r.ExecID = "agent"
	r.Executions = map[uuid.UUID]string{generation: runnerwire.MethodExec}
	marker := *r.Containment
	marker.Generation = generation
	r.Containment = &marker
	m.state.Runs[id] = r
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	a.Retirement.Generation = generation
	a.Retirement.ExecID = "agent"
	held := heldExitSub{sub, make(chan struct{}), make(chan struct{})}
	m.sub = held
	done := make(chan error, 1)
	go func() { _, err := allState(t.Context(), m); done <- err }()
	<-held.entered
	result, err := m.apply(t.Context(), a)
	if err != nil || result.Outcome != "deferred" {
		t.Fatalf("inflight observer: %+v %v", result, err)
	}
	if m.state.Runs[id].Retirement != nil {
		t.Fatal("retirement hid unfinished observer")
	}
	restore := failStateSave(t, m)
	close(held.release)
	if err := <-done; err == nil {
		t.Fatal("failed exit persistence advertised success")
	}
	restore()
	// Fresh retirement still cannot discard the observer's unsent durable exit.
	a.ID = uuid.New()
	if _, err := m.apply(t.Context(), a); err == nil {
		t.Fatal("old absent-exit proof accepted late exit")
	}
	r = m.state.Runs[id]
	a.Retirement.ExitHash = exitDigest(r.Exit)
	if got, err := m.apply(t.Context(), a); err != nil || got.Outcome != "deferred" {
		t.Fatalf("unacknowledged exit: %+v %v", got, err)
	}
	if m.state.Runs[id].Retired {
		t.Fatal("late observer evidence discarded")
	}
}
