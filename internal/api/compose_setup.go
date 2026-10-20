// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/setup"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// SetupItem is a per-requirement readiness verdict for a composed run, computed
// DETERMINISTICALLY from the FINAL post-clamp spec (never LLM self-assessment) —
// the same trust rule composer.Grade uses (composer/risk.go). It generalizes the
// composeLLMAccess pattern (compose.go) to every setup requirement a proposal
// implies: secrets, onboarded workspaces, repo credentials, egress.
//
// NOT SetupCheck (setup.go) — that type's Fix is free-text prose for a human to
// read; SetupItem's Fix is a structured action a UI button can drive directly
// (add_secret/scan_workspace), so it needs its own shape rather than reusing
// SetupCheck's.
type SetupItem struct {
	Kind       string    `json:"kind"` // "llm_access" | "secret" | "workspace" | "workspace_secret" | "repo_credential" | "egress" | "backend"
	ID         string    `json:"id"`   // stable "<kind>:<key>", e.g. "secret:anthropic-api-key"
	Label      string    `json:"label"`
	RequiredBy string    `json:"required_by"`
	Status     string    `json:"status"` // "satisfied" | "missing" | "unverified"
	Detail     string    `json:"detail,omitempty"`
	Fix        *SetupFix `json:"fix,omitempty"`
	// Residency names WHERE the credential this item concerns actually lives at
	// run time, derived from the FINAL spec's own delivery mechanism (never
	// guessed): "proxy_injected" (an api_key grant — the value never leaves the
	// wardyn-proxy sidecar, injection.go), "brokered_mint" (a github_token/git_pat grant — the broker mints/resolves
	// a value and hands it directly to the in-sandbox git-credential helper at
	// task time, internal.go handleInternalMint), "resident_env" (an env_secret
	// grant — the value is in the sandbox environment for the whole run), or
	// "resident_file" (a file_secret grant — the value is a file in the sandbox
	// for the whole run). Empty when not applicable (workspace/egress/backend
	// rows carry no single credential).
	Residency string `json:"residency,omitempty"`
}

// SetupFix is the structured, actionable remedy a UI button drives directly —
// no prose-parsing. Action "none" means informational only: there is no
// button, because the only fix is the OPERATOR widening their own ceiling
// (e.g. a dropped egress domain).
//
// v1 verifies PRESENCE only (declared-present, never
// "verified" in the UI copy) — a stored secret, an onboarded+scanned workspace,
// a surviving grant. Live credential verification (does the key actually
// authenticate?) is a FUTURE seam at the egress proxy (broker-verified calls),
// not here — this stays a fast, pure read of already-stored state, no live
// probe and no new injection surface.
type SetupFix struct {
	Action      string `json:"action"` // "add_secret" | "scan_workspace" | "sign_in" | "choose_model_provider" | "none"
	SecretName  string `json:"secret_name,omitempty"`
	WorkspaceID string `json:"workspace_id,omitempty"` // ID, not source — api.scanWorkspace takes an id
	// sign_in: what to sign in to (model_provider, azure_cli or aws_cli) and its
	// id (a model provider's id, or a component's).
	Target string `json:"target,omitempty"`
	ID     string `json:"id,omitempty"`
	// choose_model_provider: the included harness (tools[].id) that has none.
	Tool string `json:"tool,omitempty"`
}

// deriveSetupItems computes the composed run's setup checklist from the FINAL
// (post-clamp) spec — the same trust boundary composer.Grade uses. Called once
// per compose round, after grading and before the advisory audit is assembled
// (runComposePipeline). Non-blocking: items never gate the proposal, they only
// inform the review panel.
func (s *Server) deriveSetupItems(ctx context.Context, owner string, run composer.RunInput, spec types.RunPolicySpec, presentSecrets map[string]bool, llmAccess *composeLLMAccess) []SetupItem {
	// applyWorkspaces (compose.go) now puts EVERY git repo, the primary included,
	// into spec.WorkspaceRepos — so referencedWorkspaces alone resolves it; no
	// separate run.Repo-keyed lookup needed here.
	return s.deriveRunSetupItems(ctx, owner, run, spec, presentSecrets, llmAccess, s.referencedWorkspaces(ctx, owner, spec))
}

