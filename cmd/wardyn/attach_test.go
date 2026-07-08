// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

// writeFile is a small test helper shared across the cmd/wardyn tests for
// materializing temp files (e.g. policy JSON bodies).
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// --------------------------------------------------------------------------
// buildWSURL: HTTP base URL -> WebSocket attach URL
// --------------------------------------------------------------------------

func TestBuildWSURL(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		runID   string
		want    string
	}{
		{
			name:    "https becomes wss",
			baseURL: "https://wardyn.example.com",
			runID:   "abc",
			want:    "wss://wardyn.example.com/api/v1/runs/abc/attach",
		},
		{
			name:    "http becomes ws",
			baseURL: "http://localhost:8080",
			runID:   "run-1",
			want:    "ws://localhost:8080/api/v1/runs/run-1/attach",
		},
		{
			name:    "trailing slash on base is trimmed (no doubled slash)",
			baseURL: "https://wardyn.example.com/",
			runID:   "xyz",
			want:    "wss://wardyn.example.com/api/v1/runs/xyz/attach",
		},
		{
			name:    "non-default port preserved",
			baseURL: "http://10.0.0.5:9443",
			runID:   "id",
			want:    "ws://10.0.0.5:9443/api/v1/runs/id/attach",
		},
		{
			name:    "already-ws scheme left untouched",
			baseURL: "ws://host",
			runID:   "r",
			want:    "ws://host/api/v1/runs/r/attach",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := buildWSURL(tc.baseURL, tc.runID)
			if got != tc.want {
				t.Errorf("buildWSURL(%q, %q) = %q, want %q", tc.baseURL, tc.runID, got, tc.want)
			}
		})
	}
}

// --------------------------------------------------------------------------
// isNormalClose: classify clean detaches vs real errors
// --------------------------------------------------------------------------

func TestIsNormalClose(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil is a clean close", err: nil, want: true},
		{
			name: "normal closure is clean",
			err:  websocket.CloseError{Code: websocket.StatusNormalClosure},
			want: true,
		},
		{
			name: "going away is clean",
			err:  websocket.CloseError{Code: websocket.StatusGoingAway},
			want: true,
		},
		{
			name: "context canceled is a clean detach",
			err:  context.Canceled,
			want: true,
		},
		{
			name: "wrapped context canceled is clean (substring match)",
			err:  errors.New("read: context canceled"),
			want: true,
		},
		{
			name: "abnormal closure is a real error",
			err:  websocket.CloseError{Code: websocket.StatusAbnormalClosure},
			want: false,
		},
		{
			name: "arbitrary error is a real error",
			err:  errors.New("connection reset by peer"),
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isNormalClose(tc.err); got != tc.want {
				t.Errorf("isNormalClose(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// --------------------------------------------------------------------------
// runAttach token guard: must refuse before dialing when no token is set
// --------------------------------------------------------------------------

func TestRunAttach_NoTokenRefuses(t *testing.T) {
	// No token => runAttach must fail fast with the no-token error and never
	// attempt to dial the (here, bogus) URL.
	c := &apiClient{baseURL: "http://127.0.0.1:0"}
	err := runAttach(context.Background(), c, "run-1")
	if err == nil {
		t.Fatal("expected no-token error, got nil")
	}
	if got := err.Error(); !strings.Contains(got, "no admin token") {
		t.Errorf("error = %q, want no-admin-token message", got)
	}
}
