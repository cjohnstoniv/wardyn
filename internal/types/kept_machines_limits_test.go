// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"testing"
)

// TestRestrictionLimitsCarryDenyKeptMachines: the knob rides a restriction into
// its limits overlay, on the wire name the console mirrors.
func TestRestrictionLimitsCarryDenyKeptMachines(t *testing.T) {
	on := true
	var l RestrictionLimits
	if err := json.Unmarshal([]byte(`{"deny_kept_machines":true}`), &l); err != nil {
		t.Fatal(err)
	}
	if l.DenyKeptMachines == nil || !*l.DenyKeptMachines {
		t.Fatalf("restriction limits dropped deny_kept_machines: %+v", l)
	}
	if o := l.Overlay(); o.DenyKeptMachines == nil || !*o.DenyKeptMachines {
		t.Errorf("Overlay() dropped DenyKeptMachines: %+v", o)
	}
	if o := (RestrictionLimits{}).Overlay(); o.DenyKeptMachines != nil {
		t.Errorf("an unset restriction produced an opinion: %+v", o)
	}
	b, _ := json.Marshal(LimitsOverlay{DenyKeptMachines: &on})
	if string(b) != `{"deny_kept_machines":true}` {
		t.Errorf("overlay wire = %s", b)
	}
}
