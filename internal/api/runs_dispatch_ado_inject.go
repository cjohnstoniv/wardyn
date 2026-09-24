// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The per-person Azure DevOps credential, AUTHORED AT DISPATCH — the
// control-plane half of the lane resolveADOInjection (injection_ado.go) serves.
//
// It is the captured-AWS-SSO lane one file over (runs_dispatch_sso_inject.go)
// applied to a forge instead of a model provider, and it keeps that lane's
// discipline exactly: everything security-relevant is decided HERE, because
// dispatch is the only moment at which the run's credential is still the one
// the operator's provider row meant. The grant carries an IMMUTABLE snapshot of
// that moment; drift between the snapshot and the live configuration is a 403
// refusal at resolve time, never a silent substitution.
//
// WHAT THE TOKEN DOES NOT DO. Measured against a real tenant: an Entra access
// token for Azure DevOps carries every scope the person consented to, whatever
// subset the request names. So the token bounds NOTHING about this run — the
// capability check the proxy makes in front of the resource is the enforcing
// layer, and it is the only one. Two consequences are authored here rather than
// assumed downstream:
//
//   - THE ORGANISATION PIN IS LOAD-BEARING. An Entra access token carries no
//     organisation claim of any kind; only the request URL says which
//     organisation is being addressed. The hosts below are therefore authored
//     from the row's OWN organisation and are exact — never the
//     `*.visualstudio.com` wildcard adoEgressDomains opens for the pat/ssh
//     lanes, which would let this credential ride to any organisation in the
//     tenant the person also belongs to.
//   - ONE CREDENTIALED DOOR PER PATH: REST goes through the TLS-MITM tunnel,
//     where the proxy's REST gate classifies every request and pins the
//     organisation, and git goes through the broker. The proxy's plain forward
//     lane never runs that gate, so it refuses every request to a host this
//     grant covers, https:// or not (refuseADOPlain,
//     internal/egress/proxy/ado_gate.go); require_tls on every rule is the
//     floor under that refusal. And a run with no per-run certificate
//     authority is REFUSED rather than downgraded: handleConnect only
//     intercepts when a CA exists, so authoring these hosts without one would
//     leave a blind tunnel carrying the sandbox's own headers.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	// adoEntraInjectHeader / adoEntraInjectFormat are the ONE wire shape an
	// Azure DevOps Entra access token has: an Authorization bearer. Both the
	// REST surface and git's smart-HTTP surface accept it, measured, with no
	// separate code path for the two.
	adoEntraInjectHeader = "Authorization"
	adoEntraInjectFormat = "Bearer %s"
	// adoEntraGrantTTLSeconds matches the captured-AWS-SSO precedent. It bounds
	// the MINT, never the hold: a re-resolve mints afresh from the person's
	// stored refresh token.
	adoEntraGrantTTLSeconds = 3600
	// adoEntraHostPort is the ONE port these hosts are authored on. Azure DevOps
	// hosted serves https and nothing else, so a port-qualified entry costs
	// nothing and buys the cleartext refusal above.
	adoEntraHostPort = "443"
	// adoEntraPlaceholderEnv is what a tool that INSISTS on a token reads. The
	// value is inert: the real credential is attached by the proxy, and
	// stripSandboxCredentials (internal/egress/proxy/inject.go) removes whatever
	// the sandbox put on the request before the brokered header goes on. The
	// sandbox therefore holds no Azure DevOps credential at any point.
	adoEntraPlaceholderEnv   = "AZURE_DEVOPS_EXT_PAT"
	adoEntraPlaceholderValue = "wardyn-proxy-injects-this-credential"
)

// adoEntraServices are the service sub-hosts one Azure DevOps organisation is
// served from, in BOTH naming schemes: `<svc>.dev.azure.com` on the modern one
// and `<org>.<svc>.visualstudio.com` on the legacy one. An organisation may be
// addressed through either, and a tool picks for itself, so both are authored.
var adoEntraServices = []string{"vssps", "vsrm", "feeds", "pkgs", "almsearch"}

