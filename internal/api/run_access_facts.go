// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/agentpolicy"
	"github.com/cjohnstoniv/wardyn/internal/ghscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// The access source kinds beyond provenance's own closed set. The console
// renders no chip for a kind it does not know.
const (
	provKindHarness = "harness" // an included harness
	provKindPosture = "posture" // a harness's tool-lane default (the harness facts)

	// ToolPolicyFact.Lane and .Enforcer.
	toolLaneGated       = "gated"
	toolLaneUngated     = "ungated"
	toolLaneAttended    = "attended"
	toolLaneUnsupported = "unsupported"
	toolEnforcerGate    = "wardyn_gate"
	toolEnforcerHarness = "harness_settings"
	toolEnforcerNone    = "none"
)

// What an access row is and which chip it carries.
const (
	accessKindHarness    = "harness"
	accessKindRepository = "repository"
	accessKindCommand    = "command"
	accessKindSecret     = "secret"
	accessKindHost       = "host"
	accessKindCustom     = "custom"
	accessKindCLI        = "cli"
	accessFromAdded      = "added"
)

// A row's status.
const (
	accessReady       = "ready"
	accessNeedsSetup  = "needs_setup"
	accessUnavailable = "unavailable"
	accessRefused     = "refused"
)

// How a host is reached, its verdict and why it is not allowed.
const (
	viaEgress    = "egress"
	viaModelLane = "model_lane"
	// viaInjected is a host the proxy opens and terminates to add a credential
	// itself (the GitHub CLI's api.github.com).
	viaInjected = "injected"

	verdictAllowed       = "allowed"
	verdictNeedsApproval = "needs_approval"
	verdictNotAllowed    = "not_allowed"

	hostReasonClamped    = "clamped"
	hostReasonDeniedList = "denied_list"
	hostReasonAlwaysDeny = "always_deny"

	// accessPolicyRow is a host's row key when no row brings it.
	accessPolicyRow = "policy"
)

// How a credential reaches the run, whose it is and what is left of it once
// the run ends.
const (
	deliveryProxy        = "proxy"
	deliveryGitBroker    = "git_broker"
	deliveryGitHelper    = "git_helper"
	deliverySandboxEnv   = "sandbox_env"
	deliverySandboxFile  = "sandbox_file"
	deliveryShortLived   = "sandbox_short_lived"
	credOwnerOwn         = "own"
	credOwnerShared      = "shared"
	credOwnerOrgApp      = "org_app"
	afterEndGone         = "gone"
	afterEndExpires      = "expires"
	afterEndStays        = "stays"
	credKindModel        = "model_provider"
	credKindGitHubApp    = "github_app"
	credKindADOSignIn    = "ado_signin"
	credKindGitPAT       = "git_pat"
	credKindSSHKey       = "ssh_key"
	credKindSecret       = "secret"
	credKindAWSRole      = "aws_role"
	signInModelProvider  = "model_provider"
	signInAWSCLI         = "aws_cli"
	signInAzureDevOps    = "azure_devops"
	fixSignIn            = "sign_in"
	fixChooseProvider    = "choose_model_provider"
	deciderYou           = "you"
	deciderAdmin         = "admin"
	deciderYouOrAdmin    = "you_or_admin"
	accessCommandRowID   = "command"
	accessSecretRowIDPfx = "secret:"
	accessHostRowIDPfx   = "host:"
)

// accessBlock is the `access` of the policy preview and of Review
// (client.AccessFacts on the wire): what the draft brings in, projected from
// what the fold decided. Setup carries the full checklist row, fix included.
type accessBlock struct {
	Rows        []client.AccessRow        `json:"rows"`
	Hosts       []client.AccessHost       `json:"hosts"`
	Credentials []client.AccessCredential `json:"credentials"`
	Unlisted    client.AccessUnlisted     `json:"unlisted"`
	Harnesses   []client.ToolPolicyFact   `json:"harnesses"`
	Setup       []SetupItem               `json:"setup"`
}

// accessInputs is what a door hands accessFacts, after the componentFactInputs
// rule: only what the fold admitted crosses, never the fold or its mode. The
// door fills the fold's half; accessFor adds its own reads.
type accessInputs struct {
	req      *createRunRequest
	spec     types.RunPolicySpec
	prov     []provenanceRow
	source   policySourceRecord
	scmSite  types.SiteConfig
	mpChoice runProviderChoice
	// facts are the component facts the door built from the same fold, and
	// comps the components the gate admitted for them.
	facts []componentFact
	comps runComponents
	// decider is who decides a request for an unlisted host (accessDecider).
	decider string
	// providerStored: the caller has stored a credential for the chosen
	// provider (presence, never liveness; true when nothing can be read).
	providerStored bool
	// ssoProxyInject is WARDYN_AWS_SSO_PROXY_INJECT as dispatch reads it.
	ssoProxyInject bool
	// imageKey is the convention image the run starts from (accessImageKey).
	imageKey string
	// level is the autonomy level the fold resolved, which picks an
	// interactive run's managed settings (attendedDefaultReasons).
	level types.AutonomyLevel
}

