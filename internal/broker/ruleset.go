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
// ruleset for a governed run to keep pushing, and the one every message here
// names. The run branch is `wardyn/<run-id>/work` (name_run_branch in
// deploy/images/common/agent-run-lib.sh) and the proxy admits anything under
// refs/heads/wardyn/<run-id>/ (proxy.BranchNSPrefix), so the exclude has to
// reach at least two segments deep, and any depth is safer.
//
// The trailing "/*" is load-bearing, not decoration: GitHub matches ref_name
// with fnmatch semantics where "*" does not cross "/", and a TRAILING "**"
// behaves the same as "*" — only "**/" is recursive. "refs/heads/wardyn/**"
// would therefore exclude nothing a run actually pushes, and the ruleset would
// refuse every governed push at GitHub itself.
//
// MEASURED against GitHub's live evaluator (read-only, through the same
// rules/branches endpoint this file reads, on public repos that already carry
// such rulesets — creating one to test needs repo admin, which this project
// does not have anywhere it may mutate):
//
//	nodejs/node #714753, include refs/heads/**
//	  zzz -> "creation" in force; zzz/foo, zzz/foo/bar -> nothing.
//	angular/angular #9964176, include refs/heads/dependabot/**/*
//	  dependabot -> nothing; dependabot/foo, dependabot/a/b/c,
//	  dependabot/a/b/c/d/e -> creation+update+non_fast_forward in force.
//	open-telemetry/opentelemetry-collector #10457348, exclude refs/heads/copilot/**/*
//	  copilot -> rule in force (NOT excluded); copilot/foo, copilot/a/b/c -> excluded.
//
// GitHub's docs say the same: "the * wildcard does not match directory
// separators (/)" and "you can include any number of slashes after qa with
// qa/**/*".
const refNamespaceGlob = "refs/heads/wardyn/**/*"