// adoEntraScopeSnapshot is the IMMUTABLE credential scope this run was
// dispatched with — the awsSSOScopeSnapshot invariant, one lane over.
//
// It is a SNAPSHOT OF FACTS, not a capability: naming a field here grants
// nothing, because resolveADOInjection re-derives every one of them from the
// live provider row and requires equality, and the owner must additionally
// equal the run token's own subject. What it buys is the case every other guard
// is blind to: an admin who re-points, re-scopes or withdraws the row while a
// run is in flight would otherwise have that run's next resolve mint against
// the new configuration — a credential changing meaning under a working agent,
// with every existing check green, because every existing check asks about the
// run and not about the credential.
type adoEntraScopeSnapshot struct {
	ProviderRowID    string `json:"provider_row_id"`
	Organisation     string `json:"organisation"`
	OwnerSubject     string `json:"owner_subject"`
	CredentialSource string `json:"credential_source"`
	TenantID         string `json:"tenant_id"`
	ClientID         string `json:"client_id"`
	TokenMode        string `json:"token_mode"`
	// Capabilities is what this run was granted, in the CLASSIFIER's vocabulary
	// (internal/adoscope). It is the set the proxy's capability gate holds the
	// run to; the scopes derived from it are only what Entra understands, and
	// they do not narrow the token (see the file header).
	Capabilities []adoscope.Capability `json:"capabilities"`
}

// authored reports whether a snapshot is present at all. Every field named here
// is required on this lane — unlike the AWS lane's shared arm, there is no
// legitimate empty owner, because the credential is one person's by
// construction.
func (sn adoEntraScopeSnapshot) authored() bool {
	return sn.ProviderRowID != "" && sn.Organisation != "" && sn.OwnerSubject != "" &&
		sn.TenantID != "" && sn.ClientID != "" && len(sn.Capabilities) > 0
}

// adoEntraRun is the dispatch-time resolution of "does this run get an Azure
// DevOps credential, whose, and for what" — the inputs the snapshot is built
// from, in one value so the decision has one spelling.
type adoEntraRun struct {
	rowID            string
	org              string
	owner            string
	tenantID         string
	clientID         string
	tokenMode        types.ADOTokenMode
	credentialSource types.CredentialSource
	caps             []adoscope.Capability
	// ceiling is the row's CapabilityCeiling at dispatch. It is not part of the
	// snapshot — the resolver reads the LIVE ceiling — but a profile already
	// outside it is refused here rather than authored and refused on first use.
	ceiling []adoscope.Capability
}

// snapshot renders the run's resolution as the immutable grant scope.
func (a adoEntraRun) snapshot() adoEntraScopeSnapshot {
	return adoEntraScopeSnapshot{
		ProviderRowID:    a.rowID,
		Organisation:     a.org,
		OwnerSubject:     a.owner,
		CredentialSource: string(a.credentialSource),
		TenantID:         a.tenantID,
		ClientID:         a.clientID,
		TokenMode:        string(a.tokenMode),
		Capabilities:     slices.Clone(a.caps),
	}
}

// resolveADOEntraRun decides whether THIS run is on the per-person Azure DevOps
// lane, from the provider rows that admitted its own repositories.
//
// THE ROW THAT ADMITTED IS THE ROW THAT DECIDES — providerFor, the same
// predicate every other lane gate asks — so a second row on the same host
// cannot answer for a repository it did not admit.
//
// Every arm below declines rather than refuses, and that is deliberate: this
// runs on EVERY dispatch on every deployment, the overwhelming majority of
// which have no Azure DevOps row at all. A run that resolves to nothing here is
// a run with no Azure DevOps credential, which is what it had before this lane
// existed. The one thing it never does is author a credential for a
// configuration it is not sure about:
//
//   - CREDENTIAL SOURCE MUST BE per_user. The stored credential is one person's
//     captured sign-in, read out of that person's own secret namespace
//     (readADOEntraBlob refuses an empty owner outright), so there is no such
//     thing as a shared one. A row whose lanes name entra but whose credential
//     source is the default `shared` is a configuration this lane cannot serve,
//     and serving it from whoever happens to have signed in would be the
//     substitution the whole lane exists to refuse.
//   - TWO ORGANISATIONS IN ONE RUN AUTHOR NOTHING. The organisation pin is the
//     only thing standing between this credential and every other organisation
//     in the tenant (an Entra token carries no organisation claim), so a run
//     spanning two of them would need two pins and gets neither.
func resolveADOEntraRun(sc types.SiteConfig, repos []string, owner string) (adoEntraRun, bool) {
	if owner == "" {
		return adoEntraRun{}, false
	}
	var out adoEntraRun
	for _, repo := range repos {
		cand, ok := adoEntraRunForRepo(sc, repo, owner)
		if !ok {
			continue
		}
		if out.rowID == "" {
			out = cand
			continue
		}
		if out.rowID != cand.rowID || out.org != cand.org {
			return adoEntraRun{}, false
		}
	}
	return out, out.rowID != ""
}

