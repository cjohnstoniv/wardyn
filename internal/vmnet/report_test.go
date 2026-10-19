// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vmnet

import (
	"log/slog"
	"sync"
	"testing"
	"time"
)

func TestReporterRateLimitsPerKind(t *testing.T) {
	c := &clock{t: time.Unix(1e9, 0)}
	var mu sync.Mutex
	var sent []refusal
	done := make(chan struct{}, 64)
	rp := newReporter(c.now, slog.New(slog.DiscardHandler), func(r refusal) {
		mu.Lock()
		sent = append(sent, r)
		mu.Unlock()
		done <- struct{}{}
	})
	wait := func(n int) {
		for i := 0; i < n; i++ {
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("report not sent")
			}
		}
	}
	// A burst of 100 IPv6 drops and one ICMP drop in the same instant: one row
	// per kind now.
	for i := 0; i < 100; i++ {
		rp.refuse(refusal{Reason: "dropped-frame", Detail: "ipv6"})
	}
	rp.refuse(refusal{Reason: "dropped-frame", Detail: "icmp"})
	wait(2)
	rp.flush() // the interval has not passed: nothing yet
	c.t = c.t.Add(reportInterval)
	rp.flush()
	wait(1)
	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 3 {
		t.Fatalf("%d reports, want 3: %+v", len(sent), sent)
	}
	if last := sent[2]; last.Detail != "ipv6" || last.Repeat != 98 {
		t.Fatalf("flushed %+v, want the held ipv6 drops as one row with 98 repeats", last)
	}
}
