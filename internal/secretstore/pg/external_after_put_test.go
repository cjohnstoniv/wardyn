// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// A Put's clean-up runs on its own deadline, six per-call timeouts, not on the
// caller's context: ending the caller's context does not stop it, and a
// clean-up that stalls ends by that deadline.
func TestAfterPut_EndsByItsOwnBound(t *testing.T) {
	s := &Store{extTimeout: 50 * time.Millisecond}
	caller, cancel := context.WithCancel(t.Context())
	start := time.Now()
	var took time.Duration
	var cause error
	s.afterPut(caller, "tok", func(ctx context.Context) {
		<-ctx.Done()
		took, cause = time.Since(start), ctx.Err()
	})
	cancel()
	WaitForCleanUps(t.Context())
	if !errors.Is(cause, context.DeadlineExceeded) || took < 300*time.Millisecond || took > 20*time.Second {
		t.Fatalf("clean-up ended after %v with %v; want its own 300ms deadline, not the caller's cancellation", took, cause)
	}
}

// A clean-up that panics ends alone: it runs on a goroutine no caller's
// recovery covers, so an uncontained panic there would end the process.
func TestAfterPut_ContainsAPanic(t *testing.T) {
	s := &Store{extTimeout: 50 * time.Millisecond}
	s.afterPut(t.Context(), "tok", func(context.Context) { panic("the store client broke") })
	WaitForCleanUps(t.Context())
}

// Puts may add clean-ups while WaitForCleanUps runs: it never panics, and once
// the Puts stop it returns after every clean-up has ended.
func TestWaitForCleanUps_PutsDuringTheWait(t *testing.T) {
	s := &Store{extTimeout: time.Second}
	waited := make(chan error, 1)
	go func() { waited <- WaitForCleanUps(t.Context()) }()
	var puts sync.WaitGroup
	for range 8 {
		puts.Go(func() {
			for range 50 {
				s.afterPut(t.Context(), "tok", func(context.Context) { time.Sleep(time.Millisecond) })
			}
		})
	}
	puts.Wait()
	if err := WaitForCleanUps(t.Context()); err != nil || cleanUps.Load() != 0 {
		t.Fatalf("after the Puts stopped: WaitForCleanUps = %v with %d clean-ups left; want nil and 0", err, cleanUps.Load())
	}
	if err := <-waited; err != nil {
		t.Fatalf("the wait that ran beside the Puts = %v; want nil", err)
	}
}

// A clean-up still running when the wait's context ends is cut at once, not
// at its own bound.
func TestWaitForCleanUps_CutsWhatOutlastsIt(t *testing.T) {
	s := &Store{extTimeout: 10 * time.Second}
	cause := make(chan error, 1)
	s.afterPut(t.Context(), "tok", func(ctx context.Context) { <-ctx.Done(); cause <- ctx.Err() })
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := WaitForCleanUps(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitForCleanUps = %v; want DeadlineExceeded", err)
	}
	select {
	case err := <-cause:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the clean-up ended with %v; want it cut (Canceled)", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the clean-up outlived the wait that cut it")
	}
	if err := WaitForCleanUps(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// With cleanUpLimit clean-ups running, the next is skipped and logged, never
// waited for: its Put answers at once, and the value it would remove stays for
// `wardynd -reconcile`. A wait here would hang the test, as the running
// clean-up ignores its context until the test lets it end.
func TestAfterPut_SkipsACleanUpPastTheCap(t *testing.T) {
	var logged bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	s := &Store{extTimeout: time.Second, cleanUpLimit: 1}
	held, ran := make(chan struct{}), make(chan struct{})
	s.afterPut(t.Context(), "tok", func(context.Context) { <-held })
	s.afterPut(t.Context(), "tok", func(context.Context) { close(ran) })
	close(held)
	if err := WaitForCleanUps(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ran:
		t.Fatal("a clean-up past the cap ran")
	default:
	}
	if !strings.Contains(logged.String(), "level=WARN") || !strings.Contains(logged.String(), "is skipped") {
		t.Fatalf("log = %q; want the skip logged at WARN", logged.String())
	}
}

// closeRecorder records how many clean-ups were running when it was closed.
type closeRecorder struct {
	closed  bool
	running int64
}

func (c *closeRecorder) Close() { c.closed, c.running = true, cleanUps.Load() }

// CloseAfterCleanUps closes the pool only once the clean-ups Puts left have
// ended, as a command that exits right after a Put needs.
func TestCloseAfterCleanUps_ClosesOnceTheCleanUpsEnd(t *testing.T) {
	s := &Store{extTimeout: time.Second}
	done := make(chan struct{})
	s.afterPut(t.Context(), "tok", func(context.Context) { time.Sleep(50 * time.Millisecond); close(done) })
	var pool closeRecorder
	CloseAfterCleanUps(&pool, 10*time.Second)
	select {
	case <-done:
	default:
		t.Fatal("the pool closed before the clean-up ended")
	}
	if !pool.closed || pool.running != 0 {
		t.Fatalf("closed=%v with %d clean-ups running; want closed with none", pool.closed, pool.running)
	}
}
