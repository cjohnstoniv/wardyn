// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func ptr[T any](v T) *T { return &v }

func overlayOf(c types.CeilingOverlay) Overlay { return Overlay{Ceiling: c} }

func limitsOverlay(l types.LimitsOverlay) Overlay { return Overlay{Limits: l} }

func mustApply(t *testing.T, base Authority, ov Overlay) Authority {
	t.Helper()
	got, _, err := ApplyOverlay(base, ov)
	if err != nil {
		t.Fatalf("ApplyOverlay: %v", err)
	}
	return got
}

func wantOverlayErr(t *testing.T, err error, reason, field string) {
	t.Helper()
	var oe *OverlayError
	if !errors.As(err, &oe) {
		t.Fatalf("error = %v, want an *OverlayError", err)
	}
	if oe.Reason != reason || oe.Field != field {
		t.Fatalf("error = %v, want %s on %s", oe, reason, field)
	}
}

func TestMeetAllowedDomains(t *testing.T) {
	base := Authority{Ceiling: types.RunPolicySpec{AllowedDomains: []string{"*.corp.example", "api.vendor.example", "10.0.0.5"}}}
	for _, tc := range []struct {
		name string
		ov   []string
		want []string
		bad  bool // refused at write
	}{
		{"an exact host under the base's wildcard", []string{"git.corp.example"}, []string{}, true},
		{"the base's own entry", []string{"api.vendor.example"}, []string{"api.vendor.example"}, false},
		{"an empty list is a value: no domains", []string{}, []string{}, false},
		{"a host outside the base", []string{"evil.example"}, []string{}, true},
		{"a wildcard wider than the base's", []string{"*.example"}, []string{"*.corp.example"}, true},
		{"a literal IP the base names exactly", []string{"10.0.0.5"}, []string{"10.0.0.5"}, false},
		{"a literal IP the base does not name", []string{"10.0.0.6"}, []string{}, true},
		{"a port narrows an any-port entry", []string{"api.vendor.example:443"}, []string{"api.vendor.example:443"}, false},
		{"a bare entry is not covered by a port-qualified base",
			[]string{"p.example"}, []string{"p.example:443"}, true},
		{"an entry nothing can match", []string{"not a host/"}, []string{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := base
			if strings.HasPrefix(tc.name, "a bare entry") {
				b = Authority{Ceiling: types.RunPolicySpec{AllowedDomains: []string{"p.example:443"}}}
			}
			ov := overlayOf(types.CeilingOverlay{AllowedDomains: &tc.ov})
			err := ValidateOverlay(b, ov)
			if tc.bad {
				wantOverlayErr(t, err, ReasonOverlayInvalid, "allowed_domains")
			} else if err != nil {
				t.Fatalf("ValidateOverlay: %v", err)
			}
			got, warns, aerr := ApplyOverlay(b, ov)
			if aerr != nil {
				t.Fatal(aerr)
			}
			if tc.bad && len(warns) == 0 {
				t.Error("the lenient meet dropped something without a warning")
			}
			if !slices.Equal(got.Ceiling.AllowedDomains, tc.want) {
				t.Errorf("allowed_domains = %v, want %v", got.Ceiling.AllowedDomains, tc.want)
			}
		})
	}
}

// A base's allow-all never covers an entry, so an overlay naming a private
// literal IP or an internal host under one is refused at write and gets nothing
// extra at resolve.
func TestMeetAllowAllNeverCoversAnEntry(t *testing.T) {
	base := Authority{Ceiling: types.RunPolicySpec{AllowAllEgress: true}}
	ov := overlayOf(types.CeilingOverlay{AllowedDomains: &[]string{"10.0.0.5", "db.corp.internal"}})
	wantOverlayErr(t, ValidateOverlay(base, ov), ReasonOverlayInvalid, "allowed_domains")
	got := mustApply(t, base, ov)
	if len(got.Ceiling.AllowedDomains) != 0 {
		t.Errorf("allowed_domains = %v: an allow-all base must not lend an entry to the overlay", got.Ceiling.AllowedDomains)
	}
	if !got.Ceiling.AllowAllEgress {
		t.Error("the overlay did not name allow_all_egress, so the base's value is inherited")
	}
}

