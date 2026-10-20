// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// gh_own_pat.go owns the STORED half of a person's own GitHub token: a
// fine-grained personal access token they created in GitHub and added to
// Wardyn, on a GitHub row that takes one (kind github, lanes ["pat"],
// credential_source per_user, github.com only). The run-time half is
// gh_own_pat_run.go. It is the GitHub twin of the Azure DevOps Server own
// token (ado_own_pat.go), and is off until an administrator writes such a row.
//
// WHAT WARDYN CHECKS, AND WHAT IT DOES NOT. Before storing a token Wardyn asks
// GitHub whose it is (GET /user) and binds it to that account's numeric id; a
// login can be renamed or reused, the id cannot. GitHub does not name an
// account Wardyn could match to the person's sign-in, so the FIRST token a
// person adds for a row decides which account is theirs, and a later one must
// be the same account unless the person says they are linking another
// (relink). Wardyn does not read the token's permissions or its expiry: it
// accepts fine-grained tokens only (they name the repositories they reach),
// and the person enters the expiry they chose, held to 30 days. Wardyn cannot
// revoke the token either: Remove deletes Wardyn's copy only.
//
// ONE PERSON'S, NEVER SHARED. The token and its binding are one sealed value in
// the person's own namespace (types.GitHubOwnTokenSecretName), read with
// ownSecret, which never falls back to the operator's row. Both doors refuse
// in the user view, since the value would land on the previewing admin.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/hostrules"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// gitHubOwnPATMaxDays is the furthest expiry a person may enter: the limit an
// Azure DevOps Server row has, and GitHub's own default for a new token.
const gitHubOwnPATMaxDays = 30

// gitHubOwnPATPrefix is how every fine-grained personal access token begins.
// A classic token (ghp_) reaches every repository its account can, so it is
// refused: the minimum scope is a token that names its repositories.
const gitHubOwnPATPrefix = "github_pat_"

// gitHubOwnPATAPIBase is where the account check calls GitHub: the API host
// the broker already reaches for the GitHub App. A variable only so the tests
// can point it at a fake.
var gitHubOwnPATAPIBase = "https://api.github.com"

// gitHubOwnPATTransport carries the account check, as adoOwnPATTransport does
// the Azure DevOps one. A variable only so the tests can replace it.
var gitHubOwnPATTransport = http.DefaultTransport

// gitHubOwnPATPermissions is what to grant on GitHub's own token page, in its
// own wording: the least a run's git needs. Contents read-only is enough for a
// run that never pushes.
var gitHubOwnPATPermissions = []string{"Contents (Read and write)", "Metadata (Read-only)"}

// The own-token audit actions. None carries the token, the account id or the login.
const (
	gitHubPATAuditOwnStore  = "github_pat.own.store"
	gitHubPATAuditOwnDelete = "github_pat.own.delete"
	gitHubPATAuditOwnUse    = "github_pat.own.resolve"
)

// The doors' sentences.
const (
	gitHubOwnPATNoSessionRefusal    = "Adding a GitHub token needs a signed-in browser session: the token is stored under your own sign-in."
	gitHubOwnPATPreviewRefusal      = "Exit the user view to add or remove a GitHub token: it is stored under your own identity."
	gitHubOwnPATUnknownRowRefusal   = "No GitHub provider row you may use takes your own token at this address"
	gitHubOwnPATTooLongRefusal      = "This token expires after the %d-day limit for a GitHub token in Wardyn."
	gitHubOwnPATExpiryPastRefusal   = "Enter the date this token expires; it must be after today."
	gitHubOwnPATTokenInvalidRefusal = "Paste a fine-grained personal access token (it starts github_pat_): one value, with no spaces."
	gitHubOwnPATRejectedRefusal     = "GitHub didn't accept this token."
	gitHubOwnPATUnavailableRefusal  = "Wardyn couldn't check this token with GitHub. Try again in a moment."
	gitHubOwnPATMismatchRefusal     = "This token belongs to a different GitHub account than the one linked here. To use that account instead, link a different account."
)

