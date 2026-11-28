// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// followCost is a fake server for one RUNNING run. /healthz answers version; the trail (oldest first, onPoll may
// append) is served newest first for ?order=desc, honours ?limit= (default 1000) and ?offset=, and sets
// X-Wardyn-Truncated as the daemon does. Every request is counted.
type followCost struct {
	mu       sync.Mutex
	version  string
	trail    []types.AuditEvent
	reads    int      // audit requests
	polls    int      // audit requests at offset 0: one per follow read
	runReads int      // GET /api/v1/runs/{id}
	rows     int      // audit rows served
	limits   []string // each audit request's limit=
	onPoll   func(poll int, f *followCost)
}

func (f *followCost) serve(t *testing.T, runID uuid.UUID) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]string{"version": f.version})
		case strings.HasPrefix(r.URL.Path, "/api/v1/audit"):
			q := r.URL.Query()
			offset, _ := strconv.Atoi(q.Get("offset"))
			limit, err := strconv.Atoi(q.Get("limit"))
			if err != nil || limit <= 0 {
				limit = 1000
			}
			f.reads++
			f.limits = append(f.limits, q.Get("limit"))
			if offset == 0 {
				f.polls++
				if f.onPoll != nil {
					f.onPoll(f.polls, f)
				}
			}
			page := slices.Clone(f.trail)
			if q.Get("order") == "desc" {
				slices.Reverse(page)
			}
			page = page[min(offset, len(page)):]
			if len(page) > limit {
				page = page[:limit]
				w.Header().Set("X-Wardyn-Truncated", "true")
			}
			f.rows += len(page)
			_ = json.NewEncoder(w).Encode(page)
		case strings.HasPrefix(r.URL.Path, "/api/v1/runs/"):
			f.runReads++
			_ = json.NewEncoder(w).Encode(types.AgentRun{ID: runID, State: types.RunRunning})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func trailOf(n int) []types.AuditEvent {
	t0 := time.Now().UTC().Add(-time.Hour)
	out := make([]types.AuditEvent, n)
	for i := range out {
		out[i] = types.AuditEvent{ID: uuid.New(), Time: t0.Add(time.Duration(i) * time.Second), Action: "egress.decision", Outcome: "success"}
	}
	return out
}

var errMinute = errors.New("a simulated minute has passed")

// simulateMinute swaps logsSleep for one that records each wait and adds it to a clock without sleeping, and stops
// the follow once the clock reaches a minute.
func simulateMinute(t *testing.T) *[]time.Duration {
	t.Helper()
	var waits []time.Duration
	var clock time.Duration
	orig := logsSleep
	logsSleep = func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		if clock += d; clock >= time.Minute {
			return errMinute
		}
		return nil
	}
	t.Cleanup(func() { logsSleep = orig })
	return &waits
}

func followFor(t *testing.T, srv *httptest.Server, runID uuid.UUID, extra ...string) (string, error) {
	t.Helper()
	return runCmdWithTimeout(t, func(root *cobra.Command) {
		root.SetArgs(append([]string{"run", "logs", runID.String(), "--url", srv.URL, "--token", "tok"}, extra...))
	})
}