func TestMeetAllowAllIsAnd(t *testing.T) {
	on, off := Authority{Ceiling: types.RunPolicySpec{AllowAllEgress: true}}, Authority{}
	if got := mustApply(t, on, overlayOf(types.CeilingOverlay{AllowAllEgress: ptr(false)})); got.Ceiling.AllowAllEgress {
		t.Error("an overlay may turn allow-all off")
	}
	wantOverlayErr(t, ValidateOverlay(off, overlayOf(types.CeilingOverlay{AllowAllEgress: ptr(true)})), ReasonOverlayInvalid, "allow_all_egress")
	if got := mustApply(t, off, overlayOf(types.CeilingOverlay{AllowAllEgress: ptr(true)})); got.Ceiling.AllowAllEgress {
		t.Error("an overlay must never turn allow-all on")
	}
}

func TestMeetMethods(t *testing.T) {
	base := Authority{Ceiling: types.RunPolicySpec{AllowedMethods: []string{"GET", "HEAD"}}}
	_, _, err := ApplyOverlay(base, overlayOf(types.CeilingOverlay{AllowedMethods: &[]string{"post"}}))
	wantOverlayErr(t, err, ReasonOverlayUnsatisfiable, "allowed_methods")

	got := mustApply(t, base, overlayOf(types.CeilingOverlay{AllowedMethods: &[]string{"get", "POST"}}))
	if !slices.Equal(got.Ceiling.AllowedMethods, []string{"GET"}) {
		t.Errorf("methods = %v, want [GET]", got.Ceiling.AllowedMethods)
	}
	wantOverlayErr(t, ValidateOverlay(base, overlayOf(types.CeilingOverlay{AllowedMethods: &[]string{"get", "POST"}})), ReasonOverlayInvalid, "allowed_methods")

	// An empty overlay list would read as "every method" in the proxy.
	for _, empty := range [][]string{{}, {" ", ""}} {
		_, _, err = ApplyOverlay(base, overlayOf(types.CeilingOverlay{AllowedMethods: &empty}))
		wantOverlayErr(t, err, ReasonOverlayInvalid, "allowed_methods")
	}
	// An overlay narrows a base that allows every method.
	got = mustApply(t, Authority{}, overlayOf(types.CeilingOverlay{AllowedMethods: &[]string{"get"}}))
	if !slices.Equal(got.Ceiling.AllowedMethods, []string{"GET"}) {
		t.Errorf("methods = %v, want [GET]", got.Ceiling.AllowedMethods)
	}
}

func TestMeetCapabilities(t *testing.T) {
	base := Authority{Ceiling: types.RunPolicySpec{AzureDevOpsCapabilities: []adoscope.Capability{adoscope.CapCodeRead, adoscope.CapWorkRead}}}
	_, _, err := ApplyOverlay(base, overlayOf(types.CeilingOverlay{AzureDevOpsCapabilities: &[]adoscope.Capability{adoscope.CapBuildRead}}))
	wantOverlayErr(t, err, ReasonOverlayUnsatisfiable, "azure_devops_capabilities")
	_, _, err = ApplyOverlay(base, overlayOf(types.CeilingOverlay{AzureDevOpsCapabilities: &[]adoscope.Capability{}}))
	wantOverlayErr(t, err, ReasonOverlayInvalid, "azure_devops_capabilities")
	got := mustApply(t, base, overlayOf(types.CeilingOverlay{AzureDevOpsCapabilities: &[]adoscope.Capability{adoscope.CapCodeRead}}))
	if !slices.Equal(got.Ceiling.AzureDevOpsCapabilities, []adoscope.Capability{adoscope.CapCodeRead}) {
		t.Errorf("capabilities = %v", got.Ceiling.AzureDevOpsCapabilities)
	}
	// A list under an empty base (the row's default profile) is a widening: refused at write, dropped at resolve.
	ov := overlayOf(types.CeilingOverlay{AzureDevOpsCapabilities: &[]adoscope.Capability{adoscope.CapCodeRead}})
	wantOverlayErr(t, ValidateOverlay(Authority{}, ov), ReasonOverlayInvalid, "azure_devops_capabilities")
	got, warns, err := ApplyOverlay(Authority{}, ov)
	if err != nil || len(warns) == 0 || len(got.Ceiling.AzureDevOpsCapabilities) != 0 {
		t.Errorf("resolve = %v, warns %v, err %v; want an empty list and a warning", got.Ceiling.AzureDevOpsCapabilities, warns, err)
	}
}

