// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Workspace providers: the org-level policy for WHERE work may come from (git
// providers and their allowed base URLs) and HOW BIG it may get (the storage
// ceilings). This file owns the write boundary — validation, the two endpoints,
// the audit row — and the two PREDICATES every admission site reads:
//
//	providersConfigured(sc)      is this install in legacy open mode?
//	providerFor(sc, cloneURL)    is this repo admitted, and by which row?
//	effectiveScmHosts(sc)        which git hosts does this deployment admit?
//
// The predicates live here, next to the validation that shapes the rows they
// read, so there is exactly ONE spelling of the match rule and of the host-claim
// precedence — the call sites (workspace create, POST /sources, run create, the
// record/scan launchers, the Build step) only ask.
package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"unicode"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/hostrules"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// PROVIDERS_400 — the refusal bodies this file writes.
//
// DRAFT (M2 canon pending): every string here is provisional until the owner's
// canon sitting freezes it. They are constants, and the tests assert THROUGH the
// constants, so the post-sitting swap is a one-file diff with no test churn.
const (
	providers400BaseURL     = "base_urls[%d]: must be an https URL with a host and no credentials, query or fragment"
	providers400HostKind    = "base_urls[%d]: %q is not a %s host"
	providers400Lane        = "lanes: %q is not available for %s (%s)"
	providers400DiskOrder   = "storage.ephemeral: default_disk_mib may not exceed max_disk_mib"
	providers400DupID       = "id %q is not unique"
	providers400ID          = "git[%d].id: %q is not a provider id — one is written in lowercase ASCII, the same shape an integration id has"
	providers400Kind        = "git[%d].kind: %q is not a git provider kind — want one of: %s"
	providers400BaseURLNone = "git[%d].base_urls: name at least one address (at most %d)"
	providers400LaneKind    = "git[%d].lanes: %q is not a lane — want one of: %s"
	providers400LaneSSHPath = "git %q: the SSH lane has no org path to bound, so it would admit the whole host — drop the SSH lane or widen this row's addresses to the bare host"
	providers400Negative    = "%s: %d is not a size in MiB — use 0 for unset"
	providers412Stale       = "providers changed since you loaded them — reload and retry"

	// laneAppReason / laneSSHReason are the "(%s)" half of providers400Lane.
	laneAppReason = "the App broker mints repository-scoped tokens on github.com only"
	laneSSHReason = "the SSH lane is limited to the two hosts that publish an SSH-over-443 endpoint, github.com and dev.azure.com"
)

// maxProviderBaseURLs bounds one row's base URLs. Eight is "an org and its
// forks", not a policy language — a row needing more is two rows.
const maxProviderBaseURLs = 8

// maxProviderBaseURLPathSegments bounds a base URL's path. One segment is the
// org/collection; two is an org and a project (Azure DevOps) or a GHES
// org/subgroup. Deeper is a REPOSITORY, and a base URL naming one repository is
// an allowlist entry pretending to be a provider.
const maxProviderBaseURLPathSegments = 2

// mountWorkspaceProviderRoutes registers the two provider endpoints. They are a
// mount function rather than two lines in routes() for the reason every other
// route family is: routes() is at its funlen ratchet, and the file's own route-tier
// map lists mounts, not individual routes.
//
// operatorOnly for BOTH, for the identical reason the /site-config pair is: a
// base URL names corporate topology (the org's forge hosts and org paths), so the
// GET is the same disclosure the sibling GET was narrowed for. A member never
// needs it — a member's refusal names the provider KIND only, never the allowed
// addresses — and the member-safe projection (memberSafeIntegration's shape) is
// the later one-line widening, the safe direction routes.go's tier note
// describes. There is no DELETE: removing a row is a PUT without it.
func (s *Server) mountWorkspaceProviderRoutes(operatorOnly chi.Router) {
	operatorOnly.Get("/workspace-providers", s.handleGetWorkspaceProviders)
	operatorOnly.Put("/workspace-providers", s.handlePutWorkspaceProviders)
}

// gitProviderRows is the stored git-provider rows, or nil. Every predicate
// starts here so the nil-block and empty-slice cases can never disagree.
func gitProviderRows(sc types.SiteConfig) []types.GitProvider {
	if sc.WorkspaceProviders == nil {
		return nil
	}
	return sc.WorkspaceProviders.Git
}

// adoServerHosts is every host an azure_devops provider row names — the Azure
// DevOps Server hosts this install knows to be Azure DevOps, which adoscope's
// name rule cannot tell from any other forge by name alone (the service hosts
// it knows itself). A disabled row counts: the question is what a host IS, not
// whether it admits.
func adoServerHosts(sc types.SiteConfig) []string {
	var out []string
	for _, row := range gitProviderRows(sc) {
		if row.Kind != types.GitProviderAzureDevOps {
			continue
		}
		for _, raw := range row.BaseURLs {
			if h := hostrules.HostOf(raw); h != "" && !slices.Contains(out, h) {
				out = append(out, h)
			}
		}
	}
	return out
}

