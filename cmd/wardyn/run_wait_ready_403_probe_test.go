// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// PROBE (F4-run-wait-upload-audit, probe a).
// Intended destination: cmd/wardyn/run_wait_ready_403_probe_test.go (package main).
// Reuses nothing beyond the package's own exitError / waitForRunReady /
// waitPollInterval; the scripted server is self-contained so it can COUNT how
// many times GET /runs/{id}/files was polled.
//
// Invariant pinned: waitForRunReady (cmd/wardyn/run_wait_ready.go) must
// NOT spin on a permanent 4xx from GET /api/v1/runs/{id}/files. A 403 (and a
// 401 / 404) is classified permanent by filesErrIsPermanent: the loop returns
// at once with "cannot read its workspace", never exit 124, and
// never issues a second /files poll. Fails if: filesErrIsPermanent starts
// treating 403 as transient, waitForRunReady's RunFiles switch drops the
// permanent arm, or the SDK stops surfacing *sdk.APIError for non-2xx
// (Client.do in pkg/client/client.go).
package main

import (
	"context"
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
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// countingReadyServer answers GET /runs/{id} with RUNNING forever and GET
// /runs/{id}/files with a fixed status + body, counting both.
func countingReadyServer(t *testing.T, runID uuid.UUID, filesStatus int, filesBody string) (*httptest.Server, func() (runPolls, filePolls int)) {
	t.Helper()
	var mu sync.Mutex
	runPolls, filePolls := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/runs/"+runID.String():
			mu.Lock()
			runPolls++
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(types.AgentRun{ID: runID, State: types.RunRunning})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/runs/"+runID.String()+"/files":
			mu.Lock()
			filePolls++
			mu.Unlock()
			w.WriteHeader(filesStatus)
			_, _ = w.Write([]byte(filesBody))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() (int, int) {
		mu.Lock()
		defer mu.Unlock()
		return runPolls, filePolls
	}
}

func TestProbeF4_WaitReady_403DoesNotSpin(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"403-forbidden", http.StatusForbidden, `{"error":"forbidden"}`},
		{"401-unauthorized", http.StatusUnauthorized, `{"error":"invalid or missing token"}`},
		{"404-not-owner", http.StatusNotFound, `{"error":"run not found"}`},
		// A 403 with an EMPTY body: APIError.Error() falls back to the raw body;
		// the permanence decision must key on Status alone, never on the body.
		{"403-empty-body", http.StatusForbidden, ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := waitPollInterval
			waitPollInterval = time.Millisecond
			t.Cleanup(func() { waitPollInterval = prev })

			runID := uuid.New()
			srv, counts := countingReadyServer(t, runID, tc.status, tc.body)
			c := &sdk.Client{BaseURL: srv.URL}

			const timeout = 5 * time.Second
			start := time.Now()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, err := waitForRunReady(ctx, c, runID, timeout, false)
			elapsed := time.Since(start)

			if err == nil {
				t.Fatalf("waitForRunReady returned nil on a %d from /files; want an immediate permanent error", tc.status)
			}
			if !strings.Contains(err.Error(), "cannot read its workspace") {
				t.Fatalf("err = %q, want the permanent 'cannot read its workspace' wrapper", err.Error())
			}
			var apiErr *sdk.APIError
			if !errors.As(err, &apiErr) || apiErr.Status != tc.status {
				t.Fatalf("err = %v, want it to wrap *sdk.APIError{Status:%d}", err, tc.status)
			}
			var ee *exitError
			if errors.As(err, &ee) && ee.code == 124 {
				t.Fatalf("a permanent %d was mapped to the timeout exit 124: the loop spun to the deadline", tc.status)
			}
			if errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("the probe's own 3s ctx deadline fired: the loop spun on a %d", tc.status)
			}
			// 5s --timeout, 1ms poll: anything over ~1s means we waited, not failed.
			if elapsed > time.Second {
				t.Fatalf("took %s to fail on a %d (timeout %s); a permanent error must return on the first tick", elapsed, tc.status, timeout)
			}
			runPolls, filePolls := counts()
			if filePolls != 1 {
				t.Fatalf("/files was polled %d times on a %d; want exactly 1 (no retry of a permanent refusal)", filePolls, tc.status)
			}
			if runPolls != 1 {
				t.Fatalf("/runs/{id} was polled %d times; want exactly 1 (the loop must exit on the same tick)", runPolls)
			}
		})
	}
}

// TestProbeF4_WaitReady_409StillWaits is the control: the ONE 4xx that is
// legitimately transient (409 = no sandbox yet, the empty-SandboxRef arm of
// handleRunFiles in internal/api/run_files.go) must keep polling. Without this
// control a "fix" that fails fast on EVERY 4xx would pass the test above while
// breaking the command's whole purpose.
func TestProbeF4_WaitReady_409StillWaits(t *testing.T) {
	prev := waitPollInterval
	waitPollInterval = time.Millisecond
	t.Cleanup(func() { waitPollInterval = prev })

	runID := uuid.New()
	srv, counts := countingReadyServer(t, runID, http.StatusConflict, `{"error":"run has no sandbox to read (state=RUNNING)"}`)
	c := &sdk.Client{BaseURL: srv.URL}

	_, err := waitForRunReady(context.Background(), c, runID, 30*time.Millisecond, false)
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 124 {
		t.Fatalf("err = %v, want exit 124 (a 409 is transient and must run to the deadline)", err)
	}
	if _, filePolls := counts(); filePolls < 2 {
		t.Fatalf("/files polled %d times on a 409; want >= 2 (it must be retried)", filePolls)
	}
}
