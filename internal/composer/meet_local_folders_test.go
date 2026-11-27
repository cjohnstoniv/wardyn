// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"reflect"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func localFoldersAuthority(base *types.LocalFolderRules) Authority {
	return Authority{Ceiling: types.RunPolicySpec{MinConfinementClass: types.CC2, LocalFolders: base}}
}

// TestMeetLocalFolders_NarrowOnly: every wall narrows whichever side sets it; an overlay's omitted field inherits
// the base's value, and only a cap above the base's is a widening (a refusal in strict mode, a warning that keeps
// the base's cap otherwise).
func TestMeetLocalFolders_NarrowOnly(t *testing.T) {
	type lf = types.LocalFolderRules
	for _, c := range []struct {
		name       string
		base, over *lf
		want       *lf
		widens     bool
	}{
		{name: "deny_remote from the overlay", base: &lf{}, over: &lf{DenyRemote: true}, want: &lf{DenyRemote: true}},
		{name: "deny_remote kept from the base", base: &lf{DenyRemote: true}, over: &lf{}, want: &lf{DenyRemote: true}},
		{name: "deny_pull from the overlay", base: &lf{}, over: &lf{DenyPull: true}, want: &lf{DenyPull: true}},
		{name: "deny_pull kept from the base", base: &lf{DenyPull: true}, over: &lf{}, want: &lf{DenyPull: true}},
		{name: "max_pull_mib lowered", base: &lf{MaxPullMiB: 10}, over: &lf{MaxPullMiB: 4}, want: &lf{MaxPullMiB: 4}},
		{name: "max_pull_mib raised", base: &lf{MaxPullMiB: 4}, over: &lf{MaxPullMiB: 10}, want: &lf{MaxPullMiB: 4}, widens: true},
		{name: "max_pull_mib set on no cap", base: nil, over: &lf{MaxPullMiB: 10}, want: &lf{MaxPullMiB: 10}},
		{name: "deny patterns added", base: &lf{Deny: []string{"a"}}, over: &lf{Deny: []string{"b"}}, want: &lf{Deny: []string{"a", "b"}}},
		{name: "deny patterns kept", base: &lf{Deny: []string{"a"}}, over: &lf{}, want: &lf{Deny: []string{"a"}}},
		{name: "partial overlay: deny_pull only", base: &lf{MaxPushMiB: 10, Deny: []string{"x"}}, over: &lf{DenyPull: true},
			want: &lf{DenyPull: true, MaxPushMiB: 10, Deny: []string{"x"}}},
		{name: "partial overlay: a larger push cap", base: &lf{MaxPushMiB: 10, Deny: []string{"x"}}, over: &lf{MaxPushMiB: 20},
			want: &lf{MaxPushMiB: 10, Deny: []string{"x"}}, widens: true},
		{name: "partial overlay: a smaller push cap", base: &lf{MaxPushMiB: 10, Deny: []string{"x"}}, over: &lf{MaxPushMiB: 5},
			want: &lf{MaxPushMiB: 5, Deny: []string{"x"}}},
	} {
		ov := Overlay{Ceiling: types.CeilingOverlay{LocalFolders: c.over}}
		out, warns, err := ApplyOverlay(localFoldersAuthority(c.base), ov)
		if err != nil || !reflect.DeepEqual(out.Ceiling.LocalFolders, c.want) {
			t.Errorf("%s: %+v, %v; want %+v", c.name, out.Ceiling.LocalFolders, err, c.want)
		}
		warned := strings.Contains(strings.Join(warns, "\n"), "local_folders")
		strict := ValidateOverlay(localFoldersAuthority(c.base), ov)
		if warned != c.widens || (strict != nil) != c.widens {
			t.Errorf("%s: warnings %v, strict %v; want a widening %v", c.name, warns, strict, c.widens)
		}
	}
	if _, _, err := ApplyOverlay(localFoldersAuthority(nil), Overlay{Ceiling: types.CeilingOverlay{LocalFolders: &types.LocalFolderRules{MaxPushMiB: -1}}}); err == nil ||
		!strings.Contains(err.Error(), "local_folders") {
		t.Errorf("a negative cap = %v, want a local_folders refusal", err)
	}
}

// TestClampLocalFolders: a proposal looser than the ceiling's walls is raised to them with a warning; one at least
// as strict is left as it is, and the proposal's own value is never written.
func TestClampLocalFolders(t *testing.T) {
	ceiling := types.RunPolicySpec{MinConfinementClass: types.CC2, LocalFolders: &types.LocalFolderRules{DenyPull: true, MaxPushMiB: 10, Deny: []string{"a"}}}
	in := types.RunPolicySpec{MinConfinementClass: types.CC2, LocalFolders: &types.LocalFolderRules{MaxPushMiB: 50, Deny: []string{"b"}}}
	out, warns := Clamp(in, ceiling, types.GovernanceLimits{})
	want := &types.LocalFolderRules{DenyPull: true, MaxPushMiB: 10, Deny: []string{"b", "a"}}
	if !reflect.DeepEqual(out.LocalFolders, want) || !strings.Contains(strings.Join(warns, "\n"), "local_folders raised") {
		t.Errorf("looser proposal = %+v, warnings %v; want %+v and a local_folders warning", out.LocalFolders, warns, want)
	}
	if in.LocalFolders.MaxPushMiB != 50 || in.LocalFolders.DenyPull {
		t.Errorf("Clamp wrote the proposal's own walls: %+v", in.LocalFolders)
	}
	strict := types.RunPolicySpec{MinConfinementClass: types.CC2, LocalFolders: &types.LocalFolderRules{DenyPull: true, DenyRemote: true, MaxPushMiB: 5, Deny: []string{"a"}}}
	out, warns = Clamp(strict, ceiling, types.GovernanceLimits{})
	if !reflect.DeepEqual(out.LocalFolders, strict.LocalFolders) || strings.Contains(strings.Join(warns, "\n"), "local_folders") {
		t.Errorf("stricter proposal = %+v, warnings %v; want it kept as it is", out.LocalFolders, warns)
	}
	out, _ = Clamp(types.RunPolicySpec{MinConfinementClass: types.CC2}, ceiling, types.GovernanceLimits{})
	if !reflect.DeepEqual(out.LocalFolders, ceiling.LocalFolders) {
		t.Errorf("no walls under a walled ceiling = %+v, want the ceiling's", out.LocalFolders)
	}
}
