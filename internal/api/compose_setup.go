// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/setup"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// SetupItem is a per-requirement readiness verdict for a composed run, computed
// DETERMINISTICALLY from the FINAL post-clamp spec (never LLM self-assessment) —
// the same trust rule composer.Grade uses (risk.go:72). It generalizes the
// composeLLMAccess pattern (compose.go:121-127) to every setup requirement a
// proposal implies: secrets, onboarded workspaces, repo credentials, egress.
//
// ponytail: NOT SetupCheck (setup.go:72) — that type's Fix is free-text prose for
// a human to read; SetupItem's Fix is a structured action a UI button can drive
// directly (add_secret/scan_workspace), so it needs its own shape rather than
// reusing SetupCheck's.
type SetupItem struct {
	Kind       string    `json:"kind"` // "llm_access" | "secret" | "workspace" | "workspace_secret" | "repo_credential" | "egress" | "backend" | "config_pair"
	ID         string    `json:"id"`   // stable "<kind>:<key>", e.g. "secret:anthropic-api-key"
	Label      string    `json:"label"`
	RequiredBy string    `json:"required_by"`
	Status     string    `json:"status"` // "satisfied" | "missing" | "unverified"
	Detail     string    `json:"detail,omitempty"`
	Fix        *SetupFix `json:"fix,omitempty"`
	// Residency names WHERE the credential this item concerns actually lives at
	// run time, derived from the FINAL spec's own delivery mechanism (never
	// guessed): "proxy_injected" (an api_key grant — the value never leaves the
	// wardyn-proxy sidecar, injection.go), "resident_mount" (a host credential
	// bind-mounted into the sandbox, e.g. the Claude subscription mount), or
	// "brokered_mint" (a github_token/git_pat grant — the broker mints/resolves
	// a value and hands it directly to the in-sandbox git-credential helper at
	// task time, internal.go handleInternalMint). Empty when not applicable
	// (workspace/egress/backend rows carry no single credential).
	Residency string `json:"residency,omitempty"`
}

// SetupFix is the structured, actionable remedy a UI button drives directly —
// no prose-parsing. Action "none" means informational only: there is no
// button, because the only fix is the OPERATOR widening their own ceiling
// (e.g. a dropped egress domain).
//
// ponytail: v1 verifies PRESENCE only (Decision 3: declared-present, never
// "verified" in the UI copy) — a stored secret, an onboarded+scanned workspace,
// a surviving grant. Live credential verification (does the key actually
// authenticate?) is a FUTURE seam at the egress proxy (broker-verified calls),
// not here — this stays a fast, pure read of already-stored state, no live
// probe and no new injection surface.
type SetupFix struct {
	Action      string `json:"action"` // "add_secret" | "scan_workspace" | "none"
	SecretName  string `json:"secret_name,omitempty"`
	WorkspaceID string `json:"workspace_id,omitempty"` // ID, not source — api.scanWorkspace takes an id
}

// composeSubscriptionState is the use_subscription <-> credential-mount PAIR's
// reconciled verdict, computed ONCE at the runComposePipeline call site
// (applyLLMCredMount) and threaded through here so setupSubscriptionMountItem
// never recomputes it — the checklist row can then never disagree with the
// Warnings panel that already carries the same Reason text.
type composeSubscriptionState struct {
	Requested bool     // the operator's per-run ask (composeRequest.UseSubscription)
	Injected  bool     // applyLLMCredMount's verdict: did the ceiling mounts land in the FINAL spec
	Managed   bool     // no ceiling mount, but a Wardyn-managed setup-token serves this run proxy-side
	Warnings  []string // applyLLMCredMount's own explanation, reused verbatim
}

