// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
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

func TestClamp_ForcesGitPushAnyBranchOff(t *testing.T) {
	got, warns := Clamp(types.RunPolicySpec{GitPushAnyBranch: true}, operatorCeiling(t))
	if got.GitPushAnyBranch {
		t.Errorf("git_push_any_branch must be forced off when the operator ceiling keeps confinement on")
	}
	if !hasWarn(warns, "git_push_any_branch disabled") {
		t.Errorf("expected git_push_any_branch warning, got %v", warns)
	}
	ceiling := operatorCeiling(t)
	ceiling.GitPushAnyBranch = true
	got, warns = Clamp(types.RunPolicySpec{GitPushAnyBranch: true}, ceiling)
	if !got.GitPushAnyBranch {
		t.Errorf("git_push_any_branch must survive when the ceiling allows it")
	}
	if hasWarn(warns, "git_push_any_branch disabled") {
		t.Errorf("no warning expected when the ceiling allows it, got %v", warns)
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

// TestClamp_FirstUseApprovalTakesStricter is HIGH-2: a proposal must never come
// out WEAKER than the ceiling on first_use_approval, in either direction — a
// weaker proposal is raised, a stronger one is left alone, and an unset ceiling
// (its own fail-closed Normalize() default) still floors to always_deny.
func TestClamp_FirstUseApprovalTakesStricter(t *testing.T) {
	cases := []struct {
		name              string
		proposed, ceiling types.FirstUseMode
		want              types.FirstUseMode
		wantWarn          bool
	}{
		{"weaker proposal raised to stricter ceiling", types.FirstUseWaitForReview, types.FirstUseAlwaysDeny, types.FirstUseAlwaysDeny, true},
		{"stronger proposal is preserved", types.FirstUseAlwaysDeny, types.FirstUseWaitForReview, types.FirstUseAlwaysDeny, false},
		{"equal is a no-op", types.FirstUseDenyWithReview, types.FirstUseDenyWithReview, types.FirstUseDenyWithReview, false},
		{"unset ceiling floors to always_deny (Normalize's own default)", types.FirstUseWaitForReview, "", types.FirstUseAlwaysDeny, true},
		// An unset PROPOSAL already normalizes to always_deny (rank 3) — that
		// already dominates a lenient ceiling, so the output is spelled out
		// explicitly (never a bare "") but nothing was actually RAISED: no warning.
		{"unset proposal normalizes to always_deny outright — no raise, no warning", "", types.FirstUseWaitForReview, types.FirstUseAlwaysDeny, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ceiling := operatorCeiling(t)
			ceiling.FirstUseApproval = tc.ceiling
			got, warns := Clamp(types.RunPolicySpec{FirstUseApproval: tc.proposed}, ceiling)
			if got.FirstUseApproval != tc.want {
				t.Errorf("first_use_approval = %q, want %q", got.FirstUseApproval, tc.want)
			}
			if hasWarn(warns, "first_use_approval raised") != tc.wantWarn {
				t.Errorf("warning present = %v, want %v (warns=%v)", !tc.wantWarn, tc.wantWarn, warns)
			}
		})
	}
}

// TestClamp_LLMInspectionInheritsCeiling is HIGH-2: nil means OFF, so an
// omitted (or merely different) proposal must inherit the ceiling's spec
// outright whenever the ceiling sets one — never stay nil under a ceiling that
// turns inspection on.
func TestClamp_LLMInspectionInheritsCeiling(t *testing.T) {
	ceiling := operatorCeiling(t)
	ceiling.LLMInspection = &types.LLMInspectionSpec{Mode: "block", DetectSecrets: true}

	// Proposal omits it entirely.
	got, warns := Clamp(types.RunPolicySpec{}, ceiling)
	if got.LLMInspection == nil || got.LLMInspection.Mode != "block" {
		t.Errorf("llm_inspection = %+v, want the ceiling's block mode", got.LLMInspection)
	}
	if !hasWarn(warns, "llm_inspection set to the operator's configured mode") {
		t.Errorf("expected an llm_inspection warning, got %v", warns)
	}

	// Proposal sets a WEAKER mode — still overridden to the ceiling's.
	got, _ = Clamp(types.RunPolicySpec{LLMInspection: &types.LLMInspectionSpec{Mode: "alert"}}, ceiling)
	if got.LLMInspection.Mode != "block" {
		t.Errorf("weaker proposed mode survived clamp: got %q, want block", got.LLMInspection.Mode)
	}

	// No ceiling opinion: proposal is left alone (including nil).
	got, warns = Clamp(types.RunPolicySpec{}, operatorCeiling(t))
	if got.LLMInspection != nil {
		t.Errorf("llm_inspection = %+v, want nil (ceiling sets none)", got.LLMInspection)
	}
	if hasWarn(warns, "llm_inspection") {
		t.Errorf("unexpected llm_inspection warning with no ceiling opinion: %v", warns)
	}
}

// TestClamp_LLMInspectionInheritUnionsSidecarHostIntoAllowedDomains is the
// bug-policy-1 regression: the AllowedDomains intersection above runs BEFORE
// the LLMInspection ceiling-inherit block, so the narrowed proposal's own
// AllowedDomains never carried the ceiling's detector_sidecar_url host — and
// internal/api's validateLLMInspection requires that host to be an EXACT
// entry on this SAME post-clamp spec's own allowed_domains, so every
// compose/profile/inline_policy run under an operator that sets
// detector_sidecar_url self-rejected. The host must land in the clamped
// output's AllowedDomains even when the PROPOSAL never mentioned it — it
// comes from the operator's own ceiling config, the same trust level as the
// DeniedDomains union above.
func TestClamp_LLMInspectionInheritUnionsSidecarHostIntoAllowedDomains(t *testing.T) {
	ceiling := operatorCeiling(t) // AllowedDomains: api.anthropic.com, github.com — sidecar host NOT in it
	ceiling.LLMInspection = &types.LLMInspectionSpec{
		Mode: "block", DetectSecrets: true,
		DetectorSidecarURL: "https://detector.acme-corp.internal:8443/scan",
	}

	// Bare proposal — mentions neither the sidecar host nor any egress at all.
	got, _ := Clamp(types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}}, ceiling)
	if got.LLMInspection == nil || got.LLMInspection.DetectorSidecarURL != ceiling.LLMInspection.DetectorSidecarURL {
		t.Fatalf("llm_inspection = %+v, want the ceiling's sidecar config inherited", got.LLMInspection)
	}
	found := false
	for _, d := range got.AllowedDomains {
		if strings.EqualFold(d, "detector.acme-corp.internal") {
			found = true
		}
	}
	if !found {
		t.Errorf("allowed_domains = %v, want the inherited sidecar's host (detector.acme-corp.internal) present so validateLLMInspection's exact-entry check passes", got.AllowedDomains)
	}
	// api.anthropic.com must still survive too — this is a union, not a replace.
	if !domainsContain(got.AllowedDomains, "api.anthropic.com") {
		t.Errorf("allowed_domains = %v, want api.anthropic.com to survive the union", got.AllowedDomains)
	}
}

