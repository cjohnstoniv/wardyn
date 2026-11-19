// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// TestPostDecision_RecordsModelTunnel: the proxy's mark on the CONNECT allow of an opaque model-host tunnel
// reaches the egress.allow row's data, and an unmarked row carries no such key.
func TestPostDecision_RecordsModelTunnel(t *testing.T) {
	post := func(marked bool) map[string]any {
		h := newHarness(t)
		body, _ := json.Marshal(egress.DecisionLog{
			Request:  egress.Request{Host: "bedrock-runtime.us-east-1.amazonaws.com", Port: 443, Method: http.MethodConnect},
			Decision: egress.Allow, RuleSource: "policy:allowlist", ModelTunnel: marked,
		})
		if w := do(t, h.srv, http.MethodPost, "/api/v1/internal/decisions", h.mintRunToken(t, uuid.New()), string(body)); w.Code >= 300 {
			t.Fatalf("post decision: %d %s", w.Code, w.Body.String())
		}
		var data map[string]any
		_ = json.Unmarshal(lastAuditEvent(t, h.audit.snapshot(), "egress.allow").Data, &data)
		return data
	}
	if data := post(true); data["model_tunnel"] != true {
		t.Errorf("marked egress.allow data = %v, want model_tunnel true", data)
	}
	if data := post(false); data["model_tunnel"] != nil {
		t.Errorf("unmarked egress.allow data = %v, want no model_tunnel", data)
	}
}
