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
	// clampPushRules) carries no actual rule: pushRulesIsSet must keep this
	// from grading a warning about rules that do not exist.
	emptySpec := base
	emptySpec.PushRules = &types.PushRulesSpec{}
	if it := itemFor(Grade(run, emptySpec), "push_rules"); it != nil {
		t.Errorf("graded %+v with an all-zero push_rules: nothing to warn about", it)
	}
}