// adoHostsLoader answers adoServerHosts for one request, reading the stored
// site config at most once and only for an address that could need it (see
// forAddresses), so a write naming no such address costs no read. A config
// that cannot be read yields none — the narrower answer: only the Azure DevOps
// service hosts then take the name rule, and an escaped name on any other host
// is refused as it always was. A nil loader answers none.
type adoHostsLoader func() []string

func (s *Server) adoHostsLoader(ctx context.Context) adoHostsLoader {
	var once sync.Once
	var hosts []string
	return func() []string {
		once.Do(func() {
			if s.cfg.Store == nil {
				return
			}
			if sc, err := s.cfg.Store.GetSiteConfig(ctx); err == nil {
				hosts = adoServerHosts(sc)
			}
		})
		return hosts
	}
}

// forAddresses is the Azure DevOps Server hosts to canonicalise and validate
// values with. Only a "%" or whitespace makes the answer matter — the name
// rule leaves every other address as written — so without one nothing is read.
func (l adoHostsLoader) forAddresses(values ...string) []string {
	if l == nil || !slices.ContainsFunc(values, func(v string) bool {
		return strings.ContainsFunc(v, func(r rune) bool { return r == '%' || unicode.IsSpace(r) })
	}) {
		return nil
	}
	return l()
}

// providersConfigured reports whether this install has ANY git-provider row —
// the ONE place "legacy open mode" is decided. Every predicate in this file
// answers "admitted" when it is false, BEFORE any other read, so an upgraded
// 0.7.1 install behaves byte-identically to what it did before this feature
// existed (the absent-row doctrine capEnforced and every GovernanceLimits zero
// value already follow).
//
// It counts rows enabled or disabled: a disabled row is still configuration —
// the admin saying "off" — and falling back to legacy open mode because the only
// row is switched off would be the opposite of what they wrote down.
func providersConfigured(sc types.SiteConfig) bool {
	return len(gitProviderRows(sc)) > 0
}

// storedWorkspaceProviders is the stored block as a VALUE — the shape
// GET /workspace-providers returns and the shape both doors' ETag is computed
// over, so a nil pointer and a present-but-empty block are one document.
func storedWorkspaceProviders(sc types.SiteConfig) types.WorkspaceProviders {
	if sc.WorkspaceProviders == nil {
		return types.WorkspaceProviders{}
	}
	return *sc.WorkspaceProviders
}

// normalizeWorkspaceProviders canonicalizes a block on its way to storage and
// returns nil for an EMPTY one, which is what makes {} the clear form on both
// doors: without it, a caller who cleared their last row would leave
// "workspace_providers":{} rendered on every GET /site-config forever.
//
// Base URLs are stored lowercase-host and trailing-slash-trimmed so the match
// rule never has to normalize at read time (and two rows cannot differ by a
// slash). Ids and kinds are NOT folded: their grammars are lowercase-only, so an
// uppercase value is a refusal at the write boundary, never a silent rewrite.
func normalizeWorkspaceProviders(p *types.WorkspaceProviders) *types.WorkspaceProviders {
	if p == nil {
		return nil
	}
	for i := range p.Git {
		for j, raw := range p.Git[i].BaseURLs {
			// An Azure DevOps row may be scoped to a PROJECT, whose name can
			// carry a space: its path takes the one spelling adoscope's name
			// rule gives, so the match rule compares one string per project.
			if p.Git[i].Kind == types.GitProviderAzureDevOps {
				if c, ok := adoscope.CanonicalURL(strings.TrimSpace(raw)); ok {
					raw = c
				}
			}
			p.Git[i].BaseURLs[j] = normalizeProviderBaseURL(raw)
		}
	}
	if p.Empty() {
		return nil
	}
	return p
}