func TestMeetLLMInspection(t *testing.T) {
	block := &types.LLMInspectionSpec{Mode: "block", DetectSecrets: true}
	alert := &types.LLMInspectionSpec{Mode: "alert", DetectSecrets: true}
	got := mustApply(t, Authority{}, overlayOf(types.CeilingOverlay{LLMInspection: block}))
	if got.Ceiling.LLMInspection == nil || got.Ceiling.LLMInspection.Mode != "block" {
		t.Fatalf("the overlay's inspection should apply over a base with none: %+v", got.Ceiling.LLMInspection)
	}
	if got.Ceiling.LLMInspection == block {
		t.Error("the result aliases the overlay")
	}
	base := Authority{Ceiling: types.RunPolicySpec{LLMInspection: block}}
	_ = mustApply(t, base, overlayOf(types.CeilingOverlay{LLMInspection: &types.LLMInspectionSpec{Mode: "block", DetectSecrets: true}}))
	_, _, err := ApplyOverlay(base, overlayOf(types.CeilingOverlay{LLMInspection: alert}))
	wantOverlayErr(t, err, ReasonOverlayInvalid, "llm_inspection")
	_, _, err = ApplyOverlay(Authority{}, overlayOf(types.CeilingOverlay{LLMInspection: &types.LLMInspectionSpec{Mode: "alert", WorkspaceSecretValues: []string{"hunter2hunter2"}}}))
	wantOverlayErr(t, err, ReasonOverlayInvalid, "llm_inspection.workspace_secret_values")
}

func TestMeetFirstUseAndHolds(t *testing.T) {
	base := Authority{Ceiling: types.RunPolicySpec{
		FirstUseApproval: types.FirstUseDenyWithReview, FirstUseHoldSeconds: 0, MaxHolds: 0,
	}}
	got := mustApply(t, base, overlayOf(types.CeilingOverlay{
		FirstUseApproval: ptr(types.FirstUseWaitForReview), FirstUseHoldSeconds: ptr(120), MaxHolds: ptr(64),
	}))
	c := got.Ceiling
	if c.FirstUseApproval != types.FirstUseDenyWithReview {
		t.Errorf("first_use_approval = %q: wait_for_review is looser than the base's", c.FirstUseApproval)
	}
	// The base's zeros are the runtime's 30 s and 16, so a larger overlay value is cut to THEM, not taken raw.
	if c.FirstUseHoldSeconds != 30 || c.MaxHolds != 16 {
		t.Errorf("hold = %ds, max_holds = %d, want the base's effective 30 and 16", c.FirstUseHoldSeconds, c.MaxHolds)
	}
	got = mustApply(t, base, overlayOf(types.CeilingOverlay{FirstUseApproval: ptr(types.FirstUseAlwaysDeny), FirstUseHoldSeconds: ptr(10)}))
	if got.Ceiling.FirstUseApproval != types.FirstUseAlwaysDeny || got.Ceiling.FirstUseHoldSeconds != 10 {
		t.Errorf("a stricter overlay must win: %+v", got.Ceiling)
	}
	wantOverlayErr(t, ValidateOverlay(base, overlayOf(types.CeilingOverlay{MaxHolds: ptr(64)})), ReasonOverlayInvalid, "max_holds")
}

