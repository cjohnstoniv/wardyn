// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// This file is the CALL-SITE half of workspace-provider policy: the admission
// verdict wired at every door a repository can enter through, and the lane veto
// wired at every site that mints a git credential. The MODEL — the rows, their
// validation, the match rule and the verdict itself — lives in
// workspace_providers.go and is never re-implemented here (admitRepoURL is the
// only opinion about what a base URL admits; there is exactly one).
//
// The ten admission sites, and why each one is a door rather than a duplicate:
//
//	 1. POST   /workspaces                 handleCreateWorkspace
//	 2. PUT    /workspaces/{id}            handleUpdateWorkspace  (over the INCOMING sources)
//	 3. POST   /workspaces/{id}/scan       handleScanWorkspace    (a server-side clone)
//	 4. POST   /workspaces/{id}/build      handleBuildWorkspace   (a server-side clone)
//	 5. POST   /sources                    handleCreateSource     (the library, outside the workspace table)
//	 6. POST   /runs                       seedAndAdmitWorkspace  (the RESOLVED spec — the un-bypassable one)
//	 7. POST   /runs  req.repo             decodeAndValidateCreateRun
//	 8. POST   /runs  req.devcontainer_repo  decodeAndValidateCreateRun
//	 9. launchRecordRun                    (record's clone, past every request-path gate)
//	10. launchSourceScanRun                (the scan launcher, ditto)
//
// Sites 9 and 10 are the two launchers whose Store.CreateRun call passes
// through neither decodeAndValidateCreateRun nor validateWorkspaceSources — the
// caller census in the tests is what keeps that list honest as launchers are
// added.
//
// Legacy open mode is the first question everywhere. providersConfigured(sc) is
// false on every install that has authored no provider row, and every entry
// point below returns "nothing to say" before reading anything else, so an
// upgraded deployment answers byte-for-byte what it answered before this
// file existed.

// The ADMIT.* strings — DRAFT (M2 canon pending). Frozen in
// docs/design/workspace-providers-prompt.md §7.1/§7.4; the tests assert THROUGH
// these constants so the M2 swap is a one-file diff.
//
// The DISCLOSURE RULE is why there are five refusals and not one:
// GET /workspace-providers is operator-only because base URLs name corporate
// topology, so a MEMBER's refusal names the shape of the problem and nothing
// else — never a base URL, never a row id, never another tenant's provider. An
// OPERATOR's refusal lists the addresses, because they are the person who can
// change them and they can already read them from the providers surface.
const (
	// admitMember is the whole of what a member is told, at every one of the ten
	// sites. It is deliberately identical everywhere: which door refused is not
	// a fact a member can act on, and varying the sentence per site would leak
	// the shape of the policy one refusal at a time.
	admitMember = "this repository's host is not an enabled git provider — ask an admin"
	// admitOperator is the ordinary operator refusal: nothing claims this host.
	admitOperator = "repository %s is outside every enabled provider's allowed addresses (%s)"
	// admitOperatorClaimed is the refusal for a repo on a host the legacy
	// scm_hosts list still names, which a provider row has since CLAIMED — so
	// the addresses to compare against are that row's, not the union.
	admitOperatorClaimed = "repository %s is on a host claimed by the %s provider %q " +
		"and is outside its allowed addresses (%s)"
	// admitDisabledRow is the refusal from a present-but-disabled row: the admin
	// said "off", and saying "outside the allowed addresses" would send them
	// looking for a base URL to widen instead of a switch to flip.
	admitDisabledRow = "repository %s is on the %s provider %q, which is turned off for this deployment"
	// admitLaneDropped is a 201 WARNING, never a refusal: a lane the row does
	// not permit drops its wiring and says so. This is the MEMBER's form — a
	// member receives the 201 of their own run, so the warning is held to the
	// same disclosure rule the refusals are and names the kind alone.
	admitLaneDropped = "the %s lane is not permitted for %s; the %s grant was not wired"
	// admitLaneDroppedOperator adds the row an operator would have to edit.
	admitLaneDroppedOperator = "the %s lane is not permitted for the %s provider %q; the %s grant was not wired"
	// admitLegacyHost is the other 201 warning: admitted for one release because
	// nothing claims its host, which is a state the admin should close.
	admitLegacyHost = "host %s is admitted through the legacy scm_hosts list; enable a provider for it before 0.8"
	// admitSSHHostLevel is the third 201 warning: the SSH scoping CEILING, said
	// out loud at the one moment it actually matters. An SSH clone URL carries no
	// path a base URL can be compared against, so a row scoped to one org admits
	// SSH for the WHOLE host — which is a narrower policy than the admin wrote.
	// Held to the disclosure rule like every other member-visible sentence: the
	// kind, never the row id and never a base URL.
	admitSSHHostLevel = "%s is admitted over SSH at the host level; the %s provider's org paths bound HTTPS clones only"
)