// deriveSetupItems computes the composed run's setup checklist from the FINAL
// (post-clamp) spec — the same trust boundary composer.Grade uses. Called once
// per compose round, after grading and before the advisory audit is assembled
// (runComposePipeline). Non-blocking: items never gate the proposal, they only
// inform the review panel (Decision 4).
func (s *Server) deriveSetupItems(ctx context.Context, run composer.RunInput, spec types.RunPolicySpec, presentSecrets map[string]bool, llmAccess *composeLLMAccess, droppedDomains []string, sub composeSubscriptionState) []SetupItem {
	var items []SetupItem

	if it, ok := s.setupBackendItem(ctx, run, spec); ok {
		items = append(items, it)
	}
	if it, ok := setupLLMAccessItem(run.Agent, llmAccess, spec); ok {
		items = append(items, it)
	}
	if it, ok := setupSubscriptionMountItem(sub); ok {
		items = append(items, it)
	}
	items = append(items, setupSecretItems(spec, presentSecrets)...)

	workspaces := s.referencedWorkspaces(ctx, spec)
	// The primary GIT workspace never lands in spec.WorkspaceRepos — applyWorkspaces
	// (compose.go) only sets run.Repo for it, adding to WorkspaceRepos ONLY for
	// index>0 repos — so referencedWorkspaces alone can't see it. Guard the
	// synthetic values applyWorkspaces sets for the OTHER two workspace kinds
	// ("local:<dir>", "ephemeral") so this lookup only ever fires for a real repo
	// slug/URL.
	if run.Repo != "" && run.Repo != "ephemeral" && !strings.HasPrefix(run.Repo, "local:") {
		if ws, ok := s.workspaceBySource(ctx, types.WorkspaceKindRepo, run.Repo); ok {
			workspaces = append([]types.Workspace{ws}, workspaces...)
		}
	}
	items = append(items, setupWorkspaceItems(workspaces)...)
	items = append(items, setupWorkspaceSecretItems(workspaces, presentSecrets)...)
	items = append(items, setupRepoCredentialItems(spec, presentSecrets)...)
	items = append(items, setupEgressDroppedItems(droppedDomains)...)
	if it, ok := setupEgressWorkspaceItem(spec, workspaces); ok {
		items = append(items, it)
	}
	return items
}

// workspaceBySource is a nil-safe wrapper around Store.GetWorkspaceBySource.
// referencedWorkspaces already guards s.cfg.Store == nil internally; this
// mirrors that guard for the one extra lookup deriveSetupItems needs (the
// primary git workspace, which referencedWorkspaces can't see — see above).
func (s *Server) workspaceBySource(ctx context.Context, kind types.WorkspaceKind, source string) (types.Workspace, bool) {
	if s.cfg.Store == nil {
		return types.Workspace{}, false
	}
	ws, err := s.cfg.Store.GetWorkspaceBySource(ctx, kind, source)
	if err != nil {
		return types.Workspace{}, false
	}
	return ws, true
}

// setupLLMAccessItem reshapes the ALREADY-COMPUTED reconcileLLMAccess verdict
// (compose.go) into a checklist row — REUSED, never recomputed, so the
// checklist can never disagree with the review panel's own no-model-access
// banner. ok=false for a non-LLM/unknown agent (llmAccess is nil): nothing to
// check, no row.
//
// Residency is derived from the SAME mount check reconcileLLMAccess/
// applyLLMCredMount use (specHasMountTarget on the FINAL spec): a Claude
// subscription run mounts the operator's resident credential
// (claudeCredTarget); every other LLM-backed run brokers an api_key that never
// leaves the proxy.
func setupLLMAccessItem(agent string, llmAccess *composeLLMAccess, spec types.RunPolicySpec) (SetupItem, bool) {
	if llmAccess == nil {
		return SetupItem{}, false
	}
	status := "missing"
	if llmAccess.Provisioned {
		status = "satisfied"
	}
	it := SetupItem{
		Kind:       "llm_access",
		ID:         "llm_access:" + agent,
		Label:      "Model access for " + agent,
		RequiredBy: "the agent's own model calls",
		Status:     status,
		Detail:     llmAccess.Note,
	}
	if agent == "claude-code" && specHasMountTarget(&spec, claudeCredTarget) {
		it.Residency = "resident_mount"
		// Subscription mode is the ONE path where the Claude CLI (a Node fetch()
		// client) sets ANTHROPIC_BASE_URL directly to https://api.anthropic.com and
		// relies on the sandbox's HTTP_PROXY/HTTPS_PROXY env to tunnel there
		// (runs.go); api-key mode instead points at the proxy's own /wardyn/llm
		// gateway URL directly, so no tunnel — and no gap — is in play there.
		it.Detail = strings.TrimSpace(it.Detail + " Node's built-in fetch() ignores HTTP(S)_PROXY by default before Node 24 (set NODE_USE_ENV_PROXY=1), so without it the Claude CLI's model calls won't traverse the proxy and — since the sandbox has no direct egress — will fail to reach api.anthropic.com (they cannot escape confinement, only error out).")
	} else {
		it.Residency = "proxy_injected"
	}
	if !llmAccess.Provisioned {
		if p, ok := agentLLMProvider(agent); ok {
			it.Fix = &SetupFix{Action: "add_secret", SecretName: p.secret}
		}
	}
	return it, true
}

