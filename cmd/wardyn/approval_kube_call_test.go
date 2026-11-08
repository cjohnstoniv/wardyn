// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestApprovalGetShowsKubeCall: `approval get` prints a held kubectl write's cluster, method, path and the
// SHA-256 of its body, and none of an MCP call's lines.
func TestApprovalGetShowsKubeCall(t *testing.T) {
	body := json.RawMessage(`{"spec":{"replicas":3}}`)
	scope := types.KubeCallScope{Cluster: "k8s.example.com", Namespace: "team-a", Method: "PATCH",
		Path: "/apis/apps/v1/namespaces/team-a/deployments/web", Query: "dryRun=All", MediaType: "application/merge-patch+json", Body: body}
	key, err := scope.ComputeKey()
	if err != nil {
		t.Fatal(err)
	}
	scope.CallSHA256 = key
	raw, _ := json.Marshal(scope)
	runID, id := uuid.New(), uuid.New()
	srv := newCmdServer(t, http.StatusOK, []types.ApprovalRequest{
		{ID: id, RunID: runID, Kind: types.ApprovalToolCall, State: types.ApprovalPending, RequestedScope: raw},
	})
	out := &strings.Builder{}
	root := rootCmd()
	root.SetArgs([]string{"approval", "get", id.String(), "--run", runID.String(), "--url", srv.URL, "--token", "tok"})
	root.SetOut(out)
	root.SetErr(&strings.Builder{})
	if err := root.Execute(); err != nil {
		t.Fatalf("approval get: %v", err)
	}
	got := out.String()
	sum := sha256.Sum256(body)
	for _, want := range []string{
		"CLUSTER:   \"k8s.example.com\"\n", "METHOD:    \"PATCH\"\n", "PATH:      \"/apis/apps/v1/namespaces/team-a/deployments/web\"\n",
		"QUERY:     \"dryRun=All\"\n", "BODY SHA256: " + hex.EncodeToString(sum[:]) + "\n", key,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	for _, not := range []string{"TOOL:", "SERVER:", "ARGUMENTS:"} {
		if strings.Contains(got, not) {
			t.Errorf("a kubectl write printed the MCP line %q:\n%s", not, got)
		}
	}
}
