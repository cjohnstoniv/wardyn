// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/hostrules"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// workspace_egress.go computes what a workspace contributes to a run's egress
// allowlist: the trusted hosts its scan found, the operator's own approvals,
// the corporate mirrors those get substituted for, the clone hosts every repo
// source needs, the deliberately WIDER set a confined replay gets, and the
// observed-egress synthesis. Every function here answers "which hosts, and why is
// that honest", and none of them launch anything.

// unionWorkspaceEgress adds every referenced workspace's trusted egress to the
// spec's AllowedDomains (deduped, in-place) AND its permanently-denied egress
// to spec.DeniedDomains (deduped, in-place) — but returns what it added to
// AllowedDomains ONLY. Three sources, all operator-sanctioned: the profile's
// EgressDomains (filename-keyed marker table — never file content),
// ApprovedEgress and DeniedEgress. The scanner's raw SuggestedEgress is
// deliberately NOT here — a hostile file must never widen an allowlist
// without a human approval.
// The return stays ALLOW-shaped: its consumers (runs_create.go's `added_domains`
// audit, compose_setup.go's "launch will also allow" copy) would misreport a
// block as a widening; setupEgressWorkspaceItem diffs spec.DeniedDomains itself.
// Deny beats allow everywhere the proxy evaluates policy (docs/POLICIES.md).
func unionWorkspaceEgress(spec *types.RunPolicySpec, workspaces []types.Workspace) []string {
	var added []string
	for _, ws := range workspaces {
		if p, ok := workspaceProfile(ws); ok {
			// Filter the merged SCAN PROFILE through this workspace's egress "off"
			// overrides: the folded contract already drops an off'd
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
// egress rather than being defeated by the profile path (closed
// together with the Overrides writer — upsertAndAttach, sources.go).
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
// AllowedDomains (deduped, in-place) and returns what it added. A self-hosted
// GHES or ADO Server has no built-in egress bundle, so the operator declares its
// host(s) once — as a provider row's base URL or in the legacy ScmHosts list —
// and every cloning run inherits them. Additive, never content-derived; an
// unconfigured site config adds nothing.
//
// It takes the site config rather than reading it: the autonomy gate grades
// these hosts and launch's unionRunEgress dispatches them, and both must see
// the SAME read (scmLaneSiteConfig) or they can disagree about the run.
func unionSiteConfigScmHosts(spec *types.RunPolicySpec, sc types.SiteConfig) []string {
	// effectiveScmHosts, not the raw ScmHosts list: once a provider row claims a
	// host, that row decides whether it is reachable — a disabled github row plus
	// a legacy scm_hosts: ["github.com"] must NOT keep unioning github.com into
	// every run's egress while admission refuses it (workspace_providers.go).
	hosts := effectiveScmHosts(sc)
	if len(hosts) == 0 {
		return nil
	}
	return unionAllowedDomains(spec, hosts)
}

// substituteArtifactEgress applies the operator's egress redirects to a run's
// AllowedDomains. Two tiers (types.SiteConfig.EgressRedirects): an ecosystem row
// DROPS that language's whole public-registry set (workspacescan.PublicRegistryHosts,
// since a mirror commonly fronts several hosts, e.g. pip's index AND its CDN); a
// network-only row (ecosystem "") DROPS exactly its one From host. Either ADDS To.
// Scope: the allow-side substitution applies ONLY to a run whose own egress
// reaches one of the redirect's public hosts, so an unrelated run never gains the
// corp host or planArtifactRedirect's injected token. A network-only row's deny
// side is NOT scoped: see appendNetworkRedirectDenials. Dropped hosts match port-
// and wildcard-aware ("*.pythonhosted.org", "pypi.org:443"). Returns a FRESH slice
// (the input when nothing is configured); a malformed From/To leaves its public
// host(s) in place (fail safe: never drop egress a build still needs).
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
		// Port-qualified, never bare: a bare entry matches EVERY port (classifyDomain
		// gives it port 0, and Policy.AllowsLiteralIP answers true from allowedExact
		// first), so a bare To of https://10.40.2.11:8443/ would trust :22 and :5432
		// too. planArtifactRedirect's MITM/token half derives the port from the same
		// redirectPort (80 for an explicit http://, 443 otherwise), so the credential
		// and the SSRF trust are scoped to one port; a mirror on another port needs
		// that port in the To.
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
// a subtraction-only substitution does nothing under any
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
// promote. ALLOW-only by return shape: the one caller (workspace_run.go) reads
// ws.DeniedEgress directly for the deny side.
// A method, so it MUST apply the operator_set provenance gate on s.cfg
// (egressProvenanceAllowed): otherwise a scan_seeded host, derived from
// UNTRUSTED repo content, would be auto-allowed on every confined replay.
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
// WARDYN_REQUIRE_OPERATOR_SET_EGRESS — default TRUE): may this
// workspace requirement row auto-widen a run's egress allowlist without an
// operator ever acting? Every path that folds a workspace's egress: requirement
// rows into a run policy (launch and confinedEgressDomains) calls this and
// nothing else, so the paths cannot disagree.
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

// Observed-egress synthesis bounds: scan the most recent runs that reference
// this workspace and, for each, at most this many audit events.
const (
	maxObservedRuns        = 50
	maxObservedAuditPerRun = 500
)

// handleObservedEgress synthesizes least-privilege egress feedback from run
// TELEMETRY: the egress hosts runs using THIS workspace were actually DENIED,
// minus what the workspace already allows or the operator already approved —
// candidates an operator can promote into the approved-egress list. Read-only
// and advisory — it never widens anything itself.
//
// The route is member-reachable, so the run scan is filtered by
// ownsRunOrAdmin (as handleListRuns and getRunAuthorized are): unfiltered, a
// member would learn which hosts a colleague's run on the shared workspace
// dialled. An admin still sees the whole workspace's telemetry, which is what
// the operator-owned promote flow needs.
func (s *Server) handleObservedEgress(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "workspace")
	if !ok {
		return
	}
	ws, ok := s.getWorkspaceReadable(w, r, id)
	if !ok {
		return
	}

	// Hosts to subtract, all three classes of "promoting this changes nothing":
	// already satisfied (the scanned profile's auto-allowed egress, the
	// operator's own approvals); already REFUSED by the promotion door itself
	// (deadApprovedEgressHosts — one of these in the list 400s the whole PUT,
	// so offering one broke promotion for every real host beside it); and
	// explicitly DENIED by the operator, where deny beats allow at the proxy
	// (policy.go), so the promotion answers 200 and the host stays blocked
	// forever.
	skip := map[string]bool{}
	for _, d := range ws.ApprovedEgress {
		skip[d] = true
	}
	for _, d := range ws.DeniedEgress {
		skip[strings.ToLower(strings.TrimSpace(d))] = true
	}
	for d := range s.deadApprovedEgressHosts() {
		skip[d] = true
	}
	if p, ok := workspaceProfile(ws); ok {
		for _, d := range p.EgressDomains {
			skip[d] = true
		}
	}

	// The NEWEST maxObservedRuns, at the database, through the Pager seam
	// firstBrokeredRepoFromRuns already uses (setup_checks.go) — this is a
	// member-reachable route that read the WHOLE runs table and then windowed it
	// in Go, so its cost grew with the deployment's entire run history.
	// The bound is now the PAGE rather than the number of matching runs found
	// while walking an unbounded list: an honest, stated ceiling on how far back
	// the advisory panel looks.
	var runs []types.AgentRun
	var err error
	if pg, ok := s.cfg.Store.(store.Pager); ok {
		runs, err = pg.ListRunsPage(r.Context(), store.Page{Limit: maxObservedRuns})
	} else {
		runs, err = s.cfg.Store.ListRuns(r.Context()) // newest-first; only test doubles lack Pager
	}
	if err != nil {
		writeServerError(w, r, "list runs", err)
		return
	}
	denied := map[string]struct{}{}
	scanned := 0
	for _, run := range runs {
		if scanned >= maxObservedRuns {
			break
		}
		if !runUsesWorkspace(run, ws) || !s.ownsRunOrAdmin(r, run) {
			continue
		}
		scanned++
		events, aerr := s.cfg.Store.QueryAuditEvents(r.Context(), run.ID, maxObservedAuditPerRun)
		if aerr != nil {
			continue // best-effort per run
		}
		for _, ev := range events {
			if ev.Action != "egress.deny" {
				continue
			}
			host := strings.ToLower(strings.TrimSpace(ev.Target))
			if host == "" || skip[host] || !hostrules.ValidApprovedHost(host) {
				continue
			}
			denied[host] = struct{}{}
		}
	}
	out := sortedKeys(denied)
	writeJSON(w, http.StatusOK, map[string]any{"denied": out, "runs_examined": scanned})
}

// runUsesWorkspace reports whether a run referenced ws, using the denormalized
// run fields (WorkspacePath = the primary local-dir source; Repo = the repo
// slug/URL) against EVERY one of ws's sources — not just the single-source
// Kind/Source mirror, which is empty for a multi-source workspace. Only the
// PRIMARY workspace is linked on the run, so observed telemetry is scoped to
// runs where ws was primary — a deliberate, honest limit (secondary
// mounts/repos aren't denormalized onto the run).
func runUsesWorkspace(run types.AgentRun, ws types.Workspace) bool {
	for _, src := range ws.Sources {
		switch src.Type {
		case types.WorkspaceSourceTypeLocalDir:
			if run.WorkspacePath != "" && run.WorkspacePath == src.Path {
				return true
			}
		case types.WorkspaceSourceTypeRepo:
			if run.Repo != "" && run.Repo == src.Source {
				return true
			}
		}
	}
	return false
}