func TestMeetConfinementAndAutoStop(t *testing.T) {
	base := Authority{Ceiling: types.RunPolicySpec{MinConfinementClass: types.CC2, AutoStopAfterSec: 3600}}
	got := mustApply(t, base, overlayOf(types.CeilingOverlay{MinConfinementClass: ptr(types.ConfinementClass("cc3")), AutoStopAfterSec: ptr(600)}))
	if got.Ceiling.MinConfinementClass != types.CC3 || got.Ceiling.AutoStopAfterSec != 600 {
		t.Errorf("got %+v, want CC3 and 600", got.Ceiling)
	}
	got = mustApply(t, base, overlayOf(types.CeilingOverlay{MinConfinementClass: ptr(types.CC1), AutoStopAfterSec: ptr(-1)}))
	if got.Ceiling.MinConfinementClass != types.CC2 || got.Ceiling.AutoStopAfterSec != 3600 {
		t.Errorf("a lower floor or a never-reap must not win: %+v", got.Ceiling)
	}
	wantOverlayErr(t, ValidateOverlay(base, overlayOf(types.CeilingOverlay{AutoStopAfterSec: ptr(0)})), ReasonOverlayInvalid, "auto_stop_after_sec")
}

func TestMeetResourcesNormaliseZero(t *testing.T) {
	def := types.ResourceLimits{CPUMillis: 2000, MemoryMiB: 4096, PidsLimit: 512}
	// A base with no resources runs at the deployment size; a larger overlay value must not exceed it.
	got := mustApply(t, Authority{}, overlayOf(types.CeilingOverlay{Resources: &types.ResourcesOverlay{CPUMillis: ptr(64000), MemoryMiB: ptr(1024)}}))
	r := got.Ceiling.Resources
	if r == nil || r.CPUMillis != def.CPUMillis || r.MemoryMiB != 1024 {
		t.Fatalf("resources = %+v, want cpu %d (the deployment size) and 1024 MiB", r, def.CPUMillis)
	}
	// An explicit zero in the overlay is the deployment size too.
	got = mustApply(t, Authority{Ceiling: types.RunPolicySpec{Resources: &types.ResourceLimits{CPUMillis: 64000}}},
		overlayOf(types.CeilingOverlay{Resources: &types.ResourcesOverlay{CPUMillis: ptr(0)}}))
	if got.Ceiling.Resources.CPUMillis != def.CPUMillis {
		t.Errorf("cpu = %d, want the deployment's %d: zero is not unbounded", got.Ceiling.Resources.CPUMillis, def.CPUMillis)
	}
	// Disk has no default: zero is unbounded, so a positive overlay value narrows it.
	got = mustApply(t, Authority{}, overlayOf(types.CeilingOverlay{Resources: &types.ResourcesOverlay{DiskMiB: ptr(512)}}))
	if got.Ceiling.Resources.DiskMiB != 512 {
		t.Errorf("disk = %d, want 512", got.Ceiling.Resources.DiskMiB)
	}
}

