// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"encoding/json"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// find returns the first graded item for a field, or a zero item.
func find(items []RiskItem, field string) (RiskItem, bool) {
	for _, it := range items {
		if it.Field == field {
			return it, true
		}
	}
	return RiskItem{}, false
}

func TestGrade_ConfinementClass(t *testing.T) {
	cases := []struct {
		cc   types.ConfinementClass
		want RiskLevel
	}{
		{types.CC1, RiskHigh},
		{types.CC2, RiskMedium},
		{types.CC3, RiskLow},
	}
	for _, tc := range cases {
		t.Run(string(tc.cc), func(t *testing.T) {
			items := Grade(RunInput{}, types.RunPolicySpec{MinConfinementClass: tc.cc})
			it, ok := find(items, "min_confinement_class")
			if !ok {
				t.Fatalf("no min_confinement_class item")
			}
			if it.Level != tc.want {
				t.Errorf("CC %s graded %s, want %s", tc.cc, it.Level, tc.want)
			}
		})
	}
}

func TestGrade_AllowAllEgressIsHigh(t *testing.T) {
	items := Grade(RunInput{}, types.RunPolicySpec{AllowAllEgress: true, MinConfinementClass: types.CC2})
	it, ok := find(items, "allow_all_egress")
	if !ok || it.Level != RiskHigh {
		t.Fatalf("allow_all_egress should be HIGH, got %+v ok=%v", it, ok)
	}
}

func TestGrade_CustomEgressIsMedium_BaselineIsLow(t *testing.T) {
	// Beyond-baseline custom host → medium.
	items := Grade(RunInput{}, types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		AllowedDomains:      []string{"evil.example.com", "api.anthropic.com"},
	})
	it, ok := find(items, "allowed_domains")
	if !ok || it.Level != RiskMedium {
		t.Fatalf("custom egress should be MEDIUM, got %+v ok=%v", it, ok)
	}
	// Baseline-only allowlist → low.
	items = Grade(RunInput{}, types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		AllowedDomains:      []string{"api.anthropic.com", "github.com"},
	})
	it, _ = find(items, "allowed_domains")
	if it.Level != RiskLow {
		t.Errorf("baseline egress should be LOW, got %s", it.Level)
	}
}

func TestGrade_FirstUseApproval(t *testing.T) {
	base := types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: []string{"api.anthropic.com"}}
	base.FirstUseApproval = types.FirstUseDenyWithReview
	if it, _ := find(Grade(RunInput{}, base), "first_use_approval"); it.Level != RiskLow {
		t.Errorf("first_use_approval=true should be LOW, got %s", it.Level)
	}
	base.FirstUseApproval = types.FirstUseAlwaysDeny
	if it, _ := find(Grade(RunInput{}, base), "first_use_approval"); it.Level != RiskMedium {
		t.Errorf("first_use_approval=always_deny (non-trivial allowlist) should be MEDIUM, got %s", it.Level)
	}
	base.FirstUseApproval = types.FirstUseWaitForReview
	if it, _ := find(Grade(RunInput{}, base), "first_use_approval"); it.Level != RiskLow {
		t.Errorf("first_use_approval=wait_for_review should be LOW, got %s", it.Level)
	}
}

