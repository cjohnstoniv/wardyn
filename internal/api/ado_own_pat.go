// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// ado_own_pat.go owns the STORED half of token_mode own_pat: a personal access
// token a person created in Azure DevOps and pasted into Wardyn, on a row where
// nothing signs in. The run-time half (the resolve that injects it) is
// runs_dispatch_ado_own_pat.go.
//
// WHAT WARDYN CAN CHECK, AND WHAT IT CANNOT. Before storing a token Wardyn asks
// Azure DevOps who it belongs to (connectionData) and refuses one that is not
// the caller's, or that the organisation does not accept. It cannot read a
// pasted token's scopes or expiry, so the person enters the expiry they chose,
// held to the row's pat_max_days, and the run is held to its capabilities by
// the proxy's own request check exactly as on every other mode. It cannot revoke
// the token either: Remove deletes Wardyn's copy only.
//
// ONE PERSON'S, NEVER SHARED. The token is stored in the person's own namespace
// under a sealed name (wardyn-harness-adoown-<row>-oauth), read list-first with
// no operator fallback, and never given to the sandbox: the proxy adds it.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// adoOwnPATPrefix is this mode's reserved-name segment. Distinct from the
// Entra sign-in's "ado" (the fifth character differs), so no row id can make
// the two names collide.
const adoOwnPATPrefix = "adoown"

// adoOwnPATExpiringWindow is how long before its expiry an own token reads
// "expiring": a working week, enough to create and paste a new one.
const adoOwnPATExpiringWindow = 7 * 24 * time.Hour

// adoOwnPATMaxTokenLen bounds a pasted value. A personal access token is a few
// dozen characters; this only stops an unbounded paste reaching a header.
const adoOwnPATMaxTokenLen = 512

// adoOwnPATAPIBase is where the identity check calls Azure DevOps. A variable
// only so the tests can point it at the fake.
var adoOwnPATAPIBase = "https://dev.azure.com"

// The door's refusals. The canon ones are the approved mock's (own-token
// errors); the rest name what the caller can do.
const (
	adoOwnPATUnknownRowRefusal   = "No Azure DevOps provider row you may use takes your own token at this address"
	adoOwnPATTooLongRefusal      = "This token expires after the %d-day limit your administrator set."
	adoOwnPATExpiryPastRefusal   = "Enter the date this token expires; it must be after today."
	adoOwnPATRejectedRefusal     = "Azure DevOps didn't accept this token."
	adoOwnPATMismatchRefusal     = "This token belongs to a different Azure DevOps account than yours."
	adoOwnPATUnavailableRefusal  = "Wardyn couldn't reach Azure DevOps to check this token. Try again in a moment."
	adoOwnPATTokenInvalidRefusal = "Paste the token itself: one value, with no spaces"
)

// adoOwnPATSecretName is the sealed store name holding one person's own token
// for one row. Callers must have validated rowID (adoEntraValidRowID).
func adoOwnPATSecretName(rowID string) string {
	if !adoEntraValidRowID(rowID) {
		panic("api: adoOwnPATSecretName called with an unvalidated provider row id")
	}
	return "wardyn-harness-" + adoOwnPATPrefix + "-" + rowID + "-oauth"
}

// adoOwnPATBlob is the stored token. Org is the organisation Azure DevOps
// accepted it for; ExpiresOn is the start (00:00 UTC) of the day the person
// said it expires, which is when Wardyn stops using it — never later than
// Azure DevOps would.
type adoOwnPATBlob struct {
	Token     string    `json:"token"`
	Org       string    `json:"org"`
	ExpiresOn time.Time `json:"expires_on"`
	StoredAt  time.Time `json:"stored_at"`
}

func (b adoOwnPATBlob) valid() bool {
	return b.Token != "" && b.Org != "" && !b.ExpiresOn.IsZero()
}

func (b adoOwnPATBlob) expired(now time.Time) bool { return !now.Before(b.ExpiresOn) }

// isADOOwnPATRow reports whether row is one where each person adds their own
// token: Azure DevOps, the entra lane, per_user, token_mode own_pat.
func isADOOwnPATRow(row types.GitProvider) bool {
	return row.Kind == types.GitProviderAzureDevOps && laneAllowed(row, types.GitLaneEntra) && row.Entra != nil &&
		row.CredentialSource == types.CredentialSourcePerUser && row.Entra.TokenMode == types.ADOTokenModeOwnPAT
}

