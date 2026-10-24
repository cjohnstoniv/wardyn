// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// downEngineStop is StopProxy against an engine that does not answer: every call fails the same way, and
// each one moves the fake clock on a second, as the containment tick does.
type downEngineStop struct {
	calls  *atomic.Int64
	stopAt int64
	cancel context.CancelFunc
}

func (s downEngineStop) StopProxy(context.Context, string) error {
	if s.calls.Add(1) == s.stopAt {
		s.cancel()
	}
	return errors.New("docker unreachable")
}

// Containment that fails every tick keeps being retried every tick, but its ERROR line is logged once and
// then once per growing interval, not once a second (a runner log grew by hundreds of MB a day this way).
func TestContainmentRetryErrorIsLoggedOncePerInterval(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	m, _, _, org := managerFixture(t)
	createLocal(t, m, org)
	var calls atomic.Int64
	base := time.Now()
	m.mu.Lock()
	m.state.Revoked = true
	m.now = func() time.Time { return base.Add(time.Duration(calls.Load()) * time.Second) }
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	m.stopper = downEngineStop{calls: &calls, stopAt: 5, cancel: cancel}
	_ = m.Run(ctx)
	var lines int
	for line := range strings.Lines(buf.String()) {
		var r map[string]any
		if json.Unmarshal([]byte(line), &r) == nil && r["level"] == "ERROR" && r["msg"] == "runner containment will retry" {
			lines++
			if r["repeated"] != float64(0) {
				t.Errorf("first line repeated = %v, want 0", r["repeated"])
			}
		}
	}
	if calls.Load() < 5 {
		t.Fatalf("containment tried %d times, want every tick", calls.Load())
	}
	if lines != 1 {
		t.Fatalf("%d ERROR lines for %d identical failures within 5 s, want 1:\n%s", lines, calls.Load(), buf.String())
	}
}

// refStop fails StopProxy for one ref every time and records, on the fake clock, when each other ref
// was first stopped.
type refStop struct {
	failing string
	now     func() time.Time
	mu      sync.Mutex
	calls   map[string]int
	first   map[string]time.Time
}

func (s *refStop) StopProxy(_ context.Context, ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[ref]++
	if ref == s.failing {
		return errors.New("docker unreachable")
	}
	if _, ok := s.first[ref]; !ok {
		s.first[ref] = s.now()
	}
	return nil
}

// One run's containment fails on every tick for ten minutes of fake time. It is still tried on every
// tick, a run that lapses five minutes in is first contained on the very next tick, inheriting no wait
// from the other's failures, and the repeated ERROR line stays a handful of lines, not one a second.
func TestContainmentFailureDelaysNoOtherRunAndLogsBounded(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	m, _, _, org := managerFixture(t)
	failing, lapsing := createLocal(t, m, org), createLocal(t, m, org)
	var seconds atomic.Int64
	base := time.Now()
	now := func() time.Time { return base.Add(time.Duration(seconds.Load()) * time.Second) }
	stop := &refStop{now: now, calls: map[string]int{}, first: map[string]time.Time{}}
	const lapseAt, total = 300, 600
	ticks := make(chan time.Time)
	m.mu.Lock()
	r := m.state.Runs[failing]
	r.FailedCreation = true
	m.state.Runs[failing] = r
	stop.failing = r.Ref
	lapsingRef := m.state.Runs[lapsing].Ref
	m.now, m.stopper, m.ticks = now, stop, ticks
	// Each pass starts one fake second after the last; the second run lapses at lapseAt.
	m.tick = func(context.Context) {
		if seconds.Add(1) == lapseAt {
			m.mu.Lock()
			r := m.state.Runs[lapsing]
			r.FailedCreation = true
			m.state.Runs[lapsing] = r
			m.mu.Unlock()
		}
	}
	m.mu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error)
	go func() { done <- m.Run(ctx) }()
	for range total - 1 {
		ticks <- time.Time{}
	}
	ticks <- time.Time{} // returns once the last pass has run
	cancel()
	<-done
	stop.mu.Lock()
	defer stop.mu.Unlock()
	if got := stop.calls[stop.failing]; got < total {
		t.Errorf("the failing run was tried %d times in %d ticks, want every tick", got, total)
	}
	first, ok := stop.first[lapsingRef]
	if !ok || first.Sub(base) != lapseAt*time.Second {
		t.Fatalf("the run that lapsed at %ds was first contained at %v (tried=%v), want on that same pass", lapseAt, first.Sub(base), ok)
	}
	var lines int
	for line := range strings.Lines(buf.String()) {
		var r map[string]any
		if json.Unmarshal([]byte(line), &r) == nil && r["level"] == "ERROR" && r["msg"] == "runner containment will retry" {
			lines++
		}
	}
	if lines == 0 || lines > 10 {
		t.Errorf("%d ERROR lines over %d s of the same failure, want a handful", lines, total)
	}
}
