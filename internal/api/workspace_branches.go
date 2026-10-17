// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// GET /workspaces/{id}/branches?repo=<repo>: the New Run Files step's branch
// picker. It lists one repository's branches through the credential a run on
// that workspace would clone with. A listing that fails is a 200 with listing
// "unavailable", and the console falls back to a text field.
//
// This is a new server-side use of a credential, bounded four ways:
//   - who: only a caller the launch door would admit for the workspace
//     (mayLaunchWorkspace, then workspaceAvailableToCaller); anyone else gets
//     getWorkspaceOr404's byte-identical 404;
//   - which repository: one of the workspace's own repo sources, never a URL
//     taken from the request;
//   - where: the Azure DevOps API named by the admitting provider row (the
//     Services API for the organisation, or the Server row's own collection
//     URL), and GitHub through the organisation's App — never the repository
//     URL's own host;
//   - how often: a per-person limiter.
//
// Each listing writes one workspace.branches.list row (repository, credential
// kind, outcome, count; never a branch name). No credential and no forge error
// text reaches a log, an error body or the response.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RepoBranches is GET /workspaces/{id}/branches's answer. Branches is never
// null: the default branch first, then the rest by name, at most branchListMax.
type RepoBranches struct {
	Listing    string   `json:"listing"` // listed | unavailable
	Branches   []string `json:"branches"`
	Default    string   `json:"default,omitempty"`
	Truncated  bool     `json:"truncated"`
	Credential string   `json:"credential,omitempty"` // ado_signin | ado_own_token | github_app
	Reason     string   `json:"reason,omitempty"`     // forge_not_supported | no_credential | signin_needed | forge_refused | forge_unreachable
}

const (
	branchListingListed      = "listed"
	branchListingUnavailable = "unavailable"

	branchCredADOSignIn   = "ado_signin"
	branchCredADOOwnToken = "ado_own_token"
	branchCredGitHubApp   = "github_app"

	branchReasonNotSupported = "forge_not_supported"
	branchReasonNoCredential = "no_credential"
	branchReasonSignInNeeded = "signin_needed"
	branchReasonRefused      = "forge_refused"
	branchReasonUnreachable  = "forge_unreachable"

	branchListMax = 200

	// The per-person limiter: one Files step lists each repository once, and an
	// ADO sign-in redemption rotates the person's refresh token.
	branchListRatePerSec  = 0.5
	branchListBurst       = 10
	branchListMaxPeople   = 16384
	adoBranchAPIVersion   = "5.0" // the oldest Server release with $top on refs; Services still serves it
	adoBranchBodyMaxBytes = 1 << 20
)

func branchesUnavailable(credential, reason string) RepoBranches {
	return RepoBranches{Listing: branchListingUnavailable, Branches: []string{}, Credential: credential, Reason: reason}
}

func (s *Server) handleListWorkspaceBranches(w http.ResponseWriter, r *http.Request) {
	if who, limited := rateLimitedActor(r); limited && !s.branchLimiter.allow(who, s.cfg.Now()) {
		writeErrorReason(w, http.StatusTooManyRequests, reasonBranchListRateLimited, "too many branch lists; slow down")
		return
	}
	r = r.WithContext(withCapBatch(r.Context()))
	id, ok := parseIDParam(w, r, "id", "workspace")
	if !ok {
		return
	}
	ws, ok := s.getWorkspaceOr404(w, r, id)
	if !ok {
		return
	}
	if !s.mayLaunchWorkspace(r, ws) {
		s.denyForeignWorkspace(w, r, ws)
		return
	}
	sc, err := s.cfg.Store.GetSiteConfig(r.Context())
	if err != nil {
		writeServerError(w, r, "get site config", err)
		return
	}
	if !s.workspaceAvailableToCaller(r.Context(), sc, ws) {
		// The same bytes as a missing id; the audit row keeps the true reason.
		s.recordRefusal(r.Context(), r, authz.Deny(authz.ReasonCapabilityWorkspace, ws.ID.String(), "workspace not found"))
		writeErrorReason(w, http.StatusNotFound, reasonWorkspaceNotFound, "workspace not found")
		return
	}
	repo := r.URL.Query().Get("repo")
	if !slices.Contains(repoSourceLocators(ws.Sources), repo) {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonWorkspaceRepoUnknown,
			fmt.Sprintf("%s is not a repository of workspace %s.", repoRecordURL(repo), ws.Name))
		return
	}
	out, err := s.listRepoBranches(r.Context(), sc, repo, runIdentitySubject(r.Context(), principalFromRequest(r)))
	if err != nil {
		writeServerError(w, r, "list branches", err)
		return
	}
	s.auditBranchList(r, ws.ID.String(), repo, out)
	writeJSON(w, http.StatusOK, out)
}