// accessFor adds the reads accessFacts needs beyond the fold and builds the
// block. Both dry doors call it, so the block is one projection however the
// door was reached.
func (s *Server) accessFor(r *http.Request, in accessInputs) (*accessBlock, error) {
	var err error
	if in.decider, err = s.accessDecider(r.Context()); err != nil {
		return nil, err
	}
	in.providerStored, in.ssoProxyInject = s.accessProviderStored(r, in.mpChoice), s.cfg.AWSSSOProxyInject
	in.imageKey = accessImageKey(*in.req, s.cfg.AgentImages)
	out := accessFacts(in)
	return &out, nil
}

// accessDecider is who decides a request for a host nothing lists: an egress
// approval is the run owner's when their egress_host capability covers the
// host, an admin's otherwise (authorizeUserDecision). A security operator, or a
// caller egress_host does not bound, decides all of them; a caller holding some
// allows decides those.
func (s *Server) accessDecider(ctx context.Context) (string, error) {
	b := s.capBatchFor(ctx)
	if b.operator || b.noStore {
		return deciderYou, nil
	}
	if on, err := b.enforced(ctx, capEgressHost); err != nil || !on {
		return deciderYou, err
	}
	if _, _, err := b.scan(ctx, capEgressHost, ""); err != nil {
		return "", err
	}
	allows := slices.DeleteFunc(slices.Clone(b.byKind[capEgressHost]), func(g types.CapabilityGrant) bool {
		return g.Effect != types.CapabilityAllow
	})
	switch {
	case slices.ContainsFunc(allows, func(g types.CapabilityGrant) bool { return strings.TrimSpace(g.Value) == "*" }):
		return deciderYou, nil
	case len(allows) > 0:
		return deciderYouOrAdmin, nil
	}
	return deciderAdmin, nil
}

// accessProviderStored reports whether the caller has stored a credential for
// the chosen model provider: a names-only list of their own namespace, so no
// door reads a credential value for it. True when there is nothing to ask
// (no provider, no person) or the store cannot answer: the setup item it
// gates is a prompt, and a launch still checks the credential itself.
func (s *Server) accessProviderStored(r *http.Request, choice runProviderChoice) bool {
	owner := runIdentitySubject(r.Context(), principalFromRequest(r))
	if !choice.chosen || s.cfg.Secrets == nil || s.providerAccessMechanism(owner) {
		return true
	}
	stored, err := s.secretPresentIn(r.Context(), []string{owner}, providerCredentialName(choice.provider))
	return stored || err != nil
}

// accessFacts projects the fold onto step 5's rows, hosts and credentials. It
// is pure: everything it reads is in its input.
func accessFacts(in accessInputs) accessBlock {
	b := &accessBuilder{in: in, hosts: map[string]*client.AccessHost{}, out: accessBlock{
		Rows: []client.AccessRow{}, Hosts: []client.AccessHost{}, Credentials: []client.AccessCredential{},
		Harnesses: []client.ToolPolicyFact{}, Setup: []SetupItem{},
	}}
	for _, fact := range in.facts {
		switch fact.Kind {
		case types.ComponentAgent:
			b.harnessRow(fact)
		case types.ComponentGitProvider:
			b.repositoryRow(fact)
		}
	}
	if in.req.TaskMode == "exec" {
		b.addRow(client.AccessRow{ID: accessCommandRowID, Kind: accessKindCommand, From: accessKindCommand, Name: "Command",
			Status: accessReady, Source: client.ProvenanceSource{Kind: provKindPerson}})
	}
	for _, fact := range in.facts {
		if fact.Kind != types.ComponentAgent && fact.Kind != types.ComponentGitProvider {
			b.componentRow(fact)
		}
	}
	b.secretRows()
	b.personHostRows()
	for _, host := range in.spec.AllowedDomains {
		b.addHost(host, viaEgress, accessPolicyRow)
	}
	b.settleHosts()
	b.out.Unlisted = accessUnlisted(in.spec, in.decider)
	return b.out
}