// How a stored token relates to the account the row was bound to: the store row's `link`.
const (
	gitHubLinkFirst       = "first"        // nothing was bound
	gitHubLinkSameAccount = "same_account" // the same numeric id (a renamed login is the same account)
	gitHubLinkRelink      = "relink"       // another id, and the person said relink
)

// isGitHubOwnTokenRow reports whether row is one where each person adds their
// own GitHub token.
func isGitHubOwnTokenRow(row types.GitProvider) bool {
	return row.Kind == types.GitProviderGitHub && row.CredentialSource == types.CredentialSourcePerUser &&
		slices.Equal(row.Lanes, []types.GitLane{types.GitLanePAT})
}

// The write boundary's refusals for a GitHub per_user row.
const (
	providers400GHOwnLanes = "git[%d].lanes: a GitHub row with credential_source per_user takes only the %q lane — each person adds their own token for it"
	providers400GHOwnHost  = "git[%d].base_urls: %q is not on github.com — a person's own GitHub token is taken on github.com only"
	providers400GHOwnID    = "git[%d].id: %q cannot name a stored token — use up to 64 letters, digits, hyphens and underscores, starting with a letter or digit, not ending in -oauth"
)

// validateGitHubOwnTokenRow holds a GitHub per_user row to the one shape that
// means something: the pat lane alone, on github.com, with an id a stored
// token can be named by. Any other GitHub row is not this file's.
func validateGitHubOwnTokenRow(i int, row types.GitProvider) error {
	if row.Kind != types.GitProviderGitHub || row.CredentialSource != types.CredentialSourcePerUser {
		return nil
	}
	if !slices.Equal(row.Lanes, []types.GitLane{types.GitLanePAT}) {
		return fmt.Errorf(providers400GHOwnLanes, i, string(types.GitLanePAT))
	}
	for _, raw := range row.BaseURLs {
		if !strings.EqualFold(hostrules.HostOf(raw), "github.com") {
			return fmt.Errorf(providers400GHOwnHost, i, raw)
		}
	}
	if !adoEntraValidRowID(row.ID) {
		return fmt.Errorf(providers400GHOwnID, i, row.ID)
	}
	return nil
}

// gitHubOwnTokenSecretName is the sealed name for one row's token. Callers
// must have validated rowID (adoEntraValidRowID, every sealed name's rule).
func gitHubOwnTokenSecretName(rowID string) string {
	if !adoEntraValidRowID(rowID) {
		panic("api: gitHubOwnTokenSecretName called with an unvalidated provider row id")
	}
	return types.GitHubOwnTokenSecretName(rowID)
}

// readGitHubOwnToken loads owner's token for one row from the owner's own row
// only (ownSecret: absent in the no-credential preview). A value that does not
// parse, or parses incomplete, reads as none stored: only this file writes
// the name, and every reader of an absent token fails closed. Every caller
// reads it for its state, binding or expiry, never to hand the token out (the
// broker reads it itself at mint), so the read is a status read.
func (s *Server) readGitHubOwnToken(ctx context.Context, owner, rowID string) (types.GitHubOwnToken, bool, error) {
	if !adoEntraValidRowID(rowID) {
		return types.GitHubOwnToken{}, false, nil
	}
	raw, found, err := s.ownSecret(secretstore.WithPurpose(ctx, secretstore.PurposeStatus), owner, gitHubOwnTokenSecretName(rowID))
	if err != nil || !found {
		return types.GitHubOwnToken{}, false, err
	}
	var tok types.GitHubOwnToken
	if json.Unmarshal(raw, &tok) != nil || !tok.Valid() {
		return types.GitHubOwnToken{}, false, nil
	}
	return tok, true, nil
}

// gitHubOwnTokenRows is every enabled own-token row the caller may use (D-6: a
// row they may not use is no row at all).
func (s *Server) gitHubOwnTokenRows(ctx context.Context, sc types.SiteConfig) []types.GitProvider {
	var rows []types.GitProvider
	for _, row := range gitProviderRows(sc) {
		if !row.Disabled && isGitHubOwnTokenRow(row) && adoEntraValidRowID(row.ID) {
			rows = append(rows, row)
		}
	}
	return capVisible(ctx, s, capWorkspaceProvider, rows, func(r types.GitProvider) string { return r.ID })
}

