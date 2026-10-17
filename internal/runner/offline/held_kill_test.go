// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestHeldKillDefersNotFailsSession: a queued kill whose teardown is held (a pod on a NotReady node,
// termination unknown) holds that run as containment pending with a deferred receipt; it never fails
// Pending, so a restarted runner still reaches READY: one run's unproven containment never takes the runner offline.
func TestHeldKillDefersNotFailsSession(t *testing.T) {
	m, sub, _, org := managerFixture(t)
	id := createLocal(t, m, org)
	held := &runner.ContainmentPendingError{Kind: runner.ContainmentPendingDelete, Err: errors.New("termination unknown")}
	sub.killErr, sub.stopErr = held, held
	m.Offline(t.Context())
	a := pendingContainment(m, id, types.RunnerActionKill)
	result, err := m.apply(t.Context(), a)
	if err != nil || result.Outcome != types.RunnerActionDeferred {
		t.Fatalf("held kill: %+v %v", result, err)
	}
	r := m.state.Runs[id]
	if r.ContainmentPending == nil || r.ContainmentPending.Kind != runner.ContainmentPendingDelete || r.ProxyStoppedAt != nil {
		t.Fatalf("run %+v", r)
	}
	if err := m.BeforeReady(t.Context(), &runnerwire.Peer{}, false); err != nil {
		t.Fatalf("a held run kept the runner from READY: %v", err)
	}
	if replay, err := m.apply(t.Context(), a); err != nil || replay != result {
		t.Fatalf("replayed held kill: %+v %v", replay, err)
	}
	sub.killErr, sub.stopErr = nil, nil
	if err := m.CheckLapses(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r := m.state.Runs[id]; r.ProxyStoppedAt == nil || r.ContainmentPending != nil {
		t.Fatalf("completion evidence did not settle: %+v", r)
	}
}

// TestContainmentPendingLogOncePerMinute: CheckLapses retries a held run every tick but warns about
// it at most once a minute per run.
func TestContainmentPendingLogOncePerMinute(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	m, sub, elapsed, org := managerFixture(t)
	createLocal(t, m, org)
	m.state.Revoked = true
	sub.stopErr = &runner.ContainmentPendingError{Kind: runner.ContainmentPendingDelete, Err: errors.New("termination unknown")}
	count := func() int { return strings.Count(buf.String(), "runner containment pending") }
	for i := 0; i < 5; i++ {
		*elapsed += time.Second
		if err := m.CheckLapses(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if count() != 1 {
		t.Fatalf("five ticks in five seconds logged %d warnings", count())
	}
	*elapsed += time.Minute
	if err := m.CheckLapses(t.Context()); err != nil {
		t.Fatal(err)
	}
	if count() != 2 {
		t.Fatalf("a minute later: %d warnings", count())
	}
}