// accessBuilder accumulates one block; hosts indexes out.Hosts' entries while
// rows are added, in the order they are first brought.
type accessBuilder struct {
	in    accessInputs
	out   accessBlock
	order []string
	hosts map[string]*client.AccessHost
}

func (b *accessBuilder) addRow(row client.AccessRow) {
	if row.Hosts == nil {
		row.Hosts = []string{}
	}
	row.Credentials = []string{}
	b.out.Rows = append(b.out.Rows, row)
}

// addHost records that rowID brings host, reached via via.
func (b *accessBuilder) addHost(host, via, rowID string) {
	h, ok := b.hosts[host]
	if !ok {
		h = &client.AccessHost{Host: host, Via: via, Rows: []string{}}
		b.hosts[host] = h
		b.order = append(b.order, host)
	}
	if rowID != accessPolicyRow || len(h.Rows) == 0 {
		h.Rows = append(h.Rows, rowID)
	}
	if rowID != accessPolicyRow {
		h.Rows = slices.DeleteFunc(h.Rows, func(r string) bool { return r == accessPolicyRow })
	}
	h.Rows = slices.Compact(h.Rows)
}

// addCredential appends a credential to the last row and to the total.
func (b *accessBuilder) addCredential(c client.AccessCredential) {
	row := &b.out.Rows[len(b.out.Rows)-1]
	c.Row = row.ID
	row.Credentials = append(row.Credentials, c.ID)
	b.out.Credentials = append(b.out.Credentials, c)
}

func (b *accessBuilder) addSetup(items ...SetupItem) {
	for _, it := range items {
		if !slices.ContainsFunc(b.out.Setup, func(have SetupItem) bool { return have.ID == it.ID }) {
			b.out.Setup = append(b.out.Setup, it)
		}
	}
}

// harnessRow is the included harness: its model lane's hosts, the model
// credential and what the person still has to choose or connect.
func (b *accessBuilder) harnessRow(fact componentFact) {
	in, agent := b.in, fact.Agent
	row := client.AccessRow{ID: fact.ID, Kind: accessKindHarness, From: accessKindHarness, Name: agent.Agent, Status: accessReady,
		Source: client.ProvenanceSource{Kind: provKindHarness, Name: agent.Agent}}
	for _, h := range agent.Hosts {
		row.Hosts = append(row.Hosts, h.Host)
	}
	var setup []SetupItem
	_, needsModel := agentLLMProvider(in.req.Agent)
	switch {
	case needsModel && createDoorIsModelRun(*in.req) && !in.mpChoice.chosen:
		setup = append(setup, SetupItem{Kind: "llm_access", ID: "llm_access:" + agent.Agent, Label: "Choose a model provider",
			RequiredBy: agent.Agent, Status: "missing", Fix: &SetupFix{Action: fixChooseProvider, Tool: agent.Agent}})
	case in.mpChoice.chosen && !in.providerStored:
		p := in.mpChoice.provider
		// No provider id: a fact never carries one. The console signs in to
		// the provider it chose for this harness.
		setup = append(setup, SetupItem{Kind: "llm_access", ID: "llm_access:" + agent.Agent, Label: "Connect " + p.Name,
			RequiredBy: agent.Agent, Status: "missing", Fix: &SetupFix{Action: fixSignIn, Target: signInModelProvider}})
	}
	if len(setup) > 0 {
		row.Status = accessNeedsSetup
	}
	b.addRow(row)
	for _, h := range row.Hosts {
		b.addHost(h, viaModelLane, row.ID)
	}
	for _, c := range modelCredentials(row.ID, in.mpChoice, in.ssoProxyInject) {
		b.addCredential(c)
	}
	b.addSetup(setup...)
	b.out.Harnesses = append(b.out.Harnesses, b.toolPolicyFact(agent.Agent))
}

