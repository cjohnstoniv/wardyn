// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestRunnerNodeRecordCommand: `wardyn runner node-record` reads the run's containment, prints the
// warnings and the machines, and sends the record naming the tuple it read; a run not held sends nothing.
func TestRunnerNodeRecordCommand(t *testing.T) {
	runID, recID := uuid.New(), uuid.New()
	tuple := json.RawMessage(`{"run_id":"` + runID.String() + `","pods":[{"component":"proxy","uid":"uid-proxy-1","node_name":"n1"}]}`)
	// One body serves both calls: the containment read and the record's answer.
	srv := newCmdServer(t, http.StatusOK, map[string]any{
		"pending": map[string]any{"kind": "pending_delete", "since": time.Now().Add(-time.Hour), "bound_until": time.Now().Add(time.Hour), "tuple": tuple},
		"records": []any{}, "id": recID, "attestation": "node_powered_off", "state": "sent",
	})
	args := []string{"runner", "node-record", runID.String(), "--attestation", "node_powered_off", "--source", "provider_console", "--source-id", "i-0abc123", "--url", srv.URL, "--token", "t"}
	root := rootCmd()
	root.SetArgs(args)
	var stdout, stderr strings.Builder
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	err := root.Execute()
	out := stdout.String() + stderr.String()
	if err != nil || len(srv.reqs) != 2 {
		t.Fatalf("node-record: %v, %d requests\n%s", err, len(srv.reqs), out)
	}
	if get := srv.reqs[0]; get.method != http.MethodGet || get.path != "/api/v1/runs/"+runID.String()+"/containment" {
		t.Fatalf("first request %+v", get)
	}
	var req types.RunNodeRecordRequest
	if post := srv.last(); post.method != http.MethodPost || post.path != "/api/v1/runs/"+runID.String()+"/node-record" || json.Unmarshal(post.body, &req) != nil ||
		req.Attestation != "node_powered_off" || req.Source != "provider_console" || req.SourceID != "i-0abc123" || !jsonEqual(req.Tuple, tuple) {
		t.Fatalf("record request %+v %+v", post, req)
	}
	if !strings.Contains(stderr.String(), "A deleted Node object is not confirmation that a machine is off.") || !strings.Contains(stderr.String(), "uid-proxy-1") ||
		!strings.Contains(stdout.String(), "Node record "+recID.String()+" sent to the runner") {
		t.Fatalf("output %q", out)
	}

	free := newCmdServer(t, http.StatusOK, map[string]any{"records": []any{}})
	if err := execCmd(t, append(args[:len(args)-4], "--url", free.URL, "--token", "t")...); err == nil || !strings.Contains(err.Error(), "not waiting for containment evidence") || len(free.reqs) != 1 {
		t.Fatalf("a run not held = %v after %d requests, want a refusal before any record", err, len(free.reqs))
	}
	if err := execCmd(t, "runner", "node-record", runID.String(), "--source", "other", "--source-id", "x", "--url", free.URL, "--token", "t"); err == nil {
		t.Fatal("a record without --attestation was accepted")
	}
}

func jsonEqual(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	return string(xa) == string(ya)
}
