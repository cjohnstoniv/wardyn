// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"testing"
)

// TestAuditWorkloadDigestsWithheldBelowSuperAdmin: the launch policy digest and pin a workload row carries are
// served below a super admin without them, named in redacted_fields; every other field is served.
func TestAuditWorkloadDigestsWithheldBelowSuperAdmin(t *testing.T) {
	withheld := map[string]bool{"digest": true, "policy_pin": true, "previous": true, "policy_digest": true}
	for action, data := range map[string]map[string]any{
		"workload.apply":          {"name": "web", "digest": "ab"},
		"workload.policy_pin.set": {"name": "web", "policy_pin": "ab", "previous": "cd"},
		"workload.launch_refused": {"name": "web", "reason": reasonWorkloadPolicyPinMismatch, "policy_pin": "ab", "policy_digest": "cd"},
		"workload.delete":         {"name": "web", "policy_pin": "ab"},
	} {
		got, ok := hostViewOf(action, mustJSON(data))
		var m map[string]json.RawMessage
		if !ok || json.Unmarshal(got, &m) != nil {
			t.Fatalf("%s: view %s, %v; want a projected row", action, got, ok)
		}
		for k := range data {
			if _, kept := m[k]; kept == withheld[k] {
				t.Errorf("%s: %s served = %v, want %v", action, k, kept, !withheld[k])
			}
		}
		if _, named := m[auditRedactedFieldsKey]; !named {
			t.Errorf("%s: no %s", action, auditRedactedFieldsKey)
		}
	}
}