// GitHubOwnTokenAccess is the caller's own token on one row: GET
// /me/scm/github/tokens lists one per row, and PUT answers the row it stored.
// No row id: Org (the row's first base URL) is the key, as on /me/scm-access.
type GitHubOwnTokenAccess struct {
	Org string `json:"org"`
	// State is live, expiring (within a week of ExpiresOn), expired_signin
	// (Cause token_expired) or not_configured.
	State string `json:"state"`
	Cause string `json:"cause,omitempty"`
	// Login is the GitHub account the token was bound to, as it was named when
	// stored. Display only: the binding is the numeric id, never shown.
	Login string `json:"login,omitempty"`
	// ExpiresOn (YYYY-MM-DD) is the date the PERSON said the token expires.
	// Wardyn has not checked it with GitHub.
	ExpiresOn string `json:"expires_on,omitempty"`
	// MaxDays is the furthest expiry, in days from today, that may be entered.
	MaxDays int `json:"max_days"`
	// TokenPermissions is what to grant on GitHub's token page.
	TokenPermissions []string `json:"token_permissions"`
}

// gitHubOwnTokenAccessFor grades owner's token on row.
func (s *Server) gitHubOwnTokenAccessFor(ctx context.Context, row types.GitProvider, owner string) (GitHubOwnTokenAccess, error) {
	out := GitHubOwnTokenAccess{Org: adoOrgDisplay(row), State: modelAccessNotConfigured, MaxDays: gitHubOwnPATMaxDays,
		TokenPermissions: slices.Clone(gitHubOwnPATPermissions)}
	tok, found, err := s.readGitHubOwnToken(ctx, owner, row.ID)
	if err != nil || !found {
		return out, err
	}
	out.Login, out.ExpiresOn = tok.Login, tok.ExpiresOn.Format(time.DateOnly)
	now := s.cfg.Now()
	switch {
	case tok.Expired(now):
		out.State, out.Cause = modelAccessExpiredSignin, scmAccessCauseTokenExpired
	case now.Add(adoOwnPATExpiringWindow).After(tok.ExpiresOn):
		out.State = modelAccessExpiring
	default:
		out.State = modelAccessLive
	}
	return out, nil
}

// gitHubOwnPATRequest is PUT /me/scm/github/token's body. Org is the row's
// address as GET /me/scm/github/tokens names it; ExpiresOn is the date the
// person chose in GitHub (YYYY-MM-DD); Relink says they mean to replace the
// account this row is bound to.
type gitHubOwnPATRequest struct {
	Org       string `json:"org"`
	Token     string `json:"token"`
	ExpiresOn string `json:"expires_on"`
	Relink    bool   `json:"relink,omitempty"`
}

// mountGitHubOwnPATRoutes mounts the own-token doors on the authenticated human
// group, beside the Azure DevOps one.
func (s *Server) mountGitHubOwnPATRoutes(r chi.Router) {
	r.Get("/me/scm/github/tokens", s.handleListGitHubOwnTokens)
	r.Put("/me/scm/github/token", s.handlePutGitHubOwnPAT)
	r.Delete("/me/scm/github/token", s.handleDeleteGitHubOwnPAT)
}

// gitHubOwnPATCaller is both writing doors' first answer: the caller's session
// subject, or a refusal when there is none or the request is in the user view.
func gitHubOwnPATCaller(w http.ResponseWriter, r *http.Request) (string, bool) {
	subject := oidcHumanFromContext(r.Context())
	if subject == "" {
		writeErrorReason(w, http.StatusForbidden, reasonGitHubOwnPATNoSession, gitHubOwnPATNoSessionRefusal)
		return "", false
	}
	if oidc.MemberModeFromContext(r.Context()) {
		writeErrorReason(w, http.StatusConflict, reasonGitHubOwnPATPreviewBlocked, gitHubOwnPATPreviewRefusal)
		return "", false
	}
	return subject, true
}