// readADOOwnPAT loads owner's own token for one row. The owner's own list comes
// first, and the read is owner-only as well: For(owner).Get falls back to the
// operator's row by contract, and one person's token must never answer for
// another. An empty owner reads nothing.
func (s *Server) readADOOwnPAT(ctx context.Context, owner, rowID string) (adoOwnPATBlob, bool, error) {
	if s.cfg.Secrets == nil || owner == "" {
		return adoOwnPATBlob{}, false, nil
	}
	if !adoEntraValidRowID(rowID) {
		return adoOwnPATBlob{}, false, fmt.Errorf("azure devops provider row id %q is not a usable store name", rowID)
	}
	name := adoOwnPATSecretName(rowID)
	st := s.cfg.Secrets.For(owner)
	own, err := st.List(ctx)
	if err != nil {
		return adoOwnPATBlob{}, false, fmt.Errorf("list own azure devops token: %w", err)
	}
	if !slices.Contains(own, name) {
		return adoOwnPATBlob{}, false, nil
	}
	raw, err := st.Get(ctx, name)
	if errors.Is(err, secretstore.ErrNotFound) {
		return adoOwnPATBlob{}, false, nil
	}
	if err != nil {
		return adoOwnPATBlob{}, false, fmt.Errorf("read own azure devops token: %w", err)
	}
	var blob adoOwnPATBlob
	if err := json.Unmarshal(raw, &blob); err != nil {
		return adoOwnPATBlob{}, false, fmt.Errorf("parse own azure devops token: %w", err)
	}
	if !blob.valid() {
		return adoOwnPATBlob{}, false, nil
	}
	return blob, true, nil
}

// adoOwnPATRowFor finds the enabled own-token row the caller may use whose
// address (SCMAccess.Org) is org. A row the caller may not use is answered as
// no row at all (D-6).
func (s *Server) adoOwnPATRowFor(ctx context.Context, org string) (types.GitProvider, bool, error) {
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return types.GitProvider{}, false, err
	}
	var rows []types.GitProvider
	for _, row := range gitProviderRows(sc) {
		if !row.Disabled && isADOOwnPATRow(row) && adoEntraValidRowID(row.ID) && adoOrgDisplay(row) == org {
			rows = append(rows, row)
		}
	}
	rows = capVisible(ctx, s, capWorkspaceProvider, rows, func(r types.GitProvider) string { return r.ID })
	if len(rows) == 0 {
		return types.GitProvider{}, false, nil
	}
	return rows[0], true, nil
}

// adoOwnPATRequest is PUT /me/scm/azure-devops/token's body. Org is the row's
// address as /me/scm-access names it; ExpiresOn is the date the person chose
// in Azure DevOps (YYYY-MM-DD).
type adoOwnPATRequest struct {
	Org       string `json:"org"`
	Token     string `json:"token"`
	ExpiresOn string `json:"expires_on"`
}

// mountADOOwnPATRoutes mounts the own-token doors on the authenticated human
// group, beside /me/scm-access. Both refuse a caller with no session subject.
func (s *Server) mountADOOwnPATRoutes(r chi.Router) {
	r.Put("/me/scm/azure-devops/token", s.handlePutADOOwnPAT)
	r.Delete("/me/scm/azure-devops/token", s.handleDeleteADOOwnPAT)
}

