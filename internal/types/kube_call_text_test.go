// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"strings"
	"testing"
)

func kubeCallTextScope(t *testing.T, method, body string) json.RawMessage {
	t.Helper()
	s := KubeCallScope{Cluster: "k8s.example.com", Namespace: "team-a", Method: method,
		Path: "/apis/apps/v1/namespaces/team-a/deployments/web", HoldSeconds: 90}
	if body != "" {
		s.MediaType, s.Body = "application/merge-patch+json", json.RawMessage(body)
	}
	key, err := s.ComputeKey()
	if err != nil {
		t.Fatal(err)
	}
	s.CallSHA256 = key
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A held write's body reads as the canonical bytes with only indentation added, so a number past 2^53
// keeps every digit.
func TestKubeCallTextKeepsEveryDigit(t *testing.T) {
	got := KubeCallTextOf(kubeCallTextScope(t, "PATCH", `{"spec":{"replicas":9007199254740993}}`))
	if got == nil {
		t.Fatal("KubeCallTextOf = nil for a kubectl call scope")
	}
	if !strings.Contains(got.Arguments, "9007199254740993") {
		t.Errorf("arguments lost a digit:\n%s", got.Arguments)
	}
	if !strings.Contains(got.Arguments, "\n  \"spec\": {\n    \"replicas\": 9007199254740993\n  }\n}") {
		t.Errorf("arguments are not indented:\n%q", got.Arguments)
	}
	if got.Extra != "" {
		t.Errorf("extra = %q, want none", got.Extra)
	}
}

// A write with no body (a DELETE) has a text all the same, the empty one, so the console can tell it from a
// row whose text the server did not send.
func TestKubeCallTextOfBodilessWrite(t *testing.T) {
	got := KubeCallTextOf(kubeCallTextScope(t, "DELETE", ""))
	if got == nil {
		t.Fatal("KubeCallTextOf = nil for a kubectl call with no body")
	}
	if *got != (MCPCallText{Arguments: ""}) {
		t.Errorf("got %+v, want an empty Arguments", *got)
	}
}

func TestKubeCallTextOfOtherScopes(t *testing.T) {
	for name, raw := range map[string]json.RawMessage{
		"MCP call scope": mcpCallTextScope(t),
		"plain tool":     json.RawMessage(`{"tool":"Bash","cmd":"ls"}`),
		"junk":           json.RawMessage(`{"call_sha256":`),
		"empty":          nil,
	} {
		if got := KubeCallTextOf(raw); got != nil {
			t.Errorf("%s: got %+v, want nil", name, got)
		}
	}
}
