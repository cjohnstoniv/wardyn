// Copyright 2026 The Wardyn Authors
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

type runEndHeld struct{}

// Both graceful stops, the idle stop and the max-age stop, bring a "both ways" computer folder back before
// the sandbox stops, under the run's lock from the transition through that stop: the order is the lock, the
// revocations, the pane snapshot, the pass, the stop, the output, then the unlock. A pass cut short that keeps
// the sandbox for a resume leaves it running. Guarded by WARDYN_TEST_PG.
func TestPG_GracefulStopsBringTheComputerFolderBackUnderTheRunLock(t *testing.T) {
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
		for _, kept := range []bool{false, true} {
			t.Run(name, func(t *testing.T) {
				run := skewedRun(t, pg, 60)
				if err := pg.SetSandboxRef(ctx, run.ID, "sbx-folder-back"); err != nil {
					t.Fatal(err)
				}
				var mu sync.Mutex
				var order []string
				add := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }
				note := func(s string) func(context.Context, uuid.UUID) {
					return func(context.Context, uuid.UUID) { add(s) }
				}
				stopper := lifecycleStopper{
					pool:         pool,
					runner:       orderRunner{mu: &mu, order: &order},
					revoke:       func(context.Context, uuid.UUID) map[string]string { add("revoke"); return nil },
					snapshotPane: note("snapshot"),
					finishOutput: note("finish"),
					lockEnd: func(ctx context.Context, id uuid.UUID) (context.Context, func(), bool) {
						add("lock")
						return context.WithValue(ctx, runEndHeld{}, id), func() { add("unlock") }, true
					},
					bringBack: func(ctx context.Context, id uuid.UUID, ref string) bool {
						if ctx.Value(runEndHeld{}) != id || ref != "sbx-folder-back" {
							t.Errorf("pass back on ref %q without the run's lock", ref)
						}
						add("pass")
						return kept
					},
				}
				if applied, err := stop(stopper, run.ID); err != nil || !applied {
					t.Fatalf("stop = %v, %v; want applied", applied, err)
				}
				want := []string{"lock", "revoke", "snapshot", "pass", "stop", "finish", "unlock"}
				if kept {
					want = []string{"lock", "revoke", "snapshot", "pass", "finish", "unlock"}
				}
				if !slices.Equal(order, want) {
					t.Fatalf("kept %v: order %v, want %v", kept, order, want)
				}
			})
		}
	}
}

// A graceful stop whose end hold has passed by its sandbox stop leaves the sandbox to the readers, as every other end
// does: the hold is asked on the ctx lockEnd returned, just before StopSandbox, and the rest of the stop (the output)
// still runs. Catches the idle or max-age stop made past its end hold. Guarded by WARDYN_TEST_PG.
func TestPG_GracefulStopsSkipTheStopPastTheEndHold(t *testing.T) {
	pool := revocationPool(t)
	ctx := context.Background()
	pg := store.PG{Pool: pool}
	run := skewedRun(t, pg, 60)
	if err := pg.SetSandboxRef(ctx, run.ID, "sbx-hold-passed"); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var order []string
	add := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }
	stopper := lifecycleStopper{
		pool:         pool,
		runner:       orderRunner{mu: &mu, order: &order},
		finishOutput: func(context.Context, uuid.UUID) { add("finish") },
		lockEnd: func(ctx context.Context, id uuid.UUID) (context.Context, func(), bool) {
			return context.WithValue(ctx, runEndHeld{}, id), func() {}, true
		},
		holdPassed: func(ctx context.Context) bool {
			if ctx.Value(runEndHeld{}) != run.ID {
				t.Error("the end hold was asked on a ctx lockEnd did not return")
			}
			add("hold")
			return true
		},
	}
	if out, err := stopper.StopRun(ctx, run.ID, time.Now().Add(time.Hour)); err != nil || !out.Applied {
		t.Fatalf("stop = %+v, %v; want applied", out, err)
	}
	if want := []string{"hold", "finish"}; !slices.Equal(order, want) {
		t.Fatalf("order %v, want %v: no sandbox stop past the end hold", order, want)
	}
}
