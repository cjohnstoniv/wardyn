// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"encoding/json"
	"testing"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// TestRunPolicyDigest_WireName pins policy_digest on the run the server returns.
func TestRunPolicyDigest_WireName(t *testing.T) {
	var run client.AgentRun
	if err := json.Unmarshal([]byte(`{"policy_digest":"abc123"}`), &run); err != nil || run.PolicyDigest != "abc123" {
		t.Errorf("run = %+v (%v); want PolicyDigest abc123", run, err)
	}
	if body, _ := json.Marshal(client.AgentRun{}); json.Valid(body) && string(body) != "" {
		var m map[string]json.RawMessage
		_ = json.Unmarshal(body, &m)
		if _, ok := m["policy_digest"]; ok {
			t.Errorf("run = %s; a run with no digest sends none", body)
		}
	}
}
