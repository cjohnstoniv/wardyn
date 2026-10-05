// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// gapCovSweep is a scimPurgeSweeper that fails its first call and signals on
// every call, so a test waits on the signal and never on the clock.
type gapCovSweep struct {
	calls  atomic.Int32
	called chan struct{}
}

func gapCovNewSweep() *gapCovSweep { return &gapCovSweep{called: make(chan struct{}, 64)} }

func (s *gapCovSweep) SweepSCIMPurge(context.Context) error {
	n := s.calls.Add(1)
	select {
	case s.called <- struct{}{}:
	default:
	}
	if n == 1 {
		return errors.New("gapcov: sweep failed")
	}
	return nil
}

func (s *gapCovSweep) waitCalls(t *testing.T, n int32) {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for s.calls.Load() < n {
		select {
		case <-s.called:
		case <-timeout:
			t.Fatalf("only %d sweep call(s), want %d", s.calls.Load(), n)
		}
	}
}

// A failed sweep does not stop the loop: the next tick sweeps again, and the
// loop returns once its context ends.
func TestGapCovSCIMPurgeSweeperKeepsTickingAfterAFailure(t *testing.T) {
	sw := gapCovNewSweep()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { runSCIMPurgeSweeper(ctx, sw, time.Millisecond); close(done) }()

	sw.waitCalls(t, 3)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the sweeper did not return after its context ended")
	}
}

// With its context already ended, the loop returns without sweeping.
func TestGapCovSCIMPurgeSweeperReturnsOnACancelledContext(t *testing.T) {
	sw := gapCovNewSweep()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	done := make(chan struct{})
	go func() { runSCIMPurgeSweeper(ctx, sw, time.Hour); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the sweeper did not return on a cancelled context")
	}
	if n := sw.calls.Load(); n != 0 {
		t.Fatalf("%d sweep(s) ran on a cancelled context, want none", n)
	}
}

// gapCovLogs records the messages of every slog record taken while a test runs.
type gapCovLogs struct {
	mu   sync.Mutex
	msgs []string
}

func (l *gapCovLogs) Enabled(context.Context, slog.Level) bool { return true }
func (l *gapCovLogs) WithAttrs([]slog.Attr) slog.Handler       { return l }
func (l *gapCovLogs) WithGroup(string) slog.Handler            { return l }
func (l *gapCovLogs) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	l.msgs = append(l.msgs, r.Message)
	l.mu.Unlock()
	return nil
}

func (l *gapCovLogs) count(msg string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, m := range l.msgs {
		if strings.Contains(m, msg) {
			n++
		}
	}
	return n
}

// SCIM off (no token, or a blank one) starts no sweeper; SCIM on starts one that
// runs unconditionally when there is no leader election.
func TestGapCovStartSCIMPurgeSweeperOnlyWhenSCIMIsOn(t *testing.T) {
	logs := &gapCovLogs{}
	prev := slog.Default()
	slog.SetDefault(slog.New(logs))
	t.Cleanup(func() { slog.SetDefault(prev) })
	blank, token := "  ", "scim-token-of-sufficient-length-0123456789"

	off := gapCovNewSweep()
	startSCIMPurgeSweeper(t.Context(), &bootFlags{}, nil, off, time.Millisecond)
	startSCIMPurgeSweeper(t.Context(), &bootFlags{scimToken: &blank}, nil, off, time.Millisecond)
	if n := logs.count("scim purge sweeper started"); n != 0 || off.calls.Load() != 0 {
		t.Fatalf("SCIM off: %d start record(s), %d sweep(s); want none", n, off.calls.Load())
	}

	on := gapCovNewSweep()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	startSCIMPurgeSweeper(ctx, &bootFlags{scimToken: &token}, nil, on, time.Millisecond)
	on.waitCalls(t, 2)
	if n := logs.count("scim purge sweeper started"); n != 1 {
		t.Fatalf("SCIM on: %d start record(s), want 1", n)
	}
}
