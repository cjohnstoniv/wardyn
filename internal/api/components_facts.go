// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"slices"
	"strconv"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// componentFact is one row of a dry run door's `components`: something the run
// is given access to, described from what the door already decided. A fact
// decides nothing, and a refused request has no facts — its answer is the
// refusal.
//
// What a fact may say is the member-isolation rule, kept in this one type. An
// organisation's component is here only because the gate admitted it, which it
// does only for a person granted it; that person's sandbox can reach the
// component's destinations, so the fact lists them, as the run's allowed
// domains do. A fact never carries the name of a secret the organisation
// provides, the value of a setting, or a provider row's id: a secret is how it
// is delivered and whose it is, a setting is its key, a provider is its kind.
type componentFact struct {
	Kind types.ComponentKind `json:"kind"`
	// Provider is a git_provider's kind: github or azure_devops.
	Provider string `json:"provider,omitempty"`
	// ID keys the row within one response: a stored component's id in its
	// canonical spelling, "inline:<position in the request>" for one defined on
	// the request, "git_provider:<provider>:<lane>[:<org>]" for a Git provider.
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	Version int    `json:"version,omitempty"` // a stored component's, as attached
	// Reason is why the row is on the run: org, self or inline for a component
	// (who defined it), workspace for a Git provider (its repositories).
	Reason string `json:"reason"`
	Status string `json:"status"`
	// Requirements is what the person has still to supply, as the checklist
	// rows Review shows for the same grants. Never nil.
	Requirements []SetupItem `json:"requirements"`

	// A git_provider's lane, the address its row clones from, and the draft's
	// repositories on it.
	Lane  string   `json:"lane,omitempty"`
	Org   string   `json:"org,omitempty"`
	Repos []string `json:"repos,omitempty"`

	// A component's destinations, its secrets and the keys of its settings.
	Hosts      []string              `json:"hosts,omitempty"`
	Secrets    []componentSecretFact `json:"secrets,omitempty"`
	ConfigKeys []string              `json:"config_keys,omitempty"`
	// SelfDefined: the launcher defined it, so the run reaches destinations
	// they added. AutonomyCap is the organisation's cap on such a run, absent
	// when it set none.
	SelfDefined bool                `json:"self_defined,omitempty"`
	AutonomyCap types.AutonomyLevel `json:"autonomy_cap,omitempty"`
	// VaultFloor: its credential holds the run to the strongest sandbox.
	VaultFloor bool `json:"vault_floor,omitempty"`
	// TLSIntercept: Wardyn opens the connection to one of its hosts to add a
	// header.
	TLSIntercept bool `json:"tls_intercept,omitempty"`
	// HighRisk: Review grades one of its secrets high.
	HighRisk bool `json:"high_risk,omitempty"`
}

// componentSecretFact is one secret of a component: how it reaches the run,
// and whether the organisation provides it. Never its name.
type componentSecretFact struct {
	Delivery string `json:"delivery"` // header | env | file
	Shared   bool   `json:"shared"`
}

// What a fact's status says.
const (
	componentReady = "ready"
	// componentNeedsInput: the person has something to supply (Requirements).
	componentNeedsInput = "needs_input"
	// componentUnavailable: nothing the caller can supply would make it ready —
	// the organisation has not stored a secret it provides, or the caller is no
	// person and the provider wants one's connection.
	componentUnavailable = "unavailable"
	// componentUnknown: the door did not read what would answer it.
	componentUnknown = "unknown"
)

// The lanes a git_provider fact names beyond types.GitLane's: a clone straight
// from the forge with no credential of the run's, and no lane at all.
const (
	gitLaneDirect = "direct"
	gitLaneNone   = "none"
)

// componentFacts is the `components` of Review and of the policy preview: the
// Git providers the draft's repositories live on, then its components in
// request order. Pure — everything it says was decided by the caller's gates.
// scm is Review's git_credential fact; the preview reads no credential and
// passes nil. Nil when the run has neither, so its body is the one it was.
func componentFacts(req createRunRequest, spec types.RunPolicySpec, site types.SiteConfig, comps runComponents, scm *SCMAccess) []componentFact {
	facts := gitProviderFacts(req, spec, site, scm)
	for i, a := range comps.attached {
		facts = append(facts, a.fact(i, comps))
	}
	return facts
}

// fact describes one attached component, at position ordinal of the request.
func (a attachedComponent) fact(ordinal int, comps runComponents) componentFact {
	def := a.snapshot.Definition
	f := componentFact{
		Kind: types.ComponentCustom, ID: "inline:" + strconv.Itoa(ordinal), Name: a.snapshot.Name, Version: a.snapshot.Version,
		Reason: a.source, Status: componentReady, Requirements: []SetupItem{},
		Hosts: def.Hosts, ConfigKeys: sortedKeys(def.Config), SelfDefined: a.snapshot.SelfDefined,
	}
	if a.snapshot.ComponentID != nil {
		f.ID = a.snapshot.ComponentID.String()
	}
	if f.SelfDefined {
		f.AutonomyCap = componentAutonomyCap(comps)
	}
	// The grants the gate expanded the component into: what Review's checklist,
	// the confinement floor and the risk grade read, so the fact says what they
	// say.
	var grants, missing []types.GrantSpec
	for _, sec := range def.Secrets {
		f.Secrets = append(f.Secrets, componentSecretFact{Delivery: sec.Delivery.Mode, Shared: sec.Shared})
		g := componentGrant(sec)
		grants = append(grants, g)
		if sec.Delivery.Mode == types.ComponentDeliveryHeader && !sec.Delivery.PlainHTTP {
			f.TLSIntercept = true
		}
		if !sec.Shared && slices.Contains(a.needsOwnSecrets, sec.SecretName) {
			missing = append(missing, g)
		}
	}
	f.Requirements = append(f.Requirements, setupSecretItems(types.RunPolicySpec{EligibleGrants: missing}, nil)...)
	switch {
	case a.needsAdminSecret:
		f.Status = componentUnavailable
	case len(f.Requirements) > 0:
		f.Status = componentNeedsInput
	}
	own := types.RunPolicySpec{EligibleGrants: grants}
	f.VaultFloor = comps.settings.RequireVaultForCredentials && composer.RequiredConfinementFloor(own) == types.CC3
	f.HighRisk = slices.ContainsFunc(composer.Grade(composer.RunInput{}, own), func(item composer.RiskItem) bool {
		return item.Level == composer.RiskHigh && strings.HasPrefix(item.Field, "eligible_grants[")
	})
	return f
}

