// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestValidatePolicySpec_GrantScopeShapes pins github_token and cloud_sts
// grant scopes are now validated at policy-write time (they previously received
// ONLY the kind-membership + non-negative-TTL check, unlike api_key/git_pat/
// ssh_key). Reverting the new branches makes every "want error" case accepted
// and fails here.
func TestValidatePolicySpec_GrantScopeShapes(t *testing.T) {
	grant := func(kind types.GrantKind, scope string) types.RunPolicySpec {
		return types.RunPolicySpec{
			MinConfinementClass: types.CC2,
			EligibleGrants:      []types.GrantSpec{{Kind: kind, Scope: json.RawMessage(scope)}},
		}
	}
	tests := []struct {
		name    string
		spec    types.RunPolicySpec
		wantErr bool
	}{
		// github_token: shipped templates carry empty repos + a valid perm map.
		{"github valid read", grant(types.GrantGitHubToken, `{"repos":[],"permissions":{"contents":"read"}}`), false},
		{"github valid write multi", grant(types.GrantGitHubToken, `{"repos":["acme/api","acme/web"],"permissions":{"contents":"write","pull_requests":"write"}}`), false},
		{"github no scope", grant(types.GrantGitHubToken, `null`), false},
		{"github unknown permission key", grant(types.GrantGitHubToken, `{"repos":[],"permissions":{"totally_bogus":"read"}}`), true},
		{"github malformed repo", grant(types.GrantGitHubToken, `{"repos":["not-owner-name"],"permissions":{"contents":"read"}}`), true},
		{"github mixed owners", grant(types.GrantGitHubToken, `{"repos":["acme/api","other/web"]}`), true},
		{"github scope not object", grant(types.GrantGitHubToken, `["contents"]`), true},
		// cloud_sts: empty object is the only shape; a non-object is rejected.
		{"cloudsts empty object", grant(types.GrantCloudSTS, `{}`), false},
		{"cloudsts null", grant(types.GrantCloudSTS, `null`), false},
		{"cloudsts non-object", grant(types.GrantCloudSTS, `"role-arn"`), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePolicySpec(tc.spec)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validatePolicySpec err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func TestConfinementGE(t *testing.T) {
	cases := []struct {
		have, want types.ConfinementClass
		ge         bool
	}{
		{types.CC2, types.CC1, true},
		{types.CC1, types.CC2, false},
		{types.CC3, types.CC3, true},
		{types.CC1, types.CC1, true},
		{"", types.CC1, false},                         // unknown ranks 0 (fail closed)
		{types.CC1, types.ConfinementClass("X"), true}, // unknown want ranks 0
	}
	for _, c := range cases {
		if got := confinementGE(c.have, c.want); got != c.ge {
			t.Errorf("confinementGE(%q,%q) = %v, want %v", c.have, c.want, got, c.ge)
		}
	}
}

func TestBestClass(t *testing.T) {
	if got := bestClass([]types.ConfinementClass{types.CC1, types.CC2}); got != types.CC2 {
		t.Errorf("bestClass = %q, want CC2", got)
	}
	if got := bestClass(nil); got != "" {
		t.Errorf("bestClass(nil) = %q, want empty", got)
	}
	if got := bestClass([]types.ConfinementClass{types.CC3, types.CC1}); got != types.CC3 {
		t.Errorf("bestClass strongest = %q, want CC3", got)
	}
}

func TestValidateLLMInspection(t *testing.T) {
	base := func(li *types.LLMInspectionSpec) types.RunPolicySpec {
		return types.RunPolicySpec{MinConfinementClass: types.CC2, LLMInspection: li}
	}
	cases := []struct {
		name string
		li   *types.LLMInspectionSpec
		ok   bool
	}{
		{"nil is off (valid)", nil, true},
		{"explicit off", &types.LLMInspectionSpec{Mode: "off"}, true},
		{"alert + secrets", &types.LLMInspectionSpec{Mode: "alert", DetectSecrets: true}, true},
		{"block + secrets + opts", &types.LLMInspectionSpec{Mode: "block", DetectSecrets: true, OnScannerError: "block", BlockMinSeverity: "high", MaxScanBytes: 4096}, true},
		{"mode set but no detector", &types.LLMInspectionSpec{Mode: "alert"}, false},
		{"unknown mode", &types.LLMInspectionSpec{Mode: "redact", DetectSecrets: true}, false},
		{"secret patterns ok", &types.LLMInspectionSpec{Mode: "alert", DetectSecretPatterns: true}, true},
		{"entropy ok", &types.LLMInspectionSpec{Mode: "alert", DetectEntropy: true}, true},
		{"pii ok", &types.LLMInspectionSpec{Mode: "alert", DetectPII: true}, true},
		{"sidecar url is a detector", &types.LLMInspectionSpec{Mode: "alert", DetectorSidecarURL: "http://presidio:8080/scan"}, true},
		{"bad sidecar url", &types.LLMInspectionSpec{Mode: "alert", DetectSecrets: true, DetectorSidecarURL: "presidio:8080"}, false},
		{"negative max_scan_bytes", &types.LLMInspectionSpec{Mode: "alert", DetectSecrets: true, MaxScanBytes: -1}, false},
		{"bad on_scanner_error", &types.LLMInspectionSpec{Mode: "alert", DetectSecrets: true, OnScannerError: "explode"}, false},
		{"bad block_min_severity", &types.LLMInspectionSpec{Mode: "block", DetectSecrets: true, BlockMinSeverity: "ultra"}, false},
		{"require_inspectable needs intercept_tls", &types.LLMInspectionSpec{Mode: "alert", DetectSecrets: true, RequireInspectableLLM: true}, false},
		{"require_inspectable with intercept_tls ok", &types.LLMInspectionSpec{Mode: "alert", DetectSecrets: true, RequireInspectableLLM: true, InterceptTLS: true}, true},
		{"intercept_tls alone ok", &types.LLMInspectionSpec{Mode: "alert", DetectSecrets: true, InterceptTLS: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePolicySpec(base(tc.li))
			if tc.ok && err != nil {
				t.Fatalf("expected valid, got %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("expected an error, got nil")
			}
		})
	}
}

func TestValidatePolicySpec(t *testing.T) {
	good := types.RunPolicySpec{MinConfinementClass: types.CC2}
	if err := validatePolicySpec(good); err != nil {
		t.Errorf("good policy rejected: %v", err)
	}
	if err := validatePolicySpec(types.RunPolicySpec{}); err == nil {
		t.Error("empty min_confinement_class accepted")
	}
	if err := validatePolicySpec(types.RunPolicySpec{MinConfinementClass: "CC9"}); err == nil {
		t.Error("unknown confinement class accepted")
	}
	bad := types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		EligibleGrants:      []types.GrantSpec{{Kind: "weird"}},
	}
	if err := validatePolicySpec(bad); err == nil {
		t.Error("unknown grant kind accepted")
	}

	// An empty allowed_domains WITHOUT allow-all is a valid deny-all policy:
	// validatePolicySpec must NOT newly require domains.
	denyAll := types.RunPolicySpec{MinConfinementClass: types.CC2}
	if err := validatePolicySpec(denyAll); err != nil {
		t.Errorf("empty-allowlist deny-all policy rejected: %v", err)
	}

	// Allow-all (deny-list only) mode must be accepted, including with an EMPTY
	// allowed_domains. denied_domains and first_use_approval may coexist
	// (first_use_approval is inert under allow-all; allow-all wins).
	allowAll := types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		AllowAllEgress:      true,
		DeniedDomains:       []string{"blocked.example.com"},
		FirstUseApproval:    types.FirstUseDenyWithReview,
	}
	if err := validatePolicySpec(allowAll); err != nil {
		t.Errorf("allow_all_egress policy rejected: %v", err)
	}
}

// TestValidatePolicySpec_WorkspaceTargets covers B3's structural checks added
// to validatePolicySpec: a WorkspaceRepo's (optional) Target must pass
// runner.ValidateTarget (the extracted target-prefix half of ValidateMount),
// and every WorkspaceMount + WorkspaceRepo target together must be unique — a
// clone must never land on a bind-mount target, or vice versa.
func TestValidatePolicySpec_WorkspaceTargets(t *testing.T) {
	base := func() types.RunPolicySpec {
		return types.RunPolicySpec{MinConfinementClass: types.CC2}
	}
	cases := []struct {
		name string
		spec types.RunPolicySpec
		ok   bool
	}{
		{
			"distinct mount targets ok",
			func() types.RunPolicySpec {
				s := base()
				s.WorkspaceMounts = []types.WorkspaceMount{
					{Source: "/home/u/a", Target: "/work/a"},
					{Source: "/home/u/b", Target: "/work/b"},
				}
				return s
			}(),
			true,
		},
		{
			"duplicate mount targets rejected",
			func() types.RunPolicySpec {
				s := base()
				s.WorkspaceMounts = []types.WorkspaceMount{
					{Source: "/home/u/a", Target: "/work/dup"},
					{Source: "/home/u/b", Target: "/work/dup"},
				}
				return s
			}(),
			false,
		},
		{
			"repo with valid distinct target ok",
			func() types.RunPolicySpec {
				s := base()
				s.WorkspaceMounts = []types.WorkspaceMount{{Source: "/home/u/a", Target: "/work/a"}}
				s.WorkspaceRepos = []types.WorkspaceRepo{{Repo: "org/repo", Target: "/work/repo"}}
				return s
			}(),
			true,
		},
		{
			"repo target colliding with a mount target rejected",
			func() types.RunPolicySpec {
				s := base()
				s.WorkspaceMounts = []types.WorkspaceMount{{Source: "/home/u/a", Target: "/work/shared"}}
				s.WorkspaceRepos = []types.WorkspaceRepo{{Repo: "org/repo", Target: "/work/shared"}}
				return s
			}(),
			false,
		},
		{
			"two repo targets colliding rejected",
			func() types.RunPolicySpec {
				s := base()
				s.WorkspaceRepos = []types.WorkspaceRepo{
					{Repo: "org/one", Target: "/work/dup"},
					{Repo: "org/two", Target: "/work/dup"},
				}
				return s
			}(),
			false,
		},
		{
			"repo target outside allowed prefix rejected",
			func() types.RunPolicySpec {
				s := base()
				s.WorkspaceRepos = []types.WorkspaceRepo{{Repo: "org/repo", Target: "/etc/repo"}}
				return s
			}(),
			false,
		},
		{
			"repo with no target never collides (default is a later wave's concern)",
			func() types.RunPolicySpec {
				s := base()
				s.WorkspaceMounts = []types.WorkspaceMount{{Source: "/home/u/a", Target: "/work/a"}}
				s.WorkspaceRepos = []types.WorkspaceRepo{
					{Repo: "org/one"},
					{Repo: "org/two"},
				}
				return s
			}(),
			true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validatePolicySpec(c.spec)
			if c.ok && err != nil {
				t.Fatalf("expected valid, got %v", err)
			}
			if !c.ok && err == nil {
				t.Fatal("expected an error, got nil")
			}
		})
	}
}

func TestLoadPolicySpecRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.json")
	content := `{
		"allowed_domains": ["api.anthropic.com"],
		"first_use_approval": true,
		"min_confinement_class": "CC2",
		"eligible_grants": [
			{"kind":"github_token","scope":{"repos":[],"permissions":{"contents":"read"}},"ttl_seconds":3600,"requires_approval":true}
		]
	}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	spec, err := LoadPolicySpec(path)
	if err != nil {
		t.Fatalf("LoadPolicySpec: %v", err)
	}
	if spec.MinConfinementClass != types.CC2 {
		t.Errorf("min_confinement_class = %q", spec.MinConfinementClass)
	}
	if !spec.FirstUseApproval.RaisesApproval() {
		t.Error("first_use_approval lost")
	}
	if len(spec.EligibleGrants) != 1 || spec.EligibleGrants[0].Kind != types.GrantGitHubToken {
		t.Errorf("eligible grants = %+v", spec.EligibleGrants)
	}
}

func TestLoadPolicySpecRejectsUnknownField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.json")
	// DisallowUnknownFields must reject typos that would silently widen behavior.
	content := `{"min_confinement_class":"CC2","allowd_domains":["x"]}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPolicySpec(path); err == nil {
		t.Error("unknown field accepted; want rejection (fail closed)")
	}
}

