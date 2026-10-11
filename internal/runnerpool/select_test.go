// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerpool

import (
	"fmt"
	"slices"
	"testing"

	"github.com/google/uuid"
)

func TestSelectIsDeterministicAndIgnoresListOrder(t *testing.T) {
	pool, run := uuid.New(), uuid.New()
	targets := []string{"exec-a", "exec-b", "exec-c", "exec-d"}
	want := Select(pool, run, targets)
	if want == "" {
		t.Fatal("no target selected")
	}
	for range 20 {
		shuffled := slices.Clone(targets)
		slices.Reverse(shuffled)
		if got := Select(pool, run, shuffled); got != want {
			t.Fatalf("a reordered list chose %q, want %q", got, want)
		}
	}
	if Select(pool, run, nil) != "" {
		t.Error("no targets must select nothing")
	}
}

func TestSelectSpreadsRunsAcrossTheTargetsAndKeysOnTheRun(t *testing.T) {
	pool := uuid.New()
	targets := []string{"t0", "t1", "t2", "t3"}
	seen := map[string]int{}
	for i := range 400 {
		seen[Select(pool, uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprint(i))), targets)]++
	}
	if len(seen) != len(targets) {
		t.Fatalf("400 runs reached %d of %d targets: %v", len(seen), len(targets), seen)
	}
	for id, n := range seen {
		if n < 40 {
			t.Errorf("target %s won only %d of 400 runs: %v", id, n, seen)
		}
	}
	// Another pool scores the same run differently: the pool id is part of the key.
	run := uuid.New()
	same := 0
	for range 40 {
		if Select(uuid.New(), run, targets) == Select(pool, run, targets) {
			same++
		}
	}
	if same == 40 {
		t.Error("the pool id does not change the score")
	}
}

func TestSelectedTargetIsAMemberOfTheEligibleSetOnly(t *testing.T) {
	// A target that is not offered can never win, whatever its score would have been.
	pool, run := uuid.New(), uuid.New()
	all := []string{"a", "b", "c"}
	won := Select(pool, run, all)
	rest := slices.DeleteFunc(slices.Clone(all), func(s string) bool { return s == won })
	if got := Select(pool, run, rest); got == won || !slices.Contains(rest, got) {
		t.Fatalf("selection over %v chose %q", rest, got)
	}
}