// validateWorkspaceProviders is the ONE write-boundary gate both doors run
// (PUT /workspace-providers and PUT /site-config) — a nil block is valid (legacy
// open mode), so the common "not configured" case costs nothing.
//
// refuseSSHPathScope (#380 F2) is the ONE rule this shared gate does NOT apply
// identically at both doors: an explicit SSH lane on a path-scoped row is
// refused on the console door (a human is choosing, in the moment, with an
// undo) but NOT on the site-config door (an unattended MDM/CLI re-apply of
// /etc/wardyn/site-config.json on every boot). A managed desktop that cannot
// re-apply its own stored document is a worse failure than the over-admission
// #380 closes — that config predates the rule and has no migration path, so
// the site-config door calls this with false, keeps applying the rest of the
// document, and reports the affected rows instead (sshLaneWidePastPathRows,
// surfaced by handlePutSiteConfig and warned about there) rather than 400ing
// the whole write. handlePutWorkspaceProviders (the console) calls with true.
func validateWorkspaceProviders(p *types.WorkspaceProviders, refuseSSHPathScope bool) error {
	if p == nil {
		return nil
	}
	seen := map[string]bool{}
	for i, row := range p.Git {
		if !integrationRefRE.MatchString(row.ID) {
			return fmt.Errorf(providers400ID, i, row.ID)
		}
		if seen[row.ID] {
			return fmt.Errorf(providers400DupID, row.ID)
		}
		seen[row.ID] = true
		if !row.Kind.Valid() {
			return fmt.Errorf(providers400Kind, i, string(row.Kind),
				strings.Join(types.ClosedGitProviderKindList(), ", "))
		}
		if len(row.BaseURLs) == 0 || len(row.BaseURLs) > maxProviderBaseURLs {
			return fmt.Errorf(providers400BaseURLNone, i, maxProviderBaseURLs)
		}
		if err := validateProviderBaseURLs(i, row); err != nil {
			return err
		}
		if err := validateProviderLanes(i, row, refuseSSHPathScope); err != nil {
			return err
		}
		if err := validateProviderEntra(i, row); err != nil {
			return err
		}
	}
	if err := validateOneEntraRow(p.Git); err != nil {
		return err
	}
	return validateStorageProviders(p.Storage)
}

// validateProviderLanes refuses a lane the row's own addresses can never carry.
// Lanes VETO — they mint nothing — so the only wrong value is one that reads as
// permission for a lane no run could ever take: the App broker exists on
// github.com alone, and the SSH lane reaches only the two hosts publishing an
// SSH-over-443 endpoint (sshOver443Endpoint; a port-22 GHES/ADO-Server host is
// the documented ceiling).
func validateProviderLanes(i int, row types.GitProvider, refuseSSHPathScope bool) error {
	for _, lane := range row.Lanes {
		if !lane.Valid() {
			return fmt.Errorf(providers400LaneKind, i, string(lane),
				strings.Join(types.ClosedGitLaneList(), ", "))
		}
	}
	hasGitHubCom, hasSSHEndpoint := false, false
	for _, raw := range row.BaseURLs {
		host := hostrules.HostOf(raw)
		if host == "github.com" {
			hasGitHubCom = true
		}
		if _, ok := sshOver443Endpoint(host); ok {
			hasSSHEndpoint = true
		}
	}
	if slices.Contains(row.Lanes, types.GitLaneApp) && !hasGitHubCom {
		return fmt.Errorf(providers400Lane, string(types.GitLaneApp), string(row.Kind), laneAppReason)
	}
	if slices.Contains(row.Lanes, types.GitLaneSSH) {
		if !hasSSHEndpoint {
			return fmt.Errorf(providers400Lane, string(types.GitLaneSSH), string(row.Kind), laneSSHReason)
		}
		// EXPLICIT SSH only: an empty Lanes list also carries the SSH lane
		// (laneAllowed's default-allow), and that case keeps admitting host-level
		// with only the admitSSHHostLevel WARNING, exactly as it does today — an
		// operator who never touched Lanes must not have a stored row start
		// refusing writes or PUTs on managed-desktop re-apply. An operator who
		// TICKED the SSH lane on a row an org path also scopes gets refused here
		// instead: SSH carries no path to bound (cloneTarget, sshAdmittedAbovePath),
		// so the org scoping the row's own addresses claim to have is fiction for
		// that lane, and admitting it silently is the bug #380 reports.
		//
		// refuseSSHPathScope is false ONLY on the site-config door (see
		// validateWorkspaceProviders's doc) — a stored document written before
		// this rule existed must keep re-applying; the console door always
		// passes true.
		if refuseSSHPathScope && sshLaneExceedsPathScope(row) {
			return fmt.Errorf(providers400LaneSSHPath, row.ID)
		}
	}
	return nil
}

// validateStorageProviders holds the two storage halves to the one invariant
// each has at the write boundary: a size is a non-negative number of MiB (0 =
// unset/unlimited), and a DEFAULT above a non-zero MAXIMUM is a contradiction,
// not a policy — the clamp at dispatch would silently undo it on every run.
func validateStorageProviders(st *types.StorageProviders) error {
	if st == nil {
		return nil
	}
	if e := st.Ephemeral; e != nil {
		if e.DefaultDiskMiB < 0 {
			return fmt.Errorf(providers400Negative, "storage.ephemeral.default_disk_mib", e.DefaultDiskMiB)
		}
		if e.MaxDiskMiB < 0 {
			return fmt.Errorf(providers400Negative, "storage.ephemeral.max_disk_mib", e.MaxDiskMiB)
		}
		if e.MaxDiskMiB > 0 && e.DefaultDiskMiB > e.MaxDiskMiB {
			return fmt.Errorf("%s", providers400DiskOrder)
		}
	}
	if d := st.UserDrive; d != nil && d.MaxSizeMiB < 0 {
		return fmt.Errorf(providers400Negative, "storage.user_drive.max_size_mib", d.MaxSizeMiB)
	}
	return nil
}