// TestLogsCmd_FollowQuietRunCostsLess measures one simulated minute of `run logs --follow` on a quiet run with a
// 1,200-event trail at the default 2s interval: as the follow used to read (every interval, the server's default
// page each time) and as it reads now. Both lines are logged; they are the measurement for the change.
func TestLogsCmd_FollowQuietRunCostsLess(t *testing.T) {
	measure := func(t *testing.T) *followCost {
		runID := uuid.New()
		f := &followCost{version: "0.10.5", trail: trailOf(1200)}
		srv := f.serve(t, runID)
		simulateMinute(t)
		if _, err := followFor(t, srv, runID); !errors.Is(err, errMinute) {
			t.Fatalf("run logs ended with %v, want the simulated minute", err)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		t.Logf("one minute of follow: %d audit requests, %d run reads, %d audit rows served", f.reads, f.runReads, f.rows)
		return f
	}
	var before, after *followCost
	t.Run("as it used to read", func(t *testing.T) {
		page, ceiling := logsFollowPage, logsMaxInterval
		logsFollowPage, logsMaxInterval = 0, 0 // no limit, and the wait never grows past --interval
		t.Cleanup(func() { logsFollowPage, logsMaxInterval = page, ceiling })
		before = measure(t)
	})
	t.Run("as it reads now", func(t *testing.T) { after = measure(t) })
	if before == nil || after == nil {
		return
	}
	if got, was := after.reads+after.runReads, before.reads+before.runReads; got*3 > was {
		t.Errorf("%d requests a minute, was %d: want at most a third", got, was)
	}
	if after.rows*10 > before.rows {
		t.Errorf("%d rows a minute, was %d: want at most a tenth", after.rows, before.rows)
	}
	for i, l := range after.limits[2:] { // the first read pages through 1,200 events at the default page
		if l != strconv.Itoa(logsFollowPage) {
			t.Errorf("audit request %d asked for limit %q, want %d", i+3, l, logsFollowPage)
		}
	}
}

// TestLogsCmd_FollowBacksOffAndComesBack: the wait doubles from --interval while nothing new arrives, stops at
// logsMaxInterval, and drops back to --interval at the read that finds a new event; every event prints once.
func TestLogsCmd_FollowBacksOffAndComesBack(t *testing.T) {
	runID := uuid.New()
	f := &followCost{version: "0.10.5", trail: trailOf(3), onPoll: func(poll int, f *followCost) {
		if poll == 5 {
			f.trail = append(f.trail, trailOf(1)...)
		}
	}}
	srv := f.serve(t, runID)
	waits := simulateMinute(t)
	out, err := followFor(t, srv, runID)
	if !errors.Is(err, errMinute) {
		t.Fatalf("run logs ended with %v, want the simulated minute", err)
	}
	s := time.Second
	want := []time.Duration{2 * s, 4 * s, 8 * s, 10 * s, 2 * s, 4 * s}
	if len(*waits) < len(want) || !slices.Equal((*waits)[:len(want)], want) {
		t.Errorf("waits = %v, want %v first", *waits, want)
	}
	if n := strings.Count(out, "egress.decision"); n != 4 {
		t.Errorf("%d events printed, want 4:\n%s", n, out)
	}
}

// TestLogsCmd_FollowRefusesAServerOlderThan094: a server that cannot answer newest first is refused before any
// read, with the command that works; a version that cannot be read is not refused; --follow=false never asks.
func TestLogsCmd_FollowRefusesAServerOlderThan094(t *testing.T) {
	for _, tc := range []struct {
		version string
		refused bool
	}{
		{"0.9.3", true}, {"0.8.6+abc1234", true}, {"0.9.4", false}, {"0.10.5", false}, {"dev", false}, {"", false},
	} {
		t.Run("version "+tc.version, func(t *testing.T) {
			runID := uuid.New()
			f := &followCost{version: tc.version, trail: trailOf(2)}
			srv := f.serve(t, runID)
			simulateMinute(t)
			_, err := followFor(t, srv, runID)
			f.mu.Lock()
			reads := f.reads
			f.mu.Unlock()
			if tc.refused {
				if err == nil || !strings.Contains(err.Error(), "needs 0.9.4 or later") || !strings.Contains(err.Error(), "--follow=false") || reads != 0 {
					t.Errorf("err = %v after %d audit reads, want the refusal before any read", err, reads)
				}
				return
			}
			if !errors.Is(err, errMinute) {
				t.Errorf("err = %v, want the follow to run", err)
			}
		})
	}
	runID := uuid.New()
	f := &followCost{version: "0.9.3", trail: trailOf(2)}
	srv := f.serve(t, runID)
	if out, err := followFor(t, srv, runID, "--follow=false"); err != nil || strings.Count(out, "egress.decision") != 2 {
		t.Errorf("--follow=false against 0.9.3: %v\n%s", err, out)
	}
}
