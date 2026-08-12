// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/setup"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// setup_checks.go holds the /setup/status checklist rows extracted from
// handleSetupStatus — one small pure function per row, in the order the wizard
// renders them. "info" is used for permanent / non-fixable or purely-optional
// conditions so the operator is never shown a red they cannot clear. (The
// remaining rows still live beside their state in setup.go.) firstBrokeredRepoFromRuns
// below is the one exception to "pure": it needs the store to confirm a run was
// actually brokered, not just guess from a Repo string.

// runnerCheck grades the sandbox runner: no runner (or no live class) is the one
// FAIL on the checklist — runs cannot launch at all. CC2+ is ok; a CC1-only host
// is "info", not a warning: runs work, just at the weakest isolation.
func runnerCheck(rnr SetupRunner) SetupCheck {
	if rnr.Driver == "none" || len(rnr.ConfinementClasses) == 0 {
		return SetupCheck{
			ID: "runner", Label: "Sandbox runner", Status: "fail",
			Detail: "No sandbox runner is configured, so runs cannot launch.",
			Fix:    "Start wardynd with -runner docker (built with -tags docker) so runs are confined and executed.",
		}
	}
	labeled := make([]string, len(rnr.ConfinementClasses))
	hasCC2Plus := false
	for i, c := range rnr.ConfinementClasses {
		labeled[i] = tierLabel(types.ConfinementClass(c))
		if c != "CC1" {
			hasCC2Plus = true
		}
	}
	if !hasCC2Plus {
		return SetupCheck{
			ID: "runner", Label: "Sandbox runner", Status: "info",
			Detail: "Only the Fence tier (weakest — a shared-kernel container) is available on this host; runs work but with the lowest isolation.",
			Fix:    "Unlock the Wall or Vault tier: run `wardyn setup wall` (or `wardyn setup vault`) on the host — it detects your OS/Docker setup and prints the exact steps.",
		}
	}
	return SetupCheck{
		ID: "runner", Label: "Sandbox runner", Status: "ok",
		Detail: "Runner live with the Wall tier or stronger (" + strings.Join(labeled, ", ") + ").",
	}
}

// envBuilderCheck reports whether the per-run sandbox image builder is wired — the
// path a devcontainer build or a --image (bring-your-own-image) run needs. It is
// OFF by default on the bare binary (WARDYN_ENVBUILD unset, or wardynd not built
// with -tags docker) and ON in the compose stack, with no other setup signal, so a
// devcontainer/--image run that silently no-ops otherwise reads as a random failure.
// INFO (never a warning) when off: it is optional — only devcontainer/BYOI runs need
// it; a convention-image run does not.
func envBuilderCheck(wired bool) SetupCheck {
	if wired {
		return SetupCheck{
			ID: "env_builder", Label: "Sandbox image builder", Status: "ok",
			Detail: "The per-run image builder is wired: devcontainer builds and --image (bring-your-own-image) wraps will fire.",
		}
	}
	return SetupCheck{
		ID: "env_builder", Label: "Sandbox image builder", Status: "info",
		Detail: "The per-run image builder is off (the default on the bare binary; the compose stack enables it): a --image " +
			"(bring-your-own-image) run is rejected, and a devcontainer_repo run silently falls back to the convention image instead of building.",
		Fix: "Set WARDYN_ENVBUILD=true on a wardynd built with -tags docker, or use the compose stack (it enables the builder).",
	}
}

// llmProviderCheck reports the WINNING model/harness signal (llmProvenance's
// detail, "" when there is none). INFO, never a warning, when there is none: a
// model provider is OPTIONAL — needed only for agent-harness runs or the AI Run
// Composer, so "no model" is a deliberate non-blocking state, never a gap the
// operator must clear.
func llmProviderCheck(llmDetail string) SetupCheck {
	if llmDetail != "" {
		return SetupCheck{ID: "llm_provider", Label: "LLM access", Status: "ok", Detail: llmDetail}
	}
	return SetupCheck{
		ID: "llm_provider", Label: "LLM access", Status: "info",
		Detail: "No model/harness provider configured (optional): needed only for agent-harness runs or the AI Run Composer. Bring-your-own-container and interactive runs work without one.",
		Fix:    "Optional — connect a Claude subscription/API key or Bedrock (the Integrations step, \"Connect what's outside Wardyn\"), or bind creds to a workspace/container.",
	}
}