// setupSubscriptionMountItem is the "config_pair" checklist row for the
// use_subscription <-> credential-mount PAIR: applyLLMCredMount (compose.go)
// already reconciles the operator's per-run ask against the control-plane-wide
// bless (ceilingBlessesClaudeCreds) and the FINAL egress state
// (anthropicReachable), and today silently degrades to the api-key path (or to
// no access at all) when they disagree, with the reason buried in a generic
// Warnings bullet. This surfaces that SAME verdict (sub.Injected/Warnings —
// reused verbatim, never recomputed) as its own structured row. ok=false when
// subscription mode wasn't requested this round (nothing to reconcile).
func setupSubscriptionMountItem(sub composeSubscriptionState) (SetupItem, bool) {
	if !sub.Requested {
		return SetupItem{}, false
	}
	it := SetupItem{
		Kind:       "config_pair",
		ID:         "config_pair:use_subscription:claude_cred_mount",
		Label:      "Paired setting: subscription mode + credential mount",
		RequiredBy: "the requested Claude subscription transport",
		Fix:        &SetupFix{Action: "none"},
	}
	detail := strings.Join(sub.Warnings, " ")
	switch {
	case sub.Managed:
		it.Status = "satisfied"
		it.Label = "Subscription mode: Wardyn-managed token"
		detail = "no operator-blessed credential mount, but a Wardyn-managed Claude subscription (setup-token) is connected and injected PROXY-SIDE for this run — the sandbox holds only an inert sentinel."
	case sub.Injected:
		it.Status = "satisfied"
		if detail == "" {
			detail = "the operator-blessed Claude credential mounts were injected for this run."
		}
	default:
		it.Status = "missing"
		if detail == "" {
			detail = "subscription mode was requested but not applied; the run falls back to the api-key path."
		}
	}
	it.Detail = detail
	return it, true
}

// setupSecretItems walks the FINAL spec's api_key/git_pat grants — the SAME
// scope-decode validateInlineSecretRefs uses (inline_policy.go:87) — against
// presentSecrets (the map compose.go already built via listUserSecretNames), so
// this can never disagree with the H1 422 check that gates create-run. One row
// per DISTINCT secret name: an api_key grant and a git_pat grant can name the
// same stored secret without duplicating the row.
func setupSecretItems(spec types.RunPolicySpec, presentSecrets map[string]bool) []SetupItem {
	var items []SetupItem
	seen := map[string]bool{}
	add := func(name, requiredBy, residency string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		status := "missing"
		var fix *SetupFix
		if presentSecrets[name] {
			status = "satisfied"
		} else {
			fix = &SetupFix{Action: "add_secret", SecretName: name}
		}
		items = append(items, SetupItem{
			Kind: "secret", ID: "secret:" + name, Label: "Secret: " + name,
			RequiredBy: requiredBy, Status: status, Fix: fix, Residency: residency,
		})
	}
	for _, g := range spec.EligibleGrants {
		switch g.Kind {
		case types.GrantAPIKey:
			if rule, err := injectionRuleFromScope(g.Scope); err == nil {
				// api_key: the value is resolved proxy-side (injection.go) and never
				// reaches the sandbox.
				add(rule.SecretName, "an api_key grant ("+rule.Host+")", "proxy_injected")
			}
		case types.GrantGitPAT:
			if host, name, _, err := gitPATScopeFields(g.Scope); err == nil {
				// git_pat's residency belongs to the repo_credential row (the grant
				// that actually gets minted/delivered) — this row only answers
				// whether the secret exists, so it carries no residency of its own.
				add(name, "a git_pat grant ("+host+")", "")
			}
		}
	}
	return items
}

