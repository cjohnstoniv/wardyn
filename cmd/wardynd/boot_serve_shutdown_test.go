// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeHTTPShutdowner lets a test make the HTTP drain fail without standing up
// a real listener.
type fakeHTTPShutdowner struct{ err error }

func (f fakeHTTPShutdowner) Shutdown(context.Context) error { return f.err }

// fakeBackgroundServer records whether WaitBackground and
// FlushAuthFailedStreak ran, and in what order, without a real *api.Server.
type fakeBackgroundServer struct {
	order []string
}

func (f *fakeBackgroundServer) WaitBackground()        { f.order = append(f.order, "wait") }
func (f *fakeBackgroundServer) FlushAuthFailedStreak() { f.order = append(f.order, "flush") }

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
	bg := &fakeBackgroundServer{}
	shutCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := runShutdownSequence(shutCtx, nil, fakeHTTPShutdowner{err: drainErr}, bg, nil)

	if !errors.Is(err, drainErr) {
		t.Fatalf("runShutdownSequence returned %v, want an error wrapping %v", err, drainErr)
	}
	if got := bg.order; len(got) != 2 || got[0] != "wait" || got[1] != "flush" {
		t.Errorf("runShutdownSequence(failing Shutdown) called %v, want [wait flush] — a failed HTTP drain "+
			"must not skip the background wait or the auth-fail flush", got)
	}
}

// TestRunShutdownSequence_SucceedsWhenHTTPDrainSucceeds is the control case: a
// clean Shutdown still runs WaitBackground and FlushAuthFailedStreak, in
// order, and runShutdownSequence returns nil.
func TestRunShutdownSequence_SucceedsWhenHTTPDrainSucceeds(t *testing.T) {
	bg := &fakeBackgroundServer{}
	shutCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := runShutdownSequence(shutCtx, nil, fakeHTTPShutdowner{}, bg, nil)

	if err != nil {
		t.Fatalf("runShutdownSequence returned %v, want nil", err)
	}
	if got := bg.order; len(got) != 2 || got[0] != "wait" || got[1] != "flush" {
		t.Errorf("runShutdownSequence(clean Shutdown) called %v, want [wait flush]", got)
	}
}
