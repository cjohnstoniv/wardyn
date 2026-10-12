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

type fakeQueueSweeper struct{ err error }

func (f fakeQueueSweeper) SweepRunQueue(context.Context) error { return f.err }

// Each queue sweep tick is recorded as the run_queue sweep: a clean one as a success, one that errored as
// an attempt only, so a leader whose queue sweep stopped reads as stale.
func TestRunQueueSweeper_RecordsItsTicks(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{{"success", nil}, {"error", errors.New("postgres is down")}} {
		t.Run(tc.name, func(t *testing.T) {
			ms := sweephealth.NewMemStore()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			startRunQueueSweeper(ctx, nil, fakeQueueSweeper{err: tc.err}, 5*time.Millisecond, sweephealth.New(ms, "r1", nil))
			for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(2 * time.Millisecond) {
				ticks, err := ms.Ticks(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				tk := ticks[sweephealth.RunQueue]
				if !tk.AttemptedAt.IsZero() && (tc.err != nil || !tk.SucceededAt.IsZero()) {
					if tc.err != nil && !tk.SucceededAt.IsZero() {
						t.Fatalf("a tick that errored recorded a success: %+v", tk)
					}
					return
				}
				if time.Now().After(deadline) {
					t.Fatal("the run_queue sweep never recorded a tick")
				}
			}
		})
	}
}