func domainsContain(domains []string, want string) bool {
	for _, d := range domains {
		if strings.EqualFold(d, want) {
			return true
		}
	}
	return false
}

// TestClamp_LLMInspectionDroppedUnderNilCeiling is W12-A-1 (CRIT) / W14-S1-1:
// a member's hand-authored inline_policy.llm_inspection used to pass through
// COMPLETELY unclamped whenever the ceiling set none — exactly the shipped
// default.json posture (it sets no llm_inspection at all). That let a member
// turn the content-inspection sidecar on and point it (detector_sidecar_url)
// at ANY URL the wardyn-proxy process can reach, or flip intercept_tls, with
// zero operator opinion in the way. Symmetric with the workspace_mounts drop:
// an unset ceiling is the FLOOR for this field, not "no opinion".
func TestClamp_LLMInspectionDroppedUnderNilCeiling(t *testing.T) {
	ceiling := operatorCeiling(t) // sets no llm_inspection opinion, like default.json
	hostile := types.RunPolicySpec{LLMInspection: &types.LLMInspectionSpec{
		Mode: "alert", DetectSecrets: true,
		DetectorSidecarURL: "http://attacker.example.com/exfil",
		InterceptTLS:       true,
	}}
	got, warns := Clamp(hostile, ceiling)
	if got.LLMInspection != nil {
		t.Errorf("llm_inspection = %+v, want nil — a nil-ceiling must be the FLOOR for this field, not a pass-through", got.LLMInspection)
	}
	if !hasWarn(warns, "llm_inspection dropped") {
		t.Errorf("expected an llm_inspection-dropped warning, got %v", warns)
	}
}

