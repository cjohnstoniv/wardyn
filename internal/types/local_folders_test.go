// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"reflect"
	"testing"
)

// TestLocalFolderRulesMeet: each wall meets to the stricter side whichever side holds it, an omitted field takes
// the other side's value, patterns union in order with no repeat, and nil on both sides is nil.
func TestLocalFolderRulesMeet(t *testing.T) {
	for _, c := range []struct {
		name    string
		a, b    *LocalFolderRules
		want    *LocalFolderRules
		wantNil bool
	}{
		{name: "nil both sides", wantNil: true},
		{name: "nil base takes the other", b: &LocalFolderRules{DenyPull: true}, want: &LocalFolderRules{DenyPull: true}},
		{name: "nil overlay keeps the base", a: &LocalFolderRules{MaxPushMiB: 7}, want: &LocalFolderRules{MaxPushMiB: 7}},
		{name: "deny_remote left", a: &LocalFolderRules{DenyRemote: true}, b: &LocalFolderRules{}, want: &LocalFolderRules{DenyRemote: true}},
		{name: "deny_remote right", a: &LocalFolderRules{}, b: &LocalFolderRules{DenyRemote: true}, want: &LocalFolderRules{DenyRemote: true}},
		{name: "deny_pull left", a: &LocalFolderRules{DenyPull: true}, b: &LocalFolderRules{}, want: &LocalFolderRules{DenyPull: true}},
		{name: "deny_pull right", a: &LocalFolderRules{}, b: &LocalFolderRules{DenyPull: true}, want: &LocalFolderRules{DenyPull: true}},
		{name: "max_push_mib smaller left", a: &LocalFolderRules{MaxPushMiB: 5}, b: &LocalFolderRules{MaxPushMiB: 9}, want: &LocalFolderRules{MaxPushMiB: 5}},
		{name: "max_push_mib smaller right", a: &LocalFolderRules{MaxPushMiB: 9}, b: &LocalFolderRules{MaxPushMiB: 5}, want: &LocalFolderRules{MaxPushMiB: 5}},
		{name: "max_push_mib omitted left", a: &LocalFolderRules{}, b: &LocalFolderRules{MaxPushMiB: 5}, want: &LocalFolderRules{MaxPushMiB: 5}},
		{name: "max_push_mib omitted right", a: &LocalFolderRules{MaxPushMiB: 5}, b: &LocalFolderRules{}, want: &LocalFolderRules{MaxPushMiB: 5}},
		{name: "max_pull_mib smaller left", a: &LocalFolderRules{MaxPullMiB: 3}, b: &LocalFolderRules{MaxPullMiB: 4}, want: &LocalFolderRules{MaxPullMiB: 3}},
		{name: "max_pull_mib smaller right", a: &LocalFolderRules{MaxPullMiB: 4}, b: &LocalFolderRules{MaxPullMiB: 3}, want: &LocalFolderRules{MaxPullMiB: 3}},
		{name: "max_pull_mib omitted left", a: &LocalFolderRules{}, b: &LocalFolderRules{MaxPullMiB: 3}, want: &LocalFolderRules{MaxPullMiB: 3}},
		{name: "deny union", a: &LocalFolderRules{Deny: []string{"a", "b"}}, b: &LocalFolderRules{Deny: []string{"b", "c"}}, want: &LocalFolderRules{Deny: []string{"a", "b", "c"}}},
		{name: "deny from the other side", a: &LocalFolderRules{}, b: &LocalFolderRules{Deny: []string{"x,y"}}, want: &LocalFolderRules{Deny: []string{"x,y"}}},
	} {
		got := c.a.Meet(c.b)
		if c.wantNil {
			if got != nil {
				t.Errorf("%s: %+v, want nil", c.name, got)
			}
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
	a := &LocalFolderRules{Deny: []string{"a"}}
	got := a.Meet(&LocalFolderRules{Deny: []string{"b"}})
	got.Deny[0] = "changed"
	if a.Deny[0] != "a" {
		t.Error("Meet shares the base's pattern list")
	}
}

// TestLocalFolderRulesAtLeastAsStrict: the whole-spec order reads an omitted field as no wall, so a spec that only
// sets deny_pull is not at least as strict as one that caps pushes.
func TestLocalFolderRulesAtLeastAsStrict(t *testing.T) {
	for _, c := range []struct {
		name string
		r, o *LocalFolderRules
		want bool
	}{
		{"nil both", nil, nil, true},
		{"anything over nil", &LocalFolderRules{DenyPull: true}, nil, true},
		{"nil over no walls", nil, &LocalFolderRules{}, true},
		{"nil over a wall", nil, &LocalFolderRules{MaxPullMiB: 1}, false},
		{"deny_pull alone is not a push cap", &LocalFolderRules{DenyPull: true}, &LocalFolderRules{MaxPushMiB: 10}, false},
		{"deny_remote kept", &LocalFolderRules{DenyRemote: true}, &LocalFolderRules{DenyRemote: true}, true},
		{"deny_remote dropped", &LocalFolderRules{}, &LocalFolderRules{DenyRemote: true}, false},
		{"deny_pull dropped", &LocalFolderRules{}, &LocalFolderRules{DenyPull: true}, false},
		{"smaller cap", &LocalFolderRules{MaxPushMiB: 5}, &LocalFolderRules{MaxPushMiB: 10}, true},
		{"larger cap", &LocalFolderRules{MaxPushMiB: 20}, &LocalFolderRules{MaxPushMiB: 10}, false},
		{"no cap over a cap", &LocalFolderRules{}, &LocalFolderRules{MaxPullMiB: 10}, false},
		{"a cap over no cap", &LocalFolderRules{MaxPullMiB: 10}, &LocalFolderRules{}, true},
		{"every pattern kept", &LocalFolderRules{Deny: []string{"a", "b"}}, &LocalFolderRules{Deny: []string{"b"}}, true},
		{"a pattern dropped", &LocalFolderRules{Deny: []string{"a"}}, &LocalFolderRules{Deny: []string{"b"}}, false},
	} {
		if got := c.r.AtLeastAsStrict(c.o); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	base := &LocalFolderRules{MaxPushMiB: 10, Deny: []string{"x"}}
	if (&LocalFolderRules{DenyPull: true}).WidensAsOverlay(base) {
		t.Error("an overlay that only adds deny_pull widens")
	}
	if !(&LocalFolderRules{MaxPushMiB: 20}).WidensAsOverlay(base) || (&LocalFolderRules{MaxPushMiB: 5}).WidensAsOverlay(base) {
		t.Error("only a cap above the base's is a widening")
	}
}
