// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// TestCorrelationID_WireName pins correlation_id on the create request and on the run it returns.
func TestCorrelationID_WireName(t *testing.T) {
	body, err := json.Marshal(client.CreateRunRequest{Agent: "claude-code", CorrelationID: "build-42"})
	if err != nil || !strings.Contains(string(body), `"correlation_id":"build-42"`) {
		t.Errorf("request = %s (%v); want correlation_id on the wire", body, err)
	}
	if body, _ := json.Marshal(client.CreateRunRequest{Agent: "claude-code"}); strings.Contains(string(body), "correlation_id") {
		t.Errorf("request = %s; a request with none sends none", body)
	}
	var run client.AgentRun
	if err := json.Unmarshal([]byte(`{"correlation_id":"build-42"}`), &run); err != nil || run.CorrelationID != "build-42" {
		t.Errorf("run = %+v (%v); want CorrelationID build-42", run, err)
	}
}