// adoEntraRunForRepo is resolveADOEntraRun's per-repository half.
func adoEntraRunForRepo(sc types.SiteConfig, repo, owner string) (adoEntraRun, bool) {
	row, admitted := providerFor(sc, repo)
	switch {
	case !admitted || row.ID == "" || row.Disabled:
		return adoEntraRun{}, false
	case row.Kind != types.GitProviderAzureDevOps:
		return adoEntraRun{}, false
	case !laneAllowed(row, types.GitLaneEntra) || row.Entra == nil:
		return adoEntraRun{}, false
	case row.CredentialSource != types.CredentialSourcePerUser:
		return adoEntraRun{}, false
	}
	org, ok := adoOrganisationOf(repo)
	if !ok {
		return adoEntraRun{}, false
	}
	return adoEntraRun{
		rowID: row.ID, org: org, owner: owner,
		tenantID: row.Entra.TenantID, clientID: row.Entra.ClientID,
		tokenMode: cmpTokenMode(row.Entra.TokenMode), credentialSource: row.CredentialSource,
		caps: row.Entra.Profile(), ceiling: slices.Clone(row.Entra.CapabilityCeiling),
	}, true
}

// adoOrganisationOf pulls the Azure DevOps ORGANISATION out of a clone URL, in
// both naming schemes: the first path segment of a dev.azure.com URL, or the
// leading label of an `<org>.visualstudio.com` host.
//
// The result is concatenated into host names, so it is held to a DNS LABEL and
// nothing else. A value carrying a dot would compose a different host — and on
// the legacy scheme it would also be a service sub-host (`acme.vsrm.…`) read as
// an organisation — so the legacy arm requires the host to be exactly one label
// in front of visualstudio.com rather than trimming a suffix.
func adoOrganisationOf(cloneURL string) (string, bool) {
	// nil: an organisation is only ever read off an Azure DevOps SERVICE host.
	t, ok := parseCloneTarget(cloneURL, nil)
	if !ok {
		return "", false
	}
	host := canonicalProviderHost(t.host)
	if host == "dev.azure.com" {
		seg, _, _ := strings.Cut(strings.TrimPrefix(t.path, "/"), "/")
		return adoOrganisationLabel(seg)
	}
	label, rest, found := strings.Cut(host, ".")
	if !found || rest != "visualstudio.com" {
		return "", false
	}
	return adoOrganisationLabel(label)
}

// adoOrganisationLabel holds an organisation name to a DNS label.
func adoOrganisationLabel(raw string) (string, bool) {
	org := strings.ToLower(strings.TrimSpace(raw))
	if org == "" || len(org) > 63 || strings.HasPrefix(org, "-") {
		return "", false
	}
	for _, r := range org {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		default:
			return "", false
		}
	}
	return org, true
}