// TestClamp_LLMInspectionCopyRedactsSecretValues is W12-A-3: the ceiling's
// llm_inspection is unconditionally inherited (see above), but a compose/
// profile PROPOSAL is advisory output returned straight to the caller in an
// HTTP response (and, before the fix, embedded in the run.compose audit event
// too) — it must never carry the resolved secret VALUES, only the NAMES a
// caller needs to know which secrets are covered. Dispatch alone resolves
// names->values, in memory, for the proxy sidecar (see runs_dispatch.go).
func TestClamp_LLMInspectionCopyRedactsSecretValues(t *testing.T) {
	ceiling := operatorCeiling(t)
	ceiling.LLMInspection = &types.LLMInspectionSpec{
		Mode: "alert", DetectSecrets: true,
		WorkspaceSecretNames:  []string{"prod-db-password"},
		WorkspaceSecretValues: []string{"hunter2-this-must-never-leak"},
	}
	got, _ := Clamp(types.RunPolicySpec{}, ceiling)
	if got.LLMInspection == nil {
		t.Fatal("expected llm_inspection inherited from the ceiling")
	}
	if len(got.LLMInspection.WorkspaceSecretValues) != 0 {
		t.Errorf("W12-A-3: clamp copy must zero workspace_secret_values, got %v", got.LLMInspection.WorkspaceSecretValues)
	}
	if len(got.LLMInspection.WorkspaceSecretNames) != 1 || got.LLMInspection.WorkspaceSecretNames[0] != "prod-db-password" {
		t.Errorf("workspace_secret_names should survive the copy (the proposal needs to show WHICH secrets are covered), got %v", got.LLMInspection.WorkspaceSecretNames)
	}
	// The ceiling itself must be untouched (no aliasing): a second Clamp call
	// must see the SAME ceiling values again, not an already-redacted copy.
	if len(ceiling.LLMInspection.WorkspaceSecretValues) != 1 {
		t.Errorf("Clamp must not mutate the ceiling's own LLMInspection in place, got %v", ceiling.LLMInspection.WorkspaceSecretValues)
	}
}