func ghScope(t *testing.T, perms map[string]string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(map[string]any{"repos": []string{"acme/widgets"}, "permissions": perms})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestGrade_GitHubWriteIsHigh_ReadIsMedium(t *testing.T) {
	write := types.RunPolicySpec{MinConfinementClass: types.CC2, EligibleGrants: []types.GrantSpec{
		{Kind: types.GrantGitHubToken, Scope: ghScope(t, map[string]string{"contents": "write"}), RequiresApproval: true},
	}}
	if it, ok := find(Grade(RunInput{}, write), "eligible_grants[0]"); !ok || it.Level != RiskHigh {
		t.Fatalf("github write grant should be HIGH, got %+v ok=%v", it, ok)
	}
	read := types.RunPolicySpec{MinConfinementClass: types.CC2, EligibleGrants: []types.GrantSpec{
		{Kind: types.GrantGitHubToken, Scope: ghScope(t, map[string]string{"contents": "read"}), RequiresApproval: true},
	}}
	if it, _ := find(Grade(RunInput{}, read), "eligible_grants[0]"); it.Level != RiskMedium {
		t.Errorf("github read grant should be MEDIUM, got %s", it.Level)
	}
}

func TestGrade_WriteGrantWithoutApprovalIsHigh(t *testing.T) {
	spec := types.RunPolicySpec{MinConfinementClass: types.CC2, EligibleGrants: []types.GrantSpec{
		{Kind: types.GrantGitHubToken, Scope: ghScope(t, map[string]string{"contents": "write"}), RequiresApproval: false},
	}}
	it, ok := find(Grade(RunInput{}, spec), "eligible_grants[0].requires_approval")
	if !ok || it.Level != RiskHigh {
		t.Fatalf("write grant w/ requires_approval=false should be HIGH, got %+v ok=%v", it, ok)
	}
}

func TestGrade_ApiKeyIsMedium_CloudStsIsHigh(t *testing.T) {
	api := types.RunPolicySpec{MinConfinementClass: types.CC2, EligibleGrants: []types.GrantSpec{
		{Kind: types.GrantAPIKey, RequiresApproval: true},
	}}
	if it, _ := find(Grade(RunInput{}, api), "eligible_grants[0]"); it.Level != RiskMedium {
		t.Errorf("api_key should be MEDIUM, got %s", it.Level)
	}
	sts := types.RunPolicySpec{MinConfinementClass: types.CC2, EligibleGrants: []types.GrantSpec{
		{Kind: types.GrantCloudSTS, RequiresApproval: true},
	}}
	if it, _ := find(Grade(RunInput{}, sts), "eligible_grants[0]"); it.Level != RiskHigh {
		t.Errorf("cloud_sts should be HIGH, got %s", it.Level)
	}
}

func TestGrade_GitPATAndSSHKeyAreHigh_ButNeverFloorConfinement(t *testing.T) {
	spec := types.RunPolicySpec{MinConfinementClass: types.CC1, EligibleGrants: []types.GrantSpec{
		{Kind: types.GrantGitPAT, RequiresApproval: true},
		{Kind: types.GrantSSHKey, RequiresApproval: true},
	}}
	if it, ok := find(Grade(RunInput{}, spec), "eligible_grants[0]"); !ok || it.Level != RiskHigh {
		t.Errorf("git_pat should be HIGH, got %+v ok=%v", it, ok)
	}
	if it, ok := find(Grade(RunInput{}, spec), "eligible_grants[1]"); !ok || it.Level != RiskHigh {
		t.Errorf("ssh_key should be HIGH, got %+v ok=%v", it, ok)
	}
	// Anti-brick invariant: the HIGH display grade must NOT imply a CC3 floor.
	// RequiredConfinementFloor is enforced fail-closed at compose AND run.create,
	// and an unconditional floor here would block every SCM clone on KVM-less
	// hosts (see the note on grantIsWriteCapable).
	if floor := RequiredConfinementFloor(spec); floor != "" {
		t.Fatalf("git_pat/ssh_key must not floor confinement, got %q", floor)
	}
}

func TestGrade_WorkspaceMountReadWriteIsHigh_ReadOnlyIsLow(t *testing.T) {
	rw := false
	ro := true
	specRW := types.RunPolicySpec{MinConfinementClass: types.CC2, WorkspaceMounts: []types.WorkspaceMount{
		{Source: "/host/repo", Target: "/work", ReadOnly: &rw},
	}}
	if it, ok := find(Grade(RunInput{}, specRW), "workspace_mounts[0]"); !ok || it.Level != RiskHigh {
		t.Fatalf("read-write mount should be HIGH, got %+v ok=%v", it, ok)
	}
	specRO := types.RunPolicySpec{MinConfinementClass: types.CC2, WorkspaceMounts: []types.WorkspaceMount{
		{Source: "/host/repo", Target: "/work", ReadOnly: &ro},
	}}
	if it, _ := find(Grade(RunInput{}, specRO), "workspace_mounts[0]"); it.Level != RiskLow {
		t.Errorf("read-only mount should be LOW, got %s", it.Level)
	}
}

func TestGrade_NeverReap_HighOnNonInteractive_LowOnInteractive(t *testing.T) {
	spec := types.RunPolicySpec{MinConfinementClass: types.CC2, AutoStopAfterSec: -1}
	if it, ok := find(Grade(RunInput{Interactive: false}, spec), "auto_stop_after_sec"); !ok || it.Level != RiskHigh {
		t.Fatalf("never-reap non-interactive should be HIGH, got %+v ok=%v", it, ok)
	}
	if it, _ := find(Grade(RunInput{Interactive: true}, spec), "auto_stop_after_sec"); it.Level != RiskLow {
		t.Errorf("never-reap interactive should be LOW, got %s", it.Level)
	}
}

// The headline security property: the grade is a pure function of the SPEC, so
// nothing in the (untrusted) request can change it. We grade the same spec and
// assert the same result regardless — there is no input channel into Grade other
// than the spec + run, by construction (Grade has no access to the prompt at all).
func TestGrade_IsDeterministicFunctionOfSpec(t *testing.T) {
	spec := types.RunPolicySpec{
		MinConfinementClass: types.CC1,
		AllowAllEgress:      true,
		EligibleGrants: []types.GrantSpec{
			{Kind: types.GrantGitHubToken, Scope: ghScope(t, map[string]string{"contents": "write"}), RequiresApproval: false},
		},
	}
	a := Grade(RunInput{}, spec)
	b := Grade(RunInput{}, spec)
	if OverallLevel(a) != RiskHigh {
		t.Fatalf("overall should be HIGH, got %s", OverallLevel(a))
	}
	if len(a) != len(b) {
		t.Fatalf("non-deterministic item count: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("item %d differs across calls: %+v vs %+v", i, a[i], b[i])
		}
	}
}

func TestGrade_SortedRiskiestFirst(t *testing.T) {
	spec := types.RunPolicySpec{
		MinConfinementClass: types.CC1,                     // high
		AllowedDomains:      []string{"api.anthropic.com"}, // low + first_use low/med
		FirstUseApproval:    types.FirstUseDenyWithReview,
	}
	items := Grade(RunInput{}, spec)
	for i := 1; i < len(items); i++ {
		if items[i-1].Level.rank() < items[i].Level.rank() {
			t.Fatalf("items not sorted riskiest-first at %d: %s before %s", i, items[i-1].Level, items[i].Level)
		}
	}
}

func TestOverallLevel_AllLow(t *testing.T) {
	spec := types.RunPolicySpec{MinConfinementClass: types.CC3} // all low
	if OverallLevel(Grade(RunInput{}, spec)) != RiskLow {
		t.Errorf("CC3-only spec should be overall LOW")
	}
}

// TestRequiredConfinementFloor is the deterministic blast-radius floor: a run
// holding powerful credentials (write-capable, or a third-party/production api_key)
// must be floored to Vault (CC3); benign runs (read-only creds, or the agent's own
// baseline LLM api_key) get no floor.
func TestRequiredConfinementFloor(t *testing.T) {
	apiKey := func(host string) types.GrantSpec {
		return types.GrantSpec{Kind: types.GrantAPIKey, Scope: json.RawMessage(`{"host":"` + host + `","header":"X-Api-Key"}`)}
	}
	ghWrite := types.GrantSpec{Kind: types.GrantGitHubToken, Scope: json.RawMessage(`{"repos":["o/r"],"permissions":{"contents":"write"}}`)}
	ghRead := types.GrantSpec{Kind: types.GrantGitHubToken, Scope: json.RawMessage(`{"repos":["o/r"],"permissions":{"contents":"read"}}`)}
	cloud := types.GrantSpec{Kind: types.GrantCloudSTS, Scope: json.RawMessage(`{"role":"arn:aws:iam::1:role/x"}`)}

	tests := []struct {
		name   string
		grants []types.GrantSpec
		want   types.ConfinementClass
	}{
		{"no grants", nil, ""},
		{"baseline LLM api_key only (benign)", []types.GrantSpec{apiKey("api.anthropic.com")}, ""},
		{"read-only github", []types.GrantSpec{ghRead}, ""},
		{"write-capable github -> Vault", []types.GrantSpec{ghWrite}, types.CC3},
		{"cloud STS -> Vault", []types.GrantSpec{cloud}, types.CC3},
		{"third-party/prod api_key (DB) -> Vault", []types.GrantSpec{apiKey("db.prod.internal")}, types.CC3},
		{"deploy api_key alongside benign LLM key -> Vault", []types.GrantSpec{apiKey("api.anthropic.com"), apiKey("deploy.acme.com")}, types.CC3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RequiredConfinementFloor(types.RunPolicySpec{EligibleGrants: tt.grants})
			if got != tt.want {
				t.Errorf("RequiredConfinementFloor = %q, want %q", got, tt.want)
			}
		})
	}
}