// setupWorkspaceItems maps each referenced workspace (referencedWorkspaces —
// the SAME set the launch gate walks) to a row keyed off its onboarding/scan
// Status, so this checklist can never disagree with what launch will accept.
func setupWorkspaceItems(workspaces []types.Workspace) []SetupItem {
	items := make([]SetupItem, 0, len(workspaces))
	for _, ws := range workspaces {
		it := SetupItem{
			Kind: "workspace", ID: "workspace:" + ws.ID.String(),
			Label:      "Workspace: " + ws.Name,
			RequiredBy: "the agent's working directory",
		}
		switch ws.Status {
		case types.WorkspaceReady, types.WorkspaceScanned, types.WorkspaceBuilding,
			types.WorkspaceBuildError, types.WorkspaceVerifying, types.WorkspaceVerifyFailed:
			// Any status from `scanned` onward has a usable profile and is
			// mountable (the mount gate is onboarding-based, not status-based);
			// `ready` is the fully-finalized/verified end state.
			it.Status = "satisfied"
			it.Detail = "onboarded and scanned"
			if p, ok := workspaceProfile(ws); ok {
				if len(p.ServicesNeeded) > 0 {
					it.Detail += " · needs services: " + strings.Join(p.ServicesNeeded, ", ")
				}
				if p.BuildMemoryMiB >= 4096 {
					// A heavy build heap is worth surfacing before a run OOMs.
					it.Detail += " · build wants ~" + strconv.Itoa(p.BuildMemoryMiB/1024) + " GB memory"
				}
				if n := len(p.SecretFilesPresent); n > 0 {
					// Presence-only fact (the scan never reads these files):
					// warn that a mount would expose them to the agent.
					it.Detail += " · ⚠ " + strconv.Itoa(n) + " local secret file(s) (e.g. " +
						p.SecretFilesPresent[0] + ") — readable by the agent if this directory is mounted"
				}
				if n := len(p.LeakFindings); n > 0 {
					// Content-free: count + kind only, never the value.
					it.Detail += " · ⚠ " + strconv.Itoa(n) + " suspected committed secret(s) (" +
						p.LeakFindings[0].Kind + ") — rotate/remove before mounting"
				}
			}
		case types.WorkspacePendingScan, types.WorkspaceError:
			it.Status = "unverified"
			it.Detail = string(ws.Status)
			it.Fix = &SetupFix{Action: "scan_workspace", WorkspaceID: ws.ID.String()}
		default:
			// Defense only: validateWorkspaceSources already 422s a non-onboarded
			// source earlier in the pipeline, so a resolved workspace with an
			// unrecognized/zero Status should be unreachable in practice — render
			// it as a clear gap rather than a silently-blank row.
			it.Status = "missing"
		}
		items = append(items, it)
	}
	return items
}

// maxWorkspaceSecretRows caps the per-secret checklist rows; the remainder
// collapses into one summary row. Survey-proven: a template can declare ~100
// keys (mostly optional integrations) and a row-per-key checklist trains
// operators to ignore it.
const maxWorkspaceSecretRows = 5

// setupWorkspaceSecretItems surfaces the secret NAMES the referenced
// workspaces' scanned profiles declare as REQUIRED (RequiredSecrets with
// Optional=false — optional/deploy-time needs stay in the workspace needs
// panel). Advisory only: a workspace-declared secret never blocks launch and
// never creates a grant — the operator decides whether to store one. The kind
// is "workspace_secret", NOT "secret": the review panel's destructive ("run
// will 422") styling is gated to llm_access|secret, and these rows must render
// neutral. Names are grounded via sanitizeSecretName — the SAME normalization
// groundAPIKeySecretNames uses — so the add-secret fix can't dead-end on
// secretNameRE, and presence is checked against the sanitized form. The raw
// declared name stays in the label with its provenance (these names come from
// UNTRUSTED workspace content, already charset-capped by DeriveProfile).
func setupWorkspaceSecretItems(workspaces []types.Workspace, presentSecrets map[string]bool) []SetupItem {
	type needRow struct {
		raw, ws string
	}
	bySane := map[string]needRow{}
	for _, ws := range workspaces {
		p, ok := workspaceProfile(ws)
		if !ok {
			continue
		}
		for _, n := range p.RequiredSecrets {
			if n.Optional {
				continue
			}
			sane := sanitizeSecretName(n.Name)
			if sane == "" {
				continue
			}
			if _, seen := bySane[sane]; !seen {
				bySane[sane] = needRow{raw: n.Name, ws: ws.Name}
			}
		}
	}
	if len(bySane) == 0 {
		return nil
	}
	sanes := make([]string, 0, len(bySane))
	for s := range bySane {
		sanes = append(sanes, s)
	}
	slices.Sort(sanes)

	var items []SetupItem
	for i, sane := range sanes {
		if i >= maxWorkspaceSecretRows {
			items = append(items, SetupItem{
				Kind: "workspace_secret", ID: "workspace_secret:more",
				Label:      "+" + strconv.Itoa(len(sanes)-maxWorkspaceSecretRows) + " more declared secrets",
				RequiredBy: "the referenced workspaces' committed config",
				Status:     "unverified",
				Detail:     "see each workspace's profile for the full list",
			})
			break
		}
		row := bySane[sane]
		it := SetupItem{
			Kind: "workspace_secret", ID: "workspace_secret:" + sane,
			Label:      "Workspace secret: " + row.raw,
			RequiredBy: "declared by workspace " + row.ws + " (untrusted content, names only)",
		}
		if presentSecrets[sane] {
			it.Status = "satisfied"
			it.Detail = "a secret named " + sane + " is stored"
		} else {
			it.Status = "missing"
			it.Detail = "the workspace's config expects this; store it as " + sane + " if the task needs it"
			it.Fix = &SetupFix{Action: "add_secret", SecretName: sane}
		}
		items = append(items, it)
	}
	return items
}

