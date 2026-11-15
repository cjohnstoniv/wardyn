// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/audit"
)

// order is the shared call-order log fakeHTTPShutdowner and
// fakeBackgroundServer both append to, so a test can assert on the full
// sequence across both fakes, not just the two steps after Shutdown.
type order []string

// fakeHTTPShutdowner lets a test make the HTTP drain fail without standing up
// a real listener, recording "shutdown" into the shared order log so a test
// can pin it ahead of WaitBackground/FlushAuthFailedStreak — the invariant
// the deleted AST-walking TestServeShutdownOrder used to pin by construction.
type fakeHTTPShutdowner struct {
	err error
	log *order
}

func (f fakeHTTPShutdowner) Shutdown(context.Context) error {
	*f.log = append(*f.log, "shutdown")
	return f.err
}

// fakeBackgroundServer records whether WaitBackground and
// FlushAuthFailedStreak ran, and in what order, without a real *api.Server.
// WaitBackground first gives the secret store's clean-up wait a few seconds to
// start and logs "cleanups" if it did: one started after WaitBackground, or
// not at all, is missing from the log.
type fakeBackgroundServer struct {
	log      *order
	cleanUps *fakeCleanUpWait
}

func (f *fakeBackgroundServer) WaitBackground() {
	select {
	case <-f.cleanUps.started:
		*f.log = append(*f.log, "cleanups")
	case <-time.After(5 * time.Second):
	}
	*f.log = append(*f.log, "wait")
}
func (f *fakeBackgroundServer) FlushAuthFailedStreak() { *f.log = append(*f.log, "flush") }

// fakeCleanUpWait stands in for secretstorepg.WaitForCleanUps: it notes how much time its ctx leaves (0 for
// none) and each call's deadline, closes started on its first call, and from its second call on logs
// "cleanups again" to log when log is set.
type fakeCleanUpWait struct {
	started   chan struct{}
	once      sync.Once
	left      time.Duration
	deadlines []time.Time
	log       *order
}

func newFakeCleanUpWait() *fakeCleanUpWait { return &fakeCleanUpWait{started: make(chan struct{})} }

func (f *fakeCleanUpWait) wait(ctx context.Context) error {
	if deadline, ok := ctx.Deadline(); ok {
		f.left = time.Until(deadline)
		f.deadlines = append(f.deadlines, deadline)
	}
	if len(f.deadlines) > 1 && f.log != nil {
		*f.log = append(*f.log, "cleanups again")
	}
	f.once.Do(func() { close(f.started) })
	return nil
}

// checkCleanUpWait fails unless the clean-up wait ran beside WaitBackground,
// within the background budget, and runShutdownSequence reported it.
func checkCleanUpWait(t *testing.T, cw *fakeCleanUpWait, waited bool) {
	t.Helper()
	if !waited {
		t.Error("runShutdownSequence reported no clean-up wait, so run's deferred close would wait the whole budget again")
	}
	if cw.left <= 0 || cw.left > api.BackgroundShutdownBudget {
		t.Errorf("the clean-up wait's ctx left %v; want a deadline within api.BackgroundShutdownBudget (%v)", cw.left, api.BackgroundShutdownBudget)
	}
}

// TestRunShutdownSequence_WaitsForBackgroundEvenWhenHTTPDrainFails is #990 /
// #710 item 5's behavioural pin: a SIGTERM landing on a slow (or otherwise
// failing) HTTP drain must not drop a superseded sign-in's teardown or the
// auth-fail streak flush the same way a SIGKILL would. TestServeShutdownOrder
// used to pin this by walking serveAndShutdown's AST for call order; this
// drives the real extracted sequence with a Shutdown that actually returns an
// error and proves WaitBackground and FlushAuthFailedStreak still both ran,
// in order.
func TestRunShutdownSequence_WaitsForBackgroundEvenWhenHTTPDrainFails(t *testing.T) {
	drainErr := errors.New("drain timed out")
	var log order
	cw := newFakeCleanUpWait()
	bg := &fakeBackgroundServer{log: &log, cleanUps: cw}
	shutCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	waited, err := runShutdownSequence(shutCtx, nil, fakeHTTPShutdowner{err: drainErr, log: &log}, bg, cw.wait, nil)
	checkCleanUpWait(t, cw, waited)

	if !errors.Is(err, drainErr) {
		t.Fatalf("runShutdownSequence returned %v, want an error wrapping %v", err, drainErr)
	}
	if got := []string(log); len(got) != 4 || got[0] != "shutdown" || got[1] != "cleanups" || got[2] != "wait" || got[3] != "flush" {
		t.Errorf("runShutdownSequence(failing Shutdown) called %v, want [shutdown cleanups wait flush] — a failed HTTP "+
			"drain must not skip the background wait, the clean-up wait beside it or the auth-fail flush, and must not "+
			"run any of them before Shutdown", got)
	}
}

