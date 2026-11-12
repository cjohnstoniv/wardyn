// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func dataOutRule(r types.TransferRule) *types.TransferRule { return &r }

func meetDataOutOf(base types.TransferRule, over *types.TransferRule) (Authority, []string, error) {
	return ApplyOverlay(dataOutAuthority(base), Overlay{Ceiling: types.CeilingOverlay{DataOut: over}})
}

func dataOutAuthority(base types.TransferRule) Authority {
	return Authority{Ceiling: types.RunPolicySpec{MinConfinementClass: types.CC2, DataOut: base}}
}

// TestMeetDataOutStricter: walls that say review over a base that says nothing give review, and an overlay
// looser than the base is a widening (a refusal in strict mode, a warning otherwise).
func TestMeetDataOutStricter(t *testing.T) {
	out, _, err := meetDataOutOf("", dataOutRule(types.TransferReview))
	if err != nil || out.Ceiling.DataOut != types.TransferReview {
		t.Fatalf("review over an unset base = %q, %v; want review", out.Ceiling.DataOut, err)
	}
	out, _, err = meetDataOutOf(types.TransferReview, dataOutRule(types.TransferBlocked))
	if err != nil || out.Ceiling.DataOut != types.TransferBlocked {
		t.Fatalf("blocked over review = %q, %v; want blocked", out.Ceiling.DataOut, err)
	}
	strict := ValidateOverlay(dataOutAuthority(types.TransferBlocked), Overlay{Ceiling: types.CeilingOverlay{DataOut: dataOutRule(types.TransferAllowed)}})
	if err := strict; err == nil || !strings.Contains(err.Error(), "data_out") {
		t.Errorf("allowed over a blocked base in strict mode = %v, want a data_out refusal", strict)
	}
	out, warns, err := meetDataOutOf(types.TransferBlocked, dataOutRule(types.TransferAllowed))
	if err != nil || out.Ceiling.DataOut != types.TransferBlocked || len(warns) == 0 {
		t.Errorf("allowed over a blocked base = %q, %v, warnings %v; want blocked kept and a warning", out.Ceiling.DataOut, err, warns)
	}
	if _, _, err = meetDataOutOf("", dataOutRule("open")); err == nil {
		t.Error(`data_out "open" was accepted`)
	}
}

// TestMeetDataOutBlockedImpliesDenies: blocked turns on the drive, member-drive, kept-machine and push denies.
func TestMeetDataOutBlockedImpliesDenies(t *testing.T) {
	out, _, err := meetDataOutOf("", dataOutRule(types.TransferBlocked))
	if err != nil {
		t.Fatal(err)
	}
	l := out.Limits
	if !l.DenyUserDrive || !l.DenyMemberDrives || !l.DenyKeptMachines {
		t.Errorf("limits = %+v, want DenyUserDrive, DenyMemberDrives and DenyKeptMachines", l)
	}
	if out.Ceiling.PushRules == nil || !out.Ceiling.PushRules.DenyAll {
		t.Errorf("push rules = %+v, want DenyAll", out.Ceiling.PushRules)
	}
}

func TestMeetDataOutReviewImpliesNothing(t *testing.T) {
	out, _, err := meetDataOutOf("", dataOutRule(types.TransferReview))
	if err != nil {
		t.Fatal(err)
	}
	l := out.Limits
	if l.DenyUserDrive || l.DenyMemberDrives || l.DenyKeptMachines || out.Ceiling.PushRules != nil {
		t.Errorf("review implied limits %+v and push rules %+v, want none", l, out.Ceiling.PushRules)
	}
}

// TestClampDataOutBlockedDeniesPushes: a member's own blocked sets the push deny and never a limit; a
// ceiling's stricter value raises the member's.
func TestClampDataOutBlockedDeniesPushes(t *testing.T) {
	ceiling := types.RunPolicySpec{MinConfinementClass: types.CC2}
	out, _ := ClampMember(types.RunPolicySpec{MinConfinementClass: types.CC2, DataOut: types.TransferBlocked}, ceiling, types.GovernanceLimits{}, types.ResourceLimits{}, types.ResourceLimits{})
	if out.DataOut != types.TransferBlocked || out.PushRules == nil || !out.PushRules.DenyAll {
		t.Errorf("own blocked: data_out %q, push rules %+v; want blocked and DenyAll", out.DataOut, out.PushRules)
	}
	ceiling.DataOut = types.TransferReview
	out, warns := Clamp(types.RunPolicySpec{MinConfinementClass: types.CC2}, ceiling, types.GovernanceLimits{})
	if out.DataOut != types.TransferReview || out.PushRules != nil {
		t.Errorf("under a review ceiling: data_out %q, push rules %+v; want review and no push rules", out.DataOut, out.PushRules)
	}
	if !strings.Contains(strings.Join(warns, "\n"), "data_out raised") {
		t.Errorf("warnings %v, want a data_out raised warning", warns)
	}
	in := types.RunPolicySpec{MinConfinementClass: types.CC2, DataOut: types.TransferBlocked}
	Clamp(in, types.RunPolicySpec{MinConfinementClass: types.CC2}, types.GovernanceLimits{})
	if in.PushRules != nil {
		t.Error("Clamp wrote the proposal's own push rules")
	}
}

func TestLeqDataOut(t *testing.T) {
	a, r, b := types.TransferAllowed, types.TransferReview, types.TransferBlocked
	for _, tc := range []struct {
		x, y types.TransferRule
		want bool
	}{{"", "", true}, {"", a, true}, {r, "", true}, {b, r, true}, {r, r, true}, {"", r, false}, {a, b, false}, {r, b, false}} {
		x := types.RunPolicySpec{MinConfinementClass: types.CC2, DataOut: tc.x}
		y := types.RunPolicySpec{MinConfinementClass: types.CC2, DataOut: tc.y}
		if got := Leq(x, y, types.GovernanceLimits{}, types.GovernanceLimits{}); got != tc.want {
			t.Errorf("Leq(data_out %q, %q) = %v, want %v", tc.x, tc.y, got, tc.want)
		}
	}
}