func TestMeetPushRules(t *testing.T) {
	t.Run("pack cap 0 means 32 under an active rule", func(t *testing.T) {
		base := Authority{Ceiling: types.RunPolicySpec{PushRules: &types.PushRulesSpec{DenyPaths: []string{".github/**"}}}}
		got := mustApply(t, base, overlayOf(types.CeilingOverlay{PushRules: &types.PushRulesOverlay{MaxInspectPackMiB: ptr(64)}}))
		if p := got.Ceiling.PushRules.MaxInspectPackMiB; p != 32 {
			t.Errorf("pack cap = %d, want 32: the base's 0 is the broker's 32, and meet(0, 64) taken raw would widen it", p)
		}
		wantOverlayErr(t, ValidateOverlay(base, overlayOf(types.CeilingOverlay{PushRules: &types.PushRulesOverlay{MaxInspectPackMiB: ptr(64)}})),
			ReasonOverlayInvalid, "push_rules.max_inspect_pack_mib")
	})
	t.Run("an overlay may add a rule where the base had none", func(t *testing.T) {
		got := mustApply(t, Authority{}, overlayOf(types.CeilingOverlay{PushRules: &types.PushRulesOverlay{MaxInspectPackMiB: ptr(64)}}))
		if p := got.Ceiling.PushRules.MaxInspectPackMiB; p != 64 {
			t.Errorf("pack cap = %d, want 64: a base with no rule bounds nothing", p)
		}
	})
	t.Run("paths union, bounds narrow", func(t *testing.T) {
		base := Authority{Ceiling: types.RunPolicySpec{PushRules: &types.PushRulesSpec{
			DenyPaths: []string{"a/**"}, MaxFileSizeMiB: 10, HoldSeconds: 300}}}
		got := mustApply(t, base, overlayOf(types.CeilingOverlay{PushRules: &types.PushRulesOverlay{
			DenyPaths: &[]string{"b/**"}, RequireReviewPaths: &[]string{"c/"}, MaxFileSizeMiB: ptr(5), HoldSeconds: ptr(0), DenyNewExecutables: ptr(true)}}))
		p := got.Ceiling.PushRules
		if !slices.Equal(p.DenyPaths, []string{"a/**", "b/**"}) || !slices.Equal(p.RequireReviewPaths, []string{"c/"}) ||
			p.MaxFileSizeMiB != 5 || p.HoldSeconds != 120 || !p.DenyNewExecutables {
			t.Errorf("push_rules = %+v", p)
		}
	})
	t.Run("a looser file cap is not taken", func(t *testing.T) {
		base := Authority{Ceiling: types.RunPolicySpec{PushRules: &types.PushRulesSpec{MaxFileSizeMiB: 10}}}
		got := mustApply(t, base, overlayOf(types.CeilingOverlay{PushRules: &types.PushRulesOverlay{MaxFileSizeMiB: ptr(0)}}))
		if got.Ceiling.PushRules.MaxFileSizeMiB != 10 {
			t.Errorf("max_file_size_mib = %d, want the base's 10: off is unbounded", got.Ceiling.PushRules.MaxFileSizeMiB)
		}
	})
}

func TestMeetToolRulesAreStricterPerTool(t *testing.T) {
	base := Authority{Ceiling: types.RunPolicySpec{ToolRules: []types.ToolRule{{Tool: "*", Effect: types.ToolAllow}, {Tool: "Bash", Effect: types.ToolHold}}}}
	got := mustApply(t, base, overlayOf(types.CeilingOverlay{ToolRules: &[]types.ToolRule{{Tool: "*", Effect: types.ToolHold}, {Tool: "Bash", Effect: types.ToolAllow}, {Tool: "WebFetch", Effect: types.ToolDeny}}}))
	want := []types.ToolRule{{Tool: "*", Effect: types.ToolHold}, {Tool: "Bash", Effect: types.ToolHold}, {Tool: "WebFetch", Effect: types.ToolDeny}}
	if !slices.Equal(got.Ceiling.ToolRules, want) {
		t.Errorf("tool_rules = %v, want %v", got.Ceiling.ToolRules, want)
	}
	// A tool only the base names is stricter than the overlay's unset default (hold) only where the base denies.
	base.Ceiling.ToolRules = append(base.Ceiling.ToolRules, types.ToolRule{Tool: "Read", Effect: types.ToolDeny})
	got = mustApply(t, base, overlayOf(types.CeilingOverlay{ToolRules: &[]types.ToolRule{{Tool: "*", Effect: types.ToolAllow}}}))
	if e := effectFor(toolEffects(got.Ceiling.ToolRules), "Read"); e != types.ToolDeny {
		t.Errorf("Read = %s: a deny of the base's must survive an overlay that does not name it", e)
	}
	_, _, err := ApplyOverlay(base, overlayOf(types.CeilingOverlay{ToolRules: &[]types.ToolRule{{Tool: "x", Effect: "maybe"}}}))
	wantOverlayErr(t, err, ReasonOverlayInvalid, "tool_rules")
}

