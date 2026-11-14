// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package maskstore

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
)

// A registration whose last read of the run's fence fails keeps the value it committed masked: a second
// registration of the value found it cached and returned, and this replica's read of the table moved past
// its row, so nothing would bring the value back. The registration reports the error; the value stays in
// the registry and the run's output is masked after a later read.
func TestPG_MaskARegistrationWhoseFenceReadFailsKeepsTheValue(t *testing.T) {
	pool, keys := erasurePG(t)
	reg := secretmask.NewRegistry()
	st := New(pool, keys, reg)
	ctx := t.Context()
	runID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO agent_runs (id, created_by, agent, repo, confinement_class, state, spiffe_id, runner_target)
		VALUES ($1, 'sub', 'claude-code', 'a/b', 'CC2', 'RUNNING', 'spiffe://x/' || $2, 'docker')`, runID, runID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO run_mask_manifest (run_id, owner) VALUES ($1, 'alice')`, runID); err != nil {
		t.Fatal(err)
	}
	value := []byte("registered-before-a-failed-fence-read")
	var again error
	beforeRunFenceRead = func(c context.Context) context.Context {
		again = reg.Add(runID, value)
		if err := st.Fresh(ctx, time.Now()); err != nil {
			t.Error(err)
		}
		failed, cancel := context.WithCancel(c)
		cancel()
		return failed
	}
	t.Cleanup(func() { beforeRunFenceRead = func(c context.Context) context.Context { return c } })
	if err := reg.Add(runID, value); err == nil || errors.Is(err, ErrFenced) {
		t.Fatalf("the registration whose fence read failed = %v, want the read's error", err)
	}
	if again != nil {
		t.Fatalf("the second registration = %v, want nil (the value was cached)", again)
	}
	if err := st.Fresh(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if out := reg.Masker(runID).Mask([]byte("token " + string(value))); bytes.Contains(out, value) {
		t.Errorf("the run's output still carries the value: %q", out)
	}
	st.sync.mu.Lock()
	defer st.sync.mu.Unlock()
	kept := false
	for _, rf := range st.sync.rows {
		kept = kept || rf.runID == runID
	}
	if !kept {
		t.Error("the store dropped its ref to the committed row, so a later tombstone would not drop the value")
	}
}

// A registration held after its commit while the run is erased (this replica applies the tombstones before
// the registration caches anything), whose read of the fence then fails, keeps the value only until the
// fence is read again: the next read of the table, or the next registration of the value, finds the run
// fenced and takes the value out of the registry and the store's row refs, and that registration refuses.
func TestPG_MaskARegistrationWhoseFenceReadFailsAcrossAnErasureIsSettledLater(t *testing.T) {
	for _, then := range []string{"read", "add"} {
		t.Run(then, func(t *testing.T) {
			pool, keys := erasurePG(t)
			reg := secretmask.NewRegistry()
			st := New(pool, keys, reg)
			ctx := t.Context()
			runID := uuid.New()
			if _, err := pool.Exec(ctx, `INSERT INTO agent_runs (id, created_by, agent, repo, confinement_class, state, spiffe_id, runner_target)
				VALUES ($1, 'sub', 'claude-code', 'a/b', 'CC2', 'RUNNING', 'spiffe://x/' || $2, 'docker')`, runID, runID.String()); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `INSERT INTO run_mask_manifest (run_id, owner) VALUES ($1, 'alice')`, runID); err != nil {
				t.Fatal(err)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			afterRunCommit = func() { close(entered); <-release }
			beforeRunFenceRead = func(c context.Context) context.Context {
				failed, cancel := context.WithCancel(c)
				cancel()
				return failed
			}
			t.Cleanup(func() {
				afterRunCommit = func() {}
				beforeRunFenceRead = func(c context.Context) context.Context { return c }
			})
			value := []byte("registered-across-an-erasure-before-a-failed-fence-read")
			done := make(chan error, 1)
			go func() { done <- reg.Add(runID, value) }()
			erasureEntered(t, entered)
			if _, err := pool.Exec(ctx, `UPDATE run_mask_manifest SET fenced_at = now() WHERE run_id = $1`, runID); err != nil {
				t.Fatal(err)
			}
			if _, err := st.EraseOwner(ctx, "alice"); err != nil {
				t.Fatal(err)
			}
			close(release)
			if err := erasureAwait(t, done); err == nil || errors.Is(err, ErrFenced) {
				t.Fatalf("the registration whose fence read failed = %v, want the read's error", err)
			}
			check := func(when string) {
				for _, v := range reg.Snapshot(runID) {
					if bytes.Equal(v, value) {
						t.Errorf("%s: the registry still holds the value", when)
					}
				}
				st.sync.mu.Lock()
				defer st.sync.mu.Unlock()
				for _, rf := range st.sync.rows {
					if rf.runID == runID {
						t.Errorf("%s: the store still holds a ref with the run's value", when)
					}
				}
			}
			if then == "read" {
				if err := st.Fresh(ctx, time.Now()); err != nil {
					t.Fatal(err)
				}
				check("after the next read")
			}
			if err := reg.Add(runID, value); !errors.Is(err, ErrFenced) {
				t.Errorf("a later registration of the value = %v, want ErrFenced", err)
			}
			check("after a later registration")
			if err := st.Fresh(ctx, time.Now()); err != nil {
				t.Fatal(err)
			}
			check("after a later read")
		})
	}
}