// cloneTarget is a clone URL reduced to the three facts the match rule compares.
// SSH is its own shape because an SSH clone URL carries no comparable path
// (git@ssh.dev.azure.com:v3/org/... is not /org/...), so org-path scoping is
// host-level only for SSH in v1 — a documented ceiling, not an oversight.
type cloneTarget struct {
	scheme string
	host   string
	path   string
	ssh    bool
}

// parseCloneTarget reduces a DERIVED clone URL (repoCloneURL's output, never a
// raw slug) to a cloneTarget. ok=false for anything it cannot read — including
// the empty string repoCloneURL returns for a transport that already failed
// closed — so an unreadable target is REFUSED rather than admitted by accident.
//
// The port is deliberately not part of the comparison: hostrules.HostOf discards
// it and so does the egress allowlist, so treating it as a scoping boundary here
// would claim a narrowing the rest of the system does not implement.
func parseCloneTarget(cloneURL string, adoServerHosts []string) (cloneTarget, bool) {
	raw := strings.TrimSpace(cloneURL)
	if raw == "" {
		return cloneTarget{}, false
	}
	// The traversal guard, before either form is read: a path the server and the
	// sandbox's git read differently is not a target this function can reduce
	// honestly, so it is UNREADABLE and therefore refused — with the same
	// operator/member sentences every other unreadable target earns. See
	// repoLocatorPathSafe (runs_scm.go) for why no spelling of pathAdmits
	// survives one.
	if !repoLocatorPathSafe(raw, adoServerHosts) {
		return cloneTarget{}, false
	}
	// sshCloneHost answers only for ssh:// and scp-form strings (it refuses
	// anything else carrying a scheme), so an https clone URL falls through.
	if host, ok := sshCloneHost(raw); ok {
		return cloneTarget{scheme: "ssh", host: host, ssh: true}, true
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return cloneTarget{}, false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return cloneTarget{}, false
	}
	return cloneTarget{
		scheme: scheme,
		host:   strings.ToLower(u.Hostname()),
		path:   strings.TrimSuffix(u.Path, "/"),
	}, true
}

// canonicalProviderHost folds the ssh.<host> alias onto its primary so one
// stored base URL admits (and CLAIMS) both clone forms — the same fold
// canonicalSSHKeySecret applies to the secret name.
func canonicalProviderHost(host string) string {
	return strings.TrimPrefix(host, "ssh.")
}

// hostsMatch compares a clone target's host with a provider base URL's host.
// For an SSH target the ssh.<host> alias matches in either direction; for an
// https target the host must be equal (case already folded by both sides).
func hostsMatch(t cloneTarget, baseHost string) bool {
	if t.host == baseHost {
		return true
	}
	return t.ssh && canonicalProviderHost(t.host) == canonicalProviderHost(baseHost)
}

// pathAdmits reports whether a clone path is inside a base URL's path: equal, or
// extending it AT A "/" BOUNDARY. The boundary is the whole point — without it a
// base URL of /acme would admit /acme-evil/repo.
//
// Case handling is the CALLER's (rowAdmits, via foldsPathCase): github.com
// treats /Acme and /acme as one org, so both sides are folded there; every other
// host is compared as written, which refuses a case the admin did not name
// rather than admitting one they did not.
func pathAdmits(clonePath, basePath string) bool {
	if basePath == "" || basePath == "/" {
		return true
	}
	return clonePath == basePath || strings.HasPrefix(clonePath, basePath+"/")
}

// rowAdmits reports whether one provider row's base URLs admit a clone target.
func rowAdmits(row types.GitProvider, t cloneTarget) bool {
	for _, raw := range row.BaseURLs {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" {
			continue
		}
		baseHost := strings.ToLower(u.Hostname())
		if !hostsMatch(t, baseHost) {
			continue
		}
		if t.ssh {
			return true // host-level only for SSH, see cloneTarget
		}
		if strings.ToLower(u.Scheme) != t.scheme {
			continue
		}
		clonePath, basePath := t.path, strings.TrimSuffix(u.Path, "/")
		if foldsPathCase(baseHost) {
			clonePath, basePath = strings.ToLower(clonePath), strings.ToLower(basePath)
		}
		if pathAdmits(clonePath, basePath) {
			return true
		}
	}
	return false
}

