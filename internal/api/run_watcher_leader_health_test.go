// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/sweephealth"
)

// The run_watcher tick runs on every replica into one shared row, and four of
// its sub-passes run on the sweeper leader alone. A follower's pass of each is
// an attempt, never a success, so a follower's tick cannot keep a failing or
// stopped leader's run_watcher from reading stale; the leader's pass still
// records its success.
func TestRunWatcherLeaderSubPasses_OnlyTheLeaderRecordsSuccess(t *testing.T) {
	for _, c := range []struct {
		name  string
		sweep func(t *testing.T, lease SweeperLease) func(context.Context) error
	}{
		{"run pause", func(t *testing.T, lease SweeperLease) func(context.Context) error {
			return newPauseFixture(t, 0, func(cfg *Config) { cfg.SweeperLease = lease }).srv.sweepRunPauses
		}},
		{"desktop sign-in", func(t *testing.T, lease SweeperLease) func(context.Context) error {
			f := newSigninFixture(t)
			f.srv.cfg.SweeperLease = lease
			return f.srv.sweepDesktopSignins
		}},
		{"ended desktop pause", func(t *testing.T, lease SweeperLease) func(context.Context) error {
			f := newVMSignInFixture(t, "memory")
			f.srv.cfg.SweeperLease = lease
			return f.srv.sweepEndedDesktopVMPauses
		}},
		{"service health of a run with no runner", func(t *testing.T, lease SweeperLease) func(context.Context) error {
			return newServiceHealthFixture(t, answerWith(stateFileJSON("healthy", "", 0, "null"), 0),
				func(cfg *Config) { cfg.SweeperLease = lease }, serviceRun(time.Now().UTC(), "ref-local", time.Hour)).srv.sweepServiceHealth
		}},
	} {
		for _, lead := range []bool{false, true} {
			role := map[bool]string{false: "follower", true: "leader"}[lead]
			t.Run(c.name+", "+role, func(t *testing.T) {
				lease := &fakeLease{lead: lead, termCtx: context.Background(), current: func(int64) bool { return true }}
				sweep := c.sweep(t, lease)
				ticks := sweephealth.NewMemStore()
				if err := sweephealth.New(ticks, role, nil).Tick(context.Background(), sweephealth.RunWatcher, sweep); err != nil {
					t.Fatalf("pass: %v", err)
				}
				got, err := ticks.Ticks(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				tk := got[sweephealth.RunWatcher]
				if tk.AttemptedAt.IsZero() {
					t.Error("no attempt recorded")
				}
				if succeeded := !tk.SucceededAt.IsZero(); succeeded != lead {
					t.Errorf("the %s's pass recorded success=%v, want %v", role, succeeded, lead)
				}
				if begins, ends := lease.counts(); begins != ends || (lead && begins == 0) {
					t.Errorf("lease begins %d ends %d: the leader's term was not taken and released", begins, ends)
				}
			})
		}
	}
}
