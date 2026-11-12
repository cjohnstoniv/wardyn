// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/ghscope"
)

func TestPersonalWallsSpecOverlay(t *testing.T) {
	list := []string{"pypi.org"}
	off, on := false, true
	t.Run("an allowlist turns open egress off", func(t *testing.T) {
		o := PersonalWallsSpec{AllowedDomains: &list}.Overlay()
		if o.AllowAllEgress == nil || *o.AllowAllEgress {
			t.Fatalf("allow_all_egress = %v, want false", o.AllowAllEgress)
		}
	})
	t.Run("a deny list leaves open egress alone", func(t *testing.T) {
		if o := (PersonalWallsSpec{DeniedDomains: &list}).Overlay(); o.AllowAllEgress != nil {
			t.Fatalf("allow_all_egress = %v, want unset", *o.AllowAllEgress)
		}
	})
	t.Run("a named allow_all_egress is kept", func(t *testing.T) {
		if o := (PersonalWallsSpec{AllowedDomains: &list, AllowAllEgress: &on}).Overlay(); o.AllowAllEgress == nil || !*o.AllowAllEgress {
			t.Fatal("a named value was overwritten")
		}
		if o := (PersonalWallsSpec{AllowAllEgress: &off}).Overlay(); o.AllowAllEgress == nil || *o.AllowAllEgress {
			t.Fatal("false was lost")
		}
	})
	t.Run("an unnamed desktop direction is allowed, not its default", func(t *testing.T) {
		d := PersonalWallsSpec{Desktop: &DesktopControls{PasteIn: TransferBlocked}}.Overlay().Desktop
		if d == nil || *d != (DesktopControls{PasteIn: TransferBlocked, CopyOut: TransferAllowed, Upload: TransferAllowed, Download: TransferAllowed}) {
			t.Fatalf("desktop = %+v", d)
		}
	})
	t.Run("no desktop stays no desktop", func(t *testing.T) {
		if o := (PersonalWallsSpec{}).Overlay(); o.Desktop != nil {
			t.Fatalf("desktop = %+v", o.Desktop)
		}
	})
	t.Run("the five narrowing knobs reach the overlay under the overlay's own names", func(t *testing.T) {
		ado, gh := []adoscope.Capability{adoscope.CapCodeRead}, []ghscope.Capability{ghscope.CapCodeRead}
		paths, rec, sec := []string{"secrets/**"}, RecordingScreenActions, 600
		o := PersonalWallsSpec{
			AzureDevOpsCapabilities: &ado, GitHubCapabilities: &gh, Recording: &rec, AutoStopAfterSec: &sec,
			PushRules: &PersonalWallsPushRules{DenyPaths: &paths},
		}.Overlay()
		if o.AzureDevOpsCapabilities == nil || !slices.Equal(*o.AzureDevOpsCapabilities, ado) ||
			o.GitHubCapabilities == nil || !slices.Equal(*o.GitHubCapabilities, gh) ||
			o.Recording == nil || *o.Recording != rec || o.AutoStopAfterSec == nil || *o.AutoStopAfterSec != sec ||
			o.PushRules == nil || o.PushRules.DenyPaths == nil || !slices.Equal(*o.PushRules.DenyPaths, paths) {
			t.Fatalf("overlay = %+v, want every knob carried", o)
		}
		if o.PushRules.MaxInspectPackMiB != nil || o.PushRules.HoldSeconds != nil || o.PushRules.MaxFileSizeMiB != nil {
			t.Fatalf("push_rules = %+v, want deny_paths only", o.PushRules)
		}
	})
	t.Run("deny_all reaches the overlay, with or without deny paths", func(t *testing.T) {
		yes, paths := true, []string{"a/**"}
		for _, rules := range []PersonalWallsPushRules{{DenyAll: &yes}, {DenyAll: &yes, DenyPaths: &paths}} {
			o := PersonalWallsSpec{PushRules: &rules}.Overlay()
			if o.PushRules == nil || o.PushRules.DenyAll == nil || !*o.PushRules.DenyAll {
				t.Fatalf("overlay push_rules = %+v, want deny_all true", o.PushRules)
			}
			if (rules.DenyPaths != nil) != (o.PushRules.DenyPaths != nil) {
				t.Errorf("overlay deny_paths = %v, want %v", o.PushRules.DenyPaths, rules.DenyPaths)
			}
		}
	})
	t.Run("tool rules and an empty deny list stay out of the overlay", func(t *testing.T) {
		none := []string{}
		o := PersonalWallsSpec{ToolRules: &[]ToolRule{{Tool: "Bash", Effect: ToolDeny}}, PushRules: &PersonalWallsPushRules{DenyPaths: &none}}.Overlay()
		if o.ToolRules != nil || o.PushRules != nil {
			t.Fatalf("overlay = %+v, want neither tool_rules nor push_rules", o)
		}
	})
}

func TestPersonalWallsSpecEmpty(t *testing.T) {
	none := []string{}
	cc, rec, sec, yes := CC2, RecordingTerminal, 60, true
	for name, tc := range map[string]struct {
		s    PersonalWallsSpec
		want bool
	}{
		"nothing":                {PersonalWallsSpec{}, true},
		"a desktop naming none":  {PersonalWallsSpec{Desktop: &DesktopControls{}}, true},
		"an empty allowlist":     {PersonalWallsSpec{AllowedDomains: &none}, false},
		"a floor":                {PersonalWallsSpec{MinConfinementClass: &cc}, false},
		"a blocked direction":    {PersonalWallsSpec{Desktop: &DesktopControls{Upload: TransferBlocked}}, false},
		"a withheld grant":       {PersonalWallsSpec{WithheldGrants: []GrantSpec{{Kind: GrantAPIKey}}}, false},
		"an ado list":            {PersonalWallsSpec{AzureDevOpsCapabilities: &[]adoscope.Capability{adoscope.CapCodeRead}}, false},
		"a github list":          {PersonalWallsSpec{GitHubCapabilities: &[]ghscope.Capability{ghscope.CapCodeRead}}, false},
		"a tool rule":            {PersonalWallsSpec{ToolRules: &[]ToolRule{{Tool: "Bash", Effect: ToolHold}}}, false},
		"no tool rules":          {PersonalWallsSpec{ToolRules: &[]ToolRule{}}, true},
		"a deny path":            {PersonalWallsSpec{PushRules: &PersonalWallsPushRules{DenyPaths: &[]string{"a"}}}, false},
		"no deny paths":          {PersonalWallsSpec{PushRules: &PersonalWallsPushRules{DenyPaths: &[]string{}}}, true},
		"push rules naming none": {PersonalWallsSpec{PushRules: &PersonalWallsPushRules{}}, true},
		"refusing every push":    {PersonalWallsSpec{PushRules: &PersonalWallsPushRules{DenyAll: &yes}}, false},
		"a recording floor":      {PersonalWallsSpec{Recording: &rec}, false},
		"an auto-stop cap":       {PersonalWallsSpec{AutoStopAfterSec: &sec}, false},
	} {
		if got := tc.s.Empty(); got != tc.want {
			t.Errorf("%s: Empty() = %v, want %v", name, got, tc.want)
		}
	}
}