// deriveRunSetupItems is deriveSetupItems over the workspaces a run attaches
// (runFold.wsRefs), so Review names the workspace each requirement row comes
// from, a workspace with no mount or repo of its own included.
func (s *Server) deriveRunSetupItems(ctx context.Context, owner string, run composer.RunInput, spec types.RunPolicySpec,
	presentSecrets map[string]bool, llmAccess *composeLLMAccess, workspaces []types.Workspace,
) []SetupItem {
	var items []SetupItem

	if it, ok := s.setupBackendItem(ctx, run, spec); ok {
		items = append(items, it)
	}
	if it, ok := s.setupLLMAccessItem(run.Agent, llmAccess, spec); ok {
		items = append(items, it)
	}
	// A member's rows never name an operator secret or judge one of theirs against the operator's
	// store, and the workspace facts in them are the ones that member reads on the workspace itself.
	own, read, egress := s.setupMemberView(ctx, owner, presentSecrets, workspaces)
	items = append(items, setupSecretItemsFor(spec, presentSecrets, own)...)

	items = append(items, setupWorkspaceItems(read)...)
	judge := presentSecrets
	if own != nil {
		judge = own
	}
	items = append(items, setupWorkspaceSecretItems(read, judge)...)
	items = append(items, s.setupWorkspaceIntegrationItems(ctx, owner, workspaces, presentSecrets)...)
	items = append(items, setupRepoCredentialItemsFor(spec, presentSecrets, own)...)
	if it, ok := setupEgressWorkspaceItem(spec, egress); ok {
		items = append(items, it)
	}
	return items
}

// setupMemberView is what a non-operator's checklist is built from: own, the names in their own
// namespace (nil for an operator, whose rows read the operator's store), read, each workspace as
// its read tier shows it (redactWorkspaceForRead), and egress, each workspace with its content's
// suggested hosts dropped where that tier does not see them.
func (s *Server) setupMemberView(ctx context.Context, owner string, present map[string]bool, workspaces []types.Workspace) (own map[string]bool, read, egress []types.Workspace) {
	if owner == "" {
		return nil, workspaces, workspaces
	}
	own = map[string]bool{}
	if s.cfg.Secrets != nil {
		names, err := reservedFilteredSecretNames(ctx, s.cfg.Secrets.For(owner))
		if err != nil {
			slog.WarnContext(ctx, "api: list a person's own secrets for the setup checklist", slog.Any("err", err))
		}
		for _, n := range names {
			own[n] = true
		}
	}
	for _, ws := range workspaces {
		tier := workspaceReadFull
		if ws.OwnedBy != owner {
			tier = workspaceReadMember
			if s.isSecurityOperator(ctx) {
				tier = workspaceReadSecurity
			}
		}
		read = append(read, redactWorkspaceForRead(ws, tier))
		if tier == workspaceReadMember {
			ws.Profile = withoutProfileKeys(ws.Profile, "suggested_egress")
		}
		egress = append(egress, ws)
	}
	return own, read, egress
}

// withoutProfileKeys is a scanned profile with keys deleted, decoded as redactProfileForRead does;
// one that will not decode is withheld.
func withoutProfileKeys(raw json.RawMessage, keys ...string) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	for _, k := range keys {
		delete(doc, k)
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return nil
	}
	return out
}

// setupLLMAccessItem reshapes the ALREADY-COMPUTED runLLMAccess verdict
// (runs.go) into a checklist row — REUSED, never recomputed, so the
// checklist can never disagree with the review panel's own no-model-access
// banner. ok=false for a non-LLM/unknown agent (llmAccess is nil): nothing to
// check, no row.
//
// Residency is always proxy_injected: every LLM-backed run brokers its credential
// at the proxy and it never leaves it. A legacy policy's resident .claude mount
// is stripped at dispatch (runs_dispatch_provider.go), so no run holds one.
func (s *Server) setupLLMAccessItem(agent string, llmAccess *composeLLMAccess, spec types.RunPolicySpec) (SetupItem, bool) {
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
	// Every LLM-backed run brokers its credential at the proxy: dispatch strips a
	// legacy policy's resident .claude mount (runs_dispatch_provider.go), so a
	// spec carrying one never reaches a run as a resident mount.
	it.Residency = "proxy_injected"
	if !llmAccess.Provisioned {
		if p, ok := s.llmProviderFor(agent); ok {
			// Name the run's ACTUAL resolved
			// grant secret, not the provider convention name — a run's
			// api_key grant may carry its own secret (a policy grant, or a
			// workspace's required secret), which may differ from the
			// convention default. Fixing the wrong (unused) secret would end
			// in a false green: the checklist reads "satisfied" once the
			// convention secret exists, even though the run still
			// authenticates through the grant's own secret, unaffected by
			// what the operator just added.
			secretName := p.secret
			if g, ok := apiKeyGrantForHost(&spec, p.host); ok {
				if s := apiKeyGrantScopeSecret(g.Scope); s != "" {
					secretName = s
				}
			}
			it.Fix = &SetupFix{Action: "add_secret", SecretName: secretName}
		}
	}
	return it, true
}

