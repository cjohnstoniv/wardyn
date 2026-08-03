// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"maps"
	neturl "net/url"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// workspace_egress.go computes what a workspace contributes to a run's egress
// allowlist: the trusted hosts its scan found, the operator's own approvals,
// the corporate mirrors those get substituted for, the clone hosts every repo
// source needs, and the deliberately WIDER set a confined replay gets. Split
// out of workspace_run.go when that file crossed its size cap; the grouping is
// the real seam — every function here answers "which hosts, and why is that
// honest", and none of them launch anything.

// unionWorkspaceEgress adds every referenced workspace's trusted egress to the
// spec's AllowedDomains (deduped, in-place) and returns what it added. Two
// sources, both operator-sanctioned: the scanned profile's EgressDomains
// (filename-keyed marker table — never file content) and the workspace's
// ApprovedEgress (content-derived suggestions the operator explicitly
// promoted). The scanner's raw SuggestedEgress is deliberately NOT here — a
// hostile file must never widen an allowlist without a human approval. The
// deny-list and confinement floor are unaffected.
func unionWorkspaceEgress(spec *types.RunPolicySpec, workspaces []types.Workspace) []string {
	var added []string
	for _, ws := range workspaces {
		if p, ok := workspaceProfile(ws); ok {
			added = append(added, unionAllowedDomains(spec, p.EgressDomains)...)
		}
		added = append(added, unionAllowedDomains(spec, ws.ApprovedEgress)...)
	}
	return added
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

// substituteArtifactEgress applies the operator's artifact-registry redirects to
// a run's AllowedDomains: for each CONFIGURED ecosystem it DROPS that language's
// public-registry hosts (markers.go's egress* literals, via
// workspacescan.PublicRegistryHosts) and ADDS the corporate mirror host (the host
// of the override base URL). Corp REPLACES public for configured langs;
// unconfigured langs are untouched, and markers.go stays universally correct —
// the substitution lives here, at the composition layer that reads site-config,
// never in the marker literals. Returns a FRESH slice (never mutates the input's
// backing array); a no-op (returns the input) when nothing is configured. A
// malformed override base URL leaves that ecosystem's public hosts in place
// (fail safe: never silently drop egress a build still needs).
func substituteArtifactEgress(domains []string, sc types.SiteConfig) []string {
	if len(sc.ArtifactOverrides) == 0 {
		return domains
	}
	drop := map[string]bool{}
	var add []string
	added := map[string]bool{}
	ecos := slices.Sorted(maps.Keys(sc.ArtifactOverrides)) // deterministic add order
	for _, eco := range ecos {
		corp := strings.ToLower(workspacescan.HostOf(sc.ArtifactOverrides[eco].BaseURL))
		if corp == "" {
			continue
		}
		for _, h := range workspacescan.PublicRegistryHosts(eco) {
			drop[strings.ToLower(h)] = true
		}
		if !added[corp] {
			added[corp] = true
			add = append(add, corp)
		}
	}
	out := make([]string, 0, len(domains)+len(add))
	have := map[string]bool{}
	for _, d := range domains {
		key := strings.ToLower(d)
		if drop[key] || have[key] {
			continue
		}
		have[key] = true
		out = append(out, d)
	}
	for _, a := range add {
		if !have[a] {
			have[a] = true
			out = append(out, a)
		}
	}
	return out
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
func confinedEgressDomains(ws types.Workspace) []string {
	base := &types.RunPolicySpec{AllowedDomains: workspaceCloneEgress(ws)}
	unionWorkspaceEgress(base, []types.Workspace{ws})
	return base.AllowedDomains
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
