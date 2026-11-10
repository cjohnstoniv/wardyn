// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// TestPostDecision_DNSQueryIsItsOwnRow: a decision log carrying a DNS query
// writes one run.dns.query row with its name, type and outcome, and no egress
// row.
func TestPostDecision_DNSQueryIsItsOwnRow(t *testing.T) {
	cases := []struct {
		q       egress.DNSQuery
		outcome string
	}{
		{egress.DNSQuery{Name: "origin.test", Type: "A", Outcome: "answered", Answer: "198.18.0.7", Via: "vm_relay"}, "success"},
		{egress.DNSQuery{Name: "exfil.example.com", Type: "TXT", Outcome: "refused", Via: "proxy", Repeat: 4}, "denied"},
	}
	for _, c := range cases {
		h := newHarness(t)
		q := c.q
		d := egress.Deny
		if q.Outcome == "answered" {
			d = egress.Allow
		}
		body, _ := json.Marshal(egress.DecisionLog{Request: egress.Request{Host: q.Name}, Decision: d, RuleSource: "dns:" + q.Outcome, DNS: &q})
		if w := do(t, h.srv, http.MethodPost, "/api/v1/internal/decisions", h.mintRunToken(t, uuid.New()), string(body)); w.Code >= 300 {
			t.Fatalf("post decision: %d %s", w.Code, w.Body.String())
		}
		events := h.audit.snapshot()
		for _, ev := range events {
			if strings.HasPrefix(ev.Action, "egress.") {
				t.Fatalf("a DNS query wrote %s", ev.Action)
			}
		}
		ev := lastAuditEvent(t, events, "run.dns.query")
		if ev.Target != q.Name || ev.Outcome != c.outcome {
			t.Fatalf("row target %q outcome %q, want %q %q", ev.Target, ev.Outcome, q.Name, c.outcome)
		}
		var data map[string]any
		_ = json.Unmarshal(ev.Data, &data)
		if data["name"] != q.Name || data["type"] != q.Type || data["outcome"] != q.Outcome || data["via"] != q.Via {
			t.Fatalf("row data %v", data)
		}
		if q.Answer != "" && data["answer"] != q.Answer {
			t.Fatalf("row data %v, want answer %s", data, q.Answer)
		}
		if q.Repeat > 0 && data["repeat"] != float64(q.Repeat) {
			t.Fatalf("row data %v, want repeat %d", data, q.Repeat)
		}
	}
}
