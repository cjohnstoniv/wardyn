// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v88/github"
)

// refNamespaceGlob is the ref_name pattern an operator must EXCLUDE from the
// ruleset for a governed run to keep pushing. Run branches are
// refs/heads/wardyn/<run-id>/... (proxy.BranchNSPrefix), so the exclude must
// reach at least two segments deep.
//
// The trailing "/*" is load-bearing: GitHub matches ref_name with fnmatch
// semantics where "*" never crosses "/", and a trailing "**" behaves the
// same as "*" — only "**/" is recursive. "refs/heads/wardyn/**" would
// therefore exclude nothing a run actually pushes, and the ruleset would
// refuse every governed push at GitHub itself (measured against GitHub's
// live evaluator on public repos already carrying such rulesets).
const refNamespaceGlob = "refs/heads/wardyn/**/*"

// Ref-confinement probes for VerifyRefRuleset. GitHub's "get rules for a
// branch" endpoint answers for a branch NAME whether or not it exists (an
// invented name returns 200 with an empty array), so both probes are pure
// reads that create nothing.
//
// refProbeOutside is deliberately synthetic: a name nobody would enumerate
// returns rules only when ref_name.include really is broad, which is the
// property being verified. refProbeInside sits two segments deep inside the
// run namespace, checking that ref_name.exclude actually took at the depth
// runs push to.
const (
	refProbeOutside = "probe-wardyn-ref-confinement"
	refProbeInside  = "wardyn/00000000-0000-0000-0000-000000000000/probe"
)

// refRevokeTimeout bounds the best-effort hand-back of the probe token. Short:
// nothing waits on it and its failure changes no verdict.
const refRevokeTimeout = 3 * time.Second

// VerifyRefRuleset reports whether GITHUB ITSELF confines this App's writes on
// repo ("owner/name") to the run branch namespace, and returns an
// operator-readable detail either way.
//
// This is the one mechanism that bounds the TOKEN rather than the route: an
// installation token cannot self-restrict to a ref prefix, so Wardyn's own
// branch-namespace enforcement (the receive-pack parser on the proxy's
// git-broker route) is the whole of the confinement until a repository
// ruleset exists. A ruleset survives the token escaping the proxy; the
// parser does not.
//
// Three kinds of read decide it:
//   - OUTSIDE the namespace, "creation", "update" AND "deletion" must be in
//     force (deletion counts because a leaked token could otherwise still
//     `git push --delete origin main`).
//   - Every ruleset backing those rules must report current_user_can_bypass
//     == "never"; any other value means the caller is exempt from the very
//     rule the confinement rests on, which looks like confinement but isn't.
//   - INSIDE the namespace neither "creation" nor "update" may be in force,
//     or the ruleset would refuse the run's own pushes.
//
// Limits, stated because the caller grades security on this answer: the
// bypass field's behavior for a GitHub App installation token (vs. the user
// token this was measured against) is unconfirmed, so an absent field or any
// read error is graded "unknown", never "unconfined". This covers BRANCHES
// via RULESETS only — refs/tags/* stays open to a leaked token, and classic
// branch protection doesn't surface in this endpoint, so a repo protected
// that way still reports unconfined here. Rules from disabled/evaluate
// rulesets are not returned, so a returned rule is always an active one.
func (m *githubMinter) VerifyRefRuleset(ctx context.Context, repo string) (bool, string, error) {
	owner, names, err := splitRepos([]string{repo})
	if err != nil {
		return false, "", err
	}
	name := names[0]
	// metadata:read is the least both reads need and every App holds it
	// mandatorily, so this asks for nothing new and never leaves this process.
	tok, _, err := m.MintInstallationToken(ctx, []string{repo}, map[string]string{"metadata": "read"}, defaultMaxTTL)
	if err != nil {
		return false, "", fmt.Errorf("broker: mint metadata token for ruleset read: %w", err)
	}
	opts := []gh.ClientOptionsFunc{gh.WithAuthToken(tok)}
	if m.baseURL != "" {
		opts = append(opts, gh.WithURLs(&m.baseURL, nil))
	}
	c, err := gh.NewClient(opts...)
	if err != nil {
		return false, "", fmt.Errorf("broker: build github client: %w", err)
	}
	// The probe token is minted outside Broker.mint (no audit trail) and lives
	// ~1h regardless of TTL, so hand it back once reads are done. Best effort:
	// a failed revoke must not turn a good verdict into "unknown", and
	// WithoutCancel keeps the revoke working after the caller's ctx expires.
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refRevokeTimeout)
		defer cancel()
		_, _ = c.Apps.RevokeInstallationToken(rctx)
	}()

	outside, err := readRefRules(ctx, c, owner, name, refProbeOutside)
	if err != nil {
		return false, "", err
	}
	if missing := missingRules(outside); missing != "" {
		return false, fmt.Sprintf(
			"%s: GitHub reports no %s rule in force outside %s, so a minted token that escapes the proxy can still push there.",
			repo, missing, refNamespaceGlob), nil
	}
	if bypassers, err := bypassableRulesets(ctx, c, owner, name, outside.rulesets); err != nil {
		return false, "", err
	} else if len(bypassers) > 0 {
		return false, fmt.Sprintf(
			"%s: ruleset %s reports current_user_can_bypass other than \"never\", so the caller of this read is not held to the rules the confinement rests on — the rule list is there but does not bind.",
			repo, strings.Join(bypassers, ", ")), nil
	}

	inside, err := readRefRules(ctx, c, owner, name, refProbeInside)
	if err != nil {
		return false, "", err
	}
	if inside.creation || inside.update {
		return false, fmt.Sprintf(
			"%s: the ruleset is also in force inside the run namespace (probed refs/heads/%s), so it would refuse the run's own pushes. Add %s to the ruleset's ref_name.exclude.",
			repo, refProbeInside, refNamespaceGlob), nil
	}
	return true, fmt.Sprintf(
		"%s: creation, update and deletion are restricted outside %s, nothing is in force at refs/heads/%s inside it, and every ruleset behind those rules reports current_user_can_bypass \"never\". Branches only — a branch ruleset does not bound refs/tags/*, so a leaked token can still create, move or delete tags.",
		repo, refNamespaceGlob, refProbeInside), nil
}

