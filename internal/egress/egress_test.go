// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package egress

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestDecisionConstants pins the wire values of the Decision enum. These strings
// cross the proxy<->control-plane boundary (they land in DecisionLog.Decision and
// in audit), so a typo'd or renamed constant is a silent compatibility break.
func TestDecisionConstants(t *testing.T) {
	tests := []struct {
		name string
		got  Decision
		want string
	}{
		{"allow", Allow, "allow"},
		{"deny", Deny, "deny"},
		{"pending", Pending, "pending"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if string(tt.got) != tt.want {
				t.Fatalf("Decision %s = %q, want %q", tt.name, tt.got, tt.want)
			}
		})
	}
}

// TestDecisionDistinct guards against two enum members collapsing to the same
// underlying string (e.g. a copy/paste error), which would make allow and deny
// indistinguishable on the wire.
func TestDecisionDistinct(t *testing.T) {
	all := []Decision{Allow, Deny, Pending}
	seen := map[Decision]bool{}
	for _, d := range all {
		if seen[d] {
			t.Fatalf("duplicate Decision value %q", d)
		}
		seen[d] = true
	}
}

// TestDecisionLogJSONRoundTrip verifies a fully-populated DecisionLog survives a
// marshal/unmarshal cycle unchanged, including the nested Request and the
// pointer ApprovalID. This protects the structured log contract the proxy
// streams to the control plane.
func TestDecisionLogJSONRoundTrip(t *testing.T) {
	runID := uuid.New()
	approvalID := uuid.New()
	ts := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)

	in := DecisionLog{
		Request: Request{
			RunID:  runID,
			Host:   "api.github.com",
			Port:   443,
			Method: "CONNECT",
			Path:   "",
			Time:   ts,
		},
		Decision:   Pending,
		RuleSource: "approval:" + approvalID.String(),
		ApprovalID: &approvalID,
	}

	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var out DecisionLog
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if out.Request.RunID != runID {
		t.Errorf("RunID = %v, want %v", out.Request.RunID, runID)
	}
	if out.Request.Host != "api.github.com" {
		t.Errorf("Host = %q, want %q", out.Request.Host, "api.github.com")
	}
	if out.Request.Port != 443 {
		t.Errorf("Port = %d, want 443", out.Request.Port)
	}
	if out.Request.Method != "CONNECT" {
		t.Errorf("Method = %q, want %q", out.Request.Method, "CONNECT")
	}
	if !out.Request.Time.Equal(ts) {
		t.Errorf("Time = %v, want %v", out.Request.Time, ts)
	}
	if out.Decision != Pending {
		t.Errorf("Decision = %q, want %q", out.Decision, Pending)
	}
	if out.ApprovalID == nil || *out.ApprovalID != approvalID {
		t.Errorf("ApprovalID = %v, want %v", out.ApprovalID, approvalID)
	}
}

// TestDecisionLogJSONFieldNames pins the JSON key names. Downstream consumers
// (audit pipelines, the UI decision feed) key off these snake_case names, so a
// renamed Go tag is a contract break even though the Go field is unchanged.
func TestDecisionLogJSONFieldNames(t *testing.T) {
	b, err := json.Marshal(DecisionLog{
		Request:    Request{Host: "example.com"},
		Decision:   Allow,
		RuleSource: "policy",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	for _, key := range []string{`"request"`, `"decision"`, `"rule_source"`} {
		if !strings.Contains(s, key) {
			t.Errorf("DecisionLog JSON missing key %s; got %s", key, s)
		}
	}
}

// TestDecisionLogApprovalIDOmitempty verifies the documented behavior that an
// absent approval is omitted from the wire form (omitempty on a nil pointer),
// while a present approval is included. allow/deny decisions carry no approval;
// only the first-use pending flow does.
func TestDecisionLogApprovalIDOmitempty(t *testing.T) {
	t.Run("nil approval omitted", func(t *testing.T) {
		b, err := json.Marshal(DecisionLog{
			Request:    Request{Host: "example.com"},
			Decision:   Deny,
			RuleSource: "policy:denied",
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if strings.Contains(string(b), "approval_id") {
			t.Errorf("nil ApprovalID should be omitted; got %s", b)
		}
	})

	t.Run("present approval included", func(t *testing.T) {
		id := uuid.New()
		b, err := json.Marshal(DecisionLog{
			Request:    Request{Host: "example.com"},
			Decision:   Pending,
			RuleSource: "approval:x",
			ApprovalID: &id,
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !strings.Contains(string(b), "approval_id") {
			t.Errorf("present ApprovalID should be serialized; got %s", b)
		}
	})
}

// TestInjectionRuleJSONRoundTrip verifies the InjectionRule contract survives a
// JSON round-trip. This rule travels from the broker to the proxy and decides
// how a credential is woven into an outbound request, so each field must be
// preserved exactly.
func TestInjectionRuleJSONRoundTrip(t *testing.T) {
	in := InjectionRule{
		Host:       "api.openai.com",
		Header:     "Authorization",
		SecretName: "openai_api_key",
		Format:     "Bearer %s",
	}

	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Pin the snake_case wire keys the proxy/broker rely on.
	s := string(b)
	for _, key := range []string{`"host"`, `"header"`, `"secret_name"`, `"format"`} {
		if !strings.Contains(s, key) {
			t.Errorf("InjectionRule JSON missing key %s; got %s", key, s)
		}
	}

	var out InjectionRule
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != in {
		t.Errorf("round-trip mismatch: got %+v, want %+v", out, in)
	}
}

// TestInjectionRuleFormatNeverWidensEgress documents (and lightly verifies) the
// package invariant that an InjectionRule carries a Format used to wrap the
// secret but contains NO allow/deny field of its own: injection cannot widen
// egress. The rule's job is purely credential placement; the host must still
// pass the allowlist. We assert the Format applies as a normal printf verb so
// the proxy-side wrapping behaves as the comment promises.
func TestInjectionRuleFormatNeverWidensEgress(t *testing.T) {
	r := InjectionRule{Host: "h", Header: "Authorization", SecretName: "s", Format: "Bearer %s"}

	// The Format is a plain printf template; verify it wraps a value as documented.
	got := strings.Replace(r.Format, "%s", "TOKEN", 1)
	if got != "Bearer TOKEN" {
		t.Errorf("Format wrap = %q, want %q", got, "Bearer TOKEN")
	}

	// Confirm the type has no egress-widening surface: marshaling exposes only
	// the four placement fields, never an allow/deny knob.
	b, _ := json.Marshal(r)
	for _, forbidden := range []string{"allow", "deny", "decision"} {
		if strings.Contains(string(b), forbidden) {
			t.Errorf("InjectionRule unexpectedly exposes %q field: %s", forbidden, b)
		}
	}
}