func TestMeetMountsReposUIApps(t *testing.T) {
	ro, rw := true, false
	base := Authority{Ceiling: types.RunPolicySpec{
		WorkspaceMounts: []types.WorkspaceMount{{Source: "/a", Target: "/w/a"}, {Source: "/b", Target: "/w/b", ReadOnly: &rw}},
		WorkspaceRepos:  []types.WorkspaceRepo{{Repo: "org/one"}, {Repo: "org/two", Ref: "main"}},
		UIApps:          []types.UIApp{{Name: "jupyter", Port: 8888}, {Name: "docs", Port: 8000, Path: "/x"}},
	}}
	got := mustApply(t, base, overlayOf(types.CeilingOverlay{
		WorkspaceMounts: &[]types.WorkspaceMount{{Source: "/b", Target: "/w/b", ReadOnly: &ro}, {Source: "/c", Target: "/w/c"}},
		WorkspaceRepos:  &[]types.WorkspaceRepo{{Repo: "org/two", Ref: "main"}, {Repo: "org/three"}},
		UIApps:          &[]types.UIApp{{Name: "docs", Port: 8000, Path: "/x"}, {Name: "docs", Port: 9000}},
	}))
	c := got.Ceiling
	if len(c.WorkspaceMounts) != 1 || c.WorkspaceMounts[0].Source != "/b" || !c.WorkspaceMounts[0].ReadOnlyOrDefault() {
		t.Errorf("mounts = %+v, want only /b, read-only because the overlay says so", c.WorkspaceMounts)
	}
	if !slices.Equal(c.WorkspaceRepos, []types.WorkspaceRepo{{Repo: "org/two", Ref: "main"}}) {
		t.Errorf("repos = %+v", c.WorkspaceRepos)
	}
	if len(c.UIApps) != 1 || c.UIApps[0].Name != "docs" || c.UIApps[0].Port != 8000 {
		t.Errorf("ui_apps = %+v", c.UIApps)
	}
	// A read-write overlay mount over a read-only base mount is a widening.
	wantOverlayErr(t, ValidateOverlay(base, overlayOf(types.CeilingOverlay{WorkspaceMounts: &[]types.WorkspaceMount{{Source: "/a", Target: "/w/a", ReadOnly: &rw}}})),
		ReasonOverlayInvalid, "workspace_mounts")
}

// TestMeetUIAppsMatchLeq pins that the meet, strict validation and Leq agree on
// a ui_app: an overlay app must serve the path the base app serves (an absent
// path and "/" are the same path), or it is a widening.
func TestMeetUIAppsMatchLeq(t *testing.T) {
	for _, tc := range []struct {
		name          string
		base, overlay string
		wantWiden     bool
	}{
		{"equal paths", "/a", "/a", false},
		{"absent and root", "", "/", false},
		{"root and absent", "/", "", false},
		{"differing paths", "/z", "/a", true},
		{"base root, overlay path", "", "/a", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := Authority{Ceiling: types.RunPolicySpec{MinConfinementClass: types.CC2, UIApps: []types.UIApp{{Name: "docs", Port: 8000, Path: tc.base}}}}
			ov := overlayOf(types.CeilingOverlay{UIApps: &[]types.UIApp{{Name: "docs", Port: 8000, Path: tc.overlay}}})
			err := ValidateOverlay(base, ov)
			if tc.wantWiden {
				wantOverlayErr(t, err, ReasonOverlayInvalid, "ui_apps")
			} else if err != nil {
				t.Fatalf("ValidateOverlay: %v", err)
			}
			got, warns, err := ApplyOverlay(base, ov)
			if err != nil {
				t.Fatalf("ApplyOverlay: %v", err)
			}
			if !Leq(got.Ceiling, base.Ceiling, got.Limits, base.Limits) {
				t.Errorf("Leq(ApplyOverlay(base, ov), base) is false: result ui_apps = %+v", got.Ceiling.UIApps)
			}
			if tc.wantWiden != (len(warns) == 1) {
				t.Errorf("warnings = %v, wantWiden = %v", warns, tc.wantWiden)
			}
			if tc.wantWiden && len(got.Ceiling.UIApps) != 0 {
				t.Errorf("a path-mismatched app was kept: %+v", got.Ceiling.UIApps)
			}
		})
	}
}