// TestClamp_GitHubEmptyCeilingRepoListDeniesAll is W23-S1-3: a member's
// hand-authored inline_policy github_token grant must not survive Clamp when
// the ceiling's OWN grant sets no repo allowlist. The SHIPPED default.json
// ceiling — resolveRunPolicy's real DefaultPolicy on exactly this path
// (internal/api/inline_policy.go) — ships EXACTLY this shape ("repos": [],
// a template for the composer/profile pipelines to ground, not an "any repo"
// grant for a raw member): operatorCeiling(t) above never exercises it, since
// its own github_token grant carries a non-empty repos list — that is the
// "unshipped ceiling shape" that let this bug ship. Loaded verbatim from the
// real file so this test breaks if the shipped ceiling shape ever changes.
func TestClamp_GitHubEmptyCeilingRepoListDeniesAll(t *testing.T) {
	ceiling := loadDefaultPolicyCeiling(t)
	if len(ceiling.EligibleGrants) == 0 || ceiling.EligibleGrants[0].Kind != types.GrantGitHubToken {
		t.Fatalf("examples/policies/default.json eligible_grants[0] is expected to be a github_token grant; got %+v", ceiling.EligibleGrants)
	}
	var ceilRepos struct {
		Repos []string `json:"repos"`
	}
	_ = json.Unmarshal(ceiling.EligibleGrants[0].Scope, &ceilRepos)
	if len(ceilRepos.Repos) != 0 {
		t.Fatalf("examples/policies/default.json github_token repos = %v, want empty (this test's whole premise)", ceilRepos.Repos)
	}

	// A member's hand-authored inline_policy asking for an ARBITRARY repo the
	// operator never selected or grounded.
	hostile := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{
		{Kind: types.GrantGitHubToken, Scope: mustJSON(t, map[string]any{
			"repos": []string{"attacker-org/private-repo"}, "permissions": map[string]string{"contents": "read"},
		})},
	}}
	clamped, warns := Clamp(hostile, ceiling)
	if len(clamped.EligibleGrants) != 1 {
		t.Fatalf("expected the github_token grant kept (clamped by scope, not dropped by kind), got %d", len(clamped.EligibleGrants))
	}
	var got struct {
		Repos []string `json:"repos"`
	}
	_ = json.Unmarshal(clamped.EligibleGrants[0].Scope, &got)
	if len(got.Repos) != 0 {
		t.Errorf("W23-S1-3: an empty ceiling repo list must deny ALL repos for a hand-authored spec, got %v", got.Repos)
	}
	if !hasWarn(warns, "outside operator scope") {
		t.Errorf("expected a dropped-repo warning, got %v", warns)
	}
}

// loadDefaultPolicyCeiling loads examples/policies/default.json VERBATIM
// (plain json.Unmarshal, not composer-package-reachable LoadPolicySpec/
// validatePolicySpec — internal/api imports internal/composer, so the reverse
// import would cycle) — the real shipped ceiling shape, not a test fixture's
// approximation of it.
func loadDefaultPolicyCeiling(t *testing.T) types.RunPolicySpec {
	t.Helper()
	b, err := os.ReadFile("../../examples/policies/default.json")
	if err != nil {
		t.Fatalf("read default.json: %v", err)
	}
	var spec types.RunPolicySpec
	if err := json.Unmarshal(b, &spec); err != nil {
		t.Fatalf("parse default.json: %v", err)
	}
	return spec
}

// TestClamp_AllowedMethods is HIGH-2: empty means "all" for this field (unlike
// AllowedDomains' default-deny empty), so an empty proposal must ADOPT the
// ceiling's restriction, and a non-empty one intersects down to it.
func TestClamp_AllowedMethods(t *testing.T) {
	ceiling := operatorCeiling(t)
	ceiling.AllowedMethods = []string{"GET", "POST"}

	// Empty proposal adopts the ceiling's list outright.
	got, warns := Clamp(types.RunPolicySpec{}, ceiling)
	if strings.Join(got.AllowedMethods, ",") != "GET,POST" {
		t.Errorf("allowed_methods = %v, want the ceiling's [GET POST]", got.AllowedMethods)
	}
	if !hasWarn(warns, "allowed_methods restricted") {
		t.Errorf("expected an allowed_methods warning, got %v", warns)
	}

	// Non-empty proposal intersects down (DELETE is outside the ceiling).
	got, warns = Clamp(types.RunPolicySpec{AllowedMethods: []string{"GET", "DELETE"}}, ceiling)
	if len(got.AllowedMethods) != 1 || got.AllowedMethods[0] != "GET" {
		t.Errorf("allowed_methods = %v, want [GET]", got.AllowedMethods)
	}
	if !hasWarn(warns, "dropped 1 method") {
		t.Errorf("expected a dropped-method warning, got %v", warns)
	}

	// No ceiling opinion: an empty proposal stays empty (means "all" — a no-op).
	got, warns = Clamp(types.RunPolicySpec{}, operatorCeiling(t))
	if len(got.AllowedMethods) != 0 {
		t.Errorf("allowed_methods = %v, want empty (no ceiling opinion)", got.AllowedMethods)
	}
	if hasWarn(warns, "allowed_methods") {
		t.Errorf("unexpected allowed_methods warning with no ceiling opinion: %v", warns)
	}
}

