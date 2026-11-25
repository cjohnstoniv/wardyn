// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// portalSweepProbe counts ticks, fails the first, and signals on every one, so a test waits on the signal and
// never on the clock.
type portalSweepProbe struct {
	calls  atomic.Int32
	called chan struct{}
}

func (p *portalSweepProbe) SweepPortalLifecycle(context.Context) error {
	n := p.calls.Add(1)
	select {
	case p.called <- struct{}{}:
	default:
	}
	if n == 1 {
		return errors.New("portal sweep failed")
	}
	return nil
}

// The portal sweep starts with no leader and no SCIM setting at all, keeps ticking after a failed tick, and stops
// with its context.
func TestPortalLifecycleSweeperStartsWithoutSCIMAndSurvivesAFailure(t *testing.T) {
	p := &portalSweepProbe{called: make(chan struct{}, 64)}
	ctx, cancel := context.WithCancel(t.Context())
	startPortalLifecycleSweeper(ctx, nil, p, time.Millisecond)
	timeout := time.After(10 * time.Second)
	for p.calls.Load() < 3 {
		select {
		case <-p.called:
		case <-timeout:
			t.Fatalf("only %d tick(s), want 3", p.calls.Load())
		}
	}
	cancel()
	// A tick that was in flight at the cancel may still land; after it, no more.
	time.Sleep(20 * time.Millisecond)
	settled := p.calls.Load()
	time.Sleep(50 * time.Millisecond)
	if got := p.calls.Load(); got != settled {
		t.Errorf("ticks kept running after the context ended: %d -> %d", settled, got)
	}
}

// The tick interval is a fixed, documented constant: the sweep only bounds how late a deadline or a repair runs.
func TestPortalLifecycleSweepIntervalIsFixed(t *testing.T) {
	if portalLifecycleSweepInterval != 15*time.Minute {
		t.Errorf("portalLifecycleSweepInterval = %s, want 15m", portalLifecycleSweepInterval)
	}
}
