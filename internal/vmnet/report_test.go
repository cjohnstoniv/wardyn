// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vmnet

import (
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// TestReporterNeverHoldsAFlowsRefusal: refusals that end asked flows each go
// out at once with their own flow, host and port, however many arrive in one
// interval; only flow-less kinds are rate limited.
func TestReporterNeverHoldsAFlowsRefusal(t *testing.T) {
	c := &clock{t: time.Unix(1e9, 0)}
	sent := make(chan refusal, 16)
	rp := newReporter(c.now, slog.New(slog.DiscardHandler), func(r refusal) { sent <- r })
	const n = 5
	for i := 0; i < n; i++ {
		rp.refuse(refusal{Flow: fmt.Sprintf("%016x", i), Reason: "sni-mismatch", Host: fmt.Sprintf("h%d.test", i), Port: 443 + i})
		c.t = c.t.Add(100 * time.Millisecond)
	}
	got := map[string]refusal{}
	for i := 0; i < n; i++ {
		select {
		case r := <-sent:
			got[r.Flow] = r
		case <-time.After(2 * time.Second):
			t.Fatalf("%d of %d flows' refusals sent within one interval", len(got), n)
		}
	}
	for i := 0; i < n; i++ {
		r, ok := got[fmt.Sprintf("%016x", i)]
		if !ok || r.Host != fmt.Sprintf("h%d.test", i) || r.Port != 443+i || r.Repeat != 0 {
			t.Fatalf("flow %d: %+v (sent %+v)", i, r, got)
		}
	}
}

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