// TestClamp_UIApps is PF-17: ui_apps was the one RunPolicySpec field with a
// WIDENING direction that Clamp did not touch at all — a member's inline_policy
// could declare any (name, port) pair and the UI gateway would relay that
// loopback port to a browser.
//
// It takes allowed_methods' CEILING asymmetry (set ⇒ bound, silent ⇒ leave) and
// deliberately not its ADOPT half, which is what the last case pins: an empty
// ui_apps means "this run has no UI apps", the narrowest state there is, so
// adopting the ceiling's list would have the clamp hand out relay access nobody
// asked for.
func TestClamp_UIApps(t *testing.T) {
	code := types.UIApp{Name: "code", Port: 8080}
	ceiling := operatorCeiling(t)
	ceiling.UIApps = []types.UIApp{code}

	// A proposal naming the ceiling's app survives, path and all.
	got, warns := Clamp(types.RunPolicySpec{UIApps: []types.UIApp{{Name: "code", Port: 8080, Path: "/ide"}}}, ceiling)
	if len(got.UIApps) != 1 || got.UIApps[0].Path != "/ide" {
		t.Errorf("ui_apps = %+v, want the ceiling-declared app kept with its own landing path", got.UIApps)
	}
	if hasWarn(warns, "ui_app") {
		t.Errorf("unexpected ui_apps warning for an in-ceiling app: %v", warns)
	}

	// An app the ceiling never declared is dropped...
	got, warns = Clamp(types.RunPolicySpec{UIApps: []types.UIApp{code, {Name: "shell", Port: 9999}}}, ceiling)
	if len(got.UIApps) != 1 || got.UIApps[0].Name != "code" {
		t.Errorf("ui_apps = %+v, want only the ceiling's app", got.UIApps)
	}
	if !hasWarn(warns, "dropped 1 ui_app") {
		t.Errorf("expected a dropped-ui_app warning, got %v", warns)
	}

	// ...and so is the ceiling's own NAME pointed at a different port. Matching on
	// the name alone would let a proposal borrow a blessed app's name and have the
	// gateway relay any port in the sandbox — the widening this clamp exists for.
	got, _ = Clamp(types.RunPolicySpec{UIApps: []types.UIApp{{Name: "code", Port: 9999}}}, ceiling)
	if len(got.UIApps) != 0 {
		t.Errorf("ui_apps = %+v — a ceiling app's NAME on a different port was relayed", got.UIApps)
	}

	// A SILENT ceiling is no opinion: the proposal is left exactly as authored, so
	// a profile that says nothing about ui_apps does not break every run that
	// declares one (the DefaultPolicy-ceiling member, i.e. every member today).
	got, warns = Clamp(types.RunPolicySpec{UIApps: []types.UIApp{{Name: "shell", Port: 9999}}}, operatorCeiling(t))
	if len(got.UIApps) != 1 || got.UIApps[0].Name != "shell" {
		t.Errorf("ui_apps = %+v, want untouched under a ceiling with no opinion", got.UIApps)
	}
	if hasWarn(warns, "ui_app") {
		t.Errorf("unexpected ui_apps warning with no ceiling opinion: %v", warns)
	}

	// An EMPTY proposal never adopts — the anti-case that separates this field
	// from allowed_methods above.
	if got, _ = Clamp(types.RunPolicySpec{}, ceiling); len(got.UIApps) != 0 {
		t.Errorf("ui_apps = %+v — an empty proposal ADOPTED the ceiling's apps; empty means `no UI apps`, not `all of them`", got.UIApps)
	}
}

