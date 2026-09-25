// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestGrade_PushRulesUnenforceableWithSSHOnly pins issue #176's risk row:
// push_rules is read only on the brokered git lanes (github_token, git_pat) —
// the SSH transport has no broker seam — so a policy that sets it while
// ssh_key is the run's ONLY git-capable grant is legal (never a 422 here) but
// structurally unenforceable, and the operator must be told on the Review
// rail rather than blocked at write time.
func TestGrade_PushRulesUnenforceableWithSSHOnly(t *testing.T) {
	run := RunInput{Interactive: true}
	base := types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		AutoStopAfterSec:    3600,
		PushRules:           &types.PushRulesSpec{DenyPaths: []string{".github/workflows/**"}},
		EligibleGrants:      []types.GrantSpec{{Kind: types.GrantSSHKey, RequiresApproval: true}},
	}

	it := itemFor(Grade(run, base), "push_rules")
	if it == nil {
		t.Fatal("push_rules set with ssh_key-only is not graded at all")
	}
	if it.Level != RiskMedium {
		t.Errorf("level = %q, want %q", it.Level, RiskMedium)
	}
	if !strings.Contains(strings.ToLower(it.Rationale), "ssh_key") {
		t.Errorf("rationale %q does not say why it is unenforceable", it.Rationale)
	}

	// No push_rules at all: nothing to warn about, ssh_key or not.
	noRules := base
	noRules.PushRules = nil
	if it := itemFor(Grade(run, noRules), "push_rules"); it != nil {
		t.Errorf("graded %+v with push_rules unset: nothing to warn about", it)
	}

	// push_rules set, but a brokered grant (github_token) is ALSO eligible: the
	// broker CAN read the pack, so this is not the unenforceable case.
	withGitHub := base
	withGitHub.EligibleGrants = append([]types.GrantSpec{{Kind: types.GrantGitHubToken, RequiresApproval: true}}, base.EligibleGrants...)
	if it := itemFor(Grade(run, withGitHub), "push_rules"); it != nil {
		t.Errorf("graded %+v with a brokered git grant eligible too: push_rules IS enforceable here", it)
	}

	// push_rules set, no git grant of any kind eligible: not "ssh_key is the
	// reason" — a different, ungraded-here situation (see pushRulesUnenforceable's doc).
	noGitGrant := base
	noGitGrant.EligibleGrants = nil
	if it := itemFor(Grade(run, noGitGrant), "push_rules"); it != nil {
		t.Errorf("graded %+v with no git grant at all: ssh_key is not the reason", it)
	}

	// An all-zero-but-non-nil push_rules ("push_rules": {} — what
	// composer.Clamp can hand back from an empty operator ceiling, see
	// clampPushRules) carries no actual rule: PushRulesSpec.IsSet must keep this
	// from grading a warning about rules that do not exist.
	emptySpec := base
	emptySpec.PushRules = &types.PushRulesSpec{}
	if it := itemFor(Grade(run, emptySpec), "push_rules"); it != nil {
		t.Errorf("graded %+v with an all-zero push_rules: nothing to warn about", it)
	}
}

// TestGrade_PushRulesOnANonGitHubPATForge pins issue #508 F2: the broker can
// clear a path a push left unchanged only by reading github.com, so on any
// other git_pat forge a deny_paths entry reaching a path the repository holds
// refuses every push. Fail-closed and legal, so graded, not refused — but
// graded, because the refusal otherwise arrives silently at the first push.
func TestGrade_PushRulesOnANonGitHubPATForge(t *testing.T) {
	run := RunInput{Interactive: true}
	pat := func(host string) types.GrantSpec {
		return types.GrantSpec{Kind: types.GrantGitPAT, RequiresApproval: true,
			Scope: []byte(`{"host":"` + host + `","secret_name":"pat"}`)}
	}
	spec := func(rules *types.PushRulesSpec, grants ...types.GrantSpec) types.RunPolicySpec {
		return types.RunPolicySpec{MinConfinementClass: types.CC2, AutoStopAfterSec: 3600,
			PushRules: rules, EligibleGrants: grants}
	}
	deny := &types.PushRulesSpec{DenyPaths: []string{"infra/**"}}

	it := itemFor(Grade(run, spec(deny, pat("GitLab.Example.com"))), "push_rules")
	if it == nil {
		t.Fatal("deny_paths on a non-GitHub git_pat forge is not graded")
	}
	if it.Level != RiskMedium || it.Value != "deny_paths on gitlab.example.com" {
		t.Errorf("graded %+v, want medium on gitlab.example.com", it)
	}
	if !strings.Contains(it.Rationale, "gitlab.example.com") || !strings.Contains(it.Rationale, "refuse any push") {
		t.Errorf("rationale %q does not name the host and the refusal", it.Rationale)
	}

	for name, s := range map[string]types.RunPolicySpec{
		"git_pat on github.com":     spec(deny, pat("github.com")),
		"github_token":              spec(deny, types.GrantSpec{Kind: types.GrantGitHubToken, RequiresApproval: true}),
		"no push_rules":             spec(nil, pat("gitlab.example.com")),
		"max_inspect_pack_mib only": spec(&types.PushRulesSpec{MaxInspectPackMiB: 8}, pat("gitlab.example.com")),
		"all-zero push_rules":       spec(&types.PushRulesSpec{}, pat("gitlab.example.com")),
		"no git grant":              spec(deny),
		"unparseable git_pat scope": spec(deny, types.GrantSpec{Kind: types.GrantGitPAT, Scope: []byte(`nope`)}),
	} {
		if it := itemFor(Grade(run, s), "push_rules"); it != nil {
			t.Errorf("%s: graded %+v, want no push_rules row", name, it)
		}
	}

	// One row per distinct forge, whatever the spelling.
	var rows int
	for _, it := range Grade(run, spec(deny, pat("gitlab.example.com"), pat("GITLAB.example.com."), pat("dev.azure.com"), pat("github.com"))) {
		if it.Field == "push_rules" {
			rows++
		}
	}
	if rows != 2 {
		t.Errorf("push_rules rows = %d, want 2 (gitlab.example.com, dev.azure.com)", rows)
	}
}