// errRepoNotAdmitted marks a launch refused by provider admission, so the
// handler that started the launcher can answer the admission status instead of
// the 500 every other launcher error earns — the errAgentNotEnabled shape,
// which exists for exactly the same reason on the same two launchers.
var errRepoNotAdmitted = errors.New("repo not admitted")

// admissionRefusalStatus is the status half of the disclosure rule. An OPERATOR
// gets 422: the request is well-formed and the POLICY refuses it, which is the
// same thing validateWorkspaceSources answers for an un-onboarded repo. A
// MEMBER gets 403: for them this is an authorization boundary, and a 422 would
// invite them to go on editing the body until it worked.
func admissionRefusalStatus(operator bool) int {
	if operator {
		return http.StatusUnprocessableEntity
	}
	return http.StatusForbidden
}

// admitRepoSources is the admission chokepoint at a REQUEST door: are all of
// these repositories on an enabled provider? Reports true — having written the
// refusal — when the caller must stop.
//
// It runs BEFORE the member capability check at every site it shares with
// denyMemberWorkspaceProviders, and the order is deliberate: admission is the
// operator-binding question ("may anyone clone this here?"), and a repository
// nobody may clone must not first be reported as a missing grant the admin
// could hand out.
//
// repos are RAW sources (a slug, an https URL, an scp-form SSH target) — the
// derived clone URL is computed inside, once, so no call site can compare a
// bare <org>/<name> against a base URL and miss.
func (s *Server) admitRepoSources(w http.ResponseWriter, r *http.Request, repos ...string) bool {
	repos = presentRepos(repos)
	if len(repos) == 0 || s.cfg.Store == nil {
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
	operator := s.isOperator(r.Context())
	for _, repo := range repos {
		if msg := admissionRefusal(sc, repo, operator); msg != "" {
			writeError(w, admissionRefusalStatus(operator), msg)
			return true
		}
	}
	s.auditLegacyHostAdmissions(r.Context(), sc, repos)
	return false
}

// admitLauncherRepo is admitRepoSources for a SERVER-SIDE launcher, which holds
// no ResponseWriter: the refusal comes back as an error wrapped in
// errRepoNotAdmitted, and the handler that kicked the launcher maps it with
// writeAdmissionLaunchRefusal. A site-config read failure FAILS CLOSED (the
// launchers' own doctrine: carrying on would silently substitute "no policy" for
// a policy the daemon simply could not read).
func (s *Server) admitLauncherRepo(ctx context.Context, repos ...string) error {
	repos = presentRepos(repos)
	if len(repos) == 0 || s.cfg.Store == nil {
		return nil
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return fmt.Errorf("get site config: %w", err)
	}
	if !providersConfigured(sc) {
		return nil
	}
	operator := s.isOperator(ctx)
	for _, repo := range repos {
		if msg := admissionRefusal(sc, repo, operator); msg != "" {
			return fmt.Errorf("%w: %s", errRepoNotAdmitted, msg)
		}
	}
	s.auditLegacyHostAdmissions(ctx, sc, repos)
	return nil
}

// writeAdmissionLaunchRefusal answers a launcher's admission refusal with the
// SAME status and the SAME body a request door writes — the two must not
// disagree about what "this repository is not on an enabled provider" costs.
// Reports false for any other error, which the caller maps as it always did.
func (s *Server) writeAdmissionLaunchRefusal(w http.ResponseWriter, r *http.Request, err error) bool {
	if !errors.Is(err, errRepoNotAdmitted) {
		return false
	}
	writeError(w, admissionRefusalStatus(s.isOperator(r.Context())),
		strings.TrimPrefix(err.Error(), errRepoNotAdmitted.Error()+": "))
	return true
}

// admissionRefusal is the ONE sentence one repository earns, or "" when it is
// admitted. Every branch reads admitRepoURL's verdict — the refusals differ only
// in what they DISCLOSE, never in what they decide.
func admissionRefusal(sc types.SiteConfig, repo string, operator bool) string {
	cloneURL := repoCloneURL(repo)
	v := admitRepoURL(sc, cloneURL)
	if v.Admitted {
		return ""
	}
	if !operator {
		return admitMember
	}
	switch row := v.Provider; {
	case row.ID == "":
		// No row claimed the host at all: the useful list is the union.
		return fmt.Sprintf(admitOperator, repo, strings.Join(enabledProviderAddresses(sc), ", "))
	case row.Disabled:
		return fmt.Sprintf(admitDisabledRow, repo, row.Kind, row.ID)
	case legacyScmHostNamed(sc, cloneURL):
		// The cliff guard, disclosed: this host IS still on the legacy list, and
		// a provider row's (possibly kind-wide) claim is what refused it. The
		// addresses to widen are that row's, so the union would misdirect.
		return fmt.Sprintf(admitOperatorClaimed, repo, row.Kind, row.ID, strings.Join(row.BaseURLs, ", "))
	default:
		return fmt.Sprintf(admitOperator, repo, strings.Join(enabledProviderAddresses(sc), ", "))
	}
}

// enabledProviderAddresses is every base URL an ENABLED row admits, in authored
// order — the "allowed addresses" half of the operator's refusal. Disabled rows
// are deliberately absent: listing an address that refuses everything would send
// the reader to widen a row they need to enable.
func enabledProviderAddresses(sc types.SiteConfig) []string {
	var out []string
	for _, row := range gitProviderRows(sc) {
		if !row.Disabled {
			out = append(out, row.BaseURLs...)
		}
	}
	return out
}

// legacyScmHostNamed reports whether the legacy ScmHosts list still names this
// clone target's host. It is the fact that separates "an admin never listed this
// host" from "an admin listed it and a provider row then claimed its kind" —
// admitRepoURL already decided both cases identically (refused); only the
// sentence differs. Reuses the same two predicates the match rule does.
func legacyScmHostNamed(sc types.SiteConfig, cloneURL string) bool {
	t, ok := parseCloneTarget(cloneURL, adoServerHosts(sc))
	if !ok {
		return false
	}
	for _, h := range sc.ScmHosts {
		if hostsMatch(t, strings.ToLower(strings.TrimSpace(h))) {
			return true
		}
	}
	return false
}

// legacyHostAdmittedHosts is the host of every repo in the list that was
// admitted ONLY because nothing claims it and the legacy scm_hosts list still
// names it — deduped, because the fact is about the HOST, not the repo.
func legacyHostAdmittedHosts(sc types.SiteConfig, repos []string) []string {
	var hosts []string
	seen := map[string]bool{}
	for _, repo := range repos {
		cloneURL := repoCloneURL(repo)
		if !admitRepoURL(sc, cloneURL).LegacyHost {
			continue
		}
		t, ok := parseCloneTarget(cloneURL, adoServerHosts(sc))
		if !ok || seen[t.host] {
			continue
		}
		seen[t.host] = true
		hosts = append(hosts, t.host)
	}
	return hosts
}

// auditLegacyHostAdmissions records, at EVERY door, that something was admitted
// on the one-release grace rather than by a provider row — the state an admin has
// until 0.8 to close, and the only admission outcome that is neither a refusal
// nor ordinary.
//
// Audit at every door, warn wherever there is a channel: run create's 201 and
// the three ONBOARDING doors that answer a body — POST/PUT /workspaces and POST
// /sources — all carry the sentence now. Silent onboarding would leave the
// admin who must close the state first hearing about the host only when 0.8
// stops admitting it: the wrong person, one release late. The two launchers
// have only this row. Called from admitRepoSources and admitLauncherRepo, so
// a door added later inherits the audit half by construction.
//
// Deliberately no run id: eight of the ten doors have none, and one action with
// one shape reads better in the trail than two that differ by their target.
func (s *Server) auditLegacyHostAdmissions(ctx context.Context, sc types.SiteConfig, repos []string) {
	for _, host := range legacyHostAdmittedHosts(sc, repos) {
		s.recordAudit(ctx, s.auditEvent(nil, types.ActorSystem, "wardynd", "workspace.provider.legacy_host",
			host, "success", mustJSON(map[string]any{"host": host})))
	}
}

// legacyHostAdmissionWarnings is the ADMIT_LEGACY_HOST sentence for every repo
// admitted that way — one per HOST, on EVERY response that has a warnings
// channel: run create's 201 and the three onboarding doors. The audit half is
// auditLegacyHostAdmissions', already emitted by the admission call at each of
// those doors, so this only says out loud what the trail already recorded.
func (s *Server) legacyHostAdmissionWarnings(ctx context.Context, repos ...string) []string {
	repos = presentRepos(repos)
	if len(repos) == 0 || s.cfg.Store == nil {
		return nil
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil || !providersConfigured(sc) {
		return nil
	}
	var warnings []string
	for _, host := range legacyHostAdmittedHosts(sc, repos) {
		warnings = append(warnings, fmt.Sprintf(admitLegacyHost, host))
	}
	return warnings
}

// sshHostLevelWarnings is the ADMIT_SSH_HOST_LEVEL sentence — plus its audit row,
// the run.provider.lane_drop shape — for every SSH repository on this run that
// a PATH-SCOPED row admitted host-level. Never silent: this is the one admission
// outcome that is WIDER than the policy reads, and run create is the only door
// with a warnings channel, so the audit half rides along here rather than at
// every door (an SSH source onboarded elsewhere is re-asked at launch).
//
// One sentence per repository, not per host: which repository slipped the org
// bound is the fact an admin acts on.
func (s *Server) sshHostLevelWarnings(ctx context.Context, runID uuid.UUID, repos ...string) []string {
	repos = presentRepos(repos)
	if len(repos) == 0 || s.cfg.Store == nil {
		return nil
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil || !providersConfigured(sc) {
		return nil
	}
	var warnings []string
	for _, repo := range repos {
		t, ok := parseCloneTarget(repoCloneURL(repo), adoServerHosts(sc))
		if !ok || !t.ssh {
			continue
		}
		for _, row := range admittingRows(sc, repoCloneURL(repo)) {
			if !sshAdmittedAbovePath(row, t) {
				continue
			}
			s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.provider.ssh_host_level",
				string(row.Kind), "failure", mustJSON(map[string]any{
					"lane": string(types.GitLaneSSH), "kind": string(row.Kind), "host": t.host,
				})))
			warnings = append(warnings, fmt.Sprintf(admitSSHHostLevel, repo, row.Kind))
		}
	}
	return warnings
}

// requestRepoProviderRefusals is both provider gates over the two FREE-TEXT
// repository fields on POST /runs — `repo` and `devcontainer_repo` — which reach
// neither denyMemberRequest (neither is a capability kind of its own) nor
// validateWorkspaceSources (neither is a spec entry), and are nonetheless cloned:
// `repo` by the sandbox (broker-minted for, unioned into egress) and
// `devcontainer_repo` by the image builder, SERVER-SIDE.
//
// ADMISSION over both, the member CAPABILITY over `repo` alone — because
// `devcontainer_repo` is operator-only in effect (denyMemberRequest refuses it
// from a member before this runs), so there is no member left to check.
//
// One function for the pair because decodeAndValidateCreateRun sits at the
// gocyclo ratchet, and because they are one question asked of one request.
//
// gate is #386's launch door: LAUNCH (decodeAndValidateCreateRun) passes
// true, so a repo just admitted onto a per-user Azure DevOps row with no
// usable captured sign-in for this caller 422s here (gitCredentialRefusal,
// scmaccess.go). Review (preflight.go) passes false — review finding F2:
// preflight reads the SAME fact through gitCredentialFactForRepos instead,
// informationally, and never refuses on it.
func (s *Server) requestRepoProviderRefusals(w http.ResponseWriter, r *http.Request, req createRunRequest, gate bool) bool {
	if s.admitRepoSources(w, r, req.Repo, req.DevcontainerRepo) {
		return true
	}
	if req.Repo != "" && s.denyMemberWorkspaceProviders(w, r, "runs.workspace_provider", req.Repo) {
		return true
	}
	return gate && s.gitCredentialRefusal(w, r, req.Repo, req.DevcontainerRepo)
}

// recordLaunchRefusals are the ORG-POLICY refusals a record session must clear
// before it claims anything: the agent roster (recordRosterRefusal), provider
// admission, and (review follow-up N4) the per-user Azure DevOps gate, all
// over the workspace's repo sources. They travel together because they are
// one question — may this session start on this deployment? — and because
// they share every property that decides WHERE the check goes: a bare error, not
// routed through abort(); sited before the CAS claim so a refusal costs no state;
// and mapped by handleRecordWorkspace to the status its own door answers.
//
// ONE call site rather than three because launchRecordRun sits at the funlen
// ratchet, which is what its own comment there asks the next lane to do.
func (s *Server) recordLaunchRefusals(ctx context.Context, ws types.Workspace, agent string) error {
	if rerr := s.recordRosterRefusal(ctx, agent); rerr != nil {
		return rerr
	}
	if rerr := s.admitLauncherRepo(ctx, repoSourceLocators(ws.Sources)...); rerr != nil {
		return rerr
	}
	return s.gitCredentialRefusalForLauncher(ctx, oidcHumanFromContext(ctx), repoSourceLocators(ws.Sources)...)
}

// presentRepos drops the empty locators a call site would otherwise have to
// filter itself: req.repo and req.devcontainer_repo are optional, an ephemeral
// or local_dir source carries no repository, and "no repository" is not a
// refusal — it is nothing to ask about. Filtering HERE also keeps a request with
// no repository at all from paying a site-config read.
func presentRepos(repos []string) []string {
	out := make([]string, 0, len(repos))
	for _, repo := range repos {
		if strings.TrimSpace(repo) != "" {
			out = append(out, repo)
		}
	}
	return out
}

// repoLocatorsOf is the raw repo locator of every entry on a resolved spec — the
// shape both provider gates take, spelled once so the run path's two callers
// cannot disagree about which field carries the repository.
func repoLocatorsOf(repos []types.WorkspaceRepo) []string {
	out := make([]string, 0, len(repos))
	for _, wr := range repos {
		out = append(out, wr.Repo)
	}
	return out
}

// laneAllowed reports whether a provider row permits one credential lane. An
// EMPTY Lanes list means every LEGACY lane — the field NARROWS, it never
// widens, so an absent value is today's behaviour.
//
// "Legacy" and not "every lane in the closed set" is the load-bearing word.
// This expansion is the one site where an unwritten field becomes permission,
// so a lane added to the closed set would otherwise be granted retroactively
// to every stored row on every install, with no admin having written it. A new
// lane must be NAMED on the row (GitLane.Legacy).
func laneAllowed(row types.GitProvider, lane types.GitLane) bool {
	if len(row.Lanes) == 0 {
		return lane.Legacy()
	}
	return slices.Contains(row.Lanes, lane)
}

// admittingRows is the row that ADMITTED this repository — as a one-element
// list, or none. The row that admitted is the row that decides its lanes, and
// the distinction is not academic: two rows of the same kind are a supported
// configuration (a GHES row and a github.com row are both `kind: github`, and
// two rows on one host with different lanes is exactly how an admin runs a
// strict org beside an open one). Asking "which row claims this host" instead
// answers from whichever row is listed first, which under-vetoes in one
// direction (an open row permits a lane the admitting strict row forbids) and
// over-vetoes in the other (a GHES row decides github.com's lanes).
func admittingRows(sc types.SiteConfig, cloneURL string) []types.GitProvider {
	if v := admitRepoURL(sc, cloneURL); v.Admitted && v.Provider.ID != "" {
		return []types.GitProvider{v.Provider}
	}
	return nil
}

// claimingRows is every ENABLED row that claims a HOST — the answer where only a
// host is known and no repository is in scope (a git_pat or ssh_key grant's
// scope carries the host git will dial and never a path).
//
// The caller PERMITS a lane that ANY of these rows permits, and never asks only
// the first: with no repository to match, there is no way to tell which of two
// same-host rows the clone will land on, and refusing a lane one of them grants
// would break a clone the policy allows. The narrower answer is the admitting
// row's, which is why every site that can name the repository does.
//
// Disabled rows are deliberately absent: a disabled row refuses ADMISSION
// outright, so no run reaches a lane question through one.
func claimingRows(sc types.SiteConfig, host string) []types.GitProvider {
	t := cloneTarget{host: strings.ToLower(strings.TrimSpace(host))}
	if t.host == "" {
		return nil
	}
	var out []types.GitProvider
	for _, row := range gitProviderRows(sc) {
		if !row.Disabled && rowClaimsHost(row, t) {
			out = append(out, row)
		}
	}
	return out
}

// laneVetoed is the lane half of provider policy: providers MINT NOTHING, they
// veto. When NO row among the deciders permits the lane, the wiring is dropped,
// the caller is handed the warning to put on its response, and the drop is
// audited — the applySSHLaneWarnings shape exactly, for the same reason: a
// credential that silently never arrives fails mid-clone, inside the sandbox,
// where nobody is reading.
//
// rows come from admittingRows (one row: the verdict that admitted the
// repository — always prefer this) or claimingRows (the host-only fallback,
// where permitting the union is the only safe answer).
//
// The persisted grant ROW stays where one exists. It is an eligibility record
// nothing will mint, not an issued credential — deleting it would rewrite the
// policy's provenance rather than the run's wiring.
//
// Vetoes nothing when no row decided (legacy open mode, or a host admitted
// through the legacy list): there is no policy to read a lane list off.
//
// The AUDIT names the KIND, never the row id. A member can read their own run's
// audit trail (auditScope, GET /audit?run_id=), and a row id is on the far side
// of the disclosure rule — the operator-facing 201 warning may name it, the
// durable row a member can read may not.
func (s *Server) laneVetoed(ctx context.Context, runID uuid.UUID,
	lane types.GitLane, grant types.GrantKind, rows []types.GitProvider) (string, bool) {
	if len(rows) == 0 || slices.ContainsFunc(rows, func(row types.GitProvider) bool {
		return laneAllowed(row, lane)
	}) {
		return "", false
	}
	decided := rows[0] // every row in the list vetoed; they agree on the kind
	s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.provider.lane_drop",
		string(decided.Kind), "failure", mustJSON(map[string]any{
			"lane": string(lane), "grant": string(grant), "kind": string(decided.Kind),
		})))
	if s.isOperator(ctx) {
		return fmt.Sprintf(admitLaneDroppedOperator, lane, decided.Kind, decided.ID, grant), true
	}
	return fmt.Sprintf(admitLaneDropped, lane, decided.Kind, grant), true
}