// listRepoBranches picks the arm by the clone lane a run would take: the
// person's own Azure DevOps lane (adoEntraRunForRepo, dispatch's own
// decision), else the GitHub App lane on github.com. Nothing is dialled for a
// repository no lane serves. The error is a wardynd-side failure (500).
func (s *Server) listRepoBranches(ctx context.Context, sc types.SiteConfig, repo, owner string) (RepoBranches, error) {
	cloneURL := repoCloneURL(repo)
	v := admitRepoURL(sc, cloneURL)
	if !v.Admitted {
		return branchesUnavailable("", branchReasonNotSupported), nil
	}
	if a, ok := adoEntraRunForRepo(sc, repo, owner); ok {
		return s.adoRepoBranches(ctx, sc, a, repo)
	}
	if v.Provider.Kind == types.GitProviderAzureDevOps {
		return branchesUnavailable("", branchReasonNoCredential), nil
	}
	ghRepo, onGitHub := githubAppRepo(cloneURL)
	if !onGitHub {
		return branchesUnavailable("", branchReasonNotSupported), nil
	}
	if v.Provider.ID != "" && (v.Provider.Kind != types.GitProviderGitHub || !laneAllowed(v.Provider, types.GitLaneApp)) {
		return branchesUnavailable("", branchReasonNoCredential), nil
	}
	return s.githubRepoBranches(ctx, ghRepo), nil
}