// adoEntraHosts is the EXACT host set one organisation's credential may ride
// to, bare (what an injection rule's host binding takes — buildInjector keys
// byHost verbatim).
//
// It is exact and organisation-qualified on purpose. adoEgressDomains — what
// the pat and ssh lanes open today — answers `*.visualstudio.com`, a wildcard
// that reaches every organisation in the tenant; with a credential the forge
// does not bind to an organisation (measured), that wildcard would be the whole
// pin gone.
func adoEntraHosts(org string) []string {
	hosts := make([]string, 0, 2*len(adoEntraServices)+2)
	add := func(h string) {
		if !slices.Contains(hosts, h) {
			hosts = append(hosts, h)
		}
	}
	add("dev.azure.com")
	for _, svc := range adoEntraServices {
		add(svc + ".dev.azure.com")
	}
	add(org + ".visualstudio.com")
	for _, svc := range adoEntraServices {
		add(org + "." + svc + ".visualstudio.com")
	}
	return hosts
}

// adoEntraGitHosts are the broker entries for the hosts git is served from in
// both naming schemes, plus `<org>@dev.azure.com`: the URL Azure DevOps' own
// Clone button hands out carries the organisation as a user name.
func adoEntraGitHosts(org string) []string {
	return []string{"dev.azure.com", org + "@dev.azure.com", org + ".visualstudio.com"}
}

// adoEntraEgressEntries is the same set PORT-QUALIFIED, which is what an
// allowlist entry and a TLS-MITM entry are both written as.
//
// The two halves are deliberately spelled differently and that is the F106
// producer/consumer contract, not an inconsistency: an allowlist entry states a
// port (AllowedExactHost reads a port-qualified entry as naming the host), while
// an injection rule has no port to offer and is bare. A bare MITM entry means
// ANY PORT, which would let a sandbox CONNECT to one of these hosts on a port
// nobody configured and have that tunnel terminated with the Wardyn leaf.
func adoEntraEgressEntries(org string) []string {
	hosts := adoEntraHosts(org)
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, net.JoinHostPort(h, adoEntraHostPort))
	}
	return out
}

// adoEntraLane is everything the lane adds to one run's sidecar configuration.
type adoEntraLane struct {
	injections []runner.InjectionGrant
	mitmHosts  []string
	// gate is what the proxy's REST gate holds every request on these hosts to
	// (proxy.ADOGrantConfig): the organisation, the granted capabilities and the
	// exact hosts. The injection alone would attach the person's credential with
	// nothing narrowing it — the token bounds nothing — so the two always travel
	// together; nil exactly when no injection was authored.
	gate *proxy.ADOGrantConfig
}

// adoEntraGrade is what the autonomy gate RESOLVED about this run's per-person
// Azure DevOps lane at CREATE, frozen for dispatch to author from.
//
// It exists because the two moments read site config twice. The gate grades
// from scmLaneSiteConfig (runs_autonomy.go) and freezes the run's autonomy
// level; dispatch re-reads (siteConfigForDispatch) and authors from that
// SECOND read. With nothing tying them, an admin flipping this row between the
// two — `shared` to `per_user`, or adding the entra lane — handed a run graded
// `secrets=none` the person's Entra bearer. That window is not a race to shrug
// at: it is the image resolve and the devcontainer/BYOI build inside
// finishCreateRunLaunch, up to thirty minutes.
//
// THE RESOLUTION TRAVELS, NOT THE INPUTS. Freezing the site-config snapshot
// instead would leave the repositories and the subject re-derived at dispatch
// and merely PROVEN equal by a test; freezing the answer makes all three one
// value, and any disagreement becomes a refusal rather than a silent
// substitution — the rule adoEntraScopeSnapshot already applies to the
// credential itself, one moment earlier.
//
// It rides on dispatchCeiling rather than on dispatchParams, and that is the
// same decision dispatchCeiling's own doc comment records: an optional field
// defaulting to "nobody decided" is the defect class that struct exists to
// close, and ceilingForDispatch is the one translation every dispatch lane
// already goes through.
type adoEntraGrade struct {
	// graded records that a rubric graded this run, so dispatch is held to the
	// rest of this value. Set by the two constructors below and by nothing else.
	//
	// FALSE IS NOT FAIL-OPEN. It means no rubric bound this run — no assigned
	// profile, no rubric on it, or a dispatch lane that runs no autonomy gate at
	// all — and dispatch then resolves the lane exactly as it did before this
	// type existed. A run with no cap cannot be launched above one; the escape
	// this type closes requires a rubric to have capped the run in the first
	// place.
	graded bool
	// rowID and org are the provider row and organisation the gate resolved.
	// Both empty with graded=true is the explicit "graded, and the gate saw NO
	// lane" — the case the security review reproduced.
	rowID string
	org   string
}