// toolPolicyFact is one included harness's tool policy as step 5 shows it: the
// lane its calls take and who enforces it, its default and the effects the
// person may choose (each greyed with its reason where the policy is stricter
// or the harness cannot honour it), and the policy's rules, locked. What it
// offers is what the server accepts: toolRulesWithinPolicyRefusal,
// harnessToolRulesRefusal and attendedDefaultRefusal answer the same cases.
func (b *accessBuilder) toolPolicyFact(harness string) client.ToolPolicyFact {
	req, rules := *b.in.req, b.in.spec.ToolRules
	fact := client.ToolPolicyFact{Harness: harness, Lane: toolLaneUngated, Enforcer: toolEnforcerNone, RuleTools: "any",
		PolicyRules: []client.LockedToolRule{}, MCPServers: mcpServersFor(b.in.comps, harness)}
	for _, r := range rules {
		fact.PolicyRules = append(fact.PolicyRules, client.LockedToolRule{Tool: r.Tool, Effect: r.Effect, Source: b.toolRuleSource(r)})
	}
	// What this harness on this lane can never take, for its default and for a
	// rule, by effect.
	var forDefault, forRule map[types.ToolEffect]string
	switch {
	case harness == codexHarness:
		fact.RuleTools = "mcp_only"
		// Its MCP tools' rules are enforced by the broker the servers sit behind.
		if len(fact.MCPServers) > 0 {
			fact.Enforcer = toolEnforcerMCPBroker
		}
		forDefault = map[types.ToolEffect]string{types.ToolHold: "harness_no_hold", types.ToolDeny: "harness_no_block_builtin"}
		forRule = map[types.ToolEffect]string{types.ToolHold: "harness_no_hold"}
	case !agentHasHoldLane(harness):
		fact.Lane = toolLaneUnsupported
		forDefault = map[types.ToolEffect]string{types.ToolAllow: "custom_harness_rules", types.ToolHold: "custom_harness_rules", types.ToolDeny: "custom_harness_rules"}
		forRule = forDefault
	case requestIsInteractive(req):
		fact.Lane, fact.Enforcer = toolLaneAttended, toolEnforcerHarness
		forDefault = attendedDefaultReasons(b.in.level)
	case holdLane(false, req.ToolApprovals, req.TaskMode):
		fact.Lane, fact.Enforcer = toolLaneGated, toolEnforcerGate
	}
	policyDefault := policyToolEffect(rules, toolLaneDefault(req), "*")
	fact.Default = client.ToolDefaultFact{Effective: policyDefault, Source: b.toolRuleSource(types.ToolRule{Tool: "*", Effect: policyDefault}),
		Options: toolEffectOptions(policyDefault, forDefault, "policy_stricter_default")}
	if i := slices.IndexFunc(req.Tools, func(t client.IncludedTool) bool { return t.ID == harness }); i >= 0 &&
		effectRank(req.Tools[i].DefaultEffect) > effectRank(policyDefault) {
		fact.Default.Effective, fact.Default.Source = req.Tools[i].DefaultEffect, client.ProvenanceSource{Kind: provKindPerson}
	}
	if fact.Lane == toolLaneAttended {
		// Claude Code's settings name tools (agentpolicy.WithToolRules), never "*":
		// a tool no rule names is answered by the mode the level's document sets,
		// whatever the policy's "*", which is therefore no locked row here.
		fact.Default = attendedDefaultFact(b.in.level, forDefault)
		fact.PolicyRules = slices.DeleteFunc(fact.PolicyRules, func(r client.LockedToolRule) bool { return r.Tool == "*" })
	}
	fact.RuleEffects = toolEffectOptions(policyDefault, forRule, "policy_stricter_tool")
	return fact
}

// attendedSettingsAcceptEdits is the Source.ID of an interactive Claude Code
// run's default where its level's document starts the session accepting file
// edits (L2, locked or not): edits run without asking, every other tool asks.
const attendedSettingsAcceptEdits = "accept_edits"

// attendedDefaultFact is what an interactive Claude Code run does with a tool no
// rule names, and what answers that: the level's managed settings asking (L0,
// L1); the same settings, which accept file edits unasked (L2); or, at a level
// that brings no document, the lane's own default (posture), which a
// repository's settings or hooks may answer first.
func attendedDefaultFact(level types.AutonomyLevel, cannot map[types.ToolEffect]string) client.ToolDefaultFact {
	src := client.ProvenanceSource{Kind: provKindHarness}
	switch _, doc, ok := agentpolicy.ForAgent(claudeCodeHarness, level, false, false); {
	case !ok:
		src = client.ProvenanceSource{Kind: provKindPosture}
	case agentpolicy.AcceptsEdits(doc):
		src.ID = attendedSettingsAcceptEdits
	}
	return client.ToolDefaultFact{Effective: types.ToolHold, Source: src, Options: toolEffectOptions(types.ToolHold, cannot, "policy_stricter_default")}
}

// toolEffectOptions is each effect a person may choose against the policy's
// effect: the harness's own reason where it cannot take it, stricter where the
// policy is stricter, available otherwise.
func toolEffectOptions(policy types.ToolEffect, cannot map[types.ToolEffect]string, stricter string) []client.EffectOption {
	var out []client.EffectOption
	for _, e := range []types.ToolEffect{types.ToolAllow, types.ToolHold, types.ToolDeny} {
		opt := client.EffectOption{Effect: e, Available: true}
		switch {
		case cannot[e] != "":
			opt.Available, opt.Reason = false, cannot[e]
		case effectRank(e) < effectRank(policy):
			opt.Available, opt.Reason = false, stricter
		}
		out = append(out, opt)
	}
	return out
}