// Ref-confinement probes for VerifyRefRuleset.
//
// GitHub's "get rules for a branch" endpoint answers for a branch NAME whether
// or not that branch exists (verified against api.github.com: an invented name
// returns 200 with an empty array), so both probes are pure reads that create
// nothing and need no knowledge of the repo's default branch.
//
// refProbeOutside is deliberately synthetic. Probing a REAL branch name such as
// "main" would grade a ruleset that names only "main" as confining while
// "master", "release/*" and every other ref stayed open to the token. A name
// nobody would ever enumerate returns rules only when the ruleset's
// ref_name.include really is broad (~ALL or a wildcard) — which is the property
// being verified. refProbeInside sits inside the run namespace, and is two
// segments deep like a real run branch, so it answers the other half: that the
// operator's ref_name.exclude actually took at the depth runs push to, and the
// ruleset does not also block the run's own pushes.
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
// This is the one mechanism that bounds the TOKEN rather than the route. The
// installation token cannot self-restrict to a ref prefix — InstallationTokenOptions
// carries repository names, repository ids and a permission map, and no ref or
// branch field, because GitHub's API accepts none — so Wardyn's own
// branch-namespace enforcement (the receive-pack pkt-line parser on the proxy's
// git-broker route) is the whole of the confinement until a repository ruleset
// exists. A ruleset survives the token escaping the proxy; the parser does not.
//
// Three kinds of read decide it:
//   - OUTSIDE the namespace, GET /repos/{o}/{r}/rules/branches/{branch} must
//     report "creation", "update" AND "deletion" in force (GitHub's own wording:
//     "Only allow users with bypass permission to create/update/delete matching
//     refs"). deletion is required because it is a write: without it a leaked
//     token still runs `git push --delete origin main`.
//   - For every ruleset backing those rules — the rules response carries
//     ruleset_id on each rule — GET /repos/{o}/{r}/rulesets/{id} must report
//     current_user_can_bypass == "never". Any other value ("always",
//     "pull_requests_only", "exempt") means the caller making the request is
//     exempt from the very rule the confinement rests on, which is worse than no
//     rule, because it looks like confinement.
//   - INSIDE the namespace neither "creation" nor "update" may be in force, or
//     the ruleset would refuse the very pushes a run makes.
//
// LIMITS, stated because the caller grades security on this answer:
//   - The bypass read has been measured only with a USER token: on 11 rulesets
//     across 7 public repos a plain non-admin OAuth token gets
//     current_user_can_bypass "never" while bypass_actors is omitted (GitHub
//     returns bypass_actors only to a caller with write access to the ruleset,
//     and documents that). Whether GitHub computes the field meaningfully for a
//     GitHub App INSTALLATION token is NOT known here. An UNAUTHENTICATED read
//     omits the field entirely (measured), which is why an absent field is an
//     error — unknown — and never a pass.
//   - PERMISSIONS are documented, not measured. GitHub's App-permissions
//     reference lists both reads under Metadata: read — "GET
//     /repos/{owner}/{repo}/rules/branches/{branch}" and "GET
//     /repos/{owner}/{repo}/rulesets/{ruleset_id}" are both rows under
//     "Repository permissions for Metadata"; the Administration rows for that
//     second path are its PUT, DELETE and /history variants, not the plain GET.
//     Metadata: read is the one permission every App holds mandatorily, so
//     neither read should cost a new grant. This project has NOT confirmed that
//     against a live installation, which is why every failure path here is an
//     error the caller must render as "unknown", never as "unconfined".
//   - BRANCHES only. The ruleset target enum is branch|tag|push, so the branch
//     ruleset this verifies leaves refs/tags/* open: a leaked contents:write
//     token can still create, move or delete any tag. Wardyn's own receive-pack
//     parser refuses tags, but this ruleset exists precisely for when that
//     parser is bypassed, so the hole is real and the "confined" detail says so.
//   - RULESETS only. Classic branch protection also bounds a token, but it does
//     not surface in this endpoint — measured: repos with heavily protected
//     default branches answer with an empty list. A repo protected that way is
//     genuinely protected and still reports unconfined here.
//   - Rules from rulesets with "disabled" or "evaluate" enforcement are not
//     returned, so a returned rule is an active one — measured: facebook/react
//     #6692635 (enforcement "evaluate", conditions include ~ALL) contributes
//     nothing to a rules/branches read whose branch its conditions match.
func (m *githubMinter) VerifyRefRuleset(ctx context.Context, repo string) (bool, string, error) {
	owner, names, err := splitRepos([]string{repo})
	if err != nil {
		return false, "", err
	}
	name := names[0]
	// metadata:read is the least both reads are documented to need, and it is the
	// one permission every App holds mandatorily — so this token asks for nothing
	// the App was not already granted, and is narrower than the contents:write
	// the run's own token carries. It never leaves this process.
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
	// The probe token is a real, live ghs_… — GitHub gives installation tokens
	// ~1h whatever TTL is asked for — and it is minted OUTSIDE Broker.mint, so it
	// reaches no audit trail. Hand it back as soon as the reads are done instead
	// of leaving an unaudited credential alive for the hour. Best effort by
	// design: a failed revoke must not turn a good verdict into "unknown", and
	// WithoutCancel keeps the revoke working when the caller's ctx has already
	// expired (the timeout path, which is exactly when a live token would linger).
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

// refRuleState is what one rules/branches read tells the verifier: which of the
// three rule types that bound a WRITE are in force for the branch, and which
// rulesets put them there. GitHub stamps ruleset_id on every rule it returns, so
// the join to the bypass read costs no extra call to find.
type refRuleState struct {
	creation, update, deletion bool
	rulesets                   []int64
}

// readRefRules reads the rules in force for one branch name.
func readRefRules(ctx context.Context, c *gh.Client, owner, repo, branch string) (refRuleState, error) {
	// The branch segment may contain slashes (refProbeInside does). GitHub reads
	// the whole remainder of the path as the branch name — verified: "main/extra"
	// returns [] on a repo whose ruleset targets "main" — so it needs no escaping.
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
	// the bypass read then 404s and the whole answer is UNKNOWN. Skipping it
	// would grade "confined" on a rule whose bypass nobody checked.
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
//
// Fails CLOSED on both unknowns: a read error and an absent field are errors,
// never an empty (= "all good") result. includesParents must be true —
// measured: with includes_parents=false the endpoint 404s for a ruleset
// inherited from the ORGANIZATION, and org-level rulesets are exactly the ones
// an operator is most likely to be relying on.
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

// missingRules names the absent restricting rule types, or "" when all three are
// in force. deletion counts: it is a write, and the recipe in docs/POLICIES.md
// creates it, so requiring it here keeps check and recipe describing one thing.
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