// adoEntraUngraded is the answer for a dispatch lane that runs no autonomy
// gate: the scan lane, the site-config probe, the harness login and the
// record/verify session. Named rather than a bare literal so the claim is
// greppable at the call site and a new lane has to make it on purpose.
func adoEntraUngraded() adoEntraGrade { return adoEntraGrade{} }

// adoEntraGradedAs freezes what resolveADOEntraRun answered at the gate, for
// the one door that grades. `on=false` freezes "graded with no lane", which is
// an answer and not an absence — that distinction is the whole fix.
func adoEntraGradedAs(ado adoEntraRun, on bool) adoEntraGrade {
	if !on {
		return adoEntraGrade{graded: true}
	}
	return adoEntraGrade{graded: true, rowID: ado.rowID, org: ado.org}
}

// adoEntraGradeHolds refuses a dispatch that would author a lane the autonomy
// gate never graded, or one for a DIFFERENT row or organisation than it graded.
// Returns true when dispatch may proceed; false once the run is already FAILED.
//
// Only the ESCAPE direction refuses. The mirror case — the gate graded a lane
// and dispatch now resolves none, because an admin went `per_user` to `shared`
// — leaves the run capped as if it held the credential while holding nothing,
// which is stricter than its posture and harms no one. Refusing it would turn a
// benign admin edit into a failed run, so it declines quietly, exactly as a run
// that never had a row does.
//
// The ORGANISATION is checked as well as the row, because it is the pin: an
// Entra access token carries no organisation claim (this file's header,
// measured), so the graded envelope named contoso's hosts and a dispatch
// authoring fabrikam's would credential an organisation the grade never saw.
func (s *Server) adoEntraGradeHolds(ctx context.Context, run types.AgentRun, g adoEntraGrade,
	ado adoEntraRun, on bool,
) bool {
	if !g.graded || !on {
		return true
	}
	if g.rowID == "" {
		return s.refuseADOEntraDispatch(ctx, run, "autonomy_grade_drift",
			"this run was not launched: its autonomy level was graded WITHOUT an Azure DevOps credential, and the "+
				"provider configuration changed between then and now so that dispatch would attach one. Re-launch the "+
				"run so it is graded against the configuration it will actually get.")
	}
	if g.rowID != ado.rowID || g.org != ado.org {
		return s.refuseADOEntraDispatch(ctx, run, "autonomy_grade_drift",
			fmt.Sprintf("this run was not launched: its autonomy level was graded for Azure DevOps organisation %q on "+
				"provider row %q, and dispatch resolved organisation %q on row %q. An Entra access token carries no "+
				"organisation claim, so the pin is the only thing scoping this credential and it may not be "+
				"re-decided after the run was graded. Re-launch the run.", g.org, g.rowID, ado.org, ado.rowID))
	}
	return true
}

// authorADOEntraLane is dispatchRun's one call into this file: it authors the
// run's Azure DevOps grant, injection rules, egress, TLS-MITM entries and REST
// gate grant, or declines when this run is not on the lane. ok=false means the
// run has already been marked FAILED and dispatch must stop —
// authorBedrockSSOInjection's contract, deliberately identical.
func (s *Server) authorADOEntraLane(ctx context.Context, run types.AgentRun, ado adoEntraRun, on bool,
	grade adoEntraGrade, plan dispatchLLMPlan, policy *types.RunPolicySpec, sandboxEnv map[string]string,
	injections []runner.InjectionGrant,
) (adoEntraLane, bool) {
	// Ahead of the `on` short-circuit: the case that matters is the one where
	// dispatch WOULD author a lane the grade did not include.
	if !s.adoEntraGradeHolds(ctx, run, grade, ado, on) {
		return adoEntraLane{injections: injections}, false
	}
	if !on {
		return adoEntraLane{injections: injections}, true
	}
	inj, mitm, ok := s.authorADOEntraInjection(ctx, run, ado, plan.mitmCACertPEM, plan.mitmCAKeyPEM, policy, sandboxEnv, injections)
	if !ok {
		return adoEntraLane{injections: injections}, false
	}
	return adoEntraLane{injections: inj, mitmHosts: mitm, gate: &proxy.ADOGrantConfig{
		Organization: ado.org, Capabilities: slices.Clone(ado.caps), Hosts: adoEntraHosts(ado.org),
	}}, true
}