// TestRunShutdownSequence_SucceedsWhenHTTPDrainSucceeds is the control case: a
// clean Shutdown still runs, in order, before WaitBackground and
// FlushAuthFailedStreak, and runShutdownSequence returns nil.
func TestRunShutdownSequence_SucceedsWhenHTTPDrainSucceeds(t *testing.T) {
	var log order
	cw := newFakeCleanUpWait()
	bg := &fakeBackgroundServer{log: &log, cleanUps: cw}
	shutCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	waited, err := runShutdownSequence(shutCtx, nil, fakeHTTPShutdowner{log: &log}, bg, cw.wait, nil)
	checkCleanUpWait(t, cw, waited)

	if err != nil {
		t.Fatalf("runShutdownSequence returned %v, want nil", err)
	}
	if got := []string(log); len(got) != 4 || got[0] != "shutdown" || got[1] != "cleanups" || got[2] != "wait" || got[3] != "flush" {
		t.Errorf("runShutdownSequence(clean Shutdown) called %v, want [shutdown cleanups wait flush]", got)
	}
}

// TestRunShutdownSequence_WaitsAgainForACleanUpTheBackgroundWorkStarted: a clean-up that the background work's own
// saves start after the first wait found none running is waited for too, after WaitBackground and before the
// auth-fail flush, on the same deadline, so the grace period does not grow. Catches a sequence that waits once,
// which leaves that clean-up to run's deferred close, which after a shutdown waits no more and cuts it.
func TestRunShutdownSequence_WaitsAgainForACleanUpTheBackgroundWorkStarted(t *testing.T) {
	var log order
	cw := newFakeCleanUpWait()
	cw.log = &log
	bg := &fakeBackgroundServer{log: &log, cleanUps: cw}
	shutCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	waited, err := runShutdownSequence(shutCtx, nil, fakeHTTPShutdowner{log: &log}, bg, cw.wait, nil)
	checkCleanUpWait(t, cw, waited)

	if err != nil {
		t.Fatalf("runShutdownSequence returned %v, want nil", err)
	}
	if len(cw.deadlines) != 2 || !cw.deadlines[0].Equal(cw.deadlines[1]) {
		t.Errorf("clean-up waits ran on deadlines %v; want two, on the same deadline", cw.deadlines)
	}
	if got, want := []string(log), []string{"shutdown", "cleanups", "wait", "cleanups again", "flush"}; !slices.Equal(got, want) {
		t.Errorf("runShutdownSequence called %v, want %v", got, want)
	}
}

// TestServeAndShutdown_ReportsTheCleanUpWaitAfterAShutdown: a shutdown runs
// the sequence, which waits for the secret store's clean-ups, so run's
// deferred close waits no more (a second wait would run past the grace).
func TestServeAndShutdown_ReportsTheCleanUpWaitAfterAShutdown(t *testing.T) {
	addr, empty, no, trust := "127.0.0.1:0", "", false, "wardyn.local"
	f := &bootFlags{listen: &addr, tlsCert: &empty, tlsKey: &empty, tlsTerminated: &no, trustDomain: &trust}
	rootCtx, cancel := context.WithCancel(context.Background())
	cancel() // the shutdown signal, already received
	waited, err := serveAndShutdown(rootCtx, f, tlsPosture{}, api.New(api.Config{}), "none", nil, &audit.DenialCoalescer{}, nil, nil)
	if err != nil {
		t.Fatalf("serveAndShutdown after a shutdown signal = %v; want nil", err)
	}
	if !waited || cleanUpWaitAfterServe(waited) != 0 {
		t.Fatalf("after a shutdown: cleanUpsWaited %v, run's close waits %v; want true and 0", waited, cleanUpWaitAfterServe(waited))
	}
}
