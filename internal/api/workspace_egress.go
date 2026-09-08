// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net"
	neturl "net/url"
	"strconv"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/hostrules"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// workspace_egress.go computes what a workspace contributes to a run's egress
// allowlist: the trusted hosts its scan found, the operator's own approvals,
// the corporate mirrors those get substituted for, the clone hosts every repo
// source needs, and the deliberately WIDER set a confined replay gets. Split
// out of workspace_run.go when that file crossed its size cap; the grouping is
// the real seam — every function here answers "which hosts, and why is that
// honest", and none of them launch anything.

// unionWorkspaceEgress adds every referenced workspace's trusted egress to the
// spec's AllowedDomains (deduped, in-place) AND its permanently-denied egress
// to spec.DeniedDomains (deduped, in-place) — but returns what it added to
// AllowedDomains ONLY. Three sources, all operator-sanctioned: the scanned
// profile's EgressDomains (filename-keyed marker table — never file content),
// the workspace's ApprovedEgress (content-derived suggestions the operator
// explicitly promoted), and DeniedEgress (the operator's permanent
// `deny · always` decisions — Phase 4). The scanner's raw SuggestedEgress is
// deliberately NOT here — a hostile file must never widen an allowlist
// without a human approval.
//
// The return stays ALLOW-shaped on purpose: its two production consumers,
// runs_create.go's `added_domains` audit and compose_setup.go's "launch will
// also allow: …" checklist copy, would both misreport a block as a widening
// if a deny rode along in the same slice. A caller that needs to know what
// was denied computes it itself (see setupEgressWorkspaceItem, which diffs
// spec.DeniedDomains before/after this call).
//
// The confinement floor is unaffected; the deny-list is not — and deny beats
// allow, allow_all_egress, and a runtime first-use approval alike wherever the
// proxy evaluates policy (see docs/POLICIES.md), so a workspace's permanent
// deny now rides every union this function backs (create, preflight).
func unionWorkspaceEgress(spec *types.RunPolicySpec, workspaces []types.Workspace) []string {
	var added []string
	for _, ws := range workspaces {
		if p, ok := workspaceProfile(ws); ok {
			// Filter the merged SCAN PROFILE through this workspace's egress "off"
			// overrides (GAP-EGRESS-5): the folded contract already drops an off'd
			// egress requirement row, but the same host also rides in via the profile
			// union here — which the fold never touches — so an operator who set
			// egress:<host> off would still reach it. The scan seeds the host into
			// BOTH lanes, so refusing the requirement row alone removes only the
			// redundant copy; subtracting it here closes the profile path too.
			added = append(added, unionAllowedDomains(spec, filterOffEgress(p.EgressDomains, egressOverriddenOff(ws)))...)
		}
		added = append(added, unionAllowedDomains(spec, ws.ApprovedEgress)...)
		// DeniedEgress folds in too (Phase 4) — deliberately left OUT of `added`,
		// see the doc comment above for why.
		unionDeniedDomains(spec, ws.DeniedEgress)
	}
	return added
}

// egressOverriddenOff returns the lowercased egress hosts this workspace REFUSED
// via an attachment "off" override AND that no other contributor re-added (a host
// re-laned required/optional by another attachment, or restated by the overlay,
// survives in the folded contract, so it is NOT off). This is the set to subtract
// from the merged scan-profile union so an "off" egress override actually closes
// egress rather than being defeated by the profile path (GAP-EGRESS-5, closed
// together with WSPIPE-7's Overrides writer — upsertAndAttach, sources.go).
func egressOverriddenOff(ws types.Workspace) map[string]bool {
	off := map[string]bool{}
	for _, att := range ws.Attachments {
		for key, stance := range att.Overrides {
			if stance != types.OverrideOff {
				continue
			}
			if typ, host, ok := types.SplitRequirementKey(key); ok && typ == "egress" {
				off[strings.ToLower(strings.TrimSpace(host))] = true
			}
		}
	}
	if len(off) == 0 {
		return off
	}
	for key := range effectiveRequirements(ws) {
		if typ, host, ok := types.SplitRequirementKey(key); ok && typ == "egress" {
			delete(off, strings.ToLower(strings.TrimSpace(host)))
		}
	}
	return off
}

// filterOffEgress drops every host in the off-set from domains (order-preserving,
// case-insensitive). Returns domains unchanged when the off-set is empty.
func filterOffEgress(domains []string, off map[string]bool) []string {
	if len(off) == 0 {
		return domains
	}
	out := domains[:0:0]
	for _, d := range domains {
		if off[strings.ToLower(strings.TrimSpace(d))] {
			continue
		}
		out = append(out, d)
	}
	return out
}

