// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// pgCodedError is a driver error carrying a SQLSTATE, wrapped the way
// internal/store wraps one (`fmt.Errorf("store: lock audit chain (…): %w", err)`)
// so the classifier is exercised through errors.As over a real %w chain rather
// than against a bare sentinel.
type pgCodedError struct{ code string }

func (e *pgCodedError) Error() string {
	return "ERROR: canceling statement due to lock timeout (SQLSTATE " + e.code + ")"
}
func (e *pgCodedError) SQLState() string { return e.code }

// lockTimeoutRecorder rejects ONE named event with a chain-lock timeout for the
// first `contend` attempts and accepts everything else. That is the shape of the
// episode the finding describes and the shape the poison probe is built to
// detect: an external session that inserted into audit_events and left its
// transaction open holds the advisory chain lock, so the HEAD line times out
// while the store is otherwise perfectly healthy — which is exactly what makes
// the line behind it land and "prove the store is up".
//
// Keyed on the event rather than a call countdown, because the probe strikes a
// LINE: a countdown would also starve the lines behind the head and the pass
// would break before anything could be proven, which is a different (and
// already-pinned) scenario.
type lockTimeoutRecorder struct {
	mu       sync.Mutex
	blocked  string
	contend  int
	code     string
	got      []types.AuditEvent
	rejected int
}

func (r *lockTimeoutRecorder) Record(_ context.Context, ev types.AuditEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ev.Action == r.blocked && r.contend > 0 {
		r.contend--
		r.rejected++
		return fmt.Errorf("store: lock audit chain (waited up to 5s; another transaction that inserted into audit_events may still be open): %w", &pgCodedError{code: r.code})
	}
	r.got = append(r.got, ev)
	return nil
}

func (r *lockTimeoutRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

// TestDrainChainLockTimeoutIsNotAPoisonStrike is F207.
//
// Drain already refuses to strike a line when the CLIENT's pass deadline fired.
// A DATABASE-side abort looked nothing like that: the audit-chain insert runs
// under `SET LOCAL lock_timeout` (internal/db's AuditChainLockTimeoutSQL), so a
// contended chain lock returned as an ordinary statement error with
// passCtx.Err() == nil and earned a strike. spoolPoisonAttempts contended ticks
// on the same head line, then the first line to land behind it proved "the store
// is up", and a perfectly replayable event was moved to <spool>.quarantine —
// after which the spool gauge reads 0, fully recovered, over a permanently
// incomplete trail. docs/OPERATIONS.md:210 says an outage must quarantine
// nothing, and a chain-lock outage is an outage.
func TestDrainChainLockTimeoutIsNotAPoisonStrike(t *testing.T) {
	for _, tc := range []struct {
		name, code string
	}{
		{"lock_timeout on the audit chain", sqlStateLockNotAvailable},
		{"statement_timeout / cancelled backend", sqlStateQueryCanceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "audit-spool.jsonl")
			sp, err := NewAuditSpool(path)
			if err != nil {
				t.Fatalf("NewAuditSpool: %v", err)
			}
			for _, action := range []string{"credential.mint", "run.create", "run.kill"} {
				if err := sp.Append(newTestEvent(action)); err != nil {
					t.Fatalf("append %s: %v", action, err)
				}
			}

			// One more contended tick than the poison threshold, so a strike-
			// counting Drain reaches its verdict and then gets the "store is up"
			// proof from the line behind the head.
			// Contended for longer than the poison threshold, so a
			// strike-counting Drain reaches its verdict and then gets its "the
			// store is up" proof from the very next line.
			rec := &lockTimeoutRecorder{blocked: "credential.mint", contend: spoolPoisonAttempts + 2, code: tc.code}
			for i := 0; i < spoolPoisonAttempts+6; i++ {
				_, _ = sp.Drain(context.Background(), rec, 100)
			}

			if got := sp.Quarantined(); got != 0 {
				t.Errorf("Quarantined = %d after a transient chain-lock episode; want 0 — a wait the DATABASE "+
					"aborted is a timeout, not a rejection, and the head line (a replayable credential.mint) "+
					"proved nothing about itself", got)
			}
			if _, err := os.Stat(path + ".quarantine"); !os.IsNotExist(err) {
				t.Errorf("a quarantine sidecar exists after transient lock contention (stat err = %v); want none", err)
			}
			// And the episode really did end with everything replayed — a test
			// where the drain simply stopped working would pass the two checks
			// above and prove nothing.
			if got := rec.count(); got != 3 {
				t.Errorf("replayed %d events once the lock cleared, want all 3", got)
			}
		})
	}
}

// TestAuditWriteTimedOutClassifiesOnlyTimeouts is the control: a genuine
// rejection must still earn its strike, or the poison probe stops working and
// one unacceptable line wedges the spool forever.
func TestAuditWriteTimedOutClassifiesOnlyTimeouts(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"lock_not_available", fmt.Errorf("wrapped: %w", &pgCodedError{code: "55P03"}), true},
		{"query_canceled", &pgCodedError{code: "57014"}, true},
		{"check constraint violation — a real rejection", &pgCodedError{code: "23514"}, false},
		{"not-null violation — a real rejection", &pgCodedError{code: "23502"}, false},
		{"a driverless error carries no code", fmt.Errorf("store down"), false},
		{"nil", nil, false},
	} {
		if got := auditWriteTimedOut(tc.err); got != tc.want {
			t.Errorf("%s: auditWriteTimedOut = %v, want %v", tc.name, got, tc.want)
		}
	}
}