func TestMeetLimits(t *testing.T) {
	base := Authority{Limits: types.GovernanceLimits{
		MaxConcurrentRuns: 4, MaxCPUMillis: 2000,
		AutonomyRubric: &types.AutonomyRubric{EgressOpen: types.AutonomyL1, SecretsNone: types.AutonomyL3},
		RunLimits:      types.RunLimits{MaxEndAheadSec: 7200, DefaultEndSec: 3600, AllowNoEnd: true, UserChangesLimits: true, MaxWaitSec: 600},
	}}
	got := mustApply(t, base, limitsOverlay(types.LimitsOverlay{
		DenyInteractive: ptr(true), MaxConcurrentRuns: ptr(8), MaxCPUMillis: ptr(1000), MaxMemoryMiB: ptr(512),
		AutonomyRubric: &types.AutonomyRubric{EgressOpen: types.AutonomyL3, SecretsNone: types.AutonomyL0, ConfinementCC1: types.AutonomyL2},
		MaxEndAheadSec: ptr(1800), AllowNoEnd: ptr(false), MaxWaitSec: ptr(0), PauseIdleAfterSec: ptr(300),
	}))
	l := got.Limits
	if !l.DenyInteractive || l.MaxConcurrentRuns != 4 || l.MaxCPUMillis != 1000 || l.MaxMemoryMiB != 512 {
		t.Errorf("limits = %+v", l)
	}
	if r := l.AutonomyRubric; r.EgressOpen != types.AutonomyL1 || r.SecretsNone != types.AutonomyL0 || r.ConfinementCC1 != types.AutonomyL2 {
		t.Errorf("rubric = %+v: the lower level per field, and a field one side sets takes that side", r)
	}
	if l.MaxEndAheadSec != 1800 || l.DefaultEndSec != 1800 {
		t.Errorf("end = max %d default %d: the default is clamped to the composed max", l.MaxEndAheadSec, l.DefaultEndSec)
	}
	if l.AllowNoEnd || !l.UserChangesLimits || l.MaxWaitSec != 600 || l.PauseIdleAfterSec != 300 {
		t.Errorf("run limits = %+v", l.RunLimits)
	}
	wantOverlayErr(t, ValidateOverlay(base, limitsOverlay(types.LimitsOverlay{MaxConcurrentRuns: ptr(8)})), ReasonOverlayInvalid, "max_concurrent_runs")
	_, _, err := ApplyOverlay(base, limitsOverlay(types.LimitsOverlay{MaxCPUMillis: ptr(-1)}))
	wantOverlayErr(t, err, ReasonOverlayInvalid, "max_cpu_millis")
	_, _, err = ApplyOverlay(base, limitsOverlay(types.LimitsOverlay{AutonomyRubric: &types.AutonomyRubric{EgressOpen: "L9"}}))
	wantOverlayErr(t, err, ReasonOverlayInvalid, "autonomy_rubric")
}

func TestMeetGuardrailLocksOnlyTighten(t *testing.T) {
	locked := Authority{Limits: types.GovernanceLimits{AutonomyRubric: &types.AutonomyRubric{AgentGuardrailLocks: true}}}
	got := mustApply(t, locked, limitsOverlay(types.LimitsOverlay{AutonomyRubric: &types.AutonomyRubric{EgressOpen: types.AutonomyL1}}))
	if r := got.Limits.AutonomyRubric; r == nil || !r.AgentGuardrailLocks || r.EgressOpen != types.AutonomyL1 {
		t.Errorf("rubric = %+v: an overlay that does not mention the lock must keep the base's", r)
	}
	got = mustApply(t, Authority{}, limitsOverlay(types.LimitsOverlay{AutonomyRubric: &types.AutonomyRubric{AgentGuardrailLocks: true}}))
	if r := got.Limits.AutonomyRubric; r == nil || !r.AgentGuardrailLocks {
		t.Errorf("rubric = %+v: an overlay may turn the lock on", r)
	}
}

