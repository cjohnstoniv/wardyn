// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeTakeoverProxy answers the brokered approval routes from a script of states, one per read.
type fakeTakeoverProxy struct {
	mu      sync.Mutex
	raised  string
	reads   []string // states served by successive GETs; the last repeats
	expired int
	onExp   string // state the row moves to when withdrawn
}

func (f *fakeTakeoverProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row := func(state string) {
		out := map[string]any{"id": "11111111-1111-1111-1111-111111111111", "state": strings.TrimSuffix(state, "+back")}
		if strings.HasSuffix(state, "+back") {
			out["handed_back_at"] = time.Now().UTC()
		}
		_ = json.NewEncoder(w).Encode(out)
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/wardyn/v1/approvals":
		b, _ := io.ReadAll(r.Body)
		f.raised = string(b)
		w.WriteHeader(http.StatusCreated)
		row("PENDING")
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/expire"):
		f.expired++
		if f.onExp != "" {
			f.reads = []string{f.onExp}
		}
		row(f.reads[0])
	case r.Method == http.MethodGet:
		st := f.reads[0]
		if len(f.reads) > 1 {
			f.reads = f.reads[1:]
		}
		row(st)
	default:
		http.NotFound(w, r)
	}
}

func runNeedsYou(t *testing.T, f *fakeTakeoverProxy, wait time.Duration, clock func() time.Time) (int, string) {
	t.Helper()
	srv := httptest.NewServer(f)
	defer srv.Close()
	var out strings.Builder
	tw := &takeoverWait{base: srv.URL, client: srv.Client(), poll: time.Millisecond, wait: wait,
		now: clock, sleep: func(time.Duration) {}, out: &out}
	return tw.run("the sign-in sends a code to a phone"), out.String()
}

// TestNeedsYouWaitsThroughTheTakeoverUntilHandBack: the agent stays blocked while a person has
// control and returns 0 only once control is handed back; the raise carries the kind and reason only.
func TestNeedsYouWaitsThroughTheTakeoverUntilHandBack(t *testing.T) {
	f := &fakeTakeoverProxy{reads: []string{"PENDING", "APPROVED", "APPROVED", "APPROVED+back"}}
	code, out := runNeedsYou(t, f, time.Hour, time.Now)
	if code != exitHandedBack {
		t.Fatalf("exit = %d, want %d (%s)", code, exitHandedBack, out)
	}
	if f.raised != `{"kind":"human_takeover","payload":{"reason":"the sign-in sends a code to a phone"}}` {
		t.Fatalf("raised %s", f.raised)
	}
	if strings.Count(out, "a person has control") != 1 || !strings.Contains(out, "handed back") {
		t.Fatalf("output = %q", out)
	}
	if f.expired != 0 {
		t.Fatal("withdrew a request someone had taken over")
	}
}

// TestNeedsYouDeclined: a decline tells the agent to carry on without the person.
func TestNeedsYouDeclined(t *testing.T) {
	code, _ := runNeedsYou(t, &fakeTakeoverProxy{reads: []string{"PENDING", "DENIED"}}, time.Hour, time.Now)
	if code != exitDeclined {
		t.Fatalf("exit = %d, want %d", code, exitDeclined)
	}
}

// TestNeedsYouWithdrawsWhenNobodyTakesOver: past the wait the command withdraws its own request, and
// a take over that won the race is honoured instead.
func TestNeedsYouWithdrawsWhenNobodyTakesOver(t *testing.T) {
	start := time.Now()
	n := 0
	clock := func() time.Time { n++; return start.Add(time.Duration(n) * time.Minute) }
	f := &fakeTakeoverProxy{reads: []string{"PENDING"}, onExp: "EXPIRED"}
	if code, _ := runNeedsYou(t, f, 5*time.Minute, clock); code != exitNobody || f.expired != 1 {
		t.Fatalf("exit = %d expired = %d, want %d and one withdrawal", code, f.expired, exitNobody)
	}
	n = 0
	f = &fakeTakeoverProxy{reads: []string{"PENDING"}, onExp: "APPROVED"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.ServeHTTP(w, r)
		f.mu.Lock()
		if f.expired > 0 && r.Method == http.MethodGet {
			f.reads = []string{"APPROVED+back"}
		}
		f.mu.Unlock()
	}))
	defer srv.Close()
	var out strings.Builder
	tw := &takeoverWait{base: srv.URL, client: srv.Client(), poll: time.Millisecond, wait: 5 * time.Minute,
		now: clock, sleep: func(time.Duration) {}, out: &out}
	if code := tw.run("x"); code != exitHandedBack {
		t.Fatalf("raced take over: exit = %d, want %d (%s)", code, exitHandedBack, out.String())
	}
}

// TestNeedsYouStopsWhenTheWithdrawKeepsFailing: past the wait, a proxy that refuses every /expire
// ends the command with exit 4 after maxWithdrawFailures tries, not one try every poll forever.
func TestNeedsYouStopsWhenTheWithdrawKeepsFailing(t *testing.T) {
	start := time.Now()
	n := 0
	clock := func() time.Time { n++; return start.Add(time.Duration(n) * time.Minute) }
	f := &fakeTakeoverProxy{reads: []string{"PENDING"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/expire") {
			f.mu.Lock()
			f.expired++
			stop := f.expired > 2*maxWithdrawFailures
			if stop {
				f.reads = []string{"EXPIRED"} // end an unbounded loop so the test fails rather than hangs
			}
			f.mu.Unlock()
			if stop {
				t.Error("still withdrawing after the bound")
			}
			http.Error(w, "upstream down", http.StatusBadGateway)
			return
		}
		f.ServeHTTP(w, r)
	}))
	defer srv.Close()
	var out strings.Builder
	tw := &takeoverWait{base: srv.URL, client: srv.Client(), poll: time.Millisecond, wait: 5 * time.Minute,
		now: clock, sleep: func(time.Duration) {}, out: &out}
	if code := tw.run("x"); code != exitNobody || f.expired != maxWithdrawFailures {
		t.Fatalf("exit = %d after %d withdrawals, want %d after %d (%s)", code, f.expired, exitNobody, maxWithdrawFailures, out.String())
	}
}

// TestNeedsYouUsage: no reason is a usage error and raises nothing.
func TestNeedsYouUsage(t *testing.T) {
	var out strings.Builder
	if code := needsYou([]string{"-base", "http://127.0.0.1:1"}, &out); code != exitUsage {
		t.Fatalf("exit = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(out.String(), "usage: wardyn needs-you <reason>") {
		t.Fatalf("output = %q", out.String())
	}
}
