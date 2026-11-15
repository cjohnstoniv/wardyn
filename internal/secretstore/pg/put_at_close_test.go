// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The pool closes only once a Put still running when the close began has
// handed its clean-up to afterPut and that clean-up has ended: a close that
// looked at the clean-ups alone would see none while the Put runs and close
// at once, and the clean-up would then fail on the closed pool. Catches: a
// close that does not count the Puts still running.
func TestCloseAfterCleanUps_WaitsForAPutStillRunning(t *testing.T) {
	if err := WaitForCleanUps(t.Context()); err != nil { // none left by an earlier test
		t.Fatal(err)
	}
	s := &Store{extTimeout: time.Second}
	done, err := startPut(&closeRecorder{}) // the Put's own pool, a key only: the count is the process's
	if err != nil {
		t.Fatal(err)
	}
	cleaned := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond) // the Put's own write
		s.afterPut(t.Context(), "tok", func(context.Context) { time.Sleep(50 * time.Millisecond); close(cleaned) })
		done()
	}()
	var pool closeRecorder
	CloseAfterCleanUps(&pool, 10*time.Second)
	select {
	case <-cleaned:
	default:
		t.Fatal("the pool closed while a Put was still running, before the clean-up it left had ended")
	}
	if !pool.closed || pool.running != 0 {
		t.Fatalf("closed=%v with %d clean-ups running; want closed with none", pool.closed, pool.running)
	}
}

// Once CloseAfterCleanUps has stopped waiting for a pool, a Put on it that may
// leave a clean-up is refused before it writes the secret, on both such paths:
// its clean-up would fail on the closed pool. A Put on another pool is still
// admitted. The pgxpool.Pool values here are keys only; nothing reaches them,
// and a Put that got past its refusal would panic on the zero pool's Begin.
// Catches: a close that never marks its pool, or a Put path that is not counted.
func TestPut_RefusedOnceItsPoolIsClosing(t *testing.T) {
	var closed closeRecorder
	CloseAfterCleanUps(&closed, time.Second)
	if _, err := startPut(&closed); !errors.Is(err, errStoreClosing) {
		t.Fatalf("a Put on a pool CloseAfterCleanUps closed: %v; want %v", err, errStoreClosing)
	}
	p := new(pgxpool.Pool)
	putsMu.Lock()
	closing[p] = true
	putsMu.Unlock()
	s := &Store{pool: p, owner: "alice"}
	if err := s.putExternal(t.Context(), "tok", []byte("v")); !errors.Is(err, errStoreClosing) {
		t.Errorf("a store-mode Put on a closing pool: %v; want %v", err, errStoreClosing)
	}
	if err := s.putOverPointer(t.Context(), "tok", sealedRow{}); !errors.Is(err, errStoreClosing) {
		t.Errorf("a local Put over a pointer row on a closing pool: %v; want %v", err, errStoreClosing)
	}
	done, err := startPut(new(pgxpool.Pool))
	if err != nil {
		t.Fatalf("a Put on another pool: %v; want it admitted", err)
	}
	done()
}