func TestMeetDefaultsClampWhenTheMaxTightens(t *testing.T) {
	base := Authority{Limits: types.GovernanceLimits{RunLimits: types.RunLimits{MaxWaitSec: 900, DefaultWaitSec: 900}}}
	got := mustApply(t, base, limitsOverlay(types.LimitsOverlay{MaxWaitSec: ptr(300)}))
	if got.Limits.DefaultWaitSec != 300 {
		t.Errorf("default_wait_sec = %d, want 300: a default past its max is a result the writer refuses", got.Limits.DefaultWaitSec)
	}
}

func TestMeetLeavesTheBaseAlone(t *testing.T) {
	base := Authority{Ceiling: types.RunPolicySpec{
		AllowedDomains: []string{"a.example"}, DeniedDomains: []string{"x.example"},
		WorkspaceMounts: []types.WorkspaceMount{{Source: "/a", Target: "/w"}}, PushRules: &types.PushRulesSpec{DenyPaths: []string{"a"}},
	}, Limits: types.GovernanceLimits{AutonomyRubric: &types.AutonomyRubric{EgressOpen: types.AutonomyL1}}}
	before := cloneAuthority(base)
	got := mustApply(t, base, Overlay{
		Ceiling: types.CeilingOverlay{
			AllowedDomains: &[]string{}, DeniedDomains: &[]string{"y.example"},
			PushRules: &types.PushRulesOverlay{DenyPaths: &[]string{"b"}},
		},
		Limits: types.LimitsOverlay{AutonomyRubric: &types.AutonomyRubric{EgressOpen: types.AutonomyL0}},
	})
	got.Ceiling.DeniedDomains[0] = "mutated"
	got.Ceiling.PushRules.DenyPaths[0] = "mutated"
	got.Limits.AutonomyRubric.EgressOpen = types.AutonomyL3
	got.Ceiling.WorkspaceMounts[0].Source = "mutated"
	if !sameJSON(base, before) {
		t.Errorf("ApplyOverlay changed its base argument: %+v", base)
	}
}

func TestMeetEmptyOverlayIsTheBase(t *testing.T) {
	base := Authority{Ceiling: types.RunPolicySpec{
		AllowedDomains: []string{"b.example", "a.example"}, FirstUseHoldSeconds: 0, MaxHolds: 0,
		Resources: &types.ResourceLimits{CPUMillis: 64000}, PushRules: &types.PushRulesSpec{DenyPaths: []string{"z", "a"}},
	}, Limits: types.GovernanceLimits{MaxCPUMillis: 0}}
	got, warns, err := ApplyOverlay(base, Overlay{})
	if err != nil || len(warns) != 0 {
		t.Fatalf("err %v warns %v", err, warns)
	}
	if !sameJSON(got, base) {
		t.Errorf("an empty overlay changed the base:\n got %+v\nbase %+v", got, base)
	}
}

// A wildcard suffix can match the dotted text of an address ("*.0.5" matches
// "10.0.0.5" in the proxy's host verdict) without ever granting the trust an
// EXACT entry gets over the private-address guard. An exact literal IP is
// therefore covered by an equal exact entry and by nothing else.
func TestMeetLiteralIPIsNotCoveredByAWildcard(t *testing.T) {
	base := Authority{Ceiling: types.RunPolicySpec{AllowedDomains: []string{"*.0.5"}}}
	ov := overlayOf(types.CeilingOverlay{AllowedDomains: &[]string{"10.0.0.5"}})
	wantOverlayErr(t, ValidateOverlay(base, ov), ReasonOverlayInvalid, "allowed_domains")
	if got := mustApply(t, base, ov); len(got.Ceiling.AllowedDomains) != 0 {
		t.Errorf("allowed_domains = %v: a literal IP must never come out of a wildcard", got.Ceiling.AllowedDomains)
	}
}
