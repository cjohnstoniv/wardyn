// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

func TestEgressRefusalOf(t *testing.T) {
	ev := types.AuditEvent{Action: "egress.deny", Data: []byte(`{"host":"h","rule_source":"policy:denied","by":"wardyn","refusal_reason":"default_deny","layer":"profile","layer_name":"Team A"}`)}
	got := client.EgressRefusalOf(ev)
	if got == nil || got.By != client.EgressRefusedByWardyn || got.Reason != "default_deny" || got.Layer != "profile" || got.LayerName != "Team A" {
		t.Fatalf("refusal = %+v", got)
	}
	for name, e := range map[string]types.AuditEvent{
		"unclassified deny": {Action: "egress.deny", Data: []byte(`{"host":"h","rule_source":"brokered:ado:denied"}`)},
		"an allow":          {Action: "egress.allow", Data: []byte(`{"by":"wardyn","refusal_reason":"x"}`)},
		"no data":           {Action: "egress.deny"},
	} {
		if r := client.EgressRefusalOf(e); r != nil {
			t.Errorf("%s: refusal = %+v, want nil", name, r)
		}
	}
}
