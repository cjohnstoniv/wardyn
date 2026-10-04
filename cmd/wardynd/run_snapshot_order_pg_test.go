// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// orderRevoker reports its revocation into the list the runner's teardown and
// the output contract report into.
type orderRevoker struct {
	name  string
	mu    *sync.Mutex
	order *[]string
}

func (r orderRevoker) RevokeRun(context.Context, uuid.UUID) error {
	r.mu.Lock()
	*r.order = append(*r.order, r.name)
	r.mu.Unlock()
	return nil
}

// Both graceful stops, the idle stop and the max-age stop, revoke the run's
// identity and broker credentials, then take the pane snapshot, then stop the
// sandbox, then finish the output; a stop that loses its compare-and-set
// touches none of them. Guarded by WARDYN_TEST_PG.
func TestPG_GracefulStopsSnapshotAfterTheRevocationsAndBeforeStopSandbox(t *testing.T) {
	pool := revocationPool(t)
	ctx := context.Background()
	pg := store.PG{Pool: pool}
	for name, stop := range map[string]func(lifecycleStopper, uuid.UUID) (bool, error){
		"idle stop": func(l lifecycleStopper, id uuid.UUID) (bool, error) {
			out, err := l.StopRun(ctx, id, time.Now().Add(time.Hour))
			return out.Applied, err
		},
		"max-age stop": func(l lifecycleStopper, id uuid.UUID) (bool, error) {
			out, err := l.StopRunMaxAge(ctx, id, time.Now().Add(time.Hour))
			return out.Applied, err
		},
	} {
		t.Run(name, func(t *testing.T) {
			run := skewedRun(t, pg, 60)
			if err := pg.SetSandboxRef(ctx, run.ID, "sbx-snapshot-order"); err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var order []string
			note := func(s string) func(context.Context, uuid.UUID) {
				return func(context.Context, uuid.UUID) { mu.Lock(); order = append(order, s); mu.Unlock() }
			}
			stopper := lifecycleStopper{
				pool:         pool,
				runner:       orderRunner{mu: &mu, order: &order},
				identity:     orderRevoker{"revoke identity", &mu, &order},
				broker:       orderRevoker{"revoke broker", &mu, &order},
				snapshotPane: note("snapshot"),
				finishOutput: note("finish"),
			}
			if applied, err := stop(stopper, run.ID); err != nil || !applied {
				t.Fatalf("stop = %v, %v; want applied", applied, err)
			}
			want := []string{"revoke identity", "revoke broker", "snapshot", "stop", "finish"}
			if !slices.Equal(order, want) {
				t.Fatalf("order %v, want %v", order, want)
			}

			order = nil
			if applied, err := stop(stopper, run.ID); err != nil || applied || len(order) != 0 {
				t.Fatalf("second stop = %v, %v, order %v; want a no-op that touched nothing", applied, err, order)
			}
		})
	}
}