// sshAdmittedAbovePath reports whether a row admitted an SSH target ONLY because
// SSH scoping is host-level: the target is SSH and every base URL of this row
// that matches its host carries an org path. That is the documented ceiling
// (see cloneTarget) and it is REAL — `https://github.com/acme` admits
// `git@github.com:other-org/x.git` — so the callers say it out loud rather than
// letting a path-scoped row look narrower than it is.
func sshAdmittedAbovePath(row types.GitProvider, t cloneTarget) bool {
	if !t.ssh {
		return false
	}
	for _, raw := range row.BaseURLs {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" {
			continue
		}
		if hostsMatch(t, strings.ToLower(u.Hostname())) && strings.Trim(u.Path, "/") == "" {
			return false // this row bounds the whole host anyway; nothing was widened
		}
	}
	return rowAdmits(row, t)
}

// rowClaimsHost reports whether one row CLAIMS a host — the precedence question,
// which is separate from whether it admits a given path.
//
// By HOST, and kind-wide on the two well-known hosts, because a kind cannot be
// derived for a self-hosted forge. Without this, the documented MDM content
// scm_hosts: ["github.com"] would admit github.com/other/* past a provider
// limited to https://github.com/acme, making org-path scoping vacuous on exactly
// the fleets this feature is for.
func rowClaimsHost(row types.GitProvider, t cloneTarget) bool {
	host := canonicalProviderHost(t.host)
	switch row.Kind {
	case types.GitProviderGitHub:
		if host == "github.com" {
			return true
		}
	case types.GitProviderAzureDevOps:
		if adoEgressDomains(host) != nil {
			return true
		}
	}
	for _, raw := range row.BaseURLs {
		if h := hostrules.HostOf(raw); h != "" && hostsMatch(t, h) {
			return true
		}
	}
	return false
}

// providerVerdict is admitRepoURL's full answer. providerFor is the thin
// two-value predicate over it; the extra bits exist because the two
// admitted-WITHOUT-a-row cases are reported differently to the caller (an
// unclaimed legacy host earns a warning, legacy open mode says nothing).
type providerVerdict struct {
	// Provider is the row that decided — the one that admitted, or the one whose
	// claim refused. Zero when no row was involved.
	Provider types.GitProvider
	Admitted bool
	// Unconfigured: this install has no provider rows (legacy open mode).
	Unconfigured bool
	// LegacyHost: admitted only because SiteConfig.ScmHosts still names its host
	// and NO provider row claims it — the ADMIT.LEGACY_HOST warning's condition.
	LegacyHost bool
}

// admitRepoURL is the whole admission decision for one DERIVED clone URL.
//
// Order is the precedence, and it is the order that matters:
//  1. no provider rows at all => admitted, byte-identically to 0.7.1;
//  2. an ENABLED row whose base URLs admit it => admitted, by that row;
//  3. any PRESENT row (enabled or disabled) that CLAIMS the host => refused —
//     a disabled row is the admin saying "off", and an enabled row that did not
//     match means the path is outside the addresses they listed;
//  4. an UNCLAIMED host still on the legacy ScmHosts list => admitted, with a
//     warning, for one release (an onboarded GitLab/Bitbucket repo must not go
//     un-launchable the day the first GitHub provider row is added);
//  5. anything else => refused.
func admitRepoURL(sc types.SiteConfig, cloneURL string) providerVerdict {
	rows := gitProviderRows(sc)
	if len(rows) == 0 {
		return providerVerdict{Admitted: true, Unconfigured: true}
	}
	t, ok := parseCloneTarget(cloneURL, adoServerHosts(sc))
	if !ok {
		return providerVerdict{}
	}
	for _, row := range rows {
		if !row.Disabled && rowAdmits(row, t) {
			return providerVerdict{Provider: row, Admitted: true}
		}
	}
	for _, row := range rows {
		if rowClaimsHost(row, t) {
			return providerVerdict{Provider: row}
		}
	}
	for _, h := range sc.ScmHosts {
		if hostsMatch(t, strings.ToLower(strings.TrimSpace(h))) {
			return providerVerdict{Admitted: true, LegacyHost: true}
		}
	}
	return providerVerdict{}
}

// providerFor is the admission predicate every call site asks: may this clone
// URL be cloned, and which row said so (zero when no row was involved — legacy
// open mode or an unclaimed legacy host; admitRepoURL answers which).
func providerFor(sc types.SiteConfig, cloneURL string) (types.GitProvider, bool) {
	v := admitRepoURL(sc, cloneURL)
	return v.Provider, v.Admitted
}

