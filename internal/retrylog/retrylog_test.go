// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package retrylog

import (
	"testing"
	"time"
)

func TestDelayDoublesToTheCap(t *testing.T) {
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second, 5 * time.Minute, 5 * time.Minute}
	for i, w := range want {
		if got := Delay(i + 1); got != w {
			t.Errorf("Delay(%d) = %s, want %s", i+1, got, w)
		}
	}
	if got := Delay(1000); got != Cap {
		t.Errorf("Delay(1000) = %s, want the cap", got)
	}
}

// A message recurring every second is logged at 0 s, 5 s, 15 s, 35 s, ... with the count of the
// occurrences in between, and an hour of it is a few dozen lines, not 3600.
func TestDedupeLogsOncePerGrowingIntervalWithACount(t *testing.T) {
	var d Dedupe
	start := time.Unix(1_700_000_000, 0)
	var at []time.Duration
	var counts []int
	for s := 0; s < 3600; s++ {
		now := start.Add(time.Duration(s) * time.Second)
		if log, n := d.Note("engine down", now); log {
			at = append(at, now.Sub(start))
			counts = append(counts, n)
		}
	}
	wantAt := []time.Duration{0, 5 * time.Second, 15 * time.Second, 35 * time.Second, 75 * time.Second, 155 * time.Second, 315 * time.Second, 615 * time.Second}
	for i, w := range wantAt {
		if i >= len(at) || at[i] != w {
			t.Fatalf("lines at %v, want them to start %v", at, wantAt)
		}
	}
	if counts[0] != 0 || counts[1] != 4 || counts[2] != 9 || counts[7] != 299 {
		t.Errorf("repeat counts %v, want 0, 4, 9, ... 299 at the cap", counts)
	}
	if len(at) > 20 {
		t.Errorf("an hour of a once-a-second failure logged %d lines", len(at))
	}
}

// Different messages are counted apart, and one that stops and comes back is logged at once.
func TestDedupeKeepsMessagesApartAndForgetsAStoppedOne(t *testing.T) {
	var d Dedupe
	now := time.Unix(1_700_000_000, 0)
	if log, _ := d.Note("a", now); !log {
		t.Fatal("first a not logged")
	}
	if log, _ := d.Note("b", now); !log {
		t.Fatal("first b not logged: it was counted as a repeat of a")
	}
	if log, _ := d.Note("a", now.Add(time.Second)); log {
		t.Fatal("a repeated within its interval was logged")
	}
	if log, n := d.Note("a", now.Add(2*Cap+2*time.Second)); !log || n != 0 {
		t.Fatalf("a back after 2*Cap: log=%v repeated=%d, want logged afresh", log, n)
	}
	if len(d.seen) != 1 {
		t.Errorf("%d messages kept, want b forgotten", len(d.seen))
	}
}
