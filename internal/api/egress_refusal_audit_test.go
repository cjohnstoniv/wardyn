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

func postRefusalDeny(t *testing.T, h *harness, rule string, r *egress.Refusal) map[string]any {
	t.Helper()
	body, _ := json.Marshal(egress.DecisionLog{
		Request:  egress.Request{Host: "files.partner.example", Port: 443, Method: http.MethodGet, Path: "/x"},
		Decision: egress.Deny, RuleSource: rule, Refusal: r,
	})
	if w := do(t, h.srv, http.MethodPost, "/api/v1/internal/decisions", h.mintRunToken(t, uuid.New()), string(body)); w.Code >= 300 {
		t.Fatalf("post decision: %d %s", w.Code, w.Body.String())
	}
	var data map[string]any
	_ = json.Unmarshal(lastAuditEvent(t, h.audit.events, "egress.deny").Data, &data)
	return data
}

// The classified refusal rides the egress.deny audit row, flat, beside rule_source.
func TestEgressDenyAuditRowCarriesTheRefusal(t *testing.T) {
	data := postRefusalDeny(t, newHarness(t), "policy:denied",
		&egress.Refusal{By: egress.RefusedByWardyn, Reason: "policy_denied", Layer: "profile", LayerName: "Team A"})
	if data["by"] != "wardyn" || data["refusal_reason"] != "policy_denied" || data["layer"] != "profile" || data["layer_name"] != "Team A" {
		t.Fatalf("egress.deny data = %v", data)
	}
	out := postRefusalDeny(t, newHarness(t), "builtin:dial-failed", &egress.Refusal{By: egress.RefusedByOutside, Reason: "connection_refused"})
	if out["by"] != "outside" || out["refusal_reason"] != "connection_refused" {
		t.Fatalf("egress.deny data = %v", out)
	}
	if _, ok := out["layer"]; ok {
		t.Errorf("an outside refusal carries a layer: %v", out)
	}
}

// An unclassified row stays as it was, and a sidecar-supplied refusal that is not a
// closed shape (free text where a token belongs) never reaches the audit data.
func TestEgressDenyAuditRowDropsAMalformedRefusal(t *testing.T) {
	for name, r := range map[string]*egress.Refusal{
		"none":      nil,
		"free text": {By: egress.RefusedByOutside, Reason: "Bearer abc.def"},
		"bad by":    {By: "the network", Reason: "connection_reset"},
	} {
		data := postRefusalDeny(t, newHarness(t), "builtin:dial-failed", r)
		for _, k := range []string{"by", "refusal_reason", "layer", "layer_name"} {
			if _, ok := data[k]; ok {
				t.Errorf("%s: audit data carries %q: %v", name, k, data)
			}
		}
	}
}