// handlePutADOOwnPAT stores the caller's own token for one row, after three
// checks in this order: the expiry the person entered is after today and
// within the row's pat_max_days; Azure DevOps accepts the token for the row's
// organisation; and the account it names is the caller's. The answer is the
// row's fresh /me/scm-access entry.
func (s *Server) handlePutADOOwnPAT(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	subject := oidcHumanFromContext(ctx)
	if subject == "" {
		writeErrorReason(w, http.StatusForbidden, reasonADOSignInNoSession, adoSignInNoSessionRefusal)
		return
	}
	var req adoOwnPATRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	row, org, ok := s.adoOwnPATDoorRow(w, r, req.Org)
	if !ok {
		return
	}
	now := s.cfg.Now().UTC()
	expiresOn, ok := adoOwnPATExpiry(w, req.ExpiresOn, now, row.Entra.PATDays())
	if !ok {
		return
	}
	token := strings.TrimSpace(req.Token)
	if token == "" || len(token) > adoOwnPATMaxTokenLen || strings.ContainsFunc(token, isSpaceOrControl) {
		writeErrorReason(w, http.StatusBadRequest, reasonADOOwnPATTokenInvalid, adoOwnPATTokenInvalidRefusal)
		return
	}
	audit := map[string]any{"provider_row": row.ID, "organisation": org, "expires_on": expiresOn.Format(time.DateOnly)}
	account, err := s.adoOwnPATAccount(ctx, org, token)
	switch {
	case errors.Is(err, errADOOwnPATRejected):
		audit["reason"] = reasonADOOwnPATRejected
		s.auditADOOwnPAT(ctx, subject, adoPATAuditOwnStore, row.ID, "failure", audit)
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonADOOwnPATRejected, adoOwnPATRejectedRefusal)
		return
	case err != nil:
		slog.WarnContext(ctx, "wardynd: the Azure DevOps own-token check did not complete", slog.String("row", row.ID), slog.Any("err", err))
		writeErrorReason(w, http.StatusServiceUnavailable, reasonADOOwnPATCheckUnavailable, adoOwnPATUnavailableRefusal)
		return
	}
	if basis := adoOwnPATMismatch(account, oidcEmailFromContext(ctx)); basis != "" {
		// Never the account the token named: the refusal and the audit row say
		// only that it is not the caller's.
		audit["basis"] = basis
		s.auditADOOwnPAT(ctx, subject, adoPATAuditOwnMismatch, row.ID, "failure", audit)
		writeErrorReason(w, http.StatusForbidden, reasonADOOwnPATIdentityMismatch, adoOwnPATMismatchRefusal)
		return
	}
	blob := adoOwnPATBlob{Token: token, Org: org, ExpiresOn: expiresOn, StoredAt: now}
	raw, _ := json.Marshal(blob)
	if err := s.cfg.Secrets.For(subject).Put(ctx, adoOwnPATSecretName(row.ID), raw); err != nil {
		s.auditRowNotWritten(ctx, err, types.ActorHuman, subject, subject, adoOwnPATSecretName(row.ID))
		writeServerError(w, r, "store the Azure DevOps token", err)
		return
	}
	s.auditADOOwnPAT(ctx, subject, adoPATAuditOwnStore, row.ID, "success", audit)
	s.resolvePendingADOOwnPATHolds(ctx, subject, row.ID, now)
	access, err := s.scmAccessForOwnPAT(ctx, row, subject)
	if err != nil {
		writeServerError(w, r, "read Azure DevOps access", err)
		return
	}
	writeJSON(w, http.StatusOK, access)
}

// handleDeleteADOOwnPAT deletes the caller's own token for the row at ?org=.
// Wardyn cannot revoke it in Azure DevOps; the person does that themselves.
func (s *Server) handleDeleteADOOwnPAT(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	subject := oidcHumanFromContext(ctx)
	if subject == "" {
		writeErrorReason(w, http.StatusForbidden, reasonADOSignInNoSession, adoSignInNoSessionRefusal)
		return
	}
	row, org, ok := s.adoOwnPATDoorRow(w, r, r.URL.Query().Get("org"))
	if !ok {
		return
	}
	_, found, err := s.readADOOwnPAT(secretstore.WithPurpose(ctx, secretstore.PurposeStatus), subject, row.ID)
	if err != nil {
		writeServerError(w, r, "read the Azure DevOps token", err)
		return
	}
	if found {
		if err := s.cfg.Secrets.For(subject).Delete(ctx, adoOwnPATSecretName(row.ID)); err != nil {
			writeServerError(w, r, "delete the Azure DevOps token", err)
			return
		}
		s.auditADOOwnPAT(ctx, subject, adoPATAuditOwnDelete, row.ID, "success",
			map[string]any{"provider_row": row.ID, "organisation": org})
	}
	w.WriteHeader(http.StatusNoContent)
}

// adoOwnPATDoorRow resolves the row both doors act on and its organisation.
// Unknown, disabled, not own-token, or not the caller's to use all answer the
// same 404.
func (s *Server) adoOwnPATDoorRow(w http.ResponseWriter, r *http.Request, orgKey string) (types.GitProvider, string, bool) {
	if s.cfg.Store == nil || s.cfg.Secrets == nil {
		writeErrorReason(w, http.StatusNotFound, reasonADOOwnPATUnknownRow, adoOwnPATUnknownRowRefusal)
		return types.GitProvider{}, "", false
	}
	row, found, err := s.adoOwnPATRowFor(r.Context(), orgKey)
	if err != nil {
		writeServerError(w, r, "read site config", err)
		return types.GitProvider{}, "", false
	}
	org, orgOK := adoOrganisationOf(orgKey)
	if !found || !orgOK {
		writeErrorReason(w, http.StatusNotFound, reasonADOOwnPATUnknownRow, adoOwnPATUnknownRowRefusal)
		return types.GitProvider{}, "", false
	}
	return row, org, true
}