// githubAppRepo is cloneURL's "owner/name" when it is an https github.com
// repository, the only host the App lane serves.
func githubAppRepo(cloneURL string) (string, bool) {
	t, ok := parseCloneTarget(cloneURL, nil)
	if !ok || t.ssh || t.scheme != "https" || t.host != "github.com" {
		return "", false
	}
	parts := strings.Split(strings.TrimSuffix(strings.Trim(t.path, "/"), ".git"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", false
	}
	return parts[0] + "/" + parts[1], true
}

func (s *Server) githubRepoBranches(ctx context.Context, repo string) RepoBranches {
	if s.cfg.GitHubBranches == nil {
		return branchesUnavailable(branchCredGitHubApp, branchReasonNoCredential)
	}
	names, def, truncated, err := s.cfg.GitHubBranches.ListBranches(ctx, repo, branchListMax)
	switch {
	case errors.Is(err, secretstore.ErrNotFound): // no App configured
		return branchesUnavailable(branchCredGitHubApp, branchReasonNoCredential)
	case errors.Is(err, broker.ErrGitHubRefused):
		return branchesUnavailable(branchCredGitHubApp, branchReasonRefused)
	case err != nil:
		return branchesUnavailable(branchCredGitHubApp, branchReasonUnreachable)
	}
	return branchesListed(branchCredGitHubApp, names, def, truncated)
}

// branchesListed orders names: def first, then the rest by name, at most
// branchListMax in all; truncated also when the forge said there were more.
func branchesListed(credential string, names []string, def string, truncated bool) RepoBranches {
	rest := slices.DeleteFunc(slices.Clone(names), func(n string) bool { return n == "" || n == def })
	slices.Sort(rest)
	rest = slices.Compact(rest)
	out := rest
	if def != "" {
		out = append([]string{def}, rest...)
	}
	if len(out) > branchListMax {
		out, truncated = out[:branchListMax], true
	}
	return RepoBranches{Listing: branchListingListed, Branches: out, Default: def, Truncated: truncated, Credential: credential}
}

// adoRepoBranches is the Azure DevOps arm: the person's own token (own_pat,
// Services or Server) or their own sign-in (bearer). A minted_pat row's
// sign-in can only create tokens, so it lists nothing.
func (s *Server) adoRepoBranches(ctx context.Context, sc types.SiteConfig, a adoEntraRun, repo string) (RepoBranches, error) {
	cred := branchCredADOSignIn
	if a.tokenMode == types.ADOTokenModeOwnPAT {
		cred = branchCredADOOwnToken
	}
	if a.tokenMode == types.ADOTokenModeMintedPAT {
		return branchesUnavailable("", branchReasonNoCredential), nil
	}
	base, project, name, ok := adoBranchTarget(sc, a, repo)
	if !ok {
		return branchesUnavailable(cred, branchReasonNotSupported), nil
	}
	auth, reason, err := s.adoBranchAuth(ctx, a)
	if err != nil || reason != "" {
		return branchesUnavailable(cred, reason), err
	}
	repoURL := base + "/" + url.PathEscape(project) + "/_apis/git/repositories/" + url.PathEscape(name)
	var meta struct {
		DefaultBranch string `json:"defaultBranch"`
	}
	if reason := adoBranchGet(ctx, repoURL+"?api-version="+adoBranchAPIVersion, auth, &meta); reason != "" {
		return branchesUnavailable(cred, reason), nil
	}
	var refs struct {
		Value []struct {
			Name string `json:"name"`
		} `json:"value"`
	}
	q := url.Values{"filter": {"heads/"}, "$top": {strconv.Itoa(branchListMax + 1)}, "api-version": {adoBranchAPIVersion}}
	if reason := adoBranchGet(ctx, repoURL+"/refs?"+q.Encode(), auth, &refs); reason != "" {
		return branchesUnavailable(cred, reason), nil
	}
	names := make([]string, 0, len(refs.Value))
	for _, ref := range refs.Value {
		names = append(names, strings.TrimPrefix(ref.Name, "refs/heads/"))
	}
	return branchesListed(cred, names, strings.TrimPrefix(meta.DefaultBranch, "refs/heads/"), len(names) > branchListMax), nil
}

// adoBranchTarget is where the Azure DevOps arm asks, and for which project
// and repository. The base comes from the lane, never from the repository
// URL's host: the Services API for the organisation, or the Server row's own
// collection URL (adoServerRunForRepo already held the repository to that
// host). The repository URL gives only the path after the organisation or
// collection: <project>/_git/<repo>, or _git/<repo> for a project of that name.
func adoBranchTarget(sc types.SiteConfig, a adoEntraRun, repo string) (base, project, name string, ok bool) {
	t, ok := parseCloneTarget(repoCloneURL(repo), adoServerHosts(sc))
	if !ok || t.ssh {
		return "", "", "", false
	}
	segs := strings.Split(strings.Trim(t.path, "/"), "/")
	switch {
	case a.serverHost != "":
		row, found := gitProviderRowByID(sc, a.rowID)
		if !found || len(row.BaseURLs) == 0 {
			return "", "", "", false
		}
		u, err := url.Parse(row.BaseURLs[0])
		if err != nil {
			return "", "", "", false
		}
		base = "https://" + a.serverHost + "/" + strings.Trim(u.EscapedPath(), "/")
		segs = segs[min(len(segs), len(strings.Split(a.org, "/"))):]
	case canonicalProviderHost(t.host) == "dev.azure.com":
		base = adoOwnPATAPIBase + "/" + url.PathEscape(a.org)
		segs = segs[1:]
	default: // <org>.visualstudio.com
		base = adoOwnPATAPIBase + "/" + url.PathEscape(a.org)
		if len(segs) > 0 && strings.EqualFold(segs[0], "DefaultCollection") {
			segs = segs[1:]
		}
	}
	switch {
	case len(segs) == 3 && segs[1] == "_git":
		project, name = segs[0], segs[2]
	case len(segs) == 2 && segs[0] == "_git":
		project, name = segs[1], segs[1]
	}
	// PathEscape leaves "." and "..", which would climb out of the repository
	// path on the forge's own host.
	for _, seg := range []string{project, name} {
		if seg == "" || seg == "." || seg == ".." {
			return "", "", "", false
		}
	}
	return base, project, name, true
}

// adoBranchAuth is the Authorization value for the person's own credential,
// or the reason there is none. err is a wardynd-side failure.
func (s *Server) adoBranchAuth(ctx context.Context, a adoEntraRun) (auth, reason string, err error) {
	rctx := secretstore.WithPurpose(ctx, secretstore.PurposeADORefresh)
	if a.tokenMode == types.ADOTokenModeOwnPAT {
		blob, found, err := s.readADOOwnPAT(rctx, a.owner, a.rowID)
		if err != nil {
			return "", "", err
		}
		if !found || blob.expired(s.cfg.Now()) {
			return "", branchReasonSignInNeeded, nil
		}
		return adoOwnPATHeaderValue(blob.Token), "", nil
	}
	cfg, ok, err := s.adoEntraRowConfig(rctx, a.rowID)
	if err != nil {
		return "", "", err
	}
	if !ok {
		return "", branchReasonNoCredential, nil
	}
	access, err := s.adoEntraAccessFor(rctx, cfg, a.owner, s.adoRequestScopes(rctx, cfg, a.owner), false)
	switch ADOEntraClassify(err) {
	case ADOEntraFailureNone:
		return "Bearer " + access.AccessToken, "", nil
	case ADOEntraFailureUnavailable:
		return "", branchReasonUnreachable, nil
	default:
		return "", branchReasonSignInNeeded, nil
	}
}

// adoBranchGet GETs target with auth and decodes a 200 into out, returning ""
// or the reason it could not. No redirect is followed: the request carries
// the credential, and Azure DevOps answers one it refuses with a redirect to
// its sign-in page.
func adoBranchGet(ctx context.Context, target, auth string, out any) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return branchReasonUnreachable
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/json")
	client := &http.Client{
		Transport:     adoOwnPATTransport,
		Timeout:       adoEntraRedeemTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return branchReasonUnreachable
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case adoPATTransientStatus(resp.StatusCode):
		return branchReasonUnreachable
	case resp.StatusCode != http.StatusOK:
		return branchReasonRefused
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, adoBranchBodyMaxBytes))
	if err != nil || json.Unmarshal(raw, out) != nil {
		return branchReasonUnreachable
	}
	return ""
}

// auditBranchList writes the per-call row: never a branch name, never the
// credential.
func (s *Server) auditBranchList(r *http.Request, workspaceID, repo string, out RepoBranches) {
	actorType, actor := actorFromRequest(r)
	outcome := "success"
	if out.Listing != branchListingListed {
		outcome = "failure"
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorType, actor, "workspace.branches.list", workspaceID, outcome, mustJSON(map[string]any{
		"repo": repoRecordURL(repo), "credential": out.Credential, "listing": out.Listing, "reason": out.Reason,
		"count": len(out.Branches), "truncated": out.Truncated,
	})))
}
