// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
)

// shortWarmInterval shortens the loop's interval for one test.
func shortWarmInterval(t *testing.T) time.Duration {
	t.Helper()
	old := warmInterval
	warmInterval = 5 * time.Millisecond
	t.Cleanup(func() { warmInterval = old })
	return warmInterval
}

// waitForAction polls until the fake clientset has seen at least n of action, or fails after 30 seconds.
func waitForAction(t *testing.T, cs *fake.Clientset, action string, n int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for countOf(warmSeq(cs), action) < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d %q actions after 30s, want at least %d", countOf(warmSeq(cs), action), action, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// closeWithin fails unless d.Close returns within 30 seconds.
func closeWithin(t *testing.T, d *Driver) {
	t.Helper()
	stopped := make(chan error, 1)
	go func() { stopped <- d.Close() }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Close did not return")
	}
}

func TestWarmLoopStopsOnClose(t *testing.T) {
	interval := shortWarmInterval(t)
	d, cs := warmTestDriver(t, warmTestPool("a", 1))
	d.startWarmPools()
	waitForAction(t, cs, "list pods", 4) // the first pass lists pods 3 times (warm_reconcile.go:266, 167, 292), so 4 proves one tick
	closeWithin(t, d)
	after := countOf(warmSeq(cs), "list pods")
	time.Sleep(20 * interval)
	if got := countOf(warmSeq(cs), "list pods"); got != after {
		t.Errorf("list pods actions %d after Close, then %d: the loop still runs", after, got)
	}
}

func TestWarmLoopNoPoolsRunsOnceAndStops(t *testing.T) {
	interval := shortWarmInterval(t)
	d, cs := warmTestDriver(t)
	seedPlaceholder(t, d, cs, warmTestPool("old", 1), 0, nil)
	cs.ClearActions()
	d.startWarmPools()
	waitForAction(t, cs, "delete pods", 1)
	time.Sleep(20 * interval)
	closeWithin(t, d)
	if n := countOf(warmSeq(cs), "list pods"); n != 1 {
		t.Errorf("%d list pods actions, want one pass only", n)
	}
	requirePodGone(t, cs, "wardyn-warm-old-0")
}