// laneVetoedForRepo is laneVetoed decided by the row that ADMITTED the
// repository — the answer every site with a clone URL in scope must use.
func (s *Server) laneVetoedForRepo(ctx context.Context, sc types.SiteConfig, runID uuid.UUID,
	lane types.GitLane, grant types.GrantKind, cloneURL string) (string, bool) {
	if !providersConfigured(sc) {
		return "", false
	}
	return s.laneVetoed(ctx, runID, lane, grant, admittingRows(sc, cloneURL))
}

// laneVetoedForGrantHost is laneVetoed for a grant whose scope names only a HOST
// (git_pat, ssh_key). It still prefers the admitting row wherever it can: the
// deciders are the rows that admitted THIS RUN's own repositories on that host,
// and only a run that declares none there falls back to the union of the rows
// claiming it.
func (s *Server) laneVetoedForGrantHost(ctx context.Context, sc types.SiteConfig, runID uuid.UUID,
	lane types.GitLane, grant types.GrantKind, host string, repos []string) (string, bool) {
	if !providersConfigured(sc) {
		return "", false
	}
	return s.laneVetoed(ctx, runID, lane, grant, laneRowsForGrantHost(sc, host, repos))
}

// laneRowsForGrantHost picks the rows that decide a host-scoped grant's lane:
// the ADMITTING row of every one of this run's repositories on that host, else —
// when the run names no repository there — every enabled row claiming it.
//
// The fallback permits the union rather than asking the first row, because with
// no repository in scope there is no way to tell which of two same-host rows a
// clone would land on, and vetoing a lane one of them grants would break a clone
// the policy allows.
func laneRowsForGrantHost(sc types.SiteConfig, host string, repos []string) []types.GitProvider {
	t := cloneTarget{host: strings.ToLower(strings.TrimSpace(host))}
	if t.host == "" {
		return nil
	}
	var out []types.GitProvider
	seen := map[string]bool{}
	for _, repo := range repos {
		cloneURL := repoCloneURL(repo)
		rt, ok := parseCloneTarget(cloneURL, adoServerHosts(sc))
		if !ok || !hostsMatch(rt, t.host) {
			continue
		}
		for _, row := range admittingRows(sc, cloneURL) {
			if !seen[row.ID] {
				seen[row.ID] = true
				out = append(out, row)
			}
		}
	}
	if len(out) > 0 {
		return out
	}
	return claimingRows(sc, host)
}