// setupRepoCredentialItems covers the grants groundGitHubGrants/groundGitPATGrants
// left standing in the FINAL spec: a github_token is broker-minted at clone time
// (nothing stored to check, so it is always "unverified" pre-launch); a git_pat's
// readiness is exactly whether its secret is present. This intentionally
// overlaps setupSecretItems' git_pat check — that row answers "is the secret
// stored", this one answers "is THIS grant usable" (host-scoped).
func setupRepoCredentialItems(spec types.RunPolicySpec, presentSecrets map[string]bool) []SetupItem {
	var items []SetupItem
	seenGH := false
	for _, g := range spec.EligibleGrants {
		switch g.Kind {
		case types.GrantGitHubToken:
			if seenGH {
				continue
			}
			seenGH = true
			items = append(items, SetupItem{
				Kind: "repo_credential", ID: "repo_credential:github_token",
				Label:      "GitHub repository access",
				RequiredBy: "cloning/pushing the workspace's GitHub remote",
				Status:     "unverified",
				Detail:     "minted at run start by the broker; there is nothing to check pre-launch",
				Residency:  "brokered_mint",
			})
		case types.GrantGitPAT:
			host, name, _, err := gitPATScopeFields(g.Scope)
			if err != nil {
				continue
			}
			status := "missing"
			var fix *SetupFix
			if presentSecrets[name] {
				status = "satisfied"
			} else {
				fix = &SetupFix{Action: "add_secret", SecretName: name}
			}
			items = append(items, SetupItem{
				Kind: "repo_credential", ID: "repo_credential:git_pat:" + host,
				Label:      "Repository access: " + host,
				RequiredBy: "cloning/pushing the workspace's " + host + " remote",
				Status:     status, Fix: fix, Residency: "brokered_mint",
			})
		}
	}
	return items
}

// setupEgressDroppedItems turns the pre/post-clamp domain diff (computed at the
// compose.go call site, right after composer.Clamp) into one informational row
// per dropped domain. Action "none": the only fix is the OPERATOR widening
// their own ceiling policy — no button this server can drive.
func setupEgressDroppedItems(droppedDomains []string) []SetupItem {
	items := make([]SetupItem, 0, len(droppedDomains))
	for _, d := range droppedDomains {
		items = append(items, SetupItem{
			Kind: "egress", ID: "egress:dropped:" + d,
			Label:      "Egress dropped: " + d,
			RequiredBy: "the proposed task",
			Status:     "missing",
			Detail:     "not in the operator's egress allowlist; widen the ceiling policy to allow it",
			Fix:        &SetupFix{Action: "none"},
		})
	}
	return items
}

// setupEgressWorkspaceItem is informational only: it reports the egress domains
// the referenced workspaces' scanned profiles would add at LAUNCH (the real
// union happens for real in unionWorkspaceEgress, runs.go:308) — computed here
// on a COPY of AllowedDomains, since unionWorkspaceEgress mutates its spec
// argument IN PLACE (workspace_run.go:72-92) and this proposal's spec must not
// be touched before the operator approves it. ok=false when there are no
// referenced workspaces (nothing to union).
func setupEgressWorkspaceItem(spec types.RunPolicySpec, workspaces []types.Workspace) (SetupItem, bool) {
	if len(workspaces) == 0 {
		return SetupItem{}, false
	}
	dup := spec
	dup.AllowedDomains = append([]string(nil), spec.AllowedDomains...)
	added := unionWorkspaceEgress(&dup, workspaces)
	detail := "no additional egress needed beyond the current allowlist"
	if len(added) > 0 {
		detail = "launch will also allow: " + strings.Join(added, ", ")
	}
	// Content-derived suggestions are shown, explicitly labeled NOT allowed —
	// the operator promotes them per-workspace (approved egress) if wanted.
	if suggested := workspaceSuggestedEgress(workspaces); len(suggested) > 0 {
		shown := suggested
		if len(shown) > 8 {
			shown = append(append([]string(nil), shown[:8]...), "+"+strconv.Itoa(len(suggested)-8)+" more")
		}
		detail += " · suggested by workspace content (needs review, NOT auto-allowed): " + strings.Join(shown, ", ")
	}
	return SetupItem{
		Kind: "egress", ID: "egress:workspace",
		Label:      "Workspace egress",
		RequiredBy: "the onboarded workspace's detected registries/tools",
		Status:     "satisfied",
		Detail:     detail,
	}, true
}