// setupSecretItems walks the FINAL spec's stored-secret grants — the SAME
// scope-decode validateInlineSecretRefs uses (injectionRuleFromScope,
// inline_policy.go) — against presentSecrets (s.presentSecretNames), so this can
// never disagree with the H1 422 check that gates create-run. One row per
// DISTINCT secret name: an api_key grant and a git_pat grant can name the same
// stored secret without duplicating the row.
func setupSecretItems(spec types.RunPolicySpec, presentSecrets map[string]bool) []SetupItem {
	return setupSecretItemsFor(spec, presentSecrets, nil)
}

// setupSecretItemsFor is setupSecretItems for a caller whose own secret names are own (nil for an
// operator). A member reads a secret's name only when they hold it, or when the grant is
// owner_only and so can only ever read their own; any other grant reads the operator's secret, and
// its row is "Provided by your admin", keyed by what the grant presents it to, with the operator's
// answer for its status.
func setupSecretItemsFor(spec types.RunPolicySpec, presentSecrets, own map[string]bool) []SetupItem {
	var items []SetupItem
	seen := map[string]bool{}
	var ownerOnly bool
	add := func(name, key, requiredBy, residency string) {
		if own != nil && name != "" && !own[name] {
			if !ownerOnly {
				it := providedSecretItem(key, requiredBy, residency, presentSecrets[name])
				if !seen[it.ID] {
					seen[it.ID] = true
					items = append(items, it)
				}
				return
			}
			if seen[name] {
				return
			}
			seen[name] = true
			items = append(items, secretSetupItem(name, requiredBy, residency, false))
			return
		}
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		items = append(items, secretSetupItem(name, requiredBy, residency, presentSecrets[name]))
	}
	for _, g := range spec.EligibleGrants {
		ownerOnly = g.OwnerOnly
		switch g.Kind {
		case types.GrantAPIKey:
			rule, err := injectionRuleFromScope(g.Scope)
			if err != nil {
				continue
			}
			if apiKeyScopeShared(g.Scope) {
				items = append(items, setupSharedSecretItem(rule.Host, presentSecrets[rule.SecretName]))
				continue
			}
			// api_key: the value is resolved proxy-side (injection.go) and never
			// reaches the sandbox.
			add(rule.SecretName, rule.Host, "an api_key grant ("+rule.Host+")", "proxy_injected")
		case types.GrantGitPAT:
			if sc, err := types.DecodeGitPATScope(g.Scope); err == nil {
				// git_pat's residency belongs to the repo_credential row (the grant
				// that actually gets minted/delivered) — this row only answers
				// whether the secret exists, so it carries no residency of its own.
				add(sc.SecretName, sc.Host, "a git_pat grant ("+sc.Host+")", "")
			}
		case types.GrantEnvSecret:
			if name, secretName, err := envSecretScopeFields(g.Scope); err == nil {
				add(secretName, "env:"+name, "an env_secret grant ("+name+")", "resident_env")
			}
		case types.GrantFileSecret:
			if file, secretName, err := fileSecretScopeFields(g.Scope); err == nil {
				add(secretName, "file:"+file, "a file_secret grant ("+file+")", "resident_file")
			}
		}
	}
	return items
}