// authorADOEntraInjection authors the whole lane for one run.
//
// The three refusals in front of it are the ones that must not be degradations:
//
//  1. TOKEN MODE. `minted_pat` is a row asking for a short-lived personal access
//     token minted on the control plane, and measured against a real tenant that
//     mint is refused for any delegated token — only a first-party client can
//     make one. There is no mint to make, so a run on such a row is refused
//     rather than quietly credentialed with the bearer the row did not ask for.
//  2. CAPABILITIES. A ceiling or profile naming something the catalogue cannot
//     grant (an unclassified write, a denied area) has no scopes, and an empty
//     scope set is inside every set — so a caller comparing scopes would read
//     the fail-closed answer as permission. adoscope.ScopesFor returns an ERROR
//     for exactly that reason and it is honoured here.
//  3. CERTIFICATE AUTHORITY. The proxy intercepts a CONNECT only when it holds a
//     per-run CA. Authoring these hosts without one leaves a blind tunnel: no
//     interception, no injection, no classifier, no organisation pin — and the
//     sandbox's own headers reaching Azure DevOps unexamined. A downgrade that
//     silent is worse than a failed run, so it is a refusal.
func (s *Server) authorADOEntraInjection(ctx context.Context, run types.AgentRun, ado adoEntraRun,
	caCertPEM, caKeyPEM string, policy *types.RunPolicySpec, sandboxEnv map[string]string,
	injections []runner.InjectionGrant,
) ([]runner.InjectionGrant, []string, bool) {
	if ado.tokenMode != types.ADOTokenModeBearer {
		return injections, nil, s.refuseADOEntraDispatch(ctx, run, "token_mode",
			fmt.Sprintf("this run's Azure DevOps provider row asks for token_mode %q, which Wardyn cannot issue: "+
				"minting a personal access token is refused for every delegated token by Azure DevOps itself. "+
				"Set the row to bearer, or launch without the Azure DevOps lane.", ado.tokenMode))
	}
	if _, err := adoscope.ScopesFor(ado.caps); err != nil {
		return injections, nil, s.refuseADOEntraDispatch(ctx, run, "capability_not_grantable",
			"this run's Azure DevOps provider row grants a capability Wardyn will not mint a credential for: "+err.Error())
	}
	// Empty is NOT within anything: a run granted nothing has no business
	// holding a credential.
	if len(ado.caps) == 0 || !subsetOf(ado.caps, ado.ceiling) {
		return injections, nil, s.refuseADOEntraDispatch(ctx, run, "capability_ceiling",
			"this run's Azure DevOps default profile names a capability outside the provider row's own ceiling")
	}
	if caCertPEM == "" || caKeyPEM == "" {
		return injections, nil, s.refuseADOEntraDispatch(ctx, run, "no_run_certificate_authority",
			"this run was not launched: its Azure DevOps credential needs a per-run certificate authority to be "+
				"attached on the wire, and none was provisioned. Without one the proxy tunnels these hosts blind, "+
				"so the credential, the capability check and the organisation pin would all be skipped.")
	}

	snapshot := ado.snapshot()
	hosts := adoEntraHosts(ado.org)
	grants, ok := s.createADOEntraGrants(ctx, run, snapshot, hosts)
	if !ok {
		return injections, nil, false
	}
	injections = append(injections, grants...)

	// EGRESS, port-qualified and exact, appended to the run's own allowlist.
	// Placed before the governance ceiling's re-assertion in dispatchRun so an
	// assigned profile that denies one of these hosts takes both the reach and
	// the injection rule back.
	policy.AllowedDomains = append(policy.AllowedDomains, adoEntraEgressEntries(ado.org)...)

	// The inert placeholder, never overwriting a value some earlier phase set:
	// every platform writer of this map follows the same rule, and a run whose
	// operator authored a real token for this variable is not this lane's to
	// re-decide.
	if sandboxEnv != nil && sandboxEnv[adoEntraPlaceholderEnv] == "" {
		sandboxEnv[adoEntraPlaceholderEnv] = adoEntraPlaceholderValue
	}
	// GIT GOES THROUGH THE BROKER. The REST gate refuses git on the intercepted
	// connection, so agent-run must rewrite this organisation's clone URLs onto
	// the proxy's /wardyn/git/ route (pat_broker_entra.go).
	if sandboxEnv != nil {
		addGitBrokerHosts(sandboxEnv, adoEntraGitHosts(ado.org)...)
	}
	return injections, adoEntraEgressEntries(ado.org), true
}