// handleListGitHubOwnTokens answers the caller's own token state on every
// own-token row they may use; empty with no such row, or no session.
func (s *Server) handleListGitHubOwnTokens(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := []GitHubOwnTokenAccess{}
	subject := oidcHumanFromContext(ctx)
	if s.cfg.Store == nil || subject == "" {
		writeJSON(w, http.StatusOK, out)
		return
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		writeServerError(w, r, "read site config", err)
		return
	}
	for _, row := range s.gitHubOwnTokenRows(ctx, sc) {
		access, err := s.gitHubOwnTokenAccessFor(ctx, row, subject)
		if err != nil {
			writeServerError(w, r, "read the GitHub token", err)
			return
		}
		out = append(out, access)
	}
	writeJSON(w, http.StatusOK, out)
}

// handlePutGitHubOwnPAT stores the caller's own token for one row, after these
// checks in order: the expiry is after today and within 30 days; the value is
// one fine-grained token; GitHub accepts it and names an account; and that
// account is the one the row is bound to for this person, unless relink. The
// answer is the row's fresh entry.
func (s *Server) handlePutGitHubOwnPAT(w http.ResponseWriter, r *http.Request) {
	subject, ok := gitHubOwnPATCaller(w, r)
	if !ok {
		return
	}
	// Fenced BEFORE the binding read: an erase between the read and the write
	// must refuse the write (TestCredentialWriteBeginsBeforeItsRead_*).
	r, ok = s.beginRequestWrite(w, r, subject)
	if !ok {
		return
	}
	ctx := r.Context()
	var req gitHubOwnPATRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	row, ok := s.gitHubOwnPATDoorRow(w, r, req.Org)
	if !ok {
		return
	}
	now := s.cfg.Now().UTC()
	expiresOn, ok := gitHubOwnPATExpiry(w, req.ExpiresOn, now)
	if !ok {
		return
	}
	token := strings.TrimSpace(req.Token)
	if !strings.HasPrefix(token, gitHubOwnPATPrefix) || len(token) > adoOwnPATMaxTokenLen || strings.ContainsFunc(token, isSpaceOrControl) {
		writeErrorReason(w, http.StatusBadRequest, reasonGitHubOwnPATTokenInvalid, gitHubOwnPATTokenInvalidRefusal)
		return
	}
	name := gitHubOwnTokenSecretName(row.ID)
	audit := map[string]any{"provider_row": row.ID, "expires_on": expiresOn.Format(time.DateOnly)}
	account, err := gitHubOwnPATAccount(ctx, token)
	switch {
	case errors.Is(err, errGitHubOwnPATRejected):
		audit["reason"] = reasonGitHubOwnPATRejected
		s.auditGitHubOwnPAT(ctx, subject, gitHubPATAuditOwnStore, name, "failure", audit)
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonGitHubOwnPATRejected, gitHubOwnPATRejectedRefusal)
		return
	case err != nil:
		writeErrorReason(w, http.StatusServiceUnavailable, reasonGitHubOwnPATCheckUnavailable, gitHubOwnPATUnavailableRefusal)
		return
	}
	// One lock over the binding read and the write, so two pastes cannot both
	// read "nothing bound" and the second silently replace the first's account.
	adoOwnPATWriteMu.Lock()
	defer adoOwnPATWriteMu.Unlock()
	bound, found, err := s.readGitHubOwnToken(ctx, subject, row.ID)
	if err != nil {
		writeServerError(w, r, "read the GitHub token", err)
		return
	}
	link := gitHubLinkFirst
	switch {
	case found && bound.AccountID == account.id:
		link = gitHubLinkSameAccount
	case found && !req.Relink:
		audit["reason"] = reasonGitHubOwnPATAccountMismatch
		s.auditGitHubOwnPAT(ctx, subject, gitHubPATAuditOwnStore, name, "failure", audit)
		writeErrorReason(w, http.StatusConflict, reasonGitHubOwnPATAccountMismatch, gitHubOwnPATMismatchRefusal)
		return
	case found:
		link = gitHubLinkRelink
	}
	raw, _ := json.Marshal(types.GitHubOwnToken{Token: token, AccountID: account.id, Login: account.login, ExpiresOn: expiresOn, StoredAt: now})
	if err := s.putOwned(ctx, subject, func(ctx context.Context) error {
		return s.cfg.Secrets.For(subject).Put(ctx, name, raw)
	}); err != nil {
		s.auditRowNotWritten(ctx, err, types.ActorHuman, subject, subject, name)
		writeServerError(w, r, "store the GitHub token", err)
		return
	}
	audit["link"] = link
	s.auditGitHubOwnPAT(ctx, subject, gitHubPATAuditOwnStore, name, "success", audit)
	access, err := s.gitHubOwnTokenAccessFor(ctx, row, subject)
	if err != nil {
		writeServerError(w, r, "read the GitHub token", err)
		return
	}
	writeJSON(w, http.StatusOK, access)
}