// refRuleState is what one rules/branches read tells the verifier: which of
// the three rule types that bound a WRITE are in force, and which rulesets
// put them there (GitHub stamps ruleset_id on every rule it returns).
type refRuleState struct {
	creation, update, deletion bool
	rulesets                   []int64
}

// readRefRules reads the rules in force for one branch name.
func readRefRules(ctx context.Context, c *gh.Client, owner, repo, branch string) (refRuleState, error) {
	// The branch segment may contain slashes (refProbeInside does); GitHub
	// reads the whole remainder of the path as the branch name, so it needs
	// no escaping.
	rules, _, err := c.Repositories.ListRulesForBranch(ctx, owner, repo, branch, nil)
	if err != nil {
		return refRuleState{}, fmt.Errorf("broker: read branch rules for %s/%s@%s: %w", owner, repo, branch, err)
	}
	if rules == nil {
		return refRuleState{}, nil
	}
	st := refRuleState{
		creation: len(rules.Creation) > 0,
		update:   len(rules.Update) > 0,
		deletion: len(rules.Deletion) > 0,
	}
	// A rule whose ruleset_id is absent decodes as 0 and is kept, not skipped:
	// skipping it would grade "confined" on a rule whose bypass nobody checked.
	seen := make(map[int64]bool)
	add := func(id int64) {
		if !seen[id] {
			seen[id] = true
			st.rulesets = append(st.rulesets, id)
		}
	}
	for _, r := range rules.Creation {
		add(r.GetRulesetID())
	}
	for _, r := range rules.Update {
		add(r.GetRulesetID())
	}
	for _, r := range rules.Deletion {
		add(r.GetRulesetID())
	}
	return st, nil
}

// bypassableRulesets names the rulesets among ids whose current_user_can_bypass
// is anything other than "never", i.e. the ones this caller is exempt from.
// Fails CLOSED: a read error and an absent field are errors, never an empty
// (= "all good") result. includesParents must be true, since org-level
// rulesets — the ones an operator most likely relies on — 404 without it.
func bypassableRulesets(ctx context.Context, c *gh.Client, owner, repo string, ids []int64) ([]string, error) {
	var bypassers []string
	for _, id := range ids {
		rs, _, err := c.Repositories.GetRuleset(ctx, owner, repo, id, true)
		if err != nil {
			return nil, fmt.Errorf("broker: read ruleset %d on %s/%s for its bypass mode: %w", id, owner, repo, err)
		}
		mode := rs.GetCurrentUserCanBypass()
		if mode == nil {
			return nil, fmt.Errorf("broker: ruleset %d on %s/%s returned no current_user_can_bypass, so this App's bypass is unverifiable", id, owner, repo)
		}
		if *mode != gh.BypassModeNever {
			bypassers = append(bypassers, fmt.Sprintf("%d (%s)", id, *mode))
		}
	}
	return bypassers, nil
}

// missingRules names the absent restricting rule types, or "" when all three
// are in force. deletion counts because it is a write.
func missingRules(st refRuleState) string {
	var missing []string
	if !st.creation {
		missing = append(missing, "creation")
	}
	if !st.update {
		missing = append(missing, "update")
	}
	if !st.deletion {
		missing = append(missing, "deletion")
	}
	return strings.Join(missing, "/")
}
