// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A zero --interval polls at the default pace, not in a tight loop against the daemon.
func TestLogsCmd_ZeroIntervalDoesNotSpin(t *testing.T) {
	runID := uuid.New()
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/audit"):
			polls.Add(1)
			_ = json.NewEncoder(w).Encode([]types.AuditEvent{})
		case strings.Contains(r.URL.Path, "/api/v1/runs/"):
			_ = json.NewEncoder(w).Encode(types.AgentRun{ID: runID, State: types.RunRunning})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	root := rootCmd()
	root.SetArgs([]string{"run", "logs", runID.String(), "--interval", "0s", "--url", srv.URL, "--token", "tok"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	_ = root.ExecuteContext(ctx)
	if n := polls.Load(); n > 2 {
		t.Errorf("--interval 0 polled the audit trail %d times in 500ms, want at most 2", n)
	}
}