// gitProviderFacts is one fact per Git provider, lane and organisation the
// draft's repositories classify to, in the order the repositories are named.
// A repository on another forge is no Git provider's (repository_access calls
// it "other").
func gitProviderFacts(req createRunRequest, spec types.RunPolicySpec, site types.SiteConfig, scm *SCMAccess) []componentFact {
	var facts []componentFact
	for _, repo := range previewRepos(req, spec, site) {
		if repo.kind != string(types.GitProviderGitHub) && repo.kind != string(types.GitProviderAzureDevOps) {
			continue
		}
		lane := gitProviderLane(repo, spec, site)
		id := string(types.ComponentGitProvider) + ":" + repo.kind + ":" + lane
		if repo.org != "" {
			id += ":" + repo.org
		}
		i := slices.IndexFunc(facts, func(f componentFact) bool { return f.ID == id })
		if i < 0 {
			facts = append(facts, componentFact{
				Kind: types.ComponentGitProvider, Provider: repo.kind, ID: id, Reason: "workspace",
				Status: gitProviderStatus(repo, scm), Requirements: []SetupItem{}, Lane: lane, Org: repo.org,
			})
			i = len(facts) - 1
		}
		if !slices.Contains(facts[i].Repos, repo.url) {
			facts[i].Repos = append(facts[i].Repos, repo.url)
		}
	}
	return facts
}

// gitProviderLane is the credential lane a repository's clone will take, read
// off what the doors already decided: the row that admitted the repository and
// the grants on the run's spec. It asks the predicates dispatch asks
// (adoEntraRunForRepo, laneAllowed) and vetoes nothing itself; dispatch's veto
// stays the authority on what is wired.
func gitProviderLane(repo previewRepo, spec types.RunPolicySpec, site types.SiteConfig) string {
	// The per-person Azure DevOps lane is the row's alone: no grant selects it.
	if run, ok := adoEntraRunForRepo(site, repo.locator, ""); ok {
		if run.serverHost != "" {
			return string(types.GitLanePAT)
		}
		return string(types.GitLaneEntra)
	}
	row := repo.verdict.Provider
	decided := repo.verdict.Admitted && row.ID != ""
	for _, c := range []struct {
		lane  types.GitLane
		grant types.GrantKind
	}{
		{types.GitLaneApp, types.GrantGitHubToken},
		{types.GitLanePAT, types.GrantGitPAT},
		{types.GitLaneSSH, types.GrantSSHKey},
	} {
		// The row that admitted decides its lanes; with none, nothing vetoes.
		if decided && !laneAllowed(row, c.lane) {
			continue
		}
		if slices.ContainsFunc(spec.EligibleGrants, func(g types.GrantSpec) bool { return g.Kind == c.grant && gitGrantServes(g, repo) }) {
			return string(c.lane)
		}
	}
	if directGitHubRepo(repo.locator) && (spec.AllowAllEgress || slices.Contains(spec.AllowedDomains, "github.com")) {
		return gitLaneDirect
	}
	return gitLaneNone
}

// gitGrantServes reports whether a git grant is for the repository's forge: the
// broker's own forges for a github_token, the host its scope names otherwise.
func gitGrantServes(g types.GrantSpec, repo previewRepo) bool {
	switch g.Kind {
	case types.GrantGitHubToken:
		return slices.Contains(gitBrokerForges, repo.host)
	case types.GrantGitPAT:
		sc, err := types.DecodeGitPATScope(g.Scope)
		return err == nil && strings.EqualFold(sc.Host, repo.host)
	case types.GrantSSHKey:
		host, _, _, _, err := sshKeyScopeFields(g.Scope)
		return err == nil && strings.EqualFold(canonicalProviderHost(host), repo.host)
	}
	return false
}

// gitProviderStatus is the person's connection to the provider where the door
// read it: Review's git_credential fact, for the Azure DevOps organisation it
// graded. Every other row — the preview's, and a GitHub one, which no door
// grades yet — is unknown rather than a guess. A caller that is no person (the
// shared admin token) has no connection to make: unavailable, never a prompt
// to sign in.
func gitProviderStatus(repo previewRepo, scm *SCMAccess) string {
	if scm == nil || scm.Kind != repo.kind || scm.Org != repo.org {
		return componentUnknown
	}
	switch scm.State {
	case modelAccessLive, modelAccessExpiring:
		return componentReady
	case modelAccessNotApplicable:
		return componentUnavailable
	}
	return componentNeedsInput
}