// toolRuleSource is who put a tool rule on the run, from the fold's provenance:
// the source that added it, else the policy the run started from. A "*" that no
// rule sets is the lane's own default.
func (b *accessBuilder) toolRuleSource(r types.ToolRule) client.ProvenanceSource {
	for _, p := range b.in.prov {
		if p.Field == fieldToolRules && p.Value == r.Tool+":"+string(r.Effect) && p.Effect == provEffectAdded {
			return client.ProvenanceSource(p.Source)
		}
	}
	if !slices.Contains(b.in.spec.ToolRules, r) {
		return client.ProvenanceSource{Kind: provKindHarness}
	}
	return client.ProvenanceSource(policyProvSource(b.in.source))
}

// modelCredentials is what the chosen provider hands the run. A key, token or
// subscription is set by the proxy and nothing of it is left in the sandbox.
// An AWS sign-in puts role credentials in the sandbox, issued by the SDK
// there; with WARDYN_AWS_SSO_PROXY_INJECT off the session's access token is
// in the sandbox too, as a file, and the role credentials last only as long
// as it does (no refresh token is staged). Both expire at their own lifetime,
// which AWS sets, not the run's end: lifetime_sec stays 0.
func modelCredentials(rowID string, choice runProviderChoice, ssoProxyInject bool) []client.AccessCredential {
	if !choice.chosen {
		return nil
	}
	c := client.AccessCredential{ID: rowID + ":model", Kind: credKindModel, Owner: credOwnerOwn, Delivery: deliveryProxy, AfterEnd: afterEndGone}
	if choice.provider.Kind != types.ModelProviderBedrockSSO {
		return []client.AccessCredential{c}
	}
	c.Delivery, c.AfterEnd, c.Renewed = deliveryShortLived, afterEndExpires, ssoProxyInject
	if ssoProxyInject {
		return []client.AccessCredential{c}
	}
	return []client.AccessCredential{c, {ID: rowID + ":sso_token", Kind: credKindModel, Owner: credOwnerOwn,
		Delivery: deliverySandboxFile, AfterEnd: afterEndExpires}}
}

// repositoryRow is one Git provider the draft's repositories live on. Its
// hosts and credential follow the lane: the broker's has no raw host at all.
func (b *accessBuilder) repositoryRow(fact componentFact) {
	row := client.AccessRow{ID: fact.ID, Kind: accessKindRepository, From: accessKindRepository,
		Name: cmp.Or(fact.Org, fact.Provider), Status: accessStatus(fact.Status), Source: client.ProvenanceSource{Kind: provKindWorkspace}}
	grant, perPerson := laneServingGrant(*b.in.req, b.in.spec, b.in.scmSite, fact)
	var cred *client.AccessCredential
	switch types.GitLane(fact.Lane) {
	case types.GitLaneApp:
		cred = &client.AccessCredential{Kind: credKindGitHubApp, Owner: credOwnerOrgApp, Delivery: deliveryGitBroker, AfterEnd: afterEndGone}
	case types.GitLaneEntra:
		cred = &client.AccessCredential{Kind: credKindADOSignIn, Owner: credOwnerOwn, Delivery: deliveryProxy, AfterEnd: afterEndGone}
	case types.GitLanePAT, types.GitLaneSSH:
		kind := map[string]string{string(types.GitLanePAT): credKindGitPAT, string(types.GitLaneSSH): credKindSSHKey}[fact.Lane]
		cred = &client.AccessCredential{Kind: kind, Owner: credOwnerShared, Delivery: deliveryGitHelper, AfterEnd: afterEndStays}
		switch {
		case perPerson:
			cred.Owner, cred.Delivery, cred.AfterEnd = credOwnerOwn, deliveryProxy, afterEndGone
		case grant == nil:
			cred = nil // create drops the lane: no credential, no host
		default:
			if grant.OwnerOnly {
				cred.Owner = credOwnerOwn
			}
			if sc, err := types.DecodeGitPATScope(grant.Scope); err == nil && grant.Kind == types.GrantGitPAT {
				row.Hosts = append(row.Hosts, sc.Host)
			}
			row.Hosts = append(row.Hosts, grantLaneEgress(*grant)...)
		}
	case gitLaneDirect:
		row.Hosts = slices.Clone(directGitHubEgressHosts)
	}
	b.addRow(row)
	for _, h := range row.Hosts {
		b.addHost(h, viaEgress, row.ID)
	}
	if cred != nil {
		cred.ID = row.ID + ":credential"
		b.addCredential(*cred)
	}
	b.addSetup(fact.Requirements...)
	// The door grades the person's own sign-in only where it read it (see
	// gitProviderStatus): a row that needs input is theirs to connect.
	if fact.Provider == string(types.GitProviderAzureDevOps) && fact.Status == componentNeedsInput {
		b.addSetup(SetupItem{Kind: "repo_credential", ID: "repo_credential:azure_devops:" + fact.Org, Label: "Sign in to Azure DevOps",
			RequiredBy: row.Name, Status: "missing", Fix: &SetupFix{Action: fixSignIn, Target: signInAzureDevOps}})
	}
}