func boolPtr(b bool) *bool { return &b }

// TestValidatePolicySpec_WorkspaceMounts asserts the policy-write-time half of
// the bind-mount guardrail: a policy with a dangerous WorkspaceMount.Source (or
// bad Target) is rejected (so the create-policy endpoint 400s), while an allowed
// mount validates. This mirrors the docker driver's defense-in-depth re-check.
func TestValidatePolicySpec_WorkspaceMounts(t *testing.T) {
	base := func(wm types.WorkspaceMount) types.RunPolicySpec {
		return types.RunPolicySpec{
			MinConfinementClass: types.CC2,
			WorkspaceMounts:     []types.WorkspaceMount{wm},
		}
	}

	// Allowed: absolute, non-dangerous source; allowed target prefix.
	if err := validatePolicySpec(base(types.WorkspaceMount{
		Source: "/home/maintainer/repo", Target: "/home/agent/work", ReadOnly: boolPtr(false),
	})); err != nil {
		t.Errorf("allowed workspace mount rejected: %v", err)
	}
	// Allowed: omitted read_only (defaults read-only) still validates.
	if err := validatePolicySpec(base(types.WorkspaceMount{
		Source: "/srv/data", Target: "/work/data",
	})); err != nil {
		t.Errorf("default-RO workspace mount rejected: %v", err)
	}

	denied := []types.WorkspaceMount{
		{Source: "/", Target: "/home/agent/x"},
		{Source: "/var/run/docker.sock", Target: "/work/x"},
		{Source: "/etc", Target: "/work/x"},
		{Source: "/proc", Target: "/home/agent/x"},
		{Source: "/root/.ssh", Target: "/work/x"},
		{Source: "relative", Target: "/work/x"},
		{Source: "/home/u/repo", Target: "/etc"}, // bad target prefix
	}
	for _, wm := range denied {
		if err := validatePolicySpec(base(wm)); err == nil {
			t.Errorf("dangerous workspace mount %+v accepted; want rejection (fail closed)", wm)
		}
	}
}

// TestWorkspaceMount_ReadOnlyDefault asserts the safe default: an omitted
// read_only resolves to read-only; an explicit false yields read-write.
func TestWorkspaceMount_ReadOnlyDefault(t *testing.T) {
	if !(types.WorkspaceMount{}).ReadOnlyOrDefault() {
		t.Error("omitted read_only must default to read-only (true)")
	}
	if !(types.WorkspaceMount{ReadOnly: boolPtr(true)}).ReadOnlyOrDefault() {
		t.Error("explicit read_only=true must be read-only")
	}
	if (types.WorkspaceMount{ReadOnly: boolPtr(false)}).ReadOnlyOrDefault() {
		t.Error("explicit read_only=false must be read-write")
	}
}

func TestLoadPolicySpecDefaultExample(t *testing.T) {
	// The shipped default policy must validate.
	path := filepath.Join("..", "..", "examples", "policies", "default.json")
	if _, err := os.Stat(path); err != nil {
		t.Skip("example policy not present")
	}
	if _, err := LoadPolicySpec(path); err != nil {
		t.Errorf("shipped default policy invalid: %v", err)
	}
}
