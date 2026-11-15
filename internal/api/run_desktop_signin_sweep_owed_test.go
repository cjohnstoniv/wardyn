// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// signinKillAnswersRunner is signinFakeRunner whose KillSandbox answers err, counting its calls in *kills.
type signinKillAnswersRunner struct {
	*signinFakeRunner
	err   error
	kills *int
}

func (r signinKillAnswersRunner) KillSandbox(context.Context, string) error {
	r.signinFakeRunner.mu.Lock()
	defer r.signinFakeRunner.mu.Unlock()
	*r.kills++
	return r.err
}

// signinOwedFixture is a fixture whose sign-in is owed a kill: its run is KILLED with the sandbox still named and
// the row open at the stop stage's last pass, as a stuck end's kill that left the sandbox leaves it. Its runner's
// KillSandbox answers kill.
func signinOwedFixture(t *testing.T, kill error, kills *int) *signinFixture {
	t.Helper()
	f := newSigninFixture(t)
	f.begin(t)
	if _, err := f.st.RequestDesktopSigninEnd(context.Background(), f.st.only(t).ID, signinEndDone,
		signinStage(signinStageStop, signinContinueTries)); err != nil {
		t.Fatal(err)
	}
	run := f.run
	run.State = types.RunKilled
	f.st.putRun(run)
	f.srv.cfg.Runner = signinKillAnswersRunner{f.rn, kill, kills}
	return f
}

// TestDesktopSignin_AnOwedKillThatFailsFailsTheSweep: the sweep's retry of a killed sign-in run's teardown that
// fails returns an error naming the run, so the tick is not counted healthy, and leaves the row open with no close
// row: the sandbox, and the person's display in it, is not gone.
func TestDesktopSignin_AnOwedKillThatFailsFailsTheSweep(t *testing.T) {
	kills := 0
	f := signinOwedFixture(t, errors.New("docker: kill: daemon busy"), &kills)
	err := f.srv.sweepDesktopSignins(context.Background())
	if err == nil || !strings.Contains(err.Error(), f.run.ID.String()) || kills != 1 {
		t.Fatalf("sweep = %v with %d kills; want an error naming run %s, and 1 kill", err, kills, f.run.ID)
	}
	if si := f.st.only(t); si.EndedAt != nil {
		t.Fatalf("row = %+v, want open", si)
	}
	if got := signinCloseRows(f); len(got) != 0 {
		t.Fatalf("desktop_signin.close rows = %v, want none", got)
	}
}

// TestDesktopSignin_AnOwedKillQueuedOnTheRunnerClosesTheRow: a teardown the runner queued
// (runner.ErrPendingOnRunner) counts as done, as the kill's own teardown does: the row closes with agent_resumed
// false and the sweep is healthy.
func TestDesktopSignin_AnOwedKillQueuedOnTheRunnerClosesTheRow(t *testing.T) {
	kills := 0
	f := signinOwedFixture(t, runner.ErrPendingOnRunner, &kills)
	if err := f.srv.sweepDesktopSignins(context.Background()); err != nil || kills != 1 {
		t.Fatalf("sweep = %v with %d kills; want nil and 1 kill", err, kills)
	}
	if si := f.st.only(t); si.EndedAt == nil || si.EndReason != signinEndDone {
		t.Fatalf("row = %+v, want ended %s", si, signinEndDone)
	}
	if got := signinCloseRows(f); !slices.Equal(got, []string{"success false"}) {
		t.Fatalf("desktop_signin.close rows = %v, want [success false]", got)
	}
}