// secretSetupItem is the checklist row for one stored secret a run needs: satisfied when the person's
// namespace holds it, else missing with the add_secret fix.
func secretSetupItem(name, requiredBy, residency string, present bool) SetupItem {
	status := "missing"
	var fix *SetupFix
	if present {
		status = "satisfied"
	} else {
		fix = &SetupFix{Action: "add_secret", SecretName: name}
	}
	return SetupItem{
		Kind: "secret", ID: "secret:" + name, Label: "Secret: " + name,
		RequiredBy: requiredBy, Status: status, Fix: fix, Residency: residency,
	}
}

// overrideSecretItems are the missing rows for the secrets the person's New Run edits add and they
// have not stored, so Review lists what create would refuse. A secret the checklist already names is
// not listed twice.
func overrideSecretItems(items []SetupItem, missing []client.SecretOverride) []SetupItem {
	for _, sec := range missing {
		it := secretSetupItem(sec.SecretName, "an api_key grant ("+strings.ToLower(sec.Host)+")", "proxy_injected", false)
		if !slices.ContainsFunc(items, func(x SetupItem) bool { return x.ID == it.ID }) {
			items = append(items, it)
		}
	}
	return items
}

// setupSharedSecretItem is the row for a `shared` api_key grant: a credential
// the organisation provides through one of its components. The secret's NAME
// is the operator's and no member-facing row carries it — not in the id, the
// label or a fix — so the row is keyed by the host it is presented to, which a
// person granted the component may see. There is nothing for them to add:
// only an admin can store it.
//
// A shared grant is only ever authored by the component gate, which refuses a
// run whose operator has not stored the secret before any row is derived, so
// the status read from the caller's presence map is the operator's answer.
func setupSharedSecretItem(host string, present bool) SetupItem {
	return providedSecretItem(host, "an api_key grant ("+host+")", "proxy_injected", present)
}