// createADOEntraGrants writes ONE api_key grant per host and returns the
// injection rules bound to them.
//
// One grant per host rather than one grant with a host list, because that is
// the shape every other injection lane already has: the grant scope is decoded
// by injectionRuleFromScope and by the broker with unknown fields REFUSED, and
// a list field would be a new wire shape in two decoders to save a dozen cheap
// rows. It also makes the resolve-time host pin a plain membership test against
// the snapshot's own organisation.
func (s *Server) createADOEntraGrants(ctx context.Context, run types.AgentRun,
	snapshot adoEntraScopeSnapshot, hosts []string,
) ([]runner.InjectionGrant, bool) {
	out := make([]runner.InjectionGrant, 0, len(hosts))
	for _, host := range hosts {
		scope, merr := json.Marshal(map[string]any{
			"host":        host,
			"header":      adoEntraInjectHeader,
			"format":      adoEntraInjectFormat,
			"secret_name": types.ADOEntraAccessTokenSecret,
			// THE FLAG THIS LANE CANNOT SHIP WITHOUT. Without it the proxy's
			// plain forward lane credentials a bare `http://dev.azure.com/...`
			// on port 80 — in cleartext, and on a path that never terminates
			// TLS, so the classifier and the organisation pin both sit it out.
			// Azure DevOps hosted serves https only, so there is no deployment
			// this refuses that was ever going to work.
			"require_tls": true,
			"snapshot":    snapshot,
		})
		if merr != nil {
			return nil, s.refuseADOEntraDispatch(ctx, run, "grant_scope",
				"could not author the Azure DevOps credential injection: "+merr.Error())
		}
		grantID := uuid.New()
		if _, gerr := s.cfg.Store.CreateGrant(ctx, types.CredentialGrant{
			ID: grantID, RunID: run.ID, CreatedAt: time.Now(),
			Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: scope, TTLSeconds: adoEntraGrantTTLSeconds},
		}); gerr != nil {
			return nil, s.refuseADOEntraDispatch(ctx, run, "grant_write",
				"could not author the Azure DevOps credential injection: "+gerr.Error())
		}
		rule, derr := injectionRuleFromScope(scope)
		if derr != nil {
			return nil, s.refuseADOEntraDispatch(ctx, run, "grant_scope",
				"could not author the Azure DevOps credential injection: "+derr.Error())
		}
		out = append(out, runner.InjectionGrant{GrantID: grantID, Rule: rule})
	}
	return out, true
}

// refuseADOEntraDispatch marks the run FAILED and records why. It always
// returns false so a caller can `return …, s.refuseADOEntraDispatch(…)`.
//
// The CAS is from STARTING (claimed at dispatch entry) so a concurrent kill's
// KILLED state is preserved rather than clobbered back to FAILED — the same
// rule every other dispatch-time refusal follows.
func (s *Server) refuseADOEntraDispatch(ctx context.Context, run types.AgentRun, reason, detail string) bool {
	s.failAndRevoke(ctx, run.ID, types.RunStarting, detail)
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
		run.ID.String(), "failure", mustJSON(map[string]any{
			"error": "azure devops entra inject: " + reason, "reason": reason, "detail": detail,
		})))
	return false
}
