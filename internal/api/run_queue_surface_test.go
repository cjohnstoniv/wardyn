// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

func intp(n int) *int { return &n }

// T21: a pool running its limit with nobody waiting still answers "waits": the verdict keys on the
// cap as well as on the queue, since a launch into a full pool queues.
func TestSlotsVerdictAtCapNobodyWaiting(t *testing.T) {
	for _, tc := range []struct {
		name          string
		max           *int
		used, waiting int
		want          string
	}{
		{"at the cap, nobody waiting", intp(2), 2, 0, client.SlotsWaits},
		{"over the cap, nobody waiting", intp(2), 3, 0, client.SlotsWaits},
		{"room, nobody waiting", intp(2), 1, 0, client.SlotsStartsNow},
		{"room, others waiting", intp(2), 1, 1, client.SlotsWaits},
		{"no cap, empty", nil, 0, 0, client.SlotsStartsNow},
		{"no cap, busy", nil, 40, 0, client.SlotsStartsNow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := slotsVerdict(tc.max, tc.used, tc.waiting); got != tc.want {
				t.Fatalf("slotsVerdict(%v, %d, %d) = %q, want %q", tc.max, tc.used, tc.waiting, got, tc.want)
			}
		})
	}
}

// poolSlots carries the counts and the cap, and max_wait_sec only when asked for.
func TestPoolSlotsAnswer(t *testing.T) {
	pool := types.RunnerPool{Limits: &types.RunnerPoolLimits{MaxConcurrentRuns: intp(4)}}
	got := poolSlots(pool, store.PoolCounts{Used: 4, Waiting: 2}, defaultMaxWaitSec(types.RunnerPoolRunInteractive))
	want := &client.PoolSlots{Used: 4, Waiting: 2, Max: intp(4), Verdict: client.SlotsWaits, MaxWaitSec: 3600}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("poolSlots = %+v, want %+v", got, want)
	}
	if bg := defaultMaxWaitSec(types.RunnerPoolRunBackground); bg != 86400 {
		t.Fatalf("background wait = %d, want 86400", bg)
	}
	if s := poolSlots(types.RunnerPool{}, store.PoolCounts{Used: 7}, 0); s.Max != nil || s.Verdict != client.SlotsStartsNow || s.MaxWaitSec != 0 {
		t.Fatalf("an uncapped pool answers %+v", s)
	}
}

// Privacy: the slot answer is these five counts and words, and no field can be added without this
// test saying so (counts only; no run id, owner or executor id).
func TestPoolSlotsWireIsCountsOnly(t *testing.T) {
	raw, err := json.Marshal(poolSlots(types.RunnerPool{Limits: &types.RunnerPoolLimits{MaxConcurrentRuns: intp(1)}}, store.PoolCounts{Used: 1, Waiting: 1}, 60))
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]any
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	var got []string
	for k := range keys {
		got = append(got, k)
	}
	slices.Sort(got)
	if want := []string{"max", "max_wait_sec", "used", "verdict", "waiting"}; !slices.Equal(got, want) {
		t.Fatalf("slots fields = %v, want %v", got, want)
	}
	for k, v := range keys {
		if k != "verdict" {
			if _, ok := v.(float64); !ok {
				t.Fatalf("slots.%s = %T %v, want a number", k, v, v)
			}
		}
	}
}