// adoOwnPATExpiry parses the date the person entered and holds it to (today,
// today + maxDays]. Days are UTC calendar days.
func adoOwnPATExpiry(w http.ResponseWriter, raw string, now time.Time, maxDays int) (time.Time, bool) {
	day, err := time.Parse(time.DateOnly, strings.TrimSpace(raw))
	today := now.Truncate(24 * time.Hour)
	if err != nil || !day.After(today) {
		writeErrorReason(w, http.StatusBadRequest, reasonADOOwnPATExpiryInvalid, adoOwnPATExpiryPastRefusal)
		return time.Time{}, false
	}
	if day.After(today.AddDate(0, 0, maxDays)) {
		writeErrorReason(w, http.StatusBadRequest, reasonADOOwnPATExpiryTooLong, fmt.Sprintf(adoOwnPATTooLongRefusal, maxDays))
		return time.Time{}, false
	}
	return day, true
}

func isSpaceOrControl(r rune) bool { return r <= ' ' || r == 0x7f }

// ── the identity check ─────────────────────────────────────────────────────

var (
	// errADOOwnPATRejected: Azure DevOps answered and did not accept the token
	// for this organisation (revoked, mistyped, another organisation's), or
	// named nobody.
	errADOOwnPATRejected = errors.New("azure devops did not accept the token")
	// errADOOwnPATUnavailable: the check did not complete; nothing is known
	// about the token.
	errADOOwnPATUnavailable = errors.New("the azure devops token check did not complete")
)

// adoOwnPATAccount asks Azure DevOps, with the token itself, which account it
// authenticates as in org: connectionData's authenticatedUser, whose Account
// property is the sign-in name. Any answer but a 200 naming an account is a
// refusal, except a 429 or a 5xx, which say nothing about the token.
func (s *Server) adoOwnPATAccount(ctx context.Context, org, token string) (string, error) {
	u := adoOwnPATAPIBase + "/" + url.PathEscape(org) + "/_apis/connectionData?api-version=7.1"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errADOOwnPATUnavailable, err)
	}
	req.Header.Set("Authorization", adoOwnPATHeaderValue(token))
	req.Header.Set("Accept", "application/json")
	// No redirect is followed: the request carries the token, and Azure DevOps
	// answers a token it does not accept with a redirect to its sign-in page.
	client := &http.Client{
		Transport:     http.DefaultTransport,
		Timeout:       adoEntraRedeemTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errADOOwnPATUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return "", fmt.Errorf("%w: HTTP %d", errADOOwnPATUnavailable, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: HTTP %d", errADOOwnPATRejected, resp.StatusCode)
	}
	var body struct {
		AuthenticatedUser struct {
			Properties map[string]struct {
				Value string `json:"$value"`
			} `json:"properties"`
		} `json:"authenticatedUser"`
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<18))
	if err != nil {
		return "", fmt.Errorf("%w: %w", errADOOwnPATUnavailable, err)
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", fmt.Errorf("%w: unparseable connectionData", errADOOwnPATRejected)
	}
	account := strings.TrimSpace(body.AuthenticatedUser.Properties["Account"].Value)
	if account == "" {
		return "", fmt.Errorf("%w: connectionData named no account", errADOOwnPATRejected)
	}
	return account, nil
}

// adoOwnPATMismatch reports why a token's account is not the caller's, or ""
// when it is. The account must equal the email of the caller's verified
// sign-in, case aside. A caller whose sign-in carries no email cannot be
// matched, so is refused ("no_email") rather than trusted.
func adoOwnPATMismatch(account, email string) string {
	email = strings.TrimSpace(email)
	switch {
	case email == "":
		return "no_email"
	case !strings.EqualFold(account, email):
		return "account"
	}
	return ""
}

// adoOwnPATHeaderValue is the one wire shape a personal access token has:
// Basic, an empty user name and the token as the password.
func adoOwnPATHeaderValue(token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(":"+token))
}

// auditADOOwnPAT writes one of the own-token audit rows. data never carries
// the token, nor the account a mismatched token named.
func (s *Server) auditADOOwnPAT(ctx context.Context, actor, action, rowID, outcome string, data map[string]any) {
	s.recordAudit(ctx, s.auditEvent(nil, types.ActorHuman, actor, action, adoOwnPATSecretName(rowID), outcome, mustJSON(data)))
}