// siteConfigForLaneVeto reads the provider policy persistRunGrants needs, ONCE,
// and only when this run actually declares a git credential lane — a run with no
// git grant pays nothing. An error is the caller's to answer: a policy the
// daemon could not read must not be treated as "no policy".
func (s *Server) siteConfigForLaneVeto(ctx context.Context, spec types.RunPolicySpec) (types.SiteConfig, error) {
	if s.cfg.Store == nil {
		return types.SiteConfig{}, nil
	}
	gitLane := false
	for _, g := range spec.EligibleGrants {
		switch g.Kind {
		case types.GrantGitHubToken, types.GrantGitPAT, types.GrantSSHKey:
			gitLane = true
		}
	}
	if !gitLane {
		return types.SiteConfig{}, nil
	}
	return s.cfg.Store.GetSiteConfig(ctx)
}

// laneVetoedForLauncher is laneVetoedForRepo for the two launcher-side grant
// sites (maybeGitHubReadGrant, maybeSSHKeyGrant), which hold no site config and
// no warnings channel: the drop is audited and the wiring is skipped. A read
// failure FAILS CLOSED — the same call the launchers make about their ceiling.
//
// By repository, not by host, and here it matters most: these two sites are
// handed the workspace source's own clone URL, and a wrong answer is invisible —
// a record or scan clone that should have carried a credential just fails inside
// the sandbox with no warning anywhere.
//
// ponytail: no warning is surfaced because a record/scan launch has no warnings
// field on its response; the audit row is the record. Give it one when a launcher
// grows a warnings channel, not before.
func (s *Server) laneVetoedForLauncher(ctx context.Context, runID uuid.UUID,
	lane types.GitLane, grant types.GrantKind, cloneURL string) (bool, error) {
	if s.cfg.Store == nil {
		return false, nil
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return false, fmt.Errorf("get site config: %w", err)
	}
	_, vetoed := s.laneVetoedForRepo(ctx, sc, runID, lane, grant, cloneURL)
	return vetoed, nil
}

