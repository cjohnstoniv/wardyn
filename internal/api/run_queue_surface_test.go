// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

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

// A pool's capacity is numbers only, and never says whose runs they are: the field set is fixed, and
// every value under it is a number.
func TestPoolCapacityWireIsCountsOnly(t *testing.T) {
	l := &types.RunnerLimits{RunsAtOnce: 2, Total: types.RunnerAmounts{CPUMillis: 4000, MemoryMiB: 8192, DiskMiB: 10240},
		PerRun: types.RunnerAmounts{CPUMillis: 2000, MemoryMiB: 4096, DiskMiB: 5120}}
	id := uuid.New()
	row := store.RunnerPoolCatalogueRow{Pool: types.RunnerPool{State: types.RunnerPoolActive}, ExecutorIDs: []string{"docker"}, OwnRunnerIDs: []uuid.UUID{id}}
	fits := map[string]runnerFit{"docker": {limits: l, used: types.RunnerTally{Runs: 1, CPUMillis: 2000}}, id.String(): {limits: l}}
	raw, err := json.Marshal((&Server{}).poolCapacity(row, fits, func(string) bool { return true }))
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]any
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	var got []string
	for k, v := range keys {
		got = append(got, k)
		var nums []any
		switch v := v.(type) {
		case map[string]any:
			for _, n := range v {
				nums = append(nums, n)
			}
		default:
			nums = append(nums, v)
		}
		for _, n := range nums {
			if _, ok := n.(float64); !ok {
				t.Fatalf("capacity.%s holds %T %v, want numbers only", k, n, n)
			}
		}
	}
	slices.Sort(got)
	if want := []string{"available", "in_use", "largest_per_run", "runners", "runners_online", "total"}; !slices.Equal(got, want) {
		t.Fatalf("capacity fields = %v, want %v", got, want)
	}
	if !strings.Contains(string(raw), `"in_use":{"runs":1,"cpu_millis":2000`) || !strings.Contains(string(raw), `"total":{"runs":4,"cpu_millis":8000`) {
		t.Fatalf("capacity = %s, want both runners summed", raw)
	}
}