// TestClamp_Resources is HIGH-2: each field caps at the ceiling's when the
// ceiling sets one; an unset (<=0) proposed field is the PERMISSIVE state here
// (filled in by the driver's own default later) and is capped down exactly like
// an explicit value that exceeds the ceiling.
func TestClamp_Resources(t *testing.T) {
	ceiling := operatorCeiling(t)
	ceiling.Resources = &types.ResourceLimits{CPUMillis: 2000, MemoryMiB: 4096}

	// Proposal omits Resources entirely: adopts the ceiling's caps.
	got, warns := Clamp(types.RunPolicySpec{}, ceiling)
	if got.Resources == nil || got.Resources.CPUMillis != 2000 || got.Resources.MemoryMiB != 4096 {
		t.Errorf("resources = %+v, want the ceiling's caps", got.Resources)
	}
	if !hasWarn(warns, "resources capped") {
		t.Errorf("expected a resources warning, got %v", warns)
	}

	// Proposal exceeds the ceiling on one field, stays under on another, and
	// leaves PidsLimit unset (0) — only CPUMillis should move.
	got, _ = Clamp(types.RunPolicySpec{Resources: &types.ResourceLimits{CPUMillis: 8000, MemoryMiB: 1024}}, ceiling)
	if got.Resources.CPUMillis != 2000 {
		t.Errorf("CPUMillis = %d, want capped to 2000", got.Resources.CPUMillis)
	}
	if got.Resources.MemoryMiB != 1024 {
		t.Errorf("MemoryMiB = %d, want the proposal's own 1024 (already under the cap)", got.Resources.MemoryMiB)
	}

	// No ceiling opinion (W14-S1-3): falls back to the platform defaults —
	// the SAME conservative caps CreateSandbox itself applies when a
	// Resources field is zero — never left uncapped.
	got, warns = Clamp(types.RunPolicySpec{}, operatorCeiling(t))
	if got.Resources == nil ||
		got.Resources.CPUMillis != int(runner.DefaultCPUMillis) ||
		got.Resources.MemoryMiB != int(runner.DefaultMemoryMiB) ||
		got.Resources.PidsLimit != int(runner.DefaultPidsLimit) {
		t.Errorf("resources = %+v, want the platform defaults (no ceiling opinion must not mean uncapped)", got.Resources)
	}
	if !hasWarn(warns, "resources capped") {
		t.Errorf("expected a resources warning falling back to platform defaults, got %v", warns)
	}
}