// directGitHubEgressHosts are the hosts a direct GitHub clone reaches
// (unionDirectGitHubEgress's candidates).
var directGitHubEgressHosts = []string{"github.com", "*.githubusercontent.com"}

// laneServingGrant is the wired grant (lanesWiredFor) that serves a pat or ssh
// repository row, or perPerson when the row is the person's own Azure DevOps
// lane, which no grant selects. nil for a lane create will not wire. The lane is gitProviderLane's, asked of each repository the row holds.
func laneServingGrant(req createRunRequest, spec types.RunPolicySpec, site types.SiteConfig, fact componentFact) (*types.GrantSpec, bool) {
	want := map[string]types.GrantKind{string(types.GitLanePAT): types.GrantGitPAT, string(types.GitLaneSSH): types.GrantSSHKey}[fact.Lane]
	if want == "" {
		return nil, false
	}
	for _, repo := range previewRepos(req, spec, site) {
		if !slices.Contains(fact.Repos, repo.url) || gitProviderLane(repo, spec, site) != fact.Lane {
			continue
		}
		if _, ok := adoEntraRunForRepo(site, repo.locator, ""); ok {
			return nil, true
		}
		wired := lanesWiredFor(site, req.Agent, repoLocatorsOf(spec.WorkspaceRepos), spec.EligibleGrants)
		if i := slices.IndexFunc(wired, func(g types.GrantSpec) bool { return g.Kind == want && gitGrantServes(g, repo) }); i >= 0 {
			return &wired[i], false
		}
	}
	return nil, false
}

// componentRow is an attached component: its hosts, each secret's delivery
// and what the person has still to store.
func (b *accessBuilder) componentRow(fact componentFact) {
	row := client.AccessRow{ID: fact.ID, Kind: accessKindCustom, From: accessFromAdded, Name: fact.Name,
		Status: accessStatus(fact.Status), Hosts: slices.Clone(fact.Hosts),
		Source: client.ProvenanceSource{Kind: provKindComponent, ID: fact.ID, Name: fact.Name}}
	cli := attachedCLI(b.in.comps, fact.ID)
	githubCLI := cli != nil && cli.Kind == types.CLIKindGitHub
	if cli != nil {
		row.Kind, row.InImage = accessKindCLI, cliToolInImage(cliInImage(b.in.imageKey), cli.Kind)
	}
	if githubCLI && len(githubCLIRepos(*b.in.req, b.in.spec, b.in.scmSite)) == 0 {
		row.Status, row.Reason, githubCLI = accessUnavailable, reasonCLINoGitHubRepo, false
	}
	if githubCLI && !slices.Contains(row.Hosts, ghscope.APIHost) {
		row.Hosts = append(row.Hosts, ghscope.APIHost)
	}
	mcp, mcpHost := attachedMCP(b.in.comps, fact.ID)
	if mcp != nil {
		row.Kind, row.MCP = accessKindMCP, mcp
	}
	b.addRow(row)
	if githubCLI {
		b.addHost(ghscope.APIHost, viaInjected, row.ID)
		b.addCredential(client.AccessCredential{ID: row.ID + ":cli", Kind: credKindGitHubApp, Owner: credOwnerOrgApp, Delivery: deliveryProxy, AfterEnd: afterEndGone})
	}
	for _, h := range row.Hosts {
		via := viaEgress
		if d, err := types.ParseDestination(h); mcp != nil && err == nil && d.Host == mcpHost {
			via = viaMCPBroker
		}
		b.addHost(h, via, row.ID)
	}
	if cli != nil && cli.Kind == types.CLIKindAWS && cli.AWS != nil {
		// The portal the proxy sets the person's session on, and the role
		// credentials AWS issues into the sandbox there, renewed while the run
		// lives and valid at AWS until they expire.
		b.addHost(ssoPortalHost(cli.AWS.SSORegion, ""), viaInjected, row.ID)
		b.addCredential(client.AccessCredential{ID: row.ID + ":aws_role", Kind: credKindAWSRole, Owner: credOwnerOwn,
			Delivery: deliveryShortLived, Renewed: true, AfterEnd: afterEndExpires})
	}
	for i, sec := range fact.Secrets {
		c := client.AccessCredential{ID: row.ID + ":secret:" + strconv.Itoa(i), Kind: credKindSecret, Owner: credOwnerOwn,
			Delivery: deliveryProxy, AfterEnd: afterEndGone}
		if sec.Shared {
			c.Owner = credOwnerShared
		}
		switch sec.Delivery {
		case types.ComponentDeliveryEnv:
			c.Delivery, c.AfterEnd = deliverySandboxEnv, afterEndStays
		case types.ComponentDeliveryFile:
			c.Delivery, c.AfterEnd = deliverySandboxFile, afterEndStays
		}
		b.addCredential(c)
	}
	b.addSetup(fact.Requirements...)
}

