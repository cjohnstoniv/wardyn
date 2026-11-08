// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const (
	denyIDA = "11111111-1111-4111-8111-111111111111"
	denyIDB = "22222222-2222-4222-8222-222222222222"
)

// TestDenyListLimitsWire: the two deny lists ride a restriction into its limits overlay, on the wire
// names the console mirrors, and an unset list stays off the wire.
func TestDenyListLimitsWire(t *testing.T) {
	var l RestrictionLimits
	if err := json.Unmarshal([]byte(`{"deny_components":["`+denyIDA+`"],"deny_model_providers":["`+denyIDB+`"]}`), &l); err != nil {
		t.Fatal(err)
	}
	if l.DenyComponents == nil || len(*l.DenyComponents) != 1 || l.DenyModelProviders == nil || len(*l.DenyModelProviders) != 1 {
		t.Fatalf("restriction limits dropped a deny list: %+v", l)
	}
	o := l.Overlay()
	if o.DenyComponents == nil || (*o.DenyComponents)[0] != denyIDA || o.DenyModelProviders == nil || (*o.DenyModelProviders)[0] != denyIDB {
		t.Errorf("Overlay() dropped a deny list: %+v", o)
	}
	if o := (RestrictionLimits{}).Overlay(); o.DenyComponents != nil || o.DenyModelProviders != nil {
		t.Errorf("an unset restriction produced an opinion: %+v", o)
	}
	b, _ := json.Marshal(GovernanceLimits{DenyComponents: []string{denyIDA}, DenyModelProviders: []string{denyIDB}})
	if string(b) != `{"deny_components":["`+denyIDA+`"],"deny_model_providers":["`+denyIDB+`"]}` {
		t.Errorf("limits wire = %s", b)
	}
	b, _ = json.Marshal(GovernanceLimits{})
	if strings.Contains(string(b), "deny_components") || strings.Contains(string(b), "deny_model_providers") {
		t.Errorf("an empty list reached the wire: %s", b)
	}
}

// TestValidDenyIDs: an entry is a canonical lower-case id, listed once, and the list is bounded.
func TestValidDenyIDs(t *testing.T) {
	long := make([]string, MaxDenyListEntries+1)
	for i := range long {
		long[i] = fmt.Sprintf("%08x-1111-4111-8111-111111111111", i+1)
	}
	for _, tc := range []struct {
		name string
		list []string
		ok   bool
	}{
		{"empty", nil, true},
		{"two ids", []string{denyIDA, denyIDB}, true},
		{"not an id", []string{"corp-gateway"}, false},
		{"upper case", []string{"ABCDEFAB-1111-4111-8111-111111111111"}, false},
		{"listed twice", []string{denyIDA, denyIDA}, false},
		{"empty entry", []string{""}, false},
		{"too many", long, false},
	} {
		if err := ValidDenyIDs(tc.list); (err == nil) != tc.ok {
			t.Errorf("%s: ValidDenyIDs = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}