// TestClamp_AutoStopAfterSec is HIGH-2: capped at the ceiling's maximum when
// the ceiling sets a real (positive) one; 0 (platform default) and negative
// (never reap) both rank as MORE permissive than an explicit cap.
func TestClamp_AutoStopAfterSec(t *testing.T) {
	ceiling := operatorCeiling(t)
	ceiling.AutoStopAfterSec = 3600

	cases := []struct {
		name     string
		proposed int
		want     int
		wantWarn bool
	}{
		{"never-reap is capped", -1, 3600, true},
		{"platform-default (0) is capped", 0, 3600, true},
		{"excessive positive is capped", 999999, 3600, true},
		{"already under the cap is preserved", 1800, 1800, false},
		{"exactly at the cap is a no-op", 3600, 3600, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, warns := Clamp(types.RunPolicySpec{AutoStopAfterSec: tc.proposed}, ceiling)
			if got.AutoStopAfterSec != tc.want {
				t.Errorf("auto_stop_after_sec = %d, want %d", got.AutoStopAfterSec, tc.want)
			}
			if hasWarn(warns, "auto_stop_after_sec capped") != tc.wantWarn {
				t.Errorf("warning present = %v, want %v (warns=%v)", !tc.wantWarn, tc.wantWarn, warns)
			}
		})
	}
	// No ceiling opinion (0): a POSITIVE proposal is left alone (nothing to
	// cap it against).
	got, warns := Clamp(types.RunPolicySpec{AutoStopAfterSec: 1800}, operatorCeiling(t))
	if got.AutoStopAfterSec != 1800 {
		t.Errorf("auto_stop_after_sec = %d, want 1800 preserved (no ceiling opinion)", got.AutoStopAfterSec)
	}
	if hasWarn(warns, "auto_stop_after_sec") {
		t.Errorf("unexpected auto_stop_after_sec warning with no ceiling opinion: %v", warns)
	}
	// W14-S1-3: -1 (never reap) is the single most permissive value there is —
	// a proposal may not opt OUT of the reaper just because the ceiling
	// itself opines nothing. Clamped to 0 (the platform default), not left
	// alone.
	got, warns = Clamp(types.RunPolicySpec{AutoStopAfterSec: -1}, operatorCeiling(t))
	if got.AutoStopAfterSec != 0 {
		t.Errorf("auto_stop_after_sec = %d, want 0 (never-reap must not survive an unopinionated ceiling)", got.AutoStopAfterSec)
	}
	if !hasWarn(warns, "auto_stop_after_sec") {
		t.Errorf("expected an auto_stop_after_sec warning clamping never-reap, got %v", warns)
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

func TestClamp_ToolRulesNeverWiden(t *testing.T) {
	ceiling := operatorCeiling(t)
	ceiling.ToolRules = []types.ToolRule{{Tool: "WebFetch", Effect: types.ToolDeny}}
	got, warns := Clamp(types.RunPolicySpec{ToolRules: []types.ToolRule{
		{Tool: "*", Effect: types.ToolAllow},       // widens the ceiling's implicit hold: raised
		{Tool: "Bash", Effect: types.ToolDeny},     // narrows: kept
		{Tool: "WebFetch", Effect: types.ToolHold}, // weaker than the ceiling's deny: raised
	}}, ceiling)
	want := map[string]types.ToolEffect{"*": types.ToolHold, "Bash": types.ToolDeny, "WebFetch": types.ToolDeny}
	if len(got.ToolRules) != len(want) {
		t.Fatalf("clamped rules = %v, want one per tool in %v", got.ToolRules, want)
	}
	for _, r := range got.ToolRules {
		if want[r.Tool] != r.Effect {
			t.Errorf("tool %q: effect %s, want %s", r.Tool, r.Effect, want[r.Tool])
		}
	}
	for _, w := range []string{`tool_rules: "*" raised from allow`, `tool_rules: "WebFetch" raised from hold`} {
		if !hasWarn(warns, w) {
			t.Errorf("expected warning %q, got %v", w, warns)
		}
	}
	if hasWarn(warns, `"Bash"`) {
		t.Errorf("a narrowing rule must not warn: %v", warns)
	}

	// A proposal with no rules inherits the ceiling's, so an operator deny
	// still applies to a member run that never mentioned the tool.
	got, warns = Clamp(types.RunPolicySpec{}, ceiling)
	if len(got.ToolRules) != 1 || got.ToolRules[0] != ceiling.ToolRules[0] {
		t.Errorf("silent proposal must carry the ceiling's rules, got %v", got.ToolRules)
	}
	if hasWarn(warns, "tool_rules") {
		t.Errorf("inheriting the ceiling is not a clamp: %v", warns)
	}

	// A ceiling that allows a tool lets the proposal allow it too.
	ceiling.ToolRules = []types.ToolRule{{Tool: "*", Effect: types.ToolAllow}}
	got, _ = Clamp(types.RunPolicySpec{ToolRules: []types.ToolRule{{Tool: "Read", Effect: types.ToolAllow}}}, ceiling)
	for _, r := range got.ToolRules {
		if r.Tool == "Read" && r.Effect != types.ToolAllow {
			t.Errorf("Read should stay allow under an allow-all ceiling, got %s", r.Effect)
		}
	}
}