// handleDeleteGitHubOwnPAT deletes the caller's own token for the address at
// ?org=. Wardyn cannot revoke it at GitHub; the person does that there.
//
// A cleanup, not a use (#1479): it looks at EVERY GitHub row on that address,
// whatever its state now (disabled, denied to the caller, switched away from
// per_user), and deletes the caller's sealed name for each row whose name is
// in the caller's OWN list, so a person whose access was withdrawn can still
// remove what they hold. 204 when something was deleted, or when a row the
// caller may use holds nothing of theirs; otherwise the 404 an address with no
// row gives, with no audit row.
func (s *Server) handleDeleteGitHubOwnPAT(w http.ResponseWriter, r *http.Request) {
	subject, ok := gitHubOwnPATCaller(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	org := r.URL.Query().Get("org")
	if s.cfg.Store == nil || s.cfg.Secrets == nil {
		writeErrorReason(w, http.StatusNotFound, reasonGitHubOwnPATUnknownRow, gitHubOwnPATUnknownRowRefusal)
		return
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		writeServerError(w, r, "read site config", err)
		return
	}
	usable := s.gitHubOwnTokenRows(ctx, sc)
	adoOwnPATWriteMu.Lock()
	defer adoOwnPATWriteMu.Unlock()
	view := s.cfg.Secrets.For(subject)
	held, err := view.List(secretstore.WithPurpose(ctx, secretstore.PurposeStatus))
	if err != nil {
		writeServerError(w, r, "list the GitHub token", err)
		return
	}
	deleted, found := false, false
	for _, row := range gitProviderRows(sc) {
		if row.Kind != types.GitProviderGitHub || !adoEntraValidRowID(row.ID) || adoOrgDisplay(row) != org {
			continue
		}
		isUsable := slices.ContainsFunc(usable, func(u types.GitProvider) bool { return u.ID == row.ID })
		found = found || isUsable
		name := gitHubOwnTokenSecretName(row.ID)
		if !slices.Contains(held, name) {
			continue
		}
		data := map[string]any{"provider_row": row.ID, "row_state": gitHubOwnTokenRowState(row, isUsable)}
		if err := view.Delete(ctx, name); err != nil && !errors.Is(err, secretstore.ErrNotFound) {
			data["reason"] = "store_error"
			s.auditGitHubOwnPAT(ctx, subject, gitHubPATAuditOwnDelete, name, "failure", data)
			writeServerError(w, r, "delete the GitHub token", err)
			return
		}
		s.auditGitHubOwnPAT(ctx, subject, gitHubPATAuditOwnDelete, name, "success", data)
		deleted = true
	}
	if !deleted && !found {
		writeErrorReason(w, http.StatusNotFound, reasonGitHubOwnPATUnknownRow, gitHubOwnPATUnknownRowRefusal)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// gitHubOwnTokenRowState is what the removal's audit row says about the row:
// "" when the caller may still use it, else why not.
func gitHubOwnTokenRowState(row types.GitProvider, usable bool) string {
	switch {
	case usable:
		return ""
	case row.Disabled:
		return "disabled"
	case !isGitHubOwnTokenRow(row):
		return "not_own_token"
	}
	return "capability_denied"
}

// gitHubOwnPATDoorRow resolves the row PUT acts on: unknown, disabled, not an
// own-token row, or not the caller's to use all answer the same 404.
func (s *Server) gitHubOwnPATDoorRow(w http.ResponseWriter, r *http.Request, org string) (types.GitProvider, bool) {
	if s.cfg.Store == nil || s.cfg.Secrets == nil {
		writeErrorReason(w, http.StatusNotFound, reasonGitHubOwnPATUnknownRow, gitHubOwnPATUnknownRowRefusal)
		return types.GitProvider{}, false
	}
	sc, err := s.cfg.Store.GetSiteConfig(r.Context())
	if err != nil {
		writeServerError(w, r, "read site config", err)
		return types.GitProvider{}, false
	}
	rows := s.gitHubOwnTokenRows(r.Context(), sc)
	i := slices.IndexFunc(rows, func(row types.GitProvider) bool { return adoOrgDisplay(row) == org })
	if i < 0 {
		writeErrorReason(w, http.StatusNotFound, reasonGitHubOwnPATUnknownRow, gitHubOwnPATUnknownRowRefusal)
		return types.GitProvider{}, false
	}
	return rows[i], true
}

// gitHubOwnPATExpiry parses the date the person entered and holds it to
// (today, today + 30 days]. Days are UTC calendar days.
func gitHubOwnPATExpiry(w http.ResponseWriter, raw string, now time.Time) (time.Time, bool) {
	day, err := time.Parse(time.DateOnly, strings.TrimSpace(raw))
	today := now.Truncate(24 * time.Hour)
	if err != nil || !day.After(today) {
		writeErrorReason(w, http.StatusBadRequest, reasonGitHubOwnPATExpiryInvalid, gitHubOwnPATExpiryPastRefusal)
		return time.Time{}, false
	}
	if day.After(today.AddDate(0, 0, gitHubOwnPATMaxDays)) {
		writeErrorReason(w, http.StatusBadRequest, reasonGitHubOwnPATExpiryTooLong, fmt.Sprintf(gitHubOwnPATTooLongRefusal, gitHubOwnPATMaxDays))
		return time.Time{}, false
	}
	return day, true
}

// auditGitHubOwnPAT writes one own-token audit row, targeting the sealed name.
func (s *Server) auditGitHubOwnPAT(ctx context.Context, actor, action, name, outcome string, data map[string]any) {
	s.recordAudit(ctx, s.auditEvent(nil, types.ActorHuman, actor, action, name, outcome, mustJSON(data)))
}

// ── the account check ──────────────────────────────────────────────────────

var (
	// errGitHubOwnPATRejected: GitHub answered 401, or a 200 that names no account.
	errGitHubOwnPATRejected = errors.New("github did not accept the token")
	// errGitHubOwnPATUnavailable: the check did not complete, or GitHub answered
	// something that says nothing about the token (a 403 is also its answer to
	// valid credentials after repeated failed attempts, and to a rate limit).
	errGitHubOwnPATUnavailable = errors.New("the github account check did not complete")
)

// gitHubOwnPATAccountInfo is who GitHub says a token belongs to.
type gitHubOwnPATAccountInfo struct {
	id    int64
	login string
}

// gitHubOwnPATAccount asks GitHub, with the token itself, whose it is: GET
// /user. No redirect is followed, since the request carries the token. The
// error never carries the token or the response body.
func gitHubOwnPATAccount(ctx context.Context, token string) (gitHubOwnPATAccountInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gitHubOwnPATAPIBase+"/user", nil)
	if err != nil {
		return gitHubOwnPATAccountInfo{}, fmt.Errorf("%w: build the request", errGitHubOwnPATUnavailable)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	client := &http.Client{
		Transport:     gitHubOwnPATTransport,
		Timeout:       adoEntraRedeemTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return gitHubOwnPATAccountInfo{}, fmt.Errorf("%w: %w", errGitHubOwnPATUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return gitHubOwnPATAccountInfo{}, fmt.Errorf("%w: HTTP %d", errGitHubOwnPATRejected, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return gitHubOwnPATAccountInfo{}, fmt.Errorf("%w: HTTP %d", errGitHubOwnPATUnavailable, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<18))
	if err != nil {
		return gitHubOwnPATAccountInfo{}, fmt.Errorf("%w: read the answer", errGitHubOwnPATUnavailable)
	}
	var body struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	}
	if json.Unmarshal(raw, &body) != nil || body.ID <= 0 {
		return gitHubOwnPATAccountInfo{}, fmt.Errorf("%w: no account named", errGitHubOwnPATRejected)
	}
	return gitHubOwnPATAccountInfo{id: body.ID, login: body.Login}, nil
}
