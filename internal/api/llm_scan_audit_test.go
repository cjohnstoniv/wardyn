// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// countActions buckets recorded audit events by their egress.* / llm.scan.*
// action prefix (ignoring unrelated events like identity mint).
func countActions(evs []types.AuditEvent) (egressActions, scanActions map[string]int) {
	egressActions, scanActions = map[string]int{}, map[string]int{}
	for _, ev := range evs {
		switch {
		case strings.HasPrefix(ev.Action, "egress."):
			egressActions[ev.Action]++
		case strings.HasPrefix(ev.Action, "llm.scan."):
			scanActions[ev.Action]++
		}
	}
	return
}

// TestPostDecisionEmitsLLMScanAudit verifies the control-plane mapping from a
// decision's optional Scan summary to llm.scan.* audit events.
func TestPostDecisionEmitsLLMScanAudit(t *testing.T) {
	h := newHarness(t)
	tok := h.mintRunToken(t, uuid.New())
	const path = "/api/v1/internal/decisions"

	// (1) A scanned ALERT decision emits BOTH egress.allow AND llm.scan.alert.
	h.audit.events = nil
	alert := `{"request":{"host":"api.anthropic.com","method":"POST"},"decision":"allow","rule_source":"brokered:llm",` +
		`"scan":{"scanned":true,"coverage":"inspectable","mode":"alert","action":"alert","channel":"anthropic.messages",` +
		`"findings":[{"detector":"known-secret","category":"secret","field_path":"messages[0].content","offset":3,"length":20,"severity":"critical","sample":"<secret-hidden>"}]}}`
	if w := do(t, h.srv, http.MethodPost, path, tok, alert); w.Code != http.StatusAccepted {
		t.Fatalf("alert status=%d body=%q", w.Code, w.Body.String())
	}
	eg, sc := countActions(h.audit.events)
	if eg["egress.allow"] != 1 || sc["llm.scan.alert"] != 1 {
		t.Fatalf("alert: want egress.allow=1 + llm.scan.alert=1, got egress=%v scan=%v", eg, sc)
	}

	// (2) A BLIND decision emits ONLY llm.scan.blind — never a duplicate egress event.
	h.audit.events = nil
	blind := `{"request":{"host":"api.anthropic.com","method":"CONNECT"},"decision":"allow","rule_source":"scan:opaque-tunnel",` +
		`"scan":{"scanned":false,"coverage":"tunneled-opaque","mode":"alert","action":"blind"}}`
	if w := do(t, h.srv, http.MethodPost, path, tok, blind); w.Code != http.StatusAccepted {
		t.Fatalf("blind status=%d", w.Code)
	}
	eg, sc = countActions(h.audit.events)
	if sc["llm.scan.blind"] != 1 {
		t.Fatalf("blind: want llm.scan.blind=1, got %v", sc)
	}
	if len(eg) != 0 {
		t.Fatalf("blind must NOT emit any egress event, got %v", eg)
	}

	// (3) A BLOCK decision's llm.scan.block has outcome=denied.
	h.audit.events = nil
	block := `{"request":{"host":"api.anthropic.com","method":"POST"},"decision":"deny","rule_source":"scan:blocked",` +
		`"scan":{"scanned":true,"coverage":"inspectable","mode":"block","action":"block"}}`
	if w := do(t, h.srv, http.MethodPost, path, tok, block); w.Code != http.StatusAccepted {
		t.Fatalf("block status=%d", w.Code)
	}
	var outcome string
	for _, ev := range h.audit.events {
		if ev.Action == "llm.scan.block" {
			outcome = ev.Outcome
		}
	}
	if outcome != "denied" {
		t.Fatalf("llm.scan.block outcome = %q, want denied", outcome)
	}
}
