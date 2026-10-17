// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

type pausedStop struct {
	entered chan struct{}
	release chan struct{}
}

func (s pausedStop) StopProxy(ctx context.Context, _ string) error {
	close(s.entered)
	select {
	case <-s.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestContainmentCannotConfirmChangedFailure(t *testing.T) {
	for _, change := range []string{"marker", "generation", "reference"} {
		t.Run(change, func(t *testing.T) {
			m, sub, _, org := managerFixture(t)
			id := createLocal(t, m, org)
			m.mu.Lock()
			r := m.state.Runs[id]
			r.MutationFailure = &types.RunnerMutationFailure{ID: uuid.New(), Operation: runnerwire.MethodReplaceProxy}
			m.state.Runs[id] = r
			if err := m.save(); err != nil {
				t.Fatal(err)
			}
			m.mu.Unlock()
			pause := pausedStop{entered: make(chan struct{}), release: make(chan struct{})}
			m.stopper = pause
			done := make(chan error, 1)
			go func() { done <- m.CheckLapses(t.Context()) }()
			select {
			case <-pause.entered:
			case <-t.Context().Done():
				t.Fatal("containment did not begin")
			}
			m.mu.Lock()
			current := m.state.Runs[id]
			switch change {
			case "marker":
				current.MutationFailure = &types.RunnerMutationFailure{ID: uuid.New(), Operation: runnerwire.MethodReplaceProxy}
			case "generation":
				current.Generation = uuid.New()
			case "reference":
				current.Ref = "changed-ref"
			}
			m.state.Runs[id] = current
			if err := m.save(); err != nil {
				t.Fatal(err)
			}
			m.mu.Unlock()
			close(pause.release)
			if err := <-done; err == nil {
				t.Fatal("old containment confirmed changed target")
			}
			m.mu.Lock()
			got := m.state.Runs[id]
			m.mu.Unlock()
			if got.MutationFailure.Contained || got.ProxyStoppedAt != nil {
				t.Fatal("changed target acquired stale containment proof")
			}
			m.stopper = sub
			if err := m.CheckLapses(t.Context()); err != nil {
				t.Fatal(err)
			}
			if !m.state.Runs[id].MutationFailure.Contained || sub.stops != 1 {
				t.Fatal("changed target did not get independent containment retry")
			}
		})
	}
}
