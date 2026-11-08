// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/json"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// TestRemoteSinkActivity_OnlyAllowedDecisions: the Activity flag a record carries to the runner is what moves
// the run's idle clock, so a refused request, a hold, a token-count row and the approvals poll must not set it.
func TestRemoteSinkActivity_OnlyAllowedDecisions(t *testing.T) {
	cases := []struct {
		name string
		d    egress.DecisionLog
		want bool
	}{
		{"policy allow", egress.DecisionLog{Decision: egress.Allow, RuleSource: "policy:allowlist"}, true},
		{"brokered mint allow", egress.DecisionLog{Decision: egress.Allow, RuleSource: "brokered:mint"}, true},
		{"policy deny", egress.DecisionLog{Decision: egress.Deny, RuleSource: "policy:denied"}, false},
		{"builtin deny", egress.DecisionLog{Decision: egress.Deny, RuleSource: "builtin:private-ip"}, false},
		{"approvals poll allow", egress.DecisionLog{Decision: egress.Allow, RuleSource: ruleSourceApprovals}, false},
		{"reauth timeout deny", egress.DecisionLog{Decision: egress.Deny, RuleSource: ruleSourceCredentialReauthTimeout}, false},
		{"model usage row", egress.DecisionLog{Decision: egress.Allow, RuleSource: ruleSourceLLMUsage, Usage: &egress.ModelUsage{}}, false},
	}
	rs, f := startedSink(t)
	c := accept(t, rs, f.next(), 1, 0, int64(len(cases)), 1<<20)
	for _, tc := range cases {
		id, err := rs.reserve(t.Context(), 1024)
		if err != nil {
			t.Fatalf("%s: reserve: %v", tc.name, err)
		}
		tc.d.Request.AuditReservation = id
		tc.d.Request.Host = "h.test"
		rs.emit(tc.d)
		fr := readUntil(t, c, FrameRecord)
		// readUntil acks records before the wanted type, never the wanted one itself.
		writeFrame(t, c, SinkFrame{T: FrameAck, Gen: fr.Gen, Seq: fr.Seq, Disposition: "appended"})
		var body RecordBody
		if err := json.Unmarshal(fr.Body, &body); err != nil {
			t.Fatalf("%s: decode record: %v", tc.name, err)
		}
		if !body.Admitted {
			t.Fatalf("%s: record not admitted; the test needs an admitted record", tc.name)
		}
		if body.Activity != tc.want {
			t.Errorf("%s: Activity = %v, want %v", tc.name, body.Activity, tc.want)
		}
	}
}
