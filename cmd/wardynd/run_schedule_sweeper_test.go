// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/sweephealth"
)

type fakeScheduleSweeper struct{ err error }

func (f fakeScheduleSweeper) RunScheduleSweep(context.Context) error { return f.err }

// Each schedule sweep tick is recorded as the run_schedule sweep: a clean one as a success, one that errored as
// an attempt only, so a leader whose schedule sweep stopped reads as stale.
func TestRunScheduleSweeper_RecordsItsTicks(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{{"success", nil}, {"error", errors.New("postgres is down")}} {
		t.Run(tc.name, func(t *testing.T) {
			ms := sweephealth.NewMemStore()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			startRunScheduleSweeper(ctx, nil, fakeScheduleSweeper{err: tc.err}, 5*time.Millisecond, sweephealth.New(ms, "r1", nil))
			for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(2 * time.Millisecond) {
				ticks, err := ms.Ticks(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				tk := ticks[sweephealth.RunSchedule]
				if !tk.AttemptedAt.IsZero() && (tc.err != nil || !tk.SucceededAt.IsZero()) {
					if tc.err != nil && !tk.SucceededAt.IsZero() {
						t.Fatalf("a tick that errored recorded a success: %+v", tk)
					}
					return
				}
				if time.Now().After(deadline) {
					t.Fatal("the run_schedule sweep never recorded a tick")
				}
			}
		})
	}
}
