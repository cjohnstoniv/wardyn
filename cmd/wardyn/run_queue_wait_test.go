// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// queueServer answers POST /runs (recording the body it was sent) and then GET /runs/{id} with the
// scripted runs in turn, the last one repeating. The audit trail is empty; any other request fails the test.
type queueServer struct {
	*httptest.Server
	mu      sync.Mutex
	sent    map[string]any
	audited bool
}

func newQueueServer(t *testing.T, runID uuid.UUID, script []types.AgentRun) *queueServer {
	t.Helper()
	q := &queueServer{}
	polls := 0
	q.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && (r.URL.Path == "/api/v1/runs" || r.URL.Path == "/api/v1/runs/preflight"):
			q.mu.Lock()
			q.sent = map[string]any{}
			_ = json.NewDecoder(r.Body).Decode(&q.sent)
			q.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(types.AgentRun{ID: runID, State: types.RunQueued})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/runs/"+runID.String():
			q.mu.Lock()
			run := script[min(polls, len(script)-1)]
			polls++
			q.mu.Unlock()
			run.ID = runID
			_ = json.NewEncoder(w).Encode(run)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/audit":
			q.mu.Lock()
			q.audited = true
			q.mu.Unlock()
			_, _ = w.Write([]byte("[]"))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(q.Close)
	return q
}

func (q *queueServer) maxWait() (float64, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	v, ok := q.sent["max_wait_sec"].(float64)
	return v, ok
}

func runWithStderr(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := rootCmd()
	root.SetArgs(args)
	var stderr strings.Builder
	root.SetOut(&strings.Builder{})
	root.SetErr(&stderr)
	err := root.Execute()
	return stderr.String(), err
}

// --max-wait sets max_wait_sec; with --wait and no --max-wait the server is told --timeout minus 60s
// (at least 60s, or half of a --timeout of 60s or less), so a run that waits too long fails with queue_timeout before the CLI's own 124.
func TestRunCmd_MaxWaitSecSent(t *testing.T) {
	setWaitPollInterval(t, time.Millisecond)
	for _, tc := range []struct {
		name string
		args []string
		want float64 // 0: not sent
	}{
		{"--max-wait", []string{"--max-wait", "90m"}, 5400},
		{"--wait derives it from --timeout", []string{"--wait", "--timeout", "10m"}, 540},
		{"--wait with the default --timeout", []string{"--wait"}, 29 * 60},
		{"--wait floors it at 60s", []string{"--wait", "--timeout", "90s"}, 60},
		{"--wait never outlives a --timeout of 60s or less", []string{"--wait", "--timeout", "30s"}, 15},
		{"--wait halves a --timeout of exactly 60s", []string{"--wait", "--timeout", "60s"}, 30},
		{"--max-wait beats the derived one", []string{"--wait", "--timeout", "10m", "--max-wait", "2m"}, 120},
		{"neither", nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runID := uuid.New()
			srv := newQueueServer(t, runID, []types.AgentRun{{State: types.RunCompleted}})
			args := append([]string{"run", "--url", srv.URL, "--token", "tok", "--agent", "claude-code"}, tc.args...)
			if _, err := runWithStderr(t, args...); err != nil {
				t.Fatal(err)
			}
			got, sent := srv.maxWait()
			if tc.want == 0 && sent {
				t.Fatalf("max_wait_sec %v sent, want none", got)
			}
			if tc.want != 0 && got != tc.want {
				t.Fatalf("max_wait_sec = %v (sent=%v), want %v", got, sent, tc.want)
			}
		})
	}
}

// A --max-wait under a second is refused before any run is created.
func TestRunCmd_MaxWaitRefusesLessThanASecond(t *testing.T) {
	for _, v := range []string{"0s", "500ms", "-1m"} {
		_, err := runWithStderr(t, "run", "--url", "http://127.0.0.1:1", "--token", "tok", "--agent", "claude-code", "--max-wait", v)
		if err == nil || !strings.Contains(err.Error(), "--max-wait must be at least 1s") {
			t.Errorf("--max-wait %s: err = %v", v, err)
		}
	}
}

// --wait says where a waiting run stands each time its place changes, and not otherwise.
func TestRunCmd_WaitPrintsQueuePlaceChanges(t *testing.T) {
	setWaitPollInterval(t, time.Millisecond)
	runID, poolID := uuid.New(), uuid.New()
	at := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	at2 := func(pos int) types.AgentRun {
		return types.AgentRun{State: types.RunQueued, Queue: &types.RunQueue{PoolID: poolID, QueuedAt: at, DeadlineAt: at.Add(time.Hour), Position: pos, Reason: "runner_pool_at_capacity"}}
	}
	srv := newQueueServer(t, runID, []types.AgentRun{at2(2), at2(2), at2(1), at2(0), {State: types.RunKilled}})
	stderr, err := runWithStderr(t, "run", "--url", srv.URL, "--token", "tok", "--agent", "claude-code", "--wait")
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 2 {
		t.Fatalf("err = %v, want exit 2 for the killed run", err)
	}
	var places []string
	for _, l := range strings.Split(stderr, "\n") {
		if strings.HasPrefix(l, "queued: ") {
			places = append(places, strings.Fields(l)[1])
		}
	}
	if strings.Join(places, ",") != "2,1,0" {
		t.Fatalf("places reported = %v, want 2,1,0 (each change once)\n%s", places, stderr)
	}
}

// A run the queue timed out is FAILED with no agent behind it: --wait exits 1 with the sentence and does
// not go looking for an agent exit code.
func TestRunCmd_WaitQueueTimeoutExits1WithTheSentence(t *testing.T) {
	setWaitPollInterval(t, time.Millisecond)
	runID := uuid.New()
	srv := newQueueServer(t, runID, []types.AgentRun{{State: types.RunFailed, StatusReason: string(types.RunEndQueueTimeout)}})
	stderr, err := runWithStderr(t, "run", "--url", srv.URL, "--token", "tok", "--agent", "claude-code", "--wait")
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 1 {
		t.Fatalf("err = %v, want exit 1", err)
	}
	if srv.audited {
		t.Fatal("the audit trail was read for a run no agent ran")
	}
	want := types.RunEndQueueTimeout.Sentence()
	if !strings.Contains(err.Error(), want) || !strings.Contains(stderr, want) {
		t.Fatalf("the sentence %q is missing: err=%v stderr=%s", want, err, stderr)
	}
}

// A run whose pool was removed while it waited is KILLED: --wait exits 2 and says why.
func TestRunCmd_WaitKilledByPoolRemovalNamesTheReason(t *testing.T) {
	setWaitPollInterval(t, time.Millisecond)
	runID := uuid.New()
	srv := newQueueServer(t, runID, []types.AgentRun{{State: types.RunKilled, StatusReason: string(types.RunEndRunnerPoolRemoved)}})
	stderr, err := runWithStderr(t, "run", "--url", srv.URL, "--token", "tok", "--agent", "claude-code", "--wait")
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 2 {
		t.Fatalf("err = %v, want exit 2", err)
	}
	want := types.RunEndRunnerPoolRemoved.Sentence()
	if !strings.Contains(err.Error(), want) || !strings.Contains(stderr, "reason: "+want) {
		t.Fatalf("the sentence %q is missing: err=%v stderr=%s", want, err, stderr)
	}
}
