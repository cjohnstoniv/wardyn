// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"testing"
	"time"
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
type fakeBackgroundServer struct {
	log *order
}

func (f *fakeBackgroundServer) WaitBackground()        { *f.log = append(*f.log, "wait") }
func (f *fakeBackgroundServer) FlushAuthFailedStreak() { *f.log = append(*f.log, "flush") }

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
	bg := &fakeBackgroundServer{log: &log}
	shutCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := runShutdownSequence(shutCtx, nil, fakeHTTPShutdowner{err: drainErr, log: &log}, bg, nil)

	if !errors.Is(err, drainErr) {
		t.Fatalf("runShutdownSequence returned %v, want an error wrapping %v", err, drainErr)
	}
	if got := []string(log); len(got) != 3 || got[0] != "shutdown" || got[1] != "wait" || got[2] != "flush" {
		t.Errorf("runShutdownSequence(failing Shutdown) called %v, want [shutdown wait flush] — a failed HTTP "+
			"drain must not skip the background wait or the auth-fail flush, and must not run either one before "+
			"Shutdown", got)
	}
}

// TestRunShutdownSequence_SucceedsWhenHTTPDrainSucceeds is the control case: a
// clean Shutdown still runs, in order, before WaitBackground and
// FlushAuthFailedStreak, and runShutdownSequence returns nil.
func TestRunShutdownSequence_SucceedsWhenHTTPDrainSucceeds(t *testing.T) {
	var log order
	bg := &fakeBackgroundServer{log: &log}
	shutCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := runShutdownSequence(shutCtx, nil, fakeHTTPShutdowner{log: &log}, bg, nil)

	if err != nil {
		t.Fatalf("runShutdownSequence returned %v, want nil", err)
	}
	if got := []string(log); len(got) != 3 || got[0] != "shutdown" || got[1] != "wait" || got[2] != "flush" {
		t.Errorf("runShutdownSequence(clean Shutdown) called %v, want [shutdown wait flush]", got)
	}
}
