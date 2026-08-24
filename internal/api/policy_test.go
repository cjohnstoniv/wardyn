// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"fmt"
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
		// A three-segment repo is refused at WRITE time. It has to be: the broker
		// predicate (splitRepos) and the api-side git-broker allowlist builder
		// (githubScopeRepos) disagreed about it, so accepting it here shipped a
		// policy whose run was silently NOT brokered — no /wardyn/gh/ route and
		// no injected deny of the four GitHub hosts.
		{"github three-segment repo", grant(types.GrantGitHubToken, `{"repos":["acme/widget/extra"]}`), true},
		{"github mixed owners", grant(types.GrantGitHubToken, `{"repos":["acme/api","other/web"]}`), true},
		{"github scope not object", grant(types.GrantGitHubToken, `["contents"]`), true},
		// cloud_sts: empty object is the only shape; a non-object is rejected.
		{"cloudsts empty object", grant(types.GrantCloudSTS, `{}`), false},
		{"cloudsts null", grant(types.GrantCloudSTS, `null`), false},
		{"cloudsts non-object", grant(types.GrantCloudSTS, `"role-arn"`), true},
		// bug-policy-2: api_key must fail closed on an undecodable scope, the
		// SAME as git_pat/ssh_key just below — a missing host/secret_name or
		// non-JSON scope used to be silently ACCEPTED here (the check only
		// ran `if derr == nil`), so a malformed default-policy api_key grant
		// booted wardynd clean and only 422s at first run-creation.
		{"api_key valid", grant(types.GrantAPIKey, `{"host":"api.anthropic.com","secret_name":"anthropic-api-key"}`), false},
		{"api_key missing host", grant(types.GrantAPIKey, `{"secret_name":"anthropic-api-key"}`), true},
		{"api_key missing secret_name", grant(types.GrantAPIKey, `{"host":"api.anthropic.com"}`), true},
		{"api_key non-object", grant(types.GrantAPIKey, `"not-an-object"`), true},
		{"api_key null scope", grant(types.GrantAPIKey, `null`), true},
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
	base := func(li *types.LLMInspectionSpec, domains []string) types.RunPolicySpec {
		return types.RunPolicySpec{MinConfinementClass: types.CC2, LLMInspection: li, AllowedDomains: domains}
	}
	presidio := []string{"presidio"}
	cases := []struct {
		name    string
		li      *types.LLMInspectionSpec
		domains []string
		ok      bool
	}{
		{"nil is off (valid)", nil, nil, true},
		{"explicit off", &types.LLMInspectionSpec{Mode: "off"}, nil, true},
		{"alert + secrets", &types.LLMInspectionSpec{Mode: "alert", DetectSecrets: true}, nil, true},
		{"block + secrets + opts", &types.LLMInspectionSpec{Mode: "block", DetectSecrets: true, OnScannerError: "block", BlockMinSeverity: "high", MaxScanBytes: 4096}, nil, true},
		{"mode set but no detector", &types.LLMInspectionSpec{Mode: "alert"}, nil, false},
		{"unknown mode", &types.LLMInspectionSpec{Mode: "redact", DetectSecrets: true}, nil, false},
		{"secret patterns ok", &types.LLMInspectionSpec{Mode: "alert", DetectSecretPatterns: true}, nil, true},
		{"entropy ok", &types.LLMInspectionSpec{Mode: "alert", DetectEntropy: true}, nil, true},
		{"pii ok", &types.LLMInspectionSpec{Mode: "alert", DetectPII: true}, nil, true},
		{"sidecar url is a detector, host allowlisted", &types.LLMInspectionSpec{Mode: "alert", DetectorSidecarURL: "http://presidio:8080/scan"}, presidio, true},
		// W12-A-1 belt-and-braces: a well-formed http(s) sidecar URL is STILL
		// refused when its host is not in the SAME policy's own egress
		// allowlist — the operator must explicitly bless it, exactly like a
		// brokered api_key injection requires an exact allowlist entry
		// (domainAllowedExact). Before this, only the http(s) prefix was
		// checked, so an operator-authored (or, pre-clamp-fix, a
		// member-smuggled) ceiling could point the sidecar at an unlisted host.
		{"sidecar url host NOT in operator allowlist is rejected", &types.LLMInspectionSpec{Mode: "alert", DetectorSidecarURL: "http://presidio:8080/scan"}, nil, false},
		{"bad sidecar url (no scheme, rejected before the allowlist check)", &types.LLMInspectionSpec{Mode: "alert", DetectSecrets: true, DetectorSidecarURL: "presidio:8080"}, nil, false},
		{"negative max_scan_bytes", &types.LLMInspectionSpec{Mode: "alert", DetectSecrets: true, MaxScanBytes: -1}, nil, false},
		{"bad on_scanner_error", &types.LLMInspectionSpec{Mode: "alert", DetectSecrets: true, OnScannerError: "explode"}, nil, false},
		{"bad block_min_severity", &types.LLMInspectionSpec{Mode: "block", DetectSecrets: true, BlockMinSeverity: "ultra"}, nil, false},
		{"require_inspectable needs intercept_tls", &types.LLMInspectionSpec{Mode: "alert", DetectSecrets: true, RequireInspectableLLM: true}, nil, false},
		{"require_inspectable with intercept_tls ok", &types.LLMInspectionSpec{Mode: "alert", DetectSecrets: true, RequireInspectableLLM: true, InterceptTLS: true}, nil, true},
		{"intercept_tls alone ok", &types.LLMInspectionSpec{Mode: "alert", DetectSecrets: true, InterceptTLS: true}, nil, true},
		// W12-A-2/W12-S1-1: a raw VALUE can never be authored — only NAMES.
		{"workspace_secret_values rejected on write; author workspace_secret_names instead", &types.LLMInspectionSpec{Mode: "alert", DetectSecrets: true, WorkspaceSecretValues: []string{"super-secret-value"}}, nil, false},
		{"workspace_secret_names ok (the field an operator actually authors)", &types.LLMInspectionSpec{Mode: "alert", DetectSecrets: true, WorkspaceSecretNames: []string{"prod-db-password"}}, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePolicySpec(base(tc.li, tc.domains))
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

// TestValidatePolicySpec_UIApps covers the ui_apps write-time gate. The name is
// the sharp edge: it is interpolated into the launcher path the gateway execs
// inside the sandbox (/usr/local/bin/wardyn-ui-<name>) and into the enter URL's
// query, so a slash, a space, a shell metacharacter or ".." must be impossible
// HERE — every policy ingest funnels through validatePolicySpec, and nothing
// downstream sanitises the value again.
func TestValidatePolicySpec_UIApps(t *testing.T) {
	spec := func(apps ...types.UIApp) types.RunPolicySpec {
		return types.RunPolicySpec{MinConfinementClass: types.CC2, UIApps: apps}
	}
	cases := []struct {
		name string
		spec types.RunPolicySpec
		ok   bool
	}{
		{"no apps is the default", spec(), true},
		{"one app", spec(types.UIApp{Name: "code", Port: 8080}), true},
		{"path allowed", spec(types.UIApp{Name: "code", Port: 8080, Path: "/?folder=/work"}), true},
		{"two apps", spec(types.UIApp{Name: "code", Port: 8080}, types.UIApp{Name: "web", Port: 3000}), true},
		{"empty name", spec(types.UIApp{Port: 8080}), false},
		{"uppercase name", spec(types.UIApp{Name: "Code", Port: 8080}), false},
		{"path traversal in name", spec(types.UIApp{Name: "../../etc/x", Port: 8080}), false},
		{"slash in name", spec(types.UIApp{Name: "a/b", Port: 8080}), false},
		{"shell metacharacter in name", spec(types.UIApp{Name: "a;rm", Port: 8080}), false},
		{"space in name", spec(types.UIApp{Name: "a b", Port: 8080}), false},
		{"trailing dash", spec(types.UIApp{Name: "code-", Port: 8080}), false},
		{"duplicate name", spec(types.UIApp{Name: "code", Port: 8080}, types.UIApp{Name: "code", Port: 3000}), false},
		{"duplicate port", spec(types.UIApp{Name: "code", Port: 8080}, types.UIApp{Name: "web", Port: 8080}), false},
		{"zero port", spec(types.UIApp{Name: "code"}), false},
		{"port out of range", spec(types.UIApp{Name: "code", Port: 70000}), false},
		{"relative path", spec(types.UIApp{Name: "code", Port: 8080, Path: "x"}), false},
		{"protocol-relative path is an open redirect", spec(types.UIApp{Name: "code", Port: 8080, Path: "//evil.example.com/"}), false},
		{"absolute URL is not a path", spec(types.UIApp{Name: "code", Port: 8080, Path: "https://evil.example.com/"}), false},
		{"dot-dot path", spec(types.UIApp{Name: "code", Port: 8080, Path: "/../x"}), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePolicySpec(tc.spec)
			if tc.ok && err != nil {
				t.Fatalf("want accepted, got %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("want rejected, got nil error")
			}
		})
	}

	over := make([]types.UIApp, 0, maxUIAppsPerPolicy+1)
	for i := range maxUIAppsPerPolicy + 1 {
		over = append(over, types.UIApp{Name: fmt.Sprintf("app%d", i), Port: 8000 + i})
	}
	if err := validatePolicySpec(spec(over...)); err == nil {
		t.Fatalf("want %d apps rejected (max %d)", len(over), maxUIAppsPerPolicy)
	}
}

// TestUIApp_PathOrRoot pins the empty-path default the gateway lands on.
func TestUIApp_PathOrRoot(t *testing.T) {
	if got := (types.UIApp{}).PathOrRoot(); got != "/" {
		t.Fatalf("empty path -> %q, want /", got)
	}
	if got := (types.UIApp{Path: "/ide"}).PathOrRoot(); got != "/ide" {
		t.Fatalf("set path -> %q", got)
	}
}
