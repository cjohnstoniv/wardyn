// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// The idle stop and the max-age stop reach the approval cascade through their own hooks, so the run's
// event feed can tell them apart. Guarded by WARDYN_TEST_PG.
func TestPG_StopsCallTheApprovalHookOfTheirOwnCause(t *testing.T) {
	pool := revocationPool(t)
	ctx := context.Background()
	pg := store.PG{Pool: pool}
	for name, tc := range map[string]struct {
		stop func(lifecycleStopper, uuid.UUID) error
		want string
	}{
		"idle stop": {func(l lifecycleStopper, id uuid.UUID) error {
			_, err := l.StopRun(ctx, id, time.Now().Add(time.Hour))
			return err
		}, "idle"},
		"max-age stop": {func(l lifecycleStopper, id uuid.UUID) error {
			_, err := l.StopRunMaxAge(ctx, id, time.Now().Add(time.Hour))
			return err
		}, "max-age"},
	} {
		t.Run(name, func(t *testing.T) {
			run := skewedRun(t, pg, 60)
			var called []string
			note := func(s string) func(context.Context, uuid.UUID) {
				return func(context.Context, uuid.UUID) { called = append(called, s) }
			}
			stopper := lifecycleStopper{pool: pool, cancelApprovals: note("idle"), cancelMaxAgeApprovals: note("max-age")}
			if err := tc.stop(stopper, run.ID); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(called, []string{tc.want}) {
				t.Fatalf("approval hooks called %v, want only %v", called, tc.want)
			}
		})
	}
}
