// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import "testing"

// Presence is typed: an absent field is nil (inherit), and a present empty list
// is a value (a pointer to an empty slice).
func TestCeilingOverlayPresenceIsTyped(t *testing.T) {
	o, err := DecodeCeilingOverlay([]byte(`{"allowed_domains":[],"allow_all_egress":false,"resources":{"cpu_millis":0}}`))
	if err != nil {
		t.Fatal(err)
	}
	if o.AllowedDomains == nil || len(*o.AllowedDomains) != 0 {
		t.Errorf("allowed_domains [] must decode to a present empty list, got %v", o.AllowedDomains)
	}
	if o.AllowAllEgress == nil || *o.AllowAllEgress {
		t.Errorf("allow_all_egress false must decode to a present false, got %v", o.AllowAllEgress)
	}
	if o.Resources == nil || o.Resources.CPUMillis == nil || *o.Resources.CPUMillis != 0 || o.Resources.MemoryMiB != nil {
		t.Errorf("a present zero must be distinguishable from an absent field: %+v", o.Resources)
	}
	if o.DeniedDomains != nil || o.AllowedMethods != nil || o.PushRules != nil {
		t.Error("an absent field must stay nil")
	}
	if null, err := DecodeCeilingOverlay([]byte(`{"allowed_methods":null}`)); err != nil || null.AllowedMethods != nil {
		t.Errorf("null is absent, not an empty list: %v %v", null.AllowedMethods, err)
	}
}

func TestOverlaysDecodeStrictly(t *testing.T) {
	for name, raw := range map[string]string{
		"unknown key":        `{"allowed_domain":["a"]}`,
		"unknown nested key": `{"push_rules":{"deny_path":["a"]}}`,
		"trailing data":      `{} {}`,
		"wrong type":         `{"allowed_domains":"a"}`,
		"not an object":      `[]`,
	} {
		if _, err := DecodeCeilingOverlay([]byte(raw)); err == nil {
			t.Errorf("ceiling overlay with %s decoded", name)
		}
	}
	if _, err := DecodeLimitsOverlay([]byte(`{"max_cpu_millis":1,"max_wait":2}`)); err == nil {
		t.Error("limits overlay with an unknown key decoded")
	}
	l, err := DecodeLimitsOverlay([]byte(`{"max_cpu_millis":0,"default_wait_sec":5,"autonomy_rubric":{"egress_open":"L1"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if l.MaxCPUMillis == nil || *l.MaxCPUMillis != 0 || l.DefaultWaitSec == nil || *l.DefaultWaitSec != 5 || l.AutonomyRubric.EgressOpen != AutonomyL1 {
		t.Errorf("limits overlay decoded wrongly: %+v", l)
	}
}