// A registration of the value that arrives while a read is taking it out (the run was erased across a fence
// read that failed) is refused: the value stays pending until it has left the registry, so nothing reports
// it on record in between.
func TestPG_MaskARegistrationWhileTheValueIsTakenOutIsRefused(t *testing.T) {
	pool, keys := erasurePG(t)
	reg := secretmask.NewRegistry()
	st := New(pool, keys, reg)
	ctx := t.Context()
	runID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO agent_runs (id, created_by, agent, repo, confinement_class, state, spiffe_id, runner_target)
		VALUES ($1, 'sub', 'claude-code', 'a/b', 'CC2', 'RUNNING', 'spiffe://x/' || $2, 'docker')`, runID, runID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO run_mask_manifest (run_id, owner) VALUES ($1, 'alice')`, runID); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	afterRunCommit = func() { close(entered); <-release }
	beforeRunFenceRead = func(c context.Context) context.Context {
		failed, cancel := context.WithCancel(c)
		cancel()
		return failed
	}
	t.Cleanup(func() {
		afterRunCommit = func() {}
		beforeRunFenceRead = func(c context.Context) context.Context { return c }
	})
	value := []byte("registered-again-while-it-is-taken-out")
	done := make(chan error, 1)
	go func() { done <- reg.Add(runID, value) }()
	erasureEntered(t, entered)
	if _, err := pool.Exec(ctx, `UPDATE run_mask_manifest SET fenced_at = now() WHERE run_id = $1`, runID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EraseOwner(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := erasureAwait(t, done); err == nil || errors.Is(err, ErrFenced) {
		t.Fatalf("the registration whose fence read failed = %v, want the read's error", err)
	}
	// Hold the read inside its removal of the value: the fence is read, the value is still in the registry.
	removing, proceed := make(chan struct{}), make(chan struct{})
	var held atomic.Bool
	reg.SetDropGuard(func(uuid.UUID) func() {
		if held.CompareAndSwap(false, true) {
			close(removing)
			<-proceed
		}
		return func() {}
	})
	read := make(chan error, 1)
	go func() { read <- st.Fresh(ctx, time.Now()) }()
	erasureEntered(t, removing)
	again := reg.Add(runID, value)
	close(proceed)
	if err := erasureAwait(t, read); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(again, ErrFenced) {
		t.Errorf("a registration while the value was being taken out = %v, want ErrFenced", again)
	}
	for _, v := range reg.Snapshot(runID) {
		if bytes.Equal(v, value) {
			t.Error("the registry still holds the value")
		}
	}
}