// bedrockProviderCheck surfaces a row only once the operator has touched ANY
// Bedrock knob (ok=false otherwise), so the majority who never use AWS aren't
// shown an irrelevant row. warn = partially configured, a real gap worth fixing.
func bedrockProviderCheck(bedrock SetupBedrock) (SetupCheck, bool) {
	if !bedrock.configured() {
		return SetupCheck{}, false
	}
	if bedrock.ready() {
		return SetupCheck{
			ID: "bedrock_provider", Label: "AWS Bedrock", Status: "ok",
			Detail: fmt.Sprintf("Bedrock is configured (region %s, model %s) for Claude runs via %s.", bedrock.Region, bedrock.Model, bedrock.credSourceDesc()),
		}, true
	}
	var missing []string
	if bedrock.Region == "" {
		missing = append(missing, "-bedrock-region")
	}
	if bedrock.Model == "" {
		missing = append(missing, "-bedrock-model")
	}
	if !bedrock.CredsPresent && !bedrock.AWSMount && !bedrock.BearerPresent && !bedrock.SSOPresent {
		missing = append(missing, "a credential — a read-only ~/.aws mount (-bedrock-aws-dir), a bedrock-api-key bearer secret, a container AWS SSO login, or aws-access-key-id + aws-secret-access-key secrets")
	}
	return SetupCheck{
		ID: "bedrock_provider", Label: "AWS Bedrock", Status: "warn",
		Detail: "Bedrock is partially configured; runs will NOT use it until this is complete.",
		Fix:    "Still needed: " + strings.Join(missing, ", ") + ".",
	}, true
}

// composerCheck reports whether the AI Run Composer is enabled — optional, so
// "not enabled" is info.
func composerCheck(comp SetupComposer) SetupCheck {
	if comp.Enabled {
		return SetupCheck{
			ID: "composer", Label: "AI Run Composer", Status: "ok",
			Detail: "The AI Run Composer is enabled (default backend: " + comp.Default + ").",
		}
	}
	return SetupCheck{
		ID: "composer", Label: "AI Run Composer", Status: "info",
		Detail: "The AI Run Composer is not enabled (optional); runs can still be configured manually.",
		Fix:    "Set -composer-config / WARDYN_COMPOSER_CONFIG to enable natural-language run composition.",
	}
}

// ageKeyCheck warns when the secret store's age key is EPHEMERAL: stored secrets
// become unreadable after a restart.
func ageKeyCheck(durable bool) SetupCheck {
	if durable {
		return SetupCheck{
			ID: "age_key", Label: "Secret store durability", Status: "ok",
			Detail: "The secret store age key is durable; stored secrets survive a restart.",
		}
	}
	return SetupCheck{
		ID: "age_key", Label: "Secret store durability", Status: "warn",
		Detail: "The secret store uses an EPHEMERAL age key generated at boot; stored secrets (API keys, GitHub App credentials) become unreadable after a restart.",
		Fix:    "Generate a durable key with `wardynd -gen-age-key` and set it as WARDYN_AGE_KEY (or -age-key).",
	}
}

// siteConfigCheck reports whether an operator-wide corporate baseline (upstream
// proxy, egress redirects, default SCM hosts) has been authored yet. Always
// "info" — it is optional and skippable, never a blocking gate.
//
// Deliberately does not check the deprecated ArtifactOverrides: sc always
// comes from Store.GetSiteConfig, and every persisted document has already
// gone through either the PUT-time fold (foldLegacyArtifactOverrides,
// site_config.go) or the one-time 0030 migration, so that field is provably
// always empty by the time it is read back here.
func siteConfigCheck(sc types.SiteConfig) SetupCheck {
	if sc.UpstreamProxySecretRef != "" || sc.UpstreamProxyURL != "" || len(sc.EgressRedirects) > 0 || len(sc.ScmHosts) > 0 {
		return SetupCheck{
			ID: "site_config", Label: "Site config (corporate baseline)", Status: "info",
			Detail: "An operator-wide site config is set (upstream proxy / egress redirects / SCM hosts); every run inherits it.",
		}
	}
	return SetupCheck{
		ID: "site_config", Label: "Site config (corporate baseline)", Status: "info",
		Detail: "No operator-wide site config yet (optional): a corporate upstream proxy, artifact-registry redirects, and default SCM hosts that every run would inherit.",
		Fix:    "Set one via PUT /api/v1/site-config (or the Corporate network step — the Host proxy / Artifact redirect tabs).",
	}
}

// platformChecks are the platform rows — permanent and non-fixable, so always
// "info" (and absent on a platform they do not apply to).
func platformChecks(plat setup.Platform) []SetupCheck {
	var out []SetupCheck
	if plat.WSL {
		out = append(out, SetupCheck{
			ID: "platform_wsl", Label: "WSL networking", Status: "info", Platform: "wsl",
			Detail: "Running under WSL2: host<->sandbox networking is split. Reach the UI from Windows via localhost port-forwarding, and bind wardynd to a WSL-reachable address. With Docker Desktop's default NAT networking, sandbox->wardynd callbacks don't route in host mode — workspace Verify results never report and Record captures land empty.",
			Fix:    "Enable WSL2 mirrored networking ([wsl2] networkingMode=mirrored in %UserProfile%\\.wslconfig, then `wsl --shutdown`), or run the containerized stack (`make compose-up`) where callbacks route in-network.",
		})
	}
	if plat.OS == "darwin" {
		out = append(out, SetupCheck{
			ID: "platform_macos", Label: "macOS virtualization", Status: "info", Platform: "darwin",
			Detail: "macOS has no /dev/kvm; the Vault tier (CC3, hardware-virtualized) is unavailable — runs use container isolation.",
		})
	}
	return out
}