// resolvePendingADOOwnPATHolds answers every PENDING Azure DevOps sign-in
// request of owner's for this row raised before the token was stored: a run
// held because its own token expired resumes with the new one. Best-effort; a
// request left pending ages out, and its run fails with the hold's reason.
func (s *Server) resolvePendingADOOwnPATHolds(ctx context.Context, owner, rowID string, storedAt time.Time) {
	resolver, ok := s.cfg.Store.(reauthResolver)
	if s.cfg.Approvals == nil || !ok {
		return
	}
	rows, err := s.cfg.Approvals.List(ctx, types.ApprovalPending)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: could not list pending Azure DevOps sign-in requests after a token was added", slog.Any("err", err))
		return
	}
	for _, ap := range rows {
		sc, ok := adoSignInScope(ap)
		if !ok || sc.Owner != owner || sc.ProviderID != rowID || !storedAt.After(ap.RequestedAt) {
			continue
		}
		ev := s.auditEvent(&ap.RunID, types.ActorHuman, owner, "credential.reauth.resolve", ap.ID.String(), "success",
			mustJSON(map[string]any{"approval_id": ap.ID, "owner": owner, "resolved_by": owner, "provider": adoApprovalLane}))
		if _, err := resolver.ResolveReauthApproval(ctx, ap.ID, types.ApprovalDecision{
			State: types.ApprovalApproved, DecidedBy: owner, Reason: "added a new token",
		}, ev); err == nil {
			s.approvalClosed(ctx, ap.RunID)
		}
	}
}

// ── what the dialog asks the person to create ──────────────────────────────

// adoTokenPageScope is one scope as Azure DevOps' own token page offers it: the
// area it is ticked under, the level's label, and the level's rank within that
// area (the page takes one level per area).
type adoTokenPageScope struct {
	area, level string
	rank        int
}

// adoTokenPageScopes names every scope a capability can map to in Azure
// DevOps' wording. A scope missing here is shown by its own name rather than
// dropped, so the list never asks for less than a run could hold.
var adoTokenPageScopes = map[string]adoTokenPageScope{
	"vso.analytics":                   {"Analytics", "Read", 1},
	"vso.build":                       {"Build", "Read", 1},
	"vso.build_execute":               {"Build", "Read & execute", 2},
	"vso.code":                        {"Code", "Read", 1},
	"vso.code_write":                  {"Code", "Read & write", 2},
	"vso.code_manage":                 {"Code", "Read, write, & manage", 3},
	"vso.graph":                       {"Graph", "Read", 1},
	"vso.graph_manage":                {"Graph", "Read & manage", 2},
	"vso.identity":                    {"Identity", "Read", 1},
	"vso.identity_manage":             {"Identity", "Read & manage", 2},
	"vso.memberentitlementmanagement": {"Member Entitlement Management", "Read", 1},
	"vso.packaging":                   {"Packaging", "Read", 1},
	"vso.packaging_write":             {"Packaging", "Read & write", 2},
	"vso.packaging_manage":            {"Packaging", "Read, write, & manage", 3},
	"vso.profile":                     {"User Profile", "Read", 1},
	"vso.project":                     {"Project and Team", "Read", 1},
	"vso.project_manage":              {"Project and Team", "Read, write, & manage", 3},
	"vso.release":                     {"Release", "Read", 1},
	"vso.release_execute":             {"Release", "Read, write, & execute", 2},
	"vso.release_manage":              {"Release", "Read, write, execute, & manage", 3},
	"vso.securefiles_read":            {"Secure Files", "Read", 1},
	"vso.security_manage":             {"Security", "Manage", 1},
	"vso.serviceendpoint":             {"Service Connections", "Read", 1},
	"vso.serviceendpoint_manage":      {"Service Connections", "Read, query, & manage", 3},
	"vso.test":                        {"Test Management", "Read", 1},
	"vso.variablegroups_read":         {"Variable Groups", "Read", 1},
	"vso.wiki":                        {"Wiki", "Read", 1},
	"vso.wiki_write":                  {"Wiki", "Read & write", 2},
	"vso.work":                        {"Work Items", "Read", 1},
	"vso.work_write":                  {"Work Items", "Read & write", 2},
}

// adoOwnPATTokenScopes is what the person ticks on Azure DevOps' token page for
// a row: the scopes of its ceiling, one level per area (the widest), as
// "Area (Level)", sorted. nil when the ceiling maps to no scope.
func adoOwnPATTokenScopes(ceiling []adoscope.Capability) []string {
	scopes, err := adoscope.ScopesFor(ceiling)
	if err != nil {
		return nil
	}
	widest := map[string]adoTokenPageScope{}
	var out []string
	for _, q := range scopes {
		sc := strings.TrimPrefix(q, adoscope.ResourceID+"/")
		p, ok := adoTokenPageScopes[sc]
		if !ok {
			out = append(out, sc)
			continue
		}
		if cur, seen := widest[p.area]; !seen || p.rank > cur.rank {
			widest[p.area] = p
		}
	}
	for _, p := range widest {
		out = append(out, p.area+" ("+p.level+")")
	}
	slices.Sort(out)
	return out
}
