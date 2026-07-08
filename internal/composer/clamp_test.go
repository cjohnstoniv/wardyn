// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// operatorCeiling is a representative operator policy max used across clamp tests.
func operatorCeiling(t *testing.T) types.RunPolicySpec {
	t.Helper()
	return types.RunPolicySpec{
		MinConfinementClass: types.CC2, // operator requires at least gVisor
		AllowAllEgress:      false,     // operator forbids allow-all
		AllowedDomains:      []string{"api.anthropic.com", "github.com"},
		EligibleGrants: []types.GrantSpec{
			{Kind: types.GrantGitHubToken, RequiresApproval: true,
				Scope: mustJSON(t, map[string]any{"repos": []string{"acme/widgets"}, "permissions": map[string]string{"contents": "read"}})},
		},
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestClamp_RaisesConfinementToOperatorFloor(t *testing.T) {
	// Proposal wants CC1 (weaker); operator floor is CC2.
	got, warns := Clamp(types.RunPolicySpec{MinConfinementClass: types.CC1}, operatorCeiling(t))
	if got.MinConfinementClass != types.CC2 {
		t.Errorf("confinement = %s, want CC2 (raised to floor)", got.MinConfinementClass)
	}
	if !hasWarn(warns, "confinement raised") {
		t.Errorf("expected a confinement-raise warning, got %v", warns)
	}
	// A STRONGER proposal (CC3) is left alone.
	got, _ = Clamp(types.RunPolicySpec{MinConfinementClass: types.CC3}, operatorCeiling(t))
	if got.MinConfinementClass != types.CC3 {
		t.Errorf("CC3 should be preserved, got %s", got.MinConfinementClass)
	}
}

// TestEffectiveConfinementFloor covers the per-run compose floor (E3): the
// operator's Getting Started default tier raises the policy minimum RAISE-ONLY,
// capped at the strongest class the host can enforce so a too-strong floor
// degrades instead of 422ing at launch.
func TestEffectiveConfinementFloor(t *testing.T) {
	cases := []struct {
		name             string
		policyMin, floor types.ConfinementClass
		cap              types.ConfinementClass
		want             types.ConfinementClass
	}{
		{"floor raises above policy min", types.CC1, types.CC3, types.CC3, types.CC3},
		{"availability cap degrades a too-strong floor", types.CC1, types.CC3, types.CC1, types.CC1},
		{"cap between floor and min still caps", types.CC1, types.CC3, types.CC2, types.CC2},
		{"floor weaker than policy min is a no-op", types.CC2, types.CC1, types.CC3, types.CC2},
		{"empty floor leaves the policy min", types.CC2, "", types.CC3, types.CC2},
		{"unknown cap does not cap", types.CC1, types.CC3, "", types.CC3},
		// FAIL-CLOSED: the cap degrades the per-run floor only, NEVER the operator's
		// configured policy minimum. An unenforceable CC3 policy min on a Fence-only
		// (CC1) host stays CC3 here and fails closed at the launch confinement gate —
		// compose must not become the one path that silently bypasses it.
		{"cap never lowers the policy min", types.CC3, "", types.CC1, types.CC3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EffectiveConfinementFloor(tc.policyMin, tc.floor, tc.cap); got != tc.want {
				t.Errorf("EffectiveConfinementFloor(%q,%q,%q) = %q, want %q",
					tc.policyMin, tc.floor, tc.cap, got, tc.want)
			}
		})
	}
}

// TestEffectiveConfinementFloor_FlowsThroughClampWarning proves the per-run floor
// reaches the operator through Clamp's EXISTING confinement-raise warning (zero
// new UI): feeding the effective floor as the ceiling raises a weaker proposal
// AND emits the warning; a floor weaker than the proposal is a silent no-op.
func TestEffectiveConfinementFloor_FlowsThroughClampWarning(t *testing.T) {
	ceiling := operatorCeiling(t) // policy min CC2
	// Per-run floor CC3, host can enforce CC3 → effective floor CC3.
	ceiling.MinConfinementClass = EffectiveConfinementFloor(ceiling.MinConfinementClass, types.CC3, types.CC3)
	got, warns := Clamp(types.RunPolicySpec{MinConfinementClass: types.CC1}, ceiling)
	if got.MinConfinementClass != types.CC3 {
		t.Errorf("confinement = %s, want CC3 (raised to the per-run floor)", got.MinConfinementClass)
	}
	if !hasWarn(warns, "confinement raised") {
		t.Errorf("expected the confinement-raise warning, got %v", warns)
	}
	// A proposal STRONGER than the floor: the floor is a no-op and must not warn.
	weaker := operatorCeiling(t)
	weaker.MinConfinementClass = EffectiveConfinementFloor(types.CC1, types.CC2, types.CC3) // effective floor CC2
	if _, warns := Clamp(types.RunPolicySpec{MinConfinementClass: types.CC3}, weaker); hasWarn(warns, "confinement raised") {
		t.Errorf("a floor weaker than the proposal must not warn, got %v", warns)
	}
}

// TestClampRunConfinement_RaisesRunToPolicyFloor guards against a self-inconsistent
// proposal: a run asking CC1 under a policy clamped to a CC2 floor must come out CC2
// (>= floor), or handleCreateRun would 422 the composed run (invariant 5).
func TestClampRunConfinement_RaisesRunToPolicyFloor(t *testing.T) {
	// The policy the proposal carried, clamped to the operator floor (CC2).
	clamped, _ := Clamp(types.RunPolicySpec{MinConfinementClass: types.CC1}, operatorCeiling(t))

	// Run advertised CC1 — weaker than the clamped floor. Must be raised to CC2.
	got, warn := ClampRunConfinement("CC1", clamped.MinConfinementClass)
	if got != "CC2" {
		t.Errorf("run confinement = %q, want CC2 (>= policy floor)", got)
	}
	if warn == "" {
		t.Errorf("expected a raise warning when the run class is tightened")
	}
	// An empty/unset run class also ranks below the floor and is raised.
	if got, _ := ClampRunConfinement("", clamped.MinConfinementClass); got != "CC2" {
		t.Errorf("empty run class = %q, want CC2 (raised to floor)", got)
	}
	// A run that legitimately asked for a STRONGER class than the floor is untouched.
	if got, warn := ClampRunConfinement("CC3", clamped.MinConfinementClass); got != "CC3" || warn != "" {
		t.Errorf("CC3 should be preserved with no warning, got %q / %q", got, warn)
	}
}

func TestClamp_ForcesAllowAllEgressOff(t *testing.T) {
	got, warns := Clamp(types.RunPolicySpec{AllowAllEgress: true}, operatorCeiling(t))
	if got.AllowAllEgress {
		t.Errorf("allow_all_egress must be forced off when operator forbids it")
	}
	if !hasWarn(warns, "allow_all_egress disabled") {
		t.Errorf("expected allow-all warning, got %v", warns)
	}
}

func TestClamp_IntersectsAllowedDomainsToCeiling(t *testing.T) {
	got, warns := Clamp(types.RunPolicySpec{
		AllowedDomains: []string{"api.anthropic.com", "evil.example.com"},
	}, operatorCeiling(t))
	for _, d := range got.AllowedDomains {
		if strings.Contains(d, "evil") {
			t.Errorf("evil.example.com should have been dropped, got %v", got.AllowedDomains)
		}
	}
	if !hasWarn(warns, "dropped 1 egress domain") {
		t.Errorf("expected dropped-domain warning, got %v", warns)
	}
}

// TestClamp_DenyAllCeilingClampsAllowedDomainsToEmpty is FIX #7: an operator
// shipping the strictest posture (allow_all_egress:false, allowed_domains:[])
// means default-deny-all egress. Before the fix, the `len(ceiling.AllowedDomains)
// > 0` guard skipped the whole intersect block for exactly this ceiling, so a
// prompt-injected proposal's AllowedDomains passed through untouched. It must
// instead clamp to empty, same as clampGrants fails closed on an empty ceiling.
func TestClamp_DenyAllCeilingClampsAllowedDomainsToEmpty(t *testing.T) {
	ceiling := types.RunPolicySpec{AllowAllEgress: false, AllowedDomains: []string{}}
	got, warns := Clamp(types.RunPolicySpec{AllowedDomains: []string{"exfil.example"}}, ceiling)
	if len(got.AllowedDomains) != 0 {
		t.Errorf("deny-all ceiling must clamp AllowedDomains to empty, got %v", got.AllowedDomains)
	}
	if !hasWarn(warns, "dropped 1 egress domain") {
		t.Errorf("expected dropped-domain warning recording exfil.example, got %v", warns)
	}
}

// TestClamp_NonEmptyCeilingStillIntersects is the control for the fix above:
// a non-empty allowlist ceiling still intersects normally (only the empty-ceiling
// case changed behavior).
func TestClamp_NonEmptyCeilingStillIntersects(t *testing.T) {
	ceiling := types.RunPolicySpec{AllowAllEgress: false, AllowedDomains: []string{"api.github.com"}}
	got, warns := Clamp(types.RunPolicySpec{AllowedDomains: []string{"api.github.com", "exfil.example"}}, ceiling)
	if len(got.AllowedDomains) != 1 || got.AllowedDomains[0] != "api.github.com" {
		t.Errorf("expected only api.github.com kept, got %v", got.AllowedDomains)
	}
	if !hasWarn(warns, "dropped 1 egress domain") {
		t.Errorf("expected dropped-domain warning recording exfil.example, got %v", warns)
	}
}

func TestClamp_DropsGrantKindNotInCeiling(t *testing.T) {
	got, warns := Clamp(types.RunPolicySpec{EligibleGrants: []types.GrantSpec{
		{Kind: types.GrantCloudSTS, RequiresApproval: false},
	}}, operatorCeiling(t))
	if len(got.EligibleGrants) != 0 {
		t.Errorf("cloud_sts (not in ceiling) should be dropped, got %v", got.EligibleGrants)
	}
	if !hasWarn(warns, "not in operator's eligible grants") {
		t.Errorf("expected dropped-grant warning, got %v", warns)
	}
}

func TestClamp_GitHubPermsIntersectedDownAndApprovalForced(t *testing.T) {
	// Proposal asks for contents:write + pull_requests:write, no approval, extra repo.
	got, warns := Clamp(types.RunPolicySpec{EligibleGrants: []types.GrantSpec{
		{Kind: types.GrantGitHubToken, RequiresApproval: false, Scope: mustJSON(t, map[string]any{
			"repos":       []string{"acme/widgets", "acme/secret-repo"},
			"permissions": map[string]string{"contents": "write", "pull_requests": "write"},
		})},
	}}, operatorCeiling(t))
	if len(got.EligibleGrants) != 1 {
		t.Fatalf("expected the github grant kept (clamped), got %d", len(got.EligibleGrants))
	}
	g := got.EligibleGrants[0]
	if !g.RequiresApproval {
		t.Errorf("requires_approval must be forced on (operator requires it)")
	}
	var s struct {
		Repos       []string          `json:"repos"`
		Permissions map[string]string `json:"permissions"`
	}
	if err := json.Unmarshal(g.Scope, &s); err != nil {
		t.Fatal(err)
	}
	// Ceiling only allows contents:read on acme/widgets.
	if s.Permissions["contents"] != "read" {
		t.Errorf("contents should be clamped to read, got %q", s.Permissions["contents"])
	}
	if _, ok := s.Permissions["pull_requests"]; ok {
		t.Errorf("pull_requests (not in operator policy) should be dropped, got %v", s.Permissions)
	}
	for _, r := range s.Repos {
		if strings.Contains(r, "secret-repo") {
			t.Errorf("acme/secret-repo outside operator scope should be dropped, got %v", s.Repos)
		}
	}
	if len(warns) == 0 {
		t.Errorf("expected clamp warnings for the github grant")
	}
}

func TestClamp_DropsWorkspaceMountsAlways(t *testing.T) {
	rw := false
	got, warns := Clamp(types.RunPolicySpec{WorkspaceMounts: []types.WorkspaceMount{
		{Source: "/etc", Target: "/work", ReadOnly: &rw},
	}}, operatorCeiling(t))
	if len(got.WorkspaceMounts) != 0 {
		t.Errorf("composer-proposed workspace mounts must always be dropped, got %v", got.WorkspaceMounts)
	}
	if !hasWarn(warns, "workspace mount") {
		t.Errorf("expected workspace-mount drop warning, got %v", warns)
	}
}

func TestClamp_CapsGrantTTL(t *testing.T) {
	got, warns := Clamp(types.RunPolicySpec{EligibleGrants: []types.GrantSpec{
		{Kind: types.GrantGitHubToken, RequiresApproval: true, TTLSeconds: 999999,
			Scope: mustJSON(t, map[string]any{"repos": []string{"acme/widgets"}, "permissions": map[string]string{"contents": "read"}})},
	}}, operatorCeiling(t))
	if got.EligibleGrants[0].TTLSeconds > maxGrantTTLSeconds {
		t.Errorf("TTL must be capped to %d, got %d", maxGrantTTLSeconds, got.EligibleGrants[0].TTLSeconds)
	}
	if !hasWarn(warns, "TTL capped") {
		t.Errorf("expected TTL-cap warning, got %v", warns)
	}
}

// THE headline clamp security property: an attacker-influenced proposal that
// maxes out every dangerous axis is clamped back to the operator ceiling, AND the
// resulting risk grade cannot be lower than the clamped reality. A
// prompt-injected attachment cannot smuggle a more-permissive setup past the
// operator, nor can it lower the graded risk (the grade is computed from the
// CLAMPED spec).
func TestClamp_AttackerMaxedProposalCannotExceedCeilingOrLowerRisk(t *testing.T) {
	ceiling := operatorCeiling(t)
	hostile := types.RunPolicySpec{
		MinConfinementClass: types.CC1, // weakest
		AllowAllEgress:      true,      // exfil-max
		AllowedDomains:      []string{"exfil.evil.com"},
		EligibleGrants: []types.GrantSpec{
			{Kind: types.GrantGitHubToken, RequiresApproval: false, TTLSeconds: 999999, Scope: mustJSON(t, map[string]any{
				"repos": []string{"acme/widgets", "victim/private"}, "permissions": map[string]string{"contents": "write", "administration": "write"}})},
			{Kind: types.GrantCloudSTS, RequiresApproval: false},
		},
	}
	clamped, warns := Clamp(hostile, ceiling)
	if clamped.AllowAllEgress {
		t.Errorf("allow-all must be clamped off")
	}
	if confinementRank(clamped.MinConfinementClass) < confinementRank(ceiling.MinConfinementClass) {
		t.Errorf("confinement must be at least the operator floor")
	}
	for _, d := range clamped.AllowedDomains {
		if strings.Contains(d, "evil") {
			t.Errorf("hostile egress domain survived: %v", clamped.AllowedDomains)
		}
	}
	// cloud_sts dropped; github intersected to read on acme/widgets, approval forced.
	if len(clamped.EligibleGrants) != 1 || clamped.EligibleGrants[0].Kind != types.GrantGitHubToken {
		t.Fatalf("expected only the clamped github grant, got %v", clamped.EligibleGrants)
	}
	if !clamped.EligibleGrants[0].RequiresApproval {
		t.Errorf("approval must be forced on the surviving grant")
	}
	if len(warns) == 0 {
		t.Errorf("expected warnings documenting every clamp")
	}
	// The grade of the CLAMPED spec reflects the clamped reality (now much safer):
	// no allow-all-high, confinement at least CC2, no write-without-approval.
	items := Grade(RunInput{}, clamped)
	if _, ok := find(items, "allow_all_egress"); ok {
		t.Errorf("clamped spec should not carry an allow_all_egress HIGH item")
	}
}

// FakeComposer round-trips the request and returns the preset proposal (used by
// the endpoint tests). No network.
func TestFakeComposer_RecordsRequestAndReturnsResult(t *testing.T) {
	want := Proposal{Run: RunInput{Agent: "claude-code", Repo: "acme/widgets"}, Summary: "ok"}
	f := &FakeComposer{Result: want}
	got, err := f.Propose(context.Background(), ComposeRequest{Prompt: "do a thing"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Run.Agent != "claude-code" {
		t.Errorf("proposal not returned: %+v", got)
	}
	if f.Last.Prompt != "do a thing" {
		t.Errorf("request not recorded: %+v", f.Last)
	}
}

func hasWarn(warns []string, substr string) bool {
	for _, w := range warns {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}