// effectiveScmHosts is the ONE answer to "which git hosts does this deployment
// admit": the legacy ScmHosts list MINUS every host a PRESENT provider row
// claims, UNION the hosts of every ENABLED row's base URLs.
//
// The subtraction is what keeps the three read-side consumers honest. Without
// it, scm_hosts: ["github.com"] plus a DISABLED github row would still union
// github.com into every run's egress and show GitHub "Connected" while admission
// refused it; and scm_hosts: ["dev.azure.com"] plus an ENABLED azure_devops row
// for https://acme.visualstudio.com would union dev.azure.com while the kind-wide
// claim refused every dev.azure.com repo.
//
// Read-side only. Nothing folds this back into ScmHosts: that list stays legacy,
// keeps its own validator and audit count, and is never written by this feature —
// a fold would turn github.com entries into provider rows and flip
// providersConfigured on the first PUT with no admin action at all.
func effectiveScmHosts(sc types.SiteConfig) []string {
	rows := gitProviderRows(sc)
	var out []string
	seen := map[string]bool{}
	add := func(h string) {
		if h != "" && !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	for _, raw := range sc.ScmHosts {
		h := strings.ToLower(strings.TrimSpace(raw))
		if h == "" {
			continue
		}
		claimed := false
		for _, row := range rows {
			if rowClaimsHost(row, cloneTarget{host: h}) {
				claimed = true
				break
			}
		}
		if !claimed {
			add(h)
		}
	}
	for _, row := range rows {
		if row.Disabled {
			continue
		}
		for _, raw := range row.BaseURLs {
			add(hostrules.HostOf(raw))
		}
	}
	return out
}

// sourcesNoLongerAdmitted counts the repo locators already onboarded here that
// the CANDIDATE config would refuse — every onboarded workspace's repo sources
// plus every repo in the source library, through the same providerFor the
// admission sites use.
//
// Narrowing is never silent: this number rides the PUT's response body (the
// console shows it in the save toast) and the audit datum, so an admin who
// tightens a base URL learns what they just cut off. An error here FAILS the
// write rather than reporting a comforting 0 — a false "nothing was affected" is
// the one answer this function must never give.
func (s *Server) sourcesNoLongerAdmitted(ctx context.Context, candidate types.SiteConfig) (int, error) {
	// Deduped by clone URL: one repository attached to a workspace AND sitting in
	// the source library is ONE repo the admin is about to cut off, and counting
	// it twice overstates the blast radius of their own narrowing.
	refused := map[string]bool{}
	count := func(locator string) {
		cloneURL := repoCloneURL(locator)
		if _, ok := providerFor(candidate, cloneURL); !ok {
			if cloneURL == "" {
				cloneURL = locator // no derivable URL: the locator is its own identity
			}
			refused[strings.ToLower(strings.TrimSuffix(cloneURL, ".git"))] = true
		}
	}
	workspaces, err := s.cfg.Store.ListWorkspaces(ctx)
	if err != nil {
		return 0, fmt.Errorf("list workspaces: %w", err)
	}
	for _, ws := range workspaces {
		for _, src := range ws.Sources {
			if src.Type == types.WorkspaceSourceTypeRepo {
				count(src.Source)
			}
		}
	}
	sources, err := s.cfg.Store.ListSources(ctx)
	if err != nil {
		return 0, fmt.Errorf("list sources: %w", err)
	}
	for _, src := range sources {
		if src.Kind == types.SourceRepo {
			count(src.Locator)
		}
	}
	return len(refused), nil
}

// workspaceProvidersPutResponse is PUT /workspace-providers's body: the
// persisted block plus the one advisory signal the write produces.
type workspaceProvidersPutResponse struct {
	types.WorkspaceProviders
	// SourcesNoLongerAdmitted is how many already-onboarded repo locators this
	// block refuses — always present, including as 0, because "0" is the
	// reassurance an admin narrowing a base URL is looking for.
	SourcesNoLongerAdmitted int `json:"sources_no_longer_admitted"`
}

// handleGetWorkspaceProviders returns the stored provider block.
//
// operatorOnly, for the same reason GET /site-config is (routes.go): base URLs
// name corporate topology. A member-safe projection (the memberSafeIntegration
// shape) is the later one-line widening — the safe direction.
//
// The response carries an ETag so a caller that means to base a later PUT on
// exactly this read can send it back as If-Match; a never-configured install
// gets the zero-value document with 200, never a 404.
//
//	GET /api/v1/workspace-providers
func (s *Server) handleGetWorkspaceProviders(w http.ResponseWriter, r *http.Request) {
	sc, err := s.cfg.Store.GetSiteConfig(r.Context())
	if err != nil {
		writeServerError(w, r, "get site config", err)
		return
	}
	block := storedWorkspaceProviders(sc)
	w.Header().Set("ETag", computeETag(block))
	// GitPatBrokerEnabled (#381) is projected AFTER the ETag, exactly like
	// SiteConfig's EffectiveScmHosts: a live env switch, not stored config, so
	// it must never perturb the optimistic-concurrency comparison below. Gated
	// on providersConfigured so a never-configured install keeps getting the
	// byte-identical "{}" a pre-#381 build did (TestWorkspaceProvidersGet) —
	// there is no lanes UI to annotate until a git row exists.
	if providersConfigured(sc) {
		on := !s.cfg.DisableGitPATBroker
		block.GitPatBrokerEnabled = &on
	}
	writeJSON(w, http.StatusOK, block)
}

// handlePutWorkspaceProviders replaces the WHOLE provider block: there is no
// delete route because removing a row is PUTting the document without it, and
// {} is the clear form (normalizeWorkspaceProviders turns it back into an absent
// key rather than an empty object rendered forever).
//
// If-Match (etag.go) is optional optimistic concurrency: absent behaves as
// last-writer-wins exactly as PUT /site-config does, a present-but-stale value
// is refused with 412 before the write reaches the store. The console keeps the
// GET's ETag and sends it on every PUT, so two admins on the providers page
// cannot silently overwrite each other.
//
//	PUT /api/v1/workspace-providers
func (s *Server) handlePutWorkspaceProviders(w http.ResponseWriter, r *http.Request) {
	var body types.WorkspaceProviders
	if !decodeStrict(w, r, &body) {
		return
	}
	block := normalizeWorkspaceProviders(&body)
	// GitPatBrokerEnabled is projected on read and never stored — a value that
	// rode in on a GET-spread PUT body would otherwise persist into the JSONB
	// (harmlessly inert, since every GET recomputes it fresh, but the same
	// hygiene EffectiveScmHosts gets below).
	if block != nil {
		block.GitPatBrokerEnabled = nil
	}
	// true: the console door — a human is choosing this write, in the moment,
	// with an undo (validateWorkspaceProviders's doc, #380 F2).
	if err := validateWorkspaceProviders(block, true); err != nil {
		writeError(w, http.StatusBadRequest, "invalid workspace providers: "+err.Error())
		return
	}
	// SEAM-1: serializes this read-modify-write against the site config's other
	// writers (handlePutSiteConfig and the two integration handlers), which read
	// and rewrite the SAME singleton document — see handlePutIntegration's
	// SEAM-1 comment for why an unguarded RMW here silently erases a concurrent
	// one.
	s.siteConfigMu.Lock()
	defer s.siteConfigMu.Unlock()
	ctx := r.Context()
	existing, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		writeServerError(w, r, "get site config", err)
		return
	}
	if !ifMatchSatisfied(r, computeETag(storedWorkspaceProviders(existing))) {
		writeError(w, http.StatusPreconditionFailed, providers412Stale)
		return
	}
	candidate := existing
	candidate.WorkspaceProviders = block
	refused, err := s.sourcesNoLongerAdmitted(ctx, candidate)
	if err != nil {
		writeServerError(w, r, "count sources this block refuses", err)
		return
	}
	// EffectiveScmHosts is projected on read and never stored — a value that
	// rode in on a GET-spread body would otherwise be persisted into the JSONB.
	candidate.EffectiveScmHosts = nil
	saved, err := s.cfg.Store.PutSiteConfig(ctx, candidate)
	if err != nil {
		writeServerError(w, r, "put site config", err)
		return
	}
	savedBlock := storedWorkspaceProviders(saved)
	s.recordAudit(ctx, s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"workspace_provider.write", "workspace_providers", "success",
		mustJSON(workspaceProviderAuditData(savedBlock, refused))))
	w.Header().Set("ETag", computeETag(savedBlock))
	// GitPatBrokerEnabled is projected onto the RESPONSE the same way GET
	// projects it (#381 F4) — AFTER the ETag above, which must hash the stored
	// shape. Without this the console's post-Save setDraft(result.providers)
	// dropped the field, `?? true` silently took over, and a broker-off
	// install showed the right label until Save and the wrong one after, until
	// the next reload re-fetched it from GET.
	if providersConfigured(saved) {
		on := !s.cfg.DisableGitPATBroker
		savedBlock.GitPatBrokerEnabled = &on
	}
	writeJSON(w, http.StatusOK, workspaceProvidersPutResponse{
		WorkspaceProviders: savedBlock, SourcesNoLongerAdmitted: refused,
	})
}