// setupBackendItem is the "backend" checklist row: can THIS host actually
// enforce the proposal's FINAL (post-floor/clamp) confinement class right now?
// It reuses the SAME runner-capability probe and MEMBERSHIP check the launch gate
// itself uses (runs.go: slices.Contains over caps.ConfinementClasses) — never a
// duplicate probe, never a rank compare (M8: CC2/CC3 are independent runtimes, so
// a Kata-only host advertises the non-contiguous set [CC1, CC3] and a rank compare
// would call a CC2 proposal "satisfied" here while create-run 422s on it) — so this
// row can never disagree with what create-run would 422 on. When the
// class isn't live it distinguishes an honest "needs setup" (an installable
// runtime — CC2/CC3 with the substrate simply not registered yet) from an
// honest "not fixable on this host" (Vault without /dev/kvm), reusing the exact
// hardware fact commit 74b4d0a wired into internal/setup for the Getting
// Started wizard. A "missing" verdict's Fix is always "none": the remedy is a
// host-level command (`wardyn setup wall`/`vault`) or a hardware limit, neither
// of which this server can drive with a button — Detail points at Getting
// Started instead. ok=false only when there is no explicit class to check (an empty run class
// AND an empty policy floor — nothing this run structurally requires).
func (s *Server) setupBackendItem(ctx context.Context, run composer.RunInput, spec types.RunPolicySpec) (SetupItem, bool) {
	final := types.ConfinementClass(run.ConfinementClass)
	if final == "" {
		final = spec.MinConfinementClass
	}
	if final == "" {
		return SetupItem{}, false
	}
	it := SetupItem{
		Kind: "backend", ID: "backend:" + string(final),
		Label:      "Sandbox barrier: " + tierLabel(final),
		RequiredBy: "the proposal's confinement class",
	}
	if s.cfg.Runner == nil {
		it.Status = "missing"
		it.Detail = "no sandbox runner is configured on this control plane. See Getting Started."
		it.Fix = &SetupFix{Action: "none"}
		return it, true
	}
	caps, err := s.cfg.Runner.Capabilities(ctx)
	if err != nil {
		it.Status = "unverified"
		it.Detail = "could not probe the runner's capabilities: " + err.Error()
		return it, true
	}
	// Mirror of the launch gate (runs.go): the runner must advertise EXACTLY
	// final, not merely something that outranks it.
	if slices.Contains(caps.ConfinementClasses, final) {
		it.Status = "satisfied"
		if sub := caps.Resolved[final]; sub != "" {
			it.Detail = "enforced here as " + sub + "."
		}
		return it, true
	}
	it.Status = "missing"
	it.Fix = &SetupFix{Action: "none"}
	switch final {
	case types.CC3:
		// Single-source the KVM copy: VaultKVMDetail names the real fix
		// (bind-mount /dev/kvm) for a containerized wardynd instead of
		// asserting a bare "hardware limit no install can fix" (U085).
		it.Detail = setup.VaultKVMDetail()
	case types.CC2:
		it.Detail = "no Wall (gVisor) runtime registered on this host yet — fixable, run `wardyn setup wall`. See Getting Started."
	default:
		it.Detail = "no sandbox runtime registered on this host at all — check that wardynd's runner is configured and its container daemon is reachable. See Getting Started."
	}
	return it, true
}

// tierLabel maps a ConfinementClass to its operator-facing tier name, mirroring
// the inline switch handleSetupStatus already uses (setup.go) for the same
// three classes.
func tierLabel(cc types.ConfinementClass) string {
	switch cc {
	case types.CC1:
		return "Fence"
	case types.CC2:
		return "Wall"
	case types.CC3:
		return "Vault"
	default:
		return string(cc)
	}
}