// providedSecretItem is the nameless row for a secret the operator stores, keyed by key.
func providedSecretItem(key, requiredBy, residency string, present bool) SetupItem {
	status := "missing"
	if present {
		status = "satisfied"
	}
	return SetupItem{
		Kind: "secret", ID: "secret:provided:" + key, Label: "Provided by your admin",
		RequiredBy: requiredBy, Status: status, Residency: residency,
	}
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
		case types.WorkspaceScanned:
			// The only satisfied status now: a usable profile exists and the
			// workspace is mountable (the mount gate is onboarding-based, not
			// status-based). The retired ready/building/build_error/verifying/
			// verify_failed stages collapsed into this one on migration.
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
		case types.WorkspaceScanning:
			// Still in-flight — not yet satisfied, but not a fixable gap either
			// (a scan is already running; re-triggering one 409s).
			it.Status = "unverified"
			it.Detail = string(ws.Status)
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

// setupWorkspaceSecretItems surfaces the secret NAMES a referenced workspace
// needs: its scanned profile's REQUIRED needs (RequiredSecrets, Optional=false)
// unioned with every required "secret:<NAME>" requirements-contract entry, merged
// by sanitized name so a secret never duplicates a row.
// Kind is "workspace_secret": neutral, never blocking. A contract-Required name
// used to escalate when absent, because the requirement minted a model-host
// grant from it at launch; it mints nothing now (skipRequiredSecret), so storing
// it changes nothing about the run's model access.
// Names go through sanitizeSecretName (compose_ground.go's secretNameRE fold) so
// the add-secret fix can't dead-end on secretNameRE; contract names are already
// secretNameRE-shaped. Scanner names are UNTRUSTED workspace content
// (charset-capped by DeriveProfile) and stay in the label with their provenance.
func setupWorkspaceSecretItems(workspaces []types.Workspace, presentSecrets map[string]bool) []SetupItem {
	type needRow struct {
		raw, ws  string
		required bool // Level=="required" in some workspace's requirements contract
	}
	bySane := map[string]needRow{}
	for _, ws := range workspaces {
		if p, ok := workspaceProfile(ws); ok {
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
		wsReqs := effectiveRequirements(ws)
		for _, key := range sortedKeys(wsReqs) {
			typ, name, ok := splitRequirementKey(key)
			if !ok || typ != "secret" || wsReqs[key].Level != "required" {
				continue
			}
			row := bySane[name]
			if row.raw == "" {
				row.raw, row.ws = name, ws.Name
			}
			row.required = true
			bySane[name] = row
		}
	}
	if len(bySane) == 0 {
		return nil
	}
	sanes := slices.Sorted(maps.Keys(bySane))

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
		kind, id := "workspace_secret", "workspace_secret:"+sane
		// Plain noun phrases, like every other RequiredBy string in this file
		// (e.g. "the proposal's confinement class"): compose-review.tsx renders
		// "Required by " + this value, so a phrase that ALSO starts with a
		// "required by"/"declared by" verb doubles up — "Required by required by
		// workspace X's requirements contract", observed live.
		requiredBy := "workspace " + row.ws + " (untrusted content, names only)"
		if row.required {
			requiredBy = "workspace " + row.ws + "'s requirements contract"
		}
		it := SetupItem{Label: "Workspace secret: " + row.raw, RequiredBy: requiredBy}
		if presentSecrets[sane] {
			it.Status = "satisfied"
			it.Detail = "a secret named " + sane + " is stored"
		} else {
			it.Status = "missing"
			it.Detail = "the workspace's config expects this; store it as " + sane + " if the task needs it"
			it.Fix = &SetupFix{Action: "add_secret", SecretName: sane}
		}
		it.Kind, it.ID = kind, id
		items = append(items, it)
	}
	return items
}

// setupRepoCredentialItemsFor covers the grants groundGitHubGrants/groundGitPATGrants
// left standing in the FINAL spec: a github_token is broker-minted at clone time
// (nothing stored to check, so it is always "unverified" pre-launch); a git_pat's
// readiness is exactly whether its secret is present. This intentionally
// overlaps setupSecretItems' git_pat check — that row answers "is the secret
// stored", this one answers "is THIS grant usable" (host-scoped).
//
// own is the caller's own secret names (nil for an operator): a git_pat secret a member does not
// hold is judged against their own namespace when the grant is owner_only, and is otherwise the
// operator's, offered with no name.
func setupRepoCredentialItemsFor(spec types.RunPolicySpec, presentSecrets, own map[string]bool) []SetupItem {
	var items []SetupItem
	seenGH := false
	for _, g := range spec.EligibleGrants {
		switch g.Kind {
		case types.GrantGitHubToken:
			if seenGH {
				continue
			}
			seenGH = true
			// Name the repo(s) this grant is actually SCOPED to (the
			// grounded reality groundGitHubGrants set — never the analyzer's
			// guess) so a reviewer can see it agrees with the workspace they
			// selected, instead of trusting an opaque "minted at run start".
			detail := "minted at run start by the broker; there is nothing to check pre-launch"
			if repos := ghScopeRepos(g.Scope); len(repos) > 0 {
				detail += " — scoped to " + strings.Join(repos, ", ")
			} else {
				detail += " — no repo named in scope (nothing to clone/push against)"
			}
			provider := repoCredentialProvider(spec, ghScopeRepos(g.Scope))
			items = append(items, SetupItem{
				Kind: "repo_credential", ID: "repo_credential:github_token",
				Label:      provider + " repository access",
				RequiredBy: "cloning/pushing the workspace's " + provider + " remote",
				Status:     "unverified",
				Detail:     detail,
				Residency:  "brokered_mint",
			})
		case types.GrantGitPAT:
			pat, err := types.DecodeGitPATScope(g.Scope)
			if err != nil {
				continue
			}
			host, name := pat.Host, pat.SecretName
			status := "missing"
			var fix *SetupFix
			switch {
			case own != nil && !own[name] && !g.OwnerOnly:
				if presentSecrets[name] {
					status = "satisfied"
				}
			case presentSecrets[name] && (own == nil || own[name]):
				status = "satisfied"
			default:
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

// repoCredentialProvider names the forge a repo_credential row's repositories
// live on, for its label: "Azure DevOps" when every repository the grant is
// scoped to and every workspace repository is an Azure DevOps clone URL,
// "GitHub" otherwise — the row's own default, so an unrecognised host or a mix
// reads as it always did.
func repoCredentialProvider(spec types.RunPolicySpec, scoped []string) string {
	repos := append(slices.Clone(scoped), repoLocatorsOf(spec.WorkspaceRepos)...)
	if len(repos) == 0 {
		return "GitHub"
	}
	for _, repo := range repos {
		if _, ok := adoOrganisationOf(repo); !ok {
			return "GitHub"
		}
	}
	return "Azure DevOps"
}

// setupEgressWorkspaceItem is informational only: it reports the egress domains
// the referenced workspaces' scanned profiles would add at LAUNCH (the real
// union happens on the create path, in unionWorkspaceEgress) — computed here on
// spec.Clone() (types.go), since unionWorkspaceEgress mutates its spec argument
// IN PLACE (workspace_run.go) and this proposal's spec must not be touched
// before the operator approves it. Clone, not a hand-rolled AllowedDomains-only
// append: this row now also reports the denied side, and a partial
// copy that deep-copies only AllowedDomains is exactly the bug resolvePolicy's
// own doc comment (runs_policy.go) already describes taking once on this same
// field family — a caller's `append` into a shared backing array let one run's
// egress domain silently replace another's. ok=false when there are no
// referenced workspaces (nothing to union).
func setupEgressWorkspaceItem(spec types.RunPolicySpec, workspaces []types.Workspace) (SetupItem, bool) {
	if len(workspaces) == 0 {
		return SetupItem{}, false
	}
	dup := spec.Clone()
	beforeDenied := len(dup.DeniedDomains)
	added := unionWorkspaceEgress(&dup, workspaces)
	// unionWorkspaceEgress only APPENDS new entries (unionDomains, helpers.go),
	// so everything past the pre-call length is exactly what THIS call added —
	// same trick as diffing `added`, just done by hand since the denied side
	// deliberately isn't in the return value (see unionWorkspaceEgress's doc).
	blocked := dup.DeniedDomains[beforeDenied:]
	detail := "no additional egress needed beyond the current allowlist"
	switch {
	case len(added) > 0 && len(blocked) > 0:
		detail = "launch will also allow: " + strings.Join(added, ", ") + " and will block: " + strings.Join(blocked, ", ")
	case len(added) > 0:
		detail = "launch will also allow: " + strings.Join(added, ", ")
	case len(blocked) > 0:
		// A host here is in a REFERENCED WORKSPACE's DeniedEgress — surfaced so it
		// never reads as "will also allow" while actually being blocked at runtime
		// (the same understating-the-envelope failure preflight.go's own comment
		// says this whole checklist exists to prevent).
		detail = "launch will block: " + strings.Join(blocked, ", ")
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

// setupBackendItem is the "backend" checklist row: can THIS host enforce the
// proposal's FINAL (post-floor/clamp) confinement class right now? It uses the
// launch gate's own runner-capability probe and MEMBERSHIP check
// (slices.Contains over caps.ConfinementClasses), never a rank compare — CC2/CC3
// are independent runtimes, so a Kata-only host advertises [CC1, CC3] — so it can
// never disagree with create-run. A missing class is "needs setup" when the
// runtime is installable and "not fixable on this host" otherwise (Vault without
// /dev/kvm, the internal/setup hardware fact). Its Fix is always "none": the
// remedy is a host-level command or a hardware limit, so Detail points at Getting
// Started. ok=false only when there is no explicit class to check (an empty run
// class AND an empty policy floor).
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
	// The row is the caller's: a non-operator reads what redactSetupStatusForUser
	// leaves them, so neither the probe's error nor the substrate name.
	operator := s.isOperator(ctx)
	caps, err := s.cfg.Runner.Capabilities(ctx)
	if err != nil {
		it.Status = "unverified"
		it.Detail = "could not probe the runner's capabilities"
		if operator {
			it.Detail += ": " + err.Error()
		} else {
			slog.WarnContext(ctx, "wardynd: probing the runner's capabilities for a setup row failed", slog.Any("err", err))
		}
		return it, true
	}
	// Mirror of the launch gate (runs.go): the runner must advertise EXACTLY
	// final, not merely something that outranks it.
	if slices.Contains(caps.ConfinementClasses, final) {
		it.Status = "satisfied"
		if sub := caps.Resolved[final]; sub != "" && operator {
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
		// asserting a bare "hardware limit no install can fix".
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

// maxWorkspaceIntegrationRows caps the integration rows one checklist renders,
// mirroring maxWorkspaceSecretRows: past a handful the panel stops informing
// and starts scrolling.
const maxWorkspaceIntegrationRows = 6

// setupWorkspaceIntegrationItems surfaces the integrations a referenced
// workspace's requirements contract names as REQUIRED ("integration:<id>").
// The fold degrades SILENTLY by design — an `integration:<id>` naming an
// unconfigured row opens, grants and audits nothing (applyIntegrationRequirement),
// so a missing one never bricks a run — but PREFLIGHT is where an operator asks
// what the run will actually get, so it gets a row here.
//
// Kind is "workspace_integration", NOT "secret": destructive styling is gated to
// credential absence (llm_access|secret), and this is config state, like the
// workspace/backend rows. The run still launches either way. Optional
// requirements are not rowed: they apply only when a run enables them.
func (s *Server) setupWorkspaceIntegrationItems(ctx context.Context, owner string, workspaces []types.Workspace, presentSecrets map[string]bool) []SetupItem {
	// id -> the workspace that requires it (first wins; the row is about the
	// integration, and naming one workspace is enough provenance).
	requiredBy := map[string]string{}
	var ids []string
	for _, ws := range workspaces {
		wsReqs := effectiveRequirements(ws)
		for _, key := range sortedKeys(wsReqs) {
			typ, id, ok := splitRequirementKey(key)
			if !ok || typ != "integration" || wsReqs[key].Level != "required" {
				continue
			}
			if _, seen := requiredBy[id]; !seen {
				requiredBy[id] = ws.Name
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	slices.Sort(ids)

	var items []SetupItem
	for i, id := range ids {
		if i >= maxWorkspaceIntegrationRows {
			items = append(items, SetupItem{
				Kind: "workspace_integration", ID: "workspace_integration:more",
				Label:      "+" + strconv.Itoa(len(ids)-maxWorkspaceIntegrationRows) + " more required integrations",
				RequiredBy: "the referenced workspaces' requirements",
				Status:     "unverified",
				Detail:     "see each workspace's requirements for the full list",
			})
			break
		}
		integ, ok := s.resolveIntegrationRef(ctx, owner, id)
		items = append(items, integrationSetupItem(id, requiredBy[id], integ, ok, presentSecrets))
	}
	return items
}

// integrationSetupItem states what a run will ACTUALLY get from one required
// integration. Satisfied means everything the row promises is deliverable;
// anything less is "missing" with a Detail naming precisely which half is
// absent, because "the path opens but the credential doesn't ride" and "this
// doesn't exist at all" are different problems with different fixes.
func integrationSetupItem(id, wsName string, integ types.Integration, ok bool, presentSecrets map[string]bool) SetupItem {
	it := SetupItem{
		Kind:       "workspace_integration",
		ID:         "workspace_integration:" + id,
		Label:      "Integration " + id,
		RequiredBy: "workspace " + wsName,
		Status:     "missing",
	}
	switch {
	case !ok:
		it.Detail = "No integration named " + id + " is configured, so this workspace's requirement opens nothing. Connect it under Settings."
		return it
	case integ.Disabled:
		it.Detail = "Integration " + id + " is turned off, so it opens no hosts and delivers no credential."
		return it
	case len(integ.Egress) == 0:
		it.Detail = "Integration " + id + " names no hosts, so nothing becomes reachable."
		return it
	}
	hosts := strings.Join(integ.Egress, ", ")
	secret, header, _, hasHeader := integ.HeaderSecret()
	switch {
	case !hasHeader:
		// No proxy_header-delivered secret is the HONEST state for a system
		// that authenticates outside HTTP, not a gap — the path opening is the
		// whole value.
		it.Status, it.Residency = "satisfied", "none"
		it.Detail = "Opens " + hosts + " for this run. No credential is delivered — this system authenticates outside HTTP."
	case secret == "":
		it.Detail = "Opens " + hosts + ", but names no secret to present in " + header + "."
	case !presentSecrets[secret]:
		it.Detail = "Opens " + hosts + ", but the secret " + secret + " is not in the store, so no credential is presented."
		it.Fix = &SetupFix{Action: "add_secret", SecretName: secret}
	default:
		it.Status, it.Residency = "satisfied", "proxy_injected"
		it.Detail = "Opens " + hosts + " and presents " + secret + " in " + header + ". The sandbox never holds it."
	}
	return it
}
