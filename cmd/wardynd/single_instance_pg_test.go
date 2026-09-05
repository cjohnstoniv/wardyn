// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// Postgres-backed test for the RUNTIME half of the one-replica safety control
// (claimSingleInstance / db.SingleInstanceLockKey). Before it, the ONLY
// enforcement was the Helm chart's render-time `replicas > 1` refusal, so
// `kubectl scale`, an HPA or a non-Helm replica edit started a second wardynd
// with no signal anywhere — and the failure that guards is a session recording
// persisted VERBATIM with live credentials in cleartext, because the
// secret-masking registry is process-local and fails open.
//
// Two independent pools stand in for two wardynd processes, mirroring
// gt_rotator_pg_test.go (whose pgPool helper this reuses).
//
// Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.
// Run: WARDYN_TEST_PG="postgres://wardyn:wardyn@localhost:55434/wardyn_r5fix?sslmode=disable" \
//        go test ./cmd/wardynd/... -run TestSingleInstance

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/db"
)

// once makes a release idempotent so it can be BOTH deferred and called inline.
// Without the defer, a t.Fatal partway through leaves the lock's connection
// checked out and pgxpool.Close blocks forever on cleanup — a failing test that
// wedges `go test` instead of reporting. Without the inline call, the test
// cannot prove a clean release frees the lock.
func once(f func()) func() {
	var o sync.Once
	return func() { o.Do(f) }
}

func TestSingleInstanceLock_SecondBootRefusesUntilTheFirstReleases(t *testing.T) {
	poolA := pgPool(t) // migrates the DB once

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	poolB, err := db.Connect(ctx, os.Getenv("WARDYN_TEST_PG"))
	if err != nil {
		t.Fatalf("connect poolB: %v", err)
	}
	defer poolB.Close()

	releaseA, err := claimSingleInstance(ctx, poolA, false)
	if err != nil {
		t.Fatalf("first boot was refused on an uncontended lock: %v", err)
	}
	releaseA = once(releaseA)
	defer releaseA()

	// The whole point: a second wardynd against the same database must not boot.
	if _, err := claimSingleInstance(ctx, poolB, false); err == nil {
		t.Fatal("a SECOND wardynd booted while the first holds the lock — the process-local secret-masking registry would fail open and persist live credentials in cleartext")
	} else {
		// The message is the operator's only instruction; it must name the
		// failure and the acknowledged override, not just say "refused".
		for _, want := range []string{"secretmask", "cleartext", "-allow-multi-instance"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal never says %q:\n%s", want, err.Error())
			}
		}
	}

	// -allow-multi-instance is the runtime twin of the chart's allowMultiReplica:
	// it must still start, or the chart's documented escape hatch is a lie.
	releaseAllowed, err := claimSingleInstance(ctx, poolB, true)
	if err != nil {
		t.Fatalf("-allow-multi-instance was refused while another instance holds the lock: %v", err)
	}
	releaseAllowed()

	// A clean shutdown hands the lock on — a restart must not have to wait for
	// Postgres to notice a dead session.
	releaseA()
	releaseB, err := claimSingleInstance(ctx, poolB, false)
	if err != nil {
		t.Fatalf("the next boot was still refused after a clean release: %v", err)
	}
	releaseB()
}
