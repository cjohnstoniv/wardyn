// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedLog is an io.Writer a test's slog handler shares with the watchdog's timer goroutine.
type lockedLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// requireOverLimitHoldIsCountedAndKept: a run's lock held past LockHoldLimit is counted once and logged with the
// function that took it, and it stays held: its context is live and a second locker (another replica; on the local
// locker the same one) still cannot take it. Another class's hold is not watched.
func requireOverLimitHoldIsCountedAndKept(t *testing.T, l, other Locker) {
	oldLimit := LockHoldLimit
	LockHoldLimit = 20 * time.Millisecond
	logs := &lockedLog{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { LockHoldLimit = oldLimit; slog.SetDefault(prev) })
	ctx := context.Background()
	before := LongLockHolds()

	k := testKey(t, RunOpLockClass)
	hctx, release, err := l.Lock(ctx, k, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	deadline := time.Now().Add(5 * time.Second)
	for LongLockHolds() == before && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // a second firing would count again
	if got := LongLockHolds() - before; got != 1 {
		t.Fatalf("holds counted past the limit: %d, want 1", got)
	}
	if hctx.Err() != nil {
		t.Fatalf("the watchdog ended the hold's context: %v", context.Cause(hctx))
	}
	if _, rel, ok, err := other.TryLock(ctx, k); ok || err != nil {
		if ok {
			rel()
		}
		t.Fatalf("a second taker got the held lock (ok %v, err %v): the watchdog freed it", ok, err)
	}
	if log := logs.String(); !strings.Contains(log, "held past its limit") || !strings.Contains(log, "requireOverLimitHoldIsCountedAndKept") {
		t.Errorf("no watchdog log naming the taker:\n%s", log)
	}

	_, srel, err := l.Lock(ctx, testKey(t, SiteConfigLockClass), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	srel()
	if got := LongLockHolds() - before; got != 1 {
		t.Errorf("holds counted past the limit: %d after another class's long hold, want 1", got)
	}
}

func TestLocalLocker_AHoldPastItsLimitIsCountedAndKept(t *testing.T) {
	l := NewLocalLocker()
	requireOverLimitHoldIsCountedAndKept(t, l, l)
}

func TestPGLocker_AHoldPastItsLimitIsCountedAndKept(t *testing.T) {
	a, b, _ := twoReplicas(t)
	requireOverLimitHoldIsCountedAndKept(t, a, b)
}