// workspaceProviderAuditData is workspace_provider.write's datum: what an
// incident review actually needs from this event — how many rows, which forges,
// WHICH ADDRESSES, which lanes, which rows are off, and what the write cut off.
//
// Base URLs are in the clear on purpose: they are topology, not credentials —
// the same disclosure integration.write already makes with `egress` — and a
// narrowing or opening applied by MDM is unreviewable without them.
func workspaceProviderAuditData(block types.WorkspaceProviders, refused int) map[string]any {
	kinds, lanes, urls, disabled := []string{}, []string{}, []string{}, []string{}
	for _, row := range block.Git {
		if !slices.Contains(kinds, string(row.Kind)) {
			kinds = append(kinds, string(row.Kind))
		}
		urls = append(urls, row.BaseURLs...)
		for _, lane := range row.Lanes {
			if !slices.Contains(lanes, string(lane)) {
				lanes = append(lanes, string(lane))
			}
		}
		if row.Disabled {
			disabled = append(disabled, row.ID)
		}
	}
	slices.Sort(kinds)
	slices.Sort(lanes)
	slices.Sort(urls)
	slices.Sort(disabled)
	return map[string]any{
		"git_count": len(block.Git), "kinds": kinds, "base_urls": urls,
		"lanes": lanes, "disabled": disabled,
		"sources_no_longer_admitted": refused,
	}
}