// admissionStamper is the per-request projection that fills WorkspaceSource's
// server-owned `admitted` flag, or nil in legacy open mode — where the key is
// ABSENT from every response, which is what keeps an upgraded install's
// workspace documents byte-identical.
//
// Fed by the server, never mirrored by the console: the match rule has exactly
// one spelling (admitRepoURL) and the UI already mirrors only the host union.
// One site-config read per request, shared by every row on the page.
func (s *Server) admissionStamper(ctx context.Context) func(types.Workspace) types.Workspace {
	if s.cfg.Store == nil {
		return nil
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil || !providersConfigured(sc) {
		return nil
	}
	return func(ws types.Workspace) types.Workspace { return stampSourceAdmission(sc, ws) }
}

// stampSourceAdmission sets `admitted` on every REPO source of one workspace.
// local_dir and ephemeral sources are left alone: they clone nothing, and a
// flag on them would invite the console to render a provider state for a
// directory.
func stampSourceAdmission(sc types.SiteConfig, ws types.Workspace) types.Workspace {
	// Clone before mutating, redactWorkspaceForRead's rule and for its reason:
	// the store's row shares its slices with everything else that read it in
	// this request.
	ws.Sources = slices.Clone(ws.Sources)
	for i := range ws.Sources {
		if ws.Sources[i].Type != types.WorkspaceSourceTypeRepo {
			continue
		}
		_, admitted := providerFor(sc, repoCloneURL(ws.Sources[i].Source))
		ws.Sources[i].Admitted = &admitted
	}
	return ws
}

// the onboarding doors' warning channels
//
// An admission WARNING needs a response that can carry one, and the three
// onboarding doors answered a bare row. Both envelopes embed that row, so the
// fields stay at the TOP LEVEL and every existing Workspace/Source decoder is
// unaffected — createRunResponse's shape, for the reason it has it. They live
// HERE, beside legacyHostAdmissionWarnings, rather than in the two handler files:
// the channel exists for this file's warning, and the next door added sees both
// halves at once. Today the legacy-host grace is the only sentence either
// carries; a refusal is never a warning, and nothing else about onboarding is
// advisory.

type workspaceResponse struct {
	types.Workspace
	Warnings []string `json:"warnings,omitempty"`
}

type sourceResponse struct {
	types.Source
	Warnings []string `json:"warnings,omitempty"`
}