// attachedCLI is the CLI block of the attached component a fact describes, or
// nil when it carries none.
func attachedCLI(comps runComponents, factID string) *types.ComponentCLI {
	for i, a := range comps.attached {
		if componentProvSource(a, i).ID == factID {
			return a.snapshot.Definition.CLI
		}
	}
	return nil
}

// cliToolInImage is one CLI kind's column of an image's install fact.
func cliToolInImage(f cliImageFact, kind string) string {
	switch kind {
	case types.CLIKindAzure:
		return f.AZ
	case types.CLIKindAWS:
		return f.AWS
	case types.CLIKindGitHub:
		return f.GH
	}
	return cliInImageUnknown
}

// accessImageKey is the convention image key a run starts from: its harness's
// catalogue key. A run that names its own image (or a workspace that set one)
// and an agent the operator pinned in WARDYN_AGENT_IMAGES have none, so
// cliInImage answers unknown for them.
func accessImageKey(req createRunRequest, images map[string]string) string {
	if strings.TrimSpace(req.Image) != "" {
		return ""
	}
	if _, pinned := images[req.Agent]; pinned {
		return ""
	}
	if def, ok := harnessByID(req.Agent); ok && def.ImageKey != "" {
		return def.ImageKey
	}
	return req.Agent
}

// secretRows are the person's own secrets added by header
// (overrides.agent.add_secrets). Every door refuses one the caller has not
// stored (applySecretOverrides), so each row here is a stored secret the fold
// granted.
func (b *accessBuilder) secretRows() {
	if o := b.in.req.Overrides; o == nil || o.Agent == nil {
		return
	}
	for _, sec := range b.in.req.Overrides.Agent.AddSecrets {
		row := client.AccessRow{ID: accessSecretRowIDPfx + sec.SecretName, Kind: accessKindSecret, From: accessFromAdded,
			Name: sec.SecretName, Status: accessReady, Hosts: []string{strings.ToLower(sec.Host)},
			Source: client.ProvenanceSource{Kind: provKindPerson}}
		b.addRow(row)
		b.addHost(row.Hosts[0], viaEgress, row.ID)
		b.addCredential(client.AccessCredential{ID: row.ID, Kind: credKindSecret, Owner: credOwnerOwn, Delivery: deliveryProxy, AfterEnd: afterEndGone})
	}
}

// personHostRows are the hosts the person asked to allow
// (overrides.hosts.add_allowed), whether the ceiling kept them or not.
func (b *accessBuilder) personHostRows() {
	for _, p := range b.in.prov {
		if p.Field != fieldAllowed || p.Source.Kind != provKindPerson || (p.Effect != provEffectAdded && p.Effect != provEffectClamped) {
			continue
		}
		id := accessHostRowIDPfx + p.Value
		if slices.ContainsFunc(b.out.Rows, func(r client.AccessRow) bool { return r.ID == id }) {
			continue
		}
		b.addRow(client.AccessRow{ID: id, Kind: accessKindHost, From: accessFromAdded, Name: p.Value, Status: accessReady,
			Hosts: []string{p.Value}, Source: client.ProvenanceSource{Kind: provKindPerson}})
		b.addHost(p.Value, viaEgress, id)
	}
}