// unionSiteConfigScmHosts adds the operator's site-config default SCM hosts
// (types.SiteConfig.ScmHosts, set via PUT /api/v1/site-config) to the spec's
// AllowedDomains (deduped, in-place) and returns what it added. Unlike GitHub/
// ADO — whose egress bundles are either baked into the example policies or
// derived from a git_pat/ssh_key grant's host (adoEgressDomains,
// sshOver443Endpoint) — a self-hosted GHES or ADO Server has no such built-in
// bundle, so the operator declares its host(s) once in site-config and every
// cloning run inherits them. Non-secret, additive: it only ever widens the
// allowlist with hosts the operator explicitly declared, never anything
// content-derived. No SiteConfig row / no Store configured / no ScmHosts set
// are all the common "unconfigured" case and add nothing.
func (s *Server) unionSiteConfigScmHosts(ctx context.Context, spec *types.RunPolicySpec) []string {
	if s.cfg.Store == nil {
		return nil
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil || len(sc.ScmHosts) == 0 {
		return nil
	}
	return unionAllowedDomains(spec, sc.ScmHosts)
}

// substituteArtifactEgress applies the operator's egress redirects to a run's
// AllowedDomains. Two tiers (types.SiteConfig.EgressRedirects):
//   - Ecosystem set: DROPS that language's entire public-registry host set
//     (markers.go's egress* literals, via workspacescan.PublicRegistryHosts), not
//     just the one redirect's own From host, since a mirror commonly fronts more
//     than one public host for the same ecosystem (e.g. pip's index host AND its
//     file-download CDN) and byte-identical fold-compat depends on dropping both.
//   - Ecosystem "" (network-only): DROPS exactly the one declared From host —
//     there is no per-ecosystem table to consult for an arbitrary redirect.
//
// SCOPE — matching run only (GAP-EGRESS-2). A redirect is applied ONLY when the
// run's OWN egress actually reaches one of the public hosts it fronts (its From
// host, or an ecosystem public host — the same hosts a scan of that ecosystem
// seeds into the run's egress). types.EgressRedirect promises substitution "for
// every MATCHING run"; without this, an unrelated sealed run gained the corp To
// host (and, via planArtifactRedirect, the operator's injected registry token) it
// never asked for. A run that names none of a redirect's public hosts is left
// entirely untouched by THIS function's allow-side substitution. A
// network-only row's deny side is not scoped the same way — see
// appendNetworkRedirectDenials below, applied unconditionally on every run
// regardless of whether it reaches this redirect (docs/OPERATIONS.md, "Egress
// redirects: two tiers").
//
// The dropped hosts are matched port- and wildcard-aware (GAP-EGRESS-6): a
// "*.pythonhosted.org" or "pypi.org:443" allowlist entry — both legal in
// AllowedDomains — is subtracted exactly like a bare "pypi.org" is, so public
// reach never survives beside the corp mirror.
//
// The applied tier ADDS the redirect's To host. Corp REPLACES public for
// configured, in-scope redirects; everything else is untouched. Returns a FRESH
// slice (never mutates the input's backing array); a no-op (returns the input)
// when nothing is configured. A malformed From/To leaves that redirect's public
// host(s) in place (fail safe: never silently drop egress a build still needs).
func substituteArtifactEgress(domains []string, sc types.SiteConfig) []string {
	if len(sc.EgressRedirects) == 0 {
		return domains
	}
	have := artifactRunHostSet(domains)
	dropHost := map[string]bool{} // bare hosts to remove
	var add []string
	added := map[string]bool{}
	for _, r := range sc.EgressRedirects {
		to := strings.ToLower(hostrules.HostOf(r.To))
		if to == "" {
			continue // malformed To: this redirect contributes nothing
		}
		pub := redirectPublicHosts(r)
		if len(pub) == 0 {
			continue // malformed From on a network-only row: fail safe, drop nothing
		}
		if !runReachesAny(have, pub) {
			continue // out of scope for this run: leave it entirely untouched
		}
		for _, h := range pub {
			dropHost[strings.ToLower(h)] = true
		}
		// PORT-QUALIFIED, never bare (F106). A bare allowlist entry matches on
		// EVERY port (classifyDomain gives it port 0, and Policy.AllowsLiteralIP
		// answers true from allowedExact before it ever consults the
		// port-qualified map), so a redirect To of https://10.40.2.11:8443/ used
		// to trust 10.40.2.11:22 and :5432 as well — the private-IP guard's whole
		// job, undone on ports the operator never named. The MITM/token half of
		// the SAME redirect has been port-exact since W13-S1-5
		// (planArtifactRedirect authors mitmHosts as net.JoinHostPort(host,
		// redirectPort(r.To))), so the credential was scoped to one port while
		// the SSRF trust was not. Both halves now derive the port from ONE
		// function, so a To that names no port trusts exactly the port the MITM
		// half already assumed for it — the port To's scheme names (80 for an
		// explicit http://, 443 otherwise) — a mirror reached on some other port
		// needs that port in the To, which is the same thing the token injection has
		// always required.
		entry := net.JoinHostPort(to, strconv.Itoa(redirectPort(r.To)))
		if !added[entry] {
			added[entry] = true
			add = append(add, entry)
		}
	}
	out := make([]string, 0, len(domains)+len(add))
	seen := map[string]bool{}
	for _, d := range domains {
		if entryCoversAny(d, dropHost) {
			continue
		}
		key := strings.ToLower(d)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, d)
	}
	for _, a := range add {
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	return out
}

// appendNetworkRedirectDenials denies each NETWORK-ONLY redirect's From host so
// deny-beats-allow-all closes the public route the operator redirected away
// (GAP-EGRESS-4): a subtraction-only substitution does nothing under any
// allow_all_egress policy (every learning Record session, plus any allow-all
// operator policy), leaving From fully reachable while only the credentialed corp
// To host was added. Ecosystem rows stay subtraction-only (a public CDN may still
// be a legitimate fallback). Deduped against the existing deny-list; a no-op when
// nothing network-only is configured.
func appendNetworkRedirectDenials(denied []string, sc types.SiteConfig) []string {
	have := map[string]bool{}
	for _, d := range denied {
		have[strings.ToLower(strings.TrimSpace(d))] = true
	}
	var add []string
	for _, r := range sc.EgressRedirects {
		if r.Ecosystem != "" {
			continue
		}
		from := strings.ToLower(hostrules.HostOf(r.From))
		if from == "" || have[from] {
			continue
		}
		have[from] = true
		add = append(add, from)
	}
	if len(add) == 0 {
		return denied
	}
	return append(append([]string(nil), denied...), add...)
}

// workspaceSuggestedEgress collects the referenced workspaces' content-derived
// suggested hosts (deduped, order-preserving), minus anything the profiles/
// approvals already allow. Display-only input for the setup checklist — it
// must never feed AllowedDomains.
func workspaceSuggestedEgress(workspaces []types.Workspace) []string {
	allowed := map[string]bool{}
	for _, ws := range workspaces {
		if p, ok := workspaceProfile(ws); ok {
			for _, d := range p.EgressDomains {
				allowed[d] = true
			}
		}
		for _, d := range ws.ApprovedEgress {
			allowed[d] = true
		}
	}
	var out []string
	seen := map[string]bool{}
	for _, ws := range workspaces {
		p, ok := workspaceProfile(ws)
		if !ok {
			continue
		}
		for _, d := range p.SuggestedEgress {
			if !seen[d] && !allowed[d] {
				seen[d] = true
				out = append(out, d)
			}
		}
	}
	return out
}

// confinedEgressDomains is the two-phase SETUP allowlist for a confined
// (non-learning) record replay: deliberately WIDER than a scan's narrow
// git-only set, so install commands can reach their registries. Base = git
// hosts (for clone) unioned with the workspace's trusted egress
// (profile.EgressDomains, filename-keyed marker registries) + operator
// ApprovedEgress. Content-derived SuggestedEgress is NOT included — a build
// that needs one surfaces as an observed-egress denial the operator can
// promote (least-privilege, honest).
//
// Stays ALLOW-only by return shape, matching unionWorkspaceEgress: the union
// call below also folds ws.DeniedEgress into base.DeniedDomains, but that half
// is discarded at the `return` — this function's one caller (workspace_run.go)
// reads ws.DeniedEgress directly off the raw column for the deny side, since
// there is no per-source deny CONTRACT to fold the way the required-egress
// loop below folds allows.
//
// A METHOD, not a package function, for one reason: the operator_set
// provenance gate lives on s.cfg and this path MUST apply it. It was a package
// func through 0.7's flip of RequireOperatorSetEgress, so the gate reached the
// run-create path only and a scan_seeded egress host — derived by the scanner
// from UNTRUSTED repo content — was still auto-allowed on every confined
// replay. Both paths now route through egressProvenanceAllowed.
func (s *Server) confinedEgressDomains(ws types.Workspace) []string {
	base := &types.RunPolicySpec{AllowedDomains: workspaceCloneEgress(ws)}
	unionWorkspaceEgress(base, []types.Workspace{ws})
	// The verify loop's approvals land as REQUIRED egress: rows in the folded
	// contract (workspace overlay ∪ attached sources) — the replay must honor
	// them, or the host an operator just approved is denied again on the very
	// next confined session. Optional rows stay out: a replay has no
	// enabled-optional wire, and optional means "may proceed without it".
	for key, req := range effectiveRequirements(ws) {
		if req.Level != "required" {
			continue
		}
		// Same trust boundary as the launch path (applyWorkspaceRequirements):
		// a scan_seeded row is repo content, not an operator's act.
		if !s.egressProvenanceAllowed(req) {
			continue
		}
		if typ, host, ok := types.SplitRequirementKey(key); ok && typ == "egress" {
			unionAllowedDomains(base, []string{host})
		}
	}
	return base.AllowedDomains
}

// egressProvenanceAllowed is THE decision point for the operator_set
// egress-provenance gate (RequireOperatorSetEgress,
// WARDYN_REQUIRE_OPERATOR_SET_EGRESS — default TRUE since 0.7): may this
// workspace requirement row auto-widen a run's egress allowlist without an
// operator ever acting?
//
// Every path that folds a workspace's egress: requirement rows into a run
// policy calls this and nothing else. It exists because the gate was
// originally written inline at the run-create call site, which left the
// confined-replay path (confinedEgressDomains, above) unioning every required
// row regardless of provenance — the same host the launch path refused, allowed
// on the replay. A shared predicate is what makes "both paths" checkable.
//
// It answers only the PROVENANCE question. Whether a row is enabled at all
// (required, or an optional row the operator selected) stays with each caller:
// a replay has no enabled-optional wire, a launch does.
func (s *Server) egressProvenanceAllowed(req types.WorkspaceRequirement) bool {
	return !s.cfg.RequireOperatorSetEgress || req.Provenance == "operator_set"
}

// workspaceCloneEgress is the clone-host allowlist for EVERY repo source a
// workspace holds. It must iterate Sources, not the derived ws.Source mirror:
// that field is only populated for a single-source workspace, so reading it
// silently dropped the clone host of every non-GitHub repo past the first —
// and a confined replay would then deny the clone it was launched to prove.
// GitHub sources contribute nothing here by design (they route through the
// broker, which is on-segment, not an egress host), which is exactly why the
// gap stayed invisible.
func workspaceCloneEgress(ws types.Workspace) []string {
	var hosts []string
	seen := map[string]struct{}{}
	for _, src := range workspaceSourcesOfType(ws, types.WorkspaceSourceTypeRepo) {
		for _, h := range scanEgressDomains(repoCloneURL(src.Source)) {
			if _, dup := seen[h]; dup {
				continue
			}
			seen[h] = struct{}{}
			hosts = append(hosts, h)
		}
	}
	return hosts
}

// scanEgressDomains returns the egress allowlist a scan/verify run needs to clone
// cloneURL. A GitHub HTTPS clone is routed through the Wardyn git-broker
// (url.insteadOf → wardyn-proxy, minted token proxy-side), so github.com is NOT
// allowlisted here — the broker reaches github.com out-of-band and the sandbox
// gets repo-scoped access instead of all-of-github.com. A GitHub SSH clone stays
// on the ssh-over-443 lane (the broker is HTTPS + App-token only); a non-GitHub
// HTTPS clone still gets its own host. The proxy enforces the deny-list underneath.
func scanEgressDomains(cloneURL string) []string {
	// SSH/scp clone URL: the broker can't cover ssh, so keep the PORT-QUALIFIED
	// SSH-over-443 endpoint (ssh.github.com:443) — matches ONLY :443. neturl.Parse
	// cannot parse scp-form, so this must come first.
	if host, ok := sshCloneHost(cloneURL); ok {
		if ep, ok := sshOver443Endpoint(host); ok {
			return []string{ep}
		}
		return nil
	}
	if u, err := neturl.Parse(cloneURL); err == nil && u.Hostname() != "" {
		host := u.Hostname() // strips any :port
		if !isGitHubCloneHost(host) {
			// a non-GitHub HTTPS clone (ADO, GitLab, self-hosted git) needs ONLY
			// its own host (the broker is github.com-only in v1).
			return []string{host}
		}
		// GitHub HTTPS clone → git-broker; no egress domain needed (the sandbox dials
		// wardyn-proxy, which is always reachable on-segment, not an egress host).
		return nil
	}
	return nil
}

// isHTTPScheme reports whether a clone URL's scheme is one the git broker can
// actually serve. The broker route is smart-HTTP only, so an ssh:// URL must
// never key a GitGrants entry: brokering it would deny the forge and withhold
// the SSH key (single-lane, see confineGitBrokerEgress) while leaving the run no
// working clone lane at all, because agent-run's insteadOf rewrite only matches
// bare "<org>/<repo>" slugs. A hostname check alone is NOT enough — url.Parse
// reads ssh://git@github.com/o/r as Hostname()=="github.com".
func isHTTPScheme(scheme string) bool {
	return scheme == "https" || scheme == "http"
}