// enabledGitProviderCount is site_config.write's count of ENABLED rows — the
// number that says how much this document narrows, which a disabled row does
// not contribute to.
func enabledGitProviderCount(sc types.SiteConfig) int {
	n := 0
	for _, row := range gitProviderRows(sc) {
		if !row.Disabled {
			n++
		}
	}
	return n
}

// storageProvidersConfigured reports whether either storage half carries policy
// — the boolean site_config.write records so an MDM-applied storage ceiling is
// visible in the audit log.
func storageProvidersConfigured(sc types.SiteConfig) bool {
	return sc.WorkspaceProviders != nil && sc.WorkspaceProviders.Storage != nil &&
		(sc.WorkspaceProviders.Storage.Ephemeral != nil || sc.WorkspaceProviders.Storage.UserDrive != nil)
}

// the member capability gate

// capProvider403 is the member's refusal when a git provider row this
// deployment admits is one they hold no capability grant for.
//
// It names the provider KIND and NOTHING ELSE — never a base URL, never the
// row's id. GET /workspace-providers is a SUPER-tier door precisely because
// base URLs name corporate topology, and a 403 body that listed them would be
// that same document handed to the tier the door refuses. The kind
// ("github", "azure_devops") is a closed enum this build already ships in its
// own documentation, so it discloses nothing the console does not already say.
//
// DRAFT (M2 canon pending), the same terms as the PROVIDERS_400 block above.
const capProvider403 = "you are not granted this deployment's %s provider — ask an admin to grant it, " +
	"or launch against a repository on a provider you hold"

// denyMemberWorkspaceProviders is the member half of provider admission: of the
// repositories this request brings in, is every one on a provider row the
// caller holds? Reports true — having written the 403 and an authz.denied row
// carrying `capability_workspace_provider` — when the caller must stop.
//
// It asks admitRepoURL ONLY to learn WHICH ROW a repository belongs to, and
// keys on `Provider` — NEVER on `Admitted`. The two differ on exactly the case
// org-path scoping exists for: a row that CLAIMS the host and refuses anyway (a
// disabled row, or an enabled one whose base paths did not match) answers
// Admitted=false while naming the row that decided. Keying on the admission bit
// would skip the capability check there and leave the refusal to the admission
// site alone, which is the opposite of what this helper's contract says.
//
// The admission verdict itself (is this repository admissible at all?) is a
// separate, operator-binding question wired at the admission sites; a
// repository NO row matches — legacy open mode, or a host still admitted
// through the legacy scm_hosts list — is a NO-OP here, because there is no row
// for a grant to name and the capability has nothing to say about it.
//
// Operators are exempt in one line, before the site-config read, exactly as
// denyMemberRequest is: nothing below ever costs them a store round-trip. A
// build with no store at all answers "allowed", which is capSeamAllowed's own
// documented rule for a seam running in a harness that holds no rows.
//
// repos are RAW sources (a slug, an https URL or an scp-form SSH target); the
// derived clone URL is computed HERE, once, so no call site can compare a bare
// <org>/<name> against a base URL and miss.
func (s *Server) denyMemberWorkspaceProviders(w http.ResponseWriter, r *http.Request, target string, repos ...string) bool {
	if len(repos) == 0 || s.cfg.Store == nil || s.isOperator(r.Context()) {
		return false
	}
	sc, err := s.cfg.Store.GetSiteConfig(r.Context())
	if err != nil {
		writeServerError(w, r, "get site config", err)
		return true
	}
	if !providersConfigured(sc) {
		return false // legacy open mode — byte-identical to 0.7.1
	}
	seen := map[string]bool{}
	for _, repo := range repos {
		row := admitRepoURL(sc, repoCloneURL(repo)).Provider
		if row.ID == "" || seen[row.ID] {
			continue
		}
		seen[row.ID] = true
		if s.denyMemberCapability(w, r, capWorkspaceProvider, row.ID, target,
			fmt.Sprintf(capProvider403, row.Kind)) {
			return true
		}
	}
	return false
}

// repoSourceLocators is the raw repo source of every repo entry in sources —
// the shape denyMemberWorkspaceProviders takes, and the one every workspace
// door already holds.
func repoSourceLocators(sources []types.WorkspaceSource) []string {
	var out []string
	for _, src := range sources {
		if src.Type == types.WorkspaceSourceTypeRepo && src.Source != "" {
			out = append(out, src.Source)
		}
	}
	return out
}