// settleHosts gives every host its verdict and source, in the order rows
// brought them, and settles the one-host rows' status (singleHostRowStatus).
func (b *accessBuilder) settleHosts() {
	spec := b.in.spec
	for _, host := range b.order {
		h := b.hosts[host]
		src, clamped, found := accessHostSource(b.in.prov, b.in.source, host)
		if !found && h.Rows[0] != accessPolicyRow {
			// A lane's host the fold never put on the spec: its row's source.
			src = b.out.Rows[slices.IndexFunc(b.out.Rows, func(r client.AccessRow) bool { return r.ID == h.Rows[0] })].Source
		}
		h.Source = src
		h.Verdict, h.Reason = accessHostVerdict(*h, spec, clamped)
		b.out.Hosts = append(b.out.Hosts, *h)
		for i := range b.out.Rows {
			if r := &b.out.Rows[i]; (r.Kind == accessKindHost || r.Kind == accessKindSecret) && slices.Equal(r.Hosts, []string{host}) {
				r.Status = singleHostRowStatus(r.Kind, h.Verdict, r.Status)
			}
		}
	}
}

// singleHostRowStatus is a one-host row's status once its host has a verdict.
// Either kind is refused when the host is not allowed. A secret added by
// header is injected only on an allowlisted host, so one whose host waits on
// approval needs that settled first; an allowed-host row under approval is
// the request itself and stays as it was.
func singleHostRowStatus(kind, verdict, status string) string {
	switch {
	case verdict == verdictNotAllowed:
		return accessRefused
	case verdict == verdictNeedsApproval && kind == accessKindSecret:
		return accessNeedsSetup
	}
	return status
}

// accessHostSource is who put host on the run, from the fold's provenance:
// the source that added it, else the person whose request the ceiling
// clamped (clamped true), else (found false) the policy the run started from.
func accessHostSource(prov []provenanceRow, policy policySourceRecord, host string) (src client.ProvenanceSource, clamped, found bool) {
	var clampedBy *provSource
	for _, p := range prov {
		if p.Field != fieldAllowed || !hostEqual(p.Value, host) {
			continue
		}
		switch p.Effect {
		case provEffectAdded:
			return client.ProvenanceSource(p.Source), false, true
		case provEffectClamped:
			if clampedBy == nil {
				src := p.Source
				clampedBy = &src
			}
		}
	}
	if clampedBy != nil {
		return client.ProvenanceSource(*clampedBy), true, true
	}
	return client.ProvenanceSource(policyProvSource(policy)), false, false
}

// accessHostVerdict is one host's verdict against the resolved, clamped spec,
// never against what was requested. A deny entry wins over everything. A host
// the run reaches through a lane rather than the allowlist (a model provider's)
// is allowed once that lane was admitted; the rest are allowed only when the
// spec lists them or allows all egress. Anything else waits on the first-use
// mode: a request under a review mode, refused otherwise — clamped when the
// ceiling took the person's own entry away.
func accessHostVerdict(h client.AccessHost, spec types.RunPolicySpec, clamped bool) (verdict, reason string) {
	switch {
	case ceilingDenies(spec.DeniedDomains, h.Host):
		return verdictNotAllowed, hostReasonDeniedList
	case h.Via != viaEgress || spec.AllowAllEgress || specAllowsHost(spec, h.Host):
		return verdictAllowed, ""
	case spec.FirstUseApproval.RaisesApproval():
		return verdictNeedsApproval, ""
	case clamped:
		return verdictNotAllowed, hostReasonClamped
	}
	return verdictNotAllowed, hostReasonAlwaysDeny
}

// specAllowsHost reports whether an allowlist entry covers host: the entry
// itself, or a wildcard over its name.
func specAllowsHost(spec types.RunPolicySpec, host string) bool {
	name := map[string]bool{egressEntryHost(host): true}
	return slices.ContainsFunc(spec.AllowedDomains, func(entry string) bool {
		return hostEqual(entry, host) || (strings.HasPrefix(strings.TrimSpace(entry), "*") && entryCoversAny(entry, name))
	})
}

// accessUnlisted is the line for a host nothing lists, from the effective,
// clamped spec: an unset mode is always_deny, and allow-all makes it inert.
func accessUnlisted(spec types.RunPolicySpec, decider string) client.AccessUnlisted {
	u := client.AccessUnlisted{Mode: spec.FirstUseApproval.Normalize(), AllowAll: spec.AllowAllEgress, Decider: decider}
	if u.Mode == types.FirstUseWaitForReview {
		u.HoldSec = cmp.Or(spec.FirstUseHoldSeconds, int(types.HoldWindowEgress/time.Second))
	}
	return u
}

// accessStatus is a component fact's status as a row's: a door that read
// nothing that blocks the row calls it ready; liveness stays Review's.
func accessStatus(status string) string {
	switch status {
	case componentNeedsInput:
		return accessNeedsSetup
	case componentUnavailable:
		return accessUnavailable
	}
	return accessReady
}