// refRulesetCheck grades one GitHub ref-confinement probe. Pure: the outbound
// call and its cache live in Server.githubRefRulesetCheck.
//
// NEVER "fail" — the row is advisory, and grading a security regression from a
// network error is exactly the failure mode this check has to avoid. err (any
// error: timeout, rate limit, a 403 on the read permission, a repo the
// installation cannot see, a ruleset whose bypass mode could not be read) is
// "info"/unknown; an unconfined repo is "warn".
func refRulesetCheck(repo string, confined bool, detail string, err error) SetupCheck {
	const (
		id    = "github_ref_ruleset"
		label = "GitHub ref confinement (token-side)"
		fix   = "Create the ruleset on the repo — the runnable `gh api` invocation is in docs/POLICIES.md under \"Bound the token itself: a GitHub ruleset\". It needs admin on the repo, which Wardyn does not have."
	)
	switch {
	case err != nil:
		return SetupCheck{
			ID: id, Label: label, Status: "info",
			Detail: "Could not read GitHub's rules for " + repo + ", so ref confinement is UNKNOWN — not known-bad: " + err.Error() +
				" Both reads it makes — the branch rules and the ruleset's bypass mode — are documented to need only Metadata: read, which every GitHub App holds; Wardyn has not confirmed that against a live installation." +
				" GHES is not supported here — the broker always talks to api.github.com.",
			Fix: fix,
		}
	case !confined:
		return SetupCheck{
			ID: id, Label: label, Status: "warn",
			Detail: detail +
				" Wardyn's own branch-namespace enforcement still holds on the brokered route by default (the receive-pack parser refuses any ref outside refs/heads/wardyn/<run-id>/, unless WARDYN_GIT_BROKER_ENFORCE_BRANCH_NS=false); a ruleset is what would also hold for a token that escaped the proxy." +
				" This reads RULESETS only — classic branch protection also bounds a token but does not appear in that endpoint, so a repo protected that way is graded here as unconfined.",
			Fix: fix,
		}
	default:
		return SetupCheck{
			ID: id, Label: label, Status: "ok",
			Detail: detail +
				" Graded for " + repo + " only — the checklist probes one repo, while the WARDYN_GITHUB_REQUIRE_REF_RULESET gate grades every repo in the grant." +
				" The bypass part of that verdict is read from current_user_can_bypass, which GitHub returns to the caller making the request; whether it is computed meaningfully for a GitHub App installation token is not something Wardyn has confirmed.",
		}
	}
}

// firstBrokeredRepoRunScan bounds firstBrokeredRepoFromRuns to the N most
// recent runs — a fixed window, not a COUNT/EXISTS query (ponytail: widen it
// if a real install's brokered runs turn out to sit consistently deeper).
const firstBrokeredRepoRunScan = 20

// firstBrokeredRepoFromRuns is firstBrokeredRepo's fallback for the common
// case its own doc describes: shipped policies template scope.repos: [], so
// the repo actually brokered for a run usually comes from the run's declared
// --repo / workspace repos at CREATE time instead (augmentGitBrokerGrants,
// runs_create.go), unioned into the git-broker map only when the run also
// held a github_token grant — never recorded back onto the policy.
//
// This mirrors that exact pair of facts on the most recent runs: a declared
// repo Wardyn resolves as a github.com clone (gitBrokerKeyFromSlug), AND an
// actual github_token grant on that run (its persisted CredentialGrant rows,
// not guessed from Repo being non-empty). So it can't over-claim a repo that
// was only ever cloned read-only with no credential at all — a run with no
// repo, or a repo but no github grant, contributes nothing to the broker map
// either, and is skipped here the same way.
//
// Only run.Repo (the legacy single-repo field) is scanned; a run brokered
// purely through workspace_repos, with no legacy --repo, falls through to "".
// Bounded to firstBrokeredRepoRunScan runs — at the SQL level when the store
// is a store.Pager (indexed, newest-first), else in Go over the unbounded
// (but still newest-first) List — so an install with a GitHub App configured
// and a long run history that never touches GitHub can't turn this
// (5-minute-cached) check into a store round trip per run.
func (s *Server) firstBrokeredRepoFromRuns(ctx context.Context) string {
	if s.cfg.Store == nil {
		return ""
	}
	var runs []types.AgentRun
	var err error
	if pg, ok := s.cfg.Store.(store.Pager); ok {
		runs, err = pg.ListRunsPage(ctx, store.Page{Limit: firstBrokeredRepoRunScan})
	} else {
		runs, err = s.cfg.Store.ListRuns(ctx) // newest-first; only test doubles lack Pager
	}
	if err != nil {
		return ""
	}
	for i, run := range runs {
		if i >= firstBrokeredRepoRunScan {
			break
		}
		key := gitBrokerKeyFromSlug(run.Repo)
		if key == "" {
			continue // no repo, or not a github.com slug/URL
		}
		if grants, gerr := s.cfg.Store.ListGrantsByRun(ctx, run.ID); gerr == nil &&
			slices.ContainsFunc(grants, func(g types.CredentialGrant) bool { return g.Spec.Kind == types.GrantGitHubToken }) {
			return key
		}
	}
	return ""
}
