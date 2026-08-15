// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/coder/websocket"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
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
			name: "wrapped (%w) context canceled is clean via errors.Is",
			err:  fmt.Errorf("read: %w", context.Canceled),
			want: true,
		},
		{
			name: "look-alike string is NOT clean (no substring guessing)",
			err:  errors.New("read: context canceled"),
			want: false,
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
// runAttach token guard: an empty token must NOT be a client-side hard
// refusal (H13) — local host-mode deployments run without a token, matching
// the client transport (pkg/client.Client). The dial is attempted and the server's 401 (if auth is
// actually required) is the error signal instead.
// --------------------------------------------------------------------------

func TestRunAttach_NoTokenDialsAnyway(t *testing.T) {
	// No token => runAttach must still attempt the dial rather than refusing
	// client-side. Port 0 on loopback refuses the connection, so we assert the
	// failure is a dial error, not the old hard-refusal message.
	c := &sdk.Client{BaseURL: "http://127.0.0.1:0"}
	err := runAttach(context.Background(), c, "run-1")
	if err == nil {
		t.Fatal("expected a dial error against the bogus address, got nil")
	}
	if got := err.Error(); strings.Contains(got, "no admin token") {
		t.Errorf("error = %q, must not hard-refuse on empty token", got)
	}
}

// TestRunAttach_RejectedHandshakeReturnsAPIError is the W25-W25.1-2
// regression: a server-side handshake rejection (401/403/404 — never a
// network failure) used to be flattened into websocket.Dial's raw
// "failed to WebSocket dial: expected status 101 but got NNN" text, discarding
// the HTTP response and always exiting 1. It must now come back as an
// *sdk.APIError carrying the real status + the server's {"error":...} body, so
// exitCodeFor and dialHint classify it exactly like every other API call.
func TestRunAttach_RejectedHandshakeReturnsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"not an operator"}`))
	}))
	defer srv.Close()

	c := &sdk.Client{BaseURL: srv.URL, Token: "tok"}
	err := runAttach(context.Background(), c, "run-1")
	if err == nil {
		t.Fatal("expected an error for a rejected handshake, got nil")
	}
	var ae *sdk.APIError
	if !errors.As(err, &ae) {
		t.Fatalf("error = %v (%T), want an *sdk.APIError (the HTTP rejection must not be discarded)", err, err)
	}
	if ae.Status != http.StatusForbidden {
		t.Errorf("APIError.Status = %d, want %d", ae.Status, http.StatusForbidden)
	}
	if !strings.Contains(err.Error(), "not an operator") {
		t.Errorf("error = %q, want the server's {\"error\":...} message carried through", err.Error())
	}
	if code := exitCodeFor(err); code != 2 {
		t.Errorf("exitCodeFor(403 attach rejection) = %d, want 2 (the universal auth exit code)", code)
	}
}
