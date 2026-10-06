// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// poolGuardChildMarker marks the re-executed binary TestNestedAcquireGuard_ExitsTheTestBinary runs.
const poolGuardChildMarker = "WARDYN_TEST_POOL_GUARD_CHILD"

// recordFindings makes the guard record its findings rather than exit the binary.
func recordFindings(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var findings []string
	prev := onNestedAcquire
	onNestedAcquire = func(f string) {
		mu.Lock()
		defer mu.Unlock()
		findings = append(findings, f)
	}
	t.Cleanup(func() { onNestedAcquire = prev })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(findings)
	}
}

// guardedPool is pgPool with the nested-acquire guard on, recording its findings.
func guardedPool(t *testing.T) (*pgxpool.Pool, func() []string) {
	t.Helper()
	t.Setenv(nestedAcquireGuardEnv, "1")
	findings := recordFindings(t)
	return pgPool(t), findings
}

func mustAcquire(t *testing.T, pool *pgxpool.Pool) *pgxpool.Conn {
	t.Helper()
	c, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	return c
}

func mustExec(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `SELECT 1`); err != nil {
		t.Fatalf("exec: %v", err)
	}
}

func TestNestedAcquireGuard_OneGoroutineTwoConnectionsFails(t *testing.T) {
	pool, findings := guardedPool(t)
	held := mustAcquire(t, pool)
	mustExec(t, pool)
	held.Release()

	got := findings()
	if len(got) != 1 {
		t.Fatalf("findings = %d, want 1: %q", len(got), got)
	}
	for _, want := range []string{"nested pool acquire", "held connection acquired at:", "second acquire at:", "TestNestedAcquireGuard_OneGoroutineTwoConnectionsFails"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("finding does not name %q:\n%s", want, got[0])
		}
	}
}

func TestNestedAcquireGuard_TwoGoroutinesOneEachPass(t *testing.T) {
	pool, findings := guardedPool(t)
	held := mustAcquire(t, pool)
	done := make(chan *pgxpool.Conn)
	go func() {
		c, err := pool.Acquire(context.Background())
		if err != nil {
			t.Errorf("acquire on the second goroutine: %v", err)
		}
		done <- c
	}()
	if c := <-done; c != nil {
		c.Release()
	}
	held.Release()
	if got := findings(); len(got) != 0 {
		t.Fatalf("one connection per goroutine was reported: %q", got)
	}
}

// A pool opened from pool.Config() shares the guard: a hold on one pool does not count against
// the other, while a hold on the same pool still does.
func TestNestedAcquireGuard_AnotherPoolsHoldDoesNotCount(t *testing.T) {
	pool, findings := guardedPool(t)
	other, err := pgxpool.NewWithConfig(context.Background(), pool.Config())
	if err != nil {
		t.Fatalf("open a second pool: %v", err)
	}
	t.Cleanup(other.Close)

	held := mustAcquire(t, pool)
	mustExec(t, other)
	held.Release()
	if got := findings(); len(got) != 0 {
		t.Fatalf("a hold on another pool was reported: %q", got)
	}
	held = mustAcquire(t, other)
	mustExec(t, other)
	held.Release()
	if got := findings(); len(got) != 1 {
		t.Fatalf("findings = %d, want 1 for a nested acquire on the second pool: %q", len(got), got)
	}
}

func TestNestedAcquireGuard_AllowlistedPairPasses(t *testing.T) {
	pool, findings := guardedPool(t)
	release, err := AdvisoryLockKeyed(context.Background(), pool, keyedLockTestClass, 401, 5*time.Second)
	if err != nil {
		t.Fatalf("AdvisoryLockKeyed: %v", err)
	}
	mustExec(t, pool)
	release()
	if got := findings(); len(got) != 0 {
		t.Fatalf("work under AdvisoryLockKeyed was reported: %q", got)
	}

	// The allowlist excuses the keyed lock's connection, not the goroutine: a second hold still counts.
	release, err = AdvisoryLockKeyed(context.Background(), pool, keyedLockTestClass, 402, 5*time.Second)
	if err != nil {
		t.Fatalf("AdvisoryLockKeyed: %v", err)
	}
	held := mustAcquire(t, pool)
	mustExec(t, pool)
	held.Release()
	release()
	if got := findings(); len(got) != 1 {
		t.Fatalf("findings = %d, want 1 for the hold beside the keyed lock: %q", len(got), got)
	}

	// An entry is a call path, not a function: TryAdvisoryLock is allowlisted under the tick locks
	// only, so taken anywhere else (as the lifetime holders take it) its hold still counts.
	unlock, ok, err := TryAdvisoryLock(context.Background(), pool, 0x5754_4754_4752_4431) // "WTGTGRD1"
	if err != nil || !ok {
		t.Fatalf("TryAdvisoryLock: ok=%v err=%v", ok, err)
	}
	mustExec(t, pool)
	unlock()
	if got := findings(); len(got) != 2 {
		t.Fatalf("findings = %d, want 2 once a TryAdvisoryLock hold outside the tick locks nests: %q", len(got), got)
	}
}

// A holder is cleared by its connection on every release: one released by another goroutine, and
// one the pool destroys (released mid-transaction), where pgx skips AfterRelease.
func TestNestedAcquireGuard_ReleaseClearsTheHolder(t *testing.T) {
	pool, findings := guardedPool(t)

	handed := mustAcquire(t, pool)
	released := make(chan struct{})
	go func() {
		handed.Release()
		close(released)
	}()
	<-released
	mustExec(t, pool)

	destroyed := mustAcquire(t, pool)
	if _, err := destroyed.Exec(context.Background(), `BEGIN`); err != nil {
		t.Fatalf("begin: %v", err)
	}
	destroyed.Release()
	mustExec(t, pool)

	if got := findings(); len(got) != 0 {
		t.Fatalf("a released connection still counted as held: %q", got)
	}
}

func TestNestedAcquireGuard_OffRecordsNothing(t *testing.T) {
	findings := recordFindings(t)
	t.Setenv(nestedAcquireGuardEnv, "")
	off := pgPool(t)
	if tr := off.Config().ConnConfig.Tracer; tr != nil {
		t.Fatalf("guard off, but the pool has a tracer: %T", tr)
	}
	held := mustAcquire(t, off)
	mustExec(t, off)
	held.Release()
	if got := findings(); len(got) != 0 {
		t.Fatalf("guard off, but a finding was recorded: %q", got)
	}
}

// TestNestedAcquireGuard_ExitsTheTestBinary runs a nested acquire in a re-executed copy of this
// test binary with the guard's own reporter: the child exits 2 and names both acquires.
func TestNestedAcquireGuard_ExitsTheTestBinary(t *testing.T) {
	if os.Getenv(poolGuardChildMarker) == "1" {
		pool := pgPool(t)
		held := mustAcquire(t, pool)
		defer held.Release()
		mustExec(t, pool)
		t.Fatal("the guard let a nested acquire through")
	}
	if os.Getenv("WARDYN_TEST_PG") == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping Postgres-backed guard test")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestNestedAcquireGuard_ExitsTheTestBinary$", "-test.count=1", "-test.timeout=60s")
	cmd.Env = append(os.Environ(), nestedAcquireGuardEnv+"=1", poolGuardChildMarker+"=1")
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 {
		t.Fatalf("child: err %v, want exit status 2; output:\n%s", err, out)
	}
	for _, want := range []string{"nested pool acquire", "held connection acquired at:", "second acquire at:", "mustExec"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("child output does not name %q:\n%s", want, out)
		}
	}
}
