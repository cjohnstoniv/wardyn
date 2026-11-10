// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// TestExecRun_PostsTheCommandAndDecodesTheResult: ExecRun posts the command and timeout to the run's
// exec route and decodes every field of the reply.
func TestExecRun_PostsTheCommandAndDecodesTheResult(t *testing.T) {
	id := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method+" "+r.URL.Path != "POST /api/v1/runs/"+id.String()+"/exec" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		if body["command"] != "ls -la" || body["timeout_sec"] != float64(5) || len(body) != 2 {
			t.Errorf("body = %v, want command and timeout_sec only", body)
		}
		writeJSON(w, http.StatusOK, map[string]any{"exit_code": 2, "stdout": "out", "stderr": "err", "truncated": true, "timed_out": false})
	}))
	t.Cleanup(srv.Close)
	got, err := newTestClient(srv).ExecRun(context.Background(), id, client.ExecRequest{Command: "ls -la", TimeoutSec: 5})
	want := client.ExecResult{ExitCode: 2, Stdout: "out", Stderr: "err", Truncated: true}
	if err != nil || got != want {
		t.Fatalf("ExecRun = %+v, %v; want %+v", got, err, want)
	}
}

// TestExecRun_OmitsAnUnsetTimeout: a zero TimeoutSec is left off the wire, so the server's default applies.
func TestExecRun_OmitsAnUnsetTimeout(t *testing.T) {
	b, err := json.Marshal(client.ExecRequest{Command: "true"})
	if err != nil || string(b) != `{"command":"true"}` {
		t.Fatalf("ExecRequest{Command} = %s, %v", b, err)
	}
}

// TestExecRun_DataOutRefusalIsAnAPIError: the data_out refusal comes back as an *APIError with its reason.
func TestExecRun_DataOutRefusalIsAnAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "commands are off", "reason": "data_out_blocked"})
	}))
	t.Cleanup(srv.Close)
	_, err := newTestClient(srv).ExecRun(context.Background(), uuid.New(), client.ExecRequest{Command: "cat secret"})
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict || apiErr.Reason != "data_out_blocked" {
		t.Fatalf("err = %v, want a 409 APIError with reason data_out_blocked", err)
	}
}
