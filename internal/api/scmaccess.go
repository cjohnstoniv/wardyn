// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/url"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// scmaccess.go is modelaccess.go's sibling for Azure DevOps: WHOSE credential
// clones a run, and what state it is in, for the CALLING person. Issue #386.
//
// It reuses the SIX-value vocabulary modelaccess.go already defines
// (modelAccessLive, modelAccessExpiring, modelAccessExpiredSignin,
// modelAccessNotConfigured, modelAccessSharedExpired, modelAccessNotApplicable)
// for a second subject — docs/design/ado-entra-prompt.md §1: "the six-value
// access vocabulary exists... This round reuses the vocabulary and the render
// exactly, for a second subject."
//
// WHAT IS AND ISN'T KNOWABLE TODAY. Microsoft Entra publishes no refresh-token
// expiry (ado_entra_store.go's adoEntraBlob.ExpiresAt doc comment: "Entra
// publishes none"), and this package captures no live "known dead" signal the
// way AWS SSO's ssoRefreshSpent map does — that lands with dispatch-time
// redemption (#387+). So `expiring` and the per-user arm of `expired_signin`
// are part of the vocabulary this file exports but are NOT REACHABLE from
// today's local signals: only `live` (a found, valid blob) and
// `not_configured` (none) are, for the per-user lane. A future caller that
// learns a credential actually died (a failed redemption) can widen
// adoAccessState's inputs without changing its shape or its callers.
//
// THE ADMIN ROW MODEL THIS READS AGAINST (`entra` lane, `credential_source`
// on GitProvider) IS #383's, not yet landed. This file never assumes those
// fields exist: "does per-user Azure DevOps apply to this row" is answered by
// whether s.cfg.ADOEntra resolves a config FOR IT (the seam #385 already
// wired and #383 will populate from the row), never by reading a
// `credential_source` field that is not there yet.

// scmAccessSource says where a person's live per-user connection came from —
// a second fact the copy renders beside the state (§7.5's Wardyn-sign-in vs
// dedicated-connect split), never a seventh state.
const (
	// scmAccessSourceOrg: the organisation's own Wardyn sign-in widened to
	// carry Azure DevOps scopes (adoEntraSourceLogin, ado_entra_login.go).
	scmAccessSourceOrg = "org"
	// scmAccessSourceSeparate: the dedicated Connect flow (adoEntraSourceSignIn,
	// ado_entra.go) — §0.1's fallback path.
	scmAccessSourceSeparate = "separate"
)

// scmAccessCauseRowIsNewer is the one `not_configured` cause this deployment
// can actually tell today: the person has never captured a session for this
// row. §0.1 names three more (consent declined, the connection ended, a
// different sign-in directory) — each needs a signal nothing upstream of this
// issue persists yet (a failed redemption, a login-time consent-decline
// record), so this file reports the one cause it can stand behind rather than
// guessing at the other three.
const scmAccessCauseRowIsNewer = "row_is_newer"

// SCMAccess is THIS PRINCIPAL's Azure DevOps access answer — the field the
// Getting-started chip, the Settings connected panel, the New Run rail's
// preflight line, and GET /me/scm-access all read instead of each re-deriving
// it from the provider row and the stored blob.
type SCMAccess struct {
	// State is one of the six modelaccess.go values.
	State string `json:"state"`
	// Source is "org" or "separate" — set only for a live per-user connection
	// (state == live and the row is per-user). "" for every other state: a
	// shared row's `live` makes no per-person claim (§7.5's ACCESS_SHARED_NOTE),
	// and a dead or absent connection has no source to report.
	Source string `json:"source,omitempty"`
	// Cause narrows `not_configured` — see scmAccessCauseRowIsNewer.
	Cause string `json:"cause,omitempty"`
	// Org is the Azure DevOps address this row clones from (the row's first
	// base URL) — the {org} the connect and launch dialogs name.
	Org string `json:"org,omitempty"`
}

// adoAccessState grades one Azure DevOps row + this person's captured sign-in
// into the six-value vocabulary. Pure over its arguments, mirroring
// awsSSOCredentialState's shape.
//
//   - rowConfigured: an enabled azure_devops GitProvider row exists.
//   - entraAvailable: this deployment's per-user Entra lane resolves for that
//     ROW (s.cfg.ADOEntra, checked against the row's own id) — effectively
//     "this row's credential_source is per-user". False means the row relies
//     on a stored PAT/SSH credential instead — today's `shared` behaviour.
//   - isMechanism: the caller carries no identity-provider subject (an
//     admin-token or local-mode caller) — no sign-in it could complete, and
//     under this lane no capture is ever stored for one either (ado_entra.go
//     refuses the capture outright, unlike AWS SSO's admin-bearer mechanism
//     namespace).
//   - found: a valid captured blob exists for (this person, this row).
//   - sharedPresent: under a non-entra row, whether this deployment's stored
//     git-pat-<slug>/ssh-key-<slug> secret for the row's host is present.
func adoAccessState(rowConfigured, entraAvailable, isMechanism, found, sharedPresent bool) string {
	if !rowConfigured {
		return "" // nothing per-principal to say — the absent-row doctrine (modelaccess.go's SetupModelAccess zero value)
	}
	if !entraAvailable {
		if sharedPresent {
			return modelAccessLive
		}
		return modelAccessSharedExpired
	}
	if isMechanism {
		return modelAccessNotApplicable
	}
	if found {
		return modelAccessLive
	}
	return modelAccessNotConfigured
}

// adoProviderRow is the first ENABLED azure_devops GitProvider row — one row,
// because #383's model (not yet landed) is one Azure DevOps organisation per
// deployment for this round; a second row is a future-proofing question for
// whoever lands per-row disambiguation, not this issue.
func adoProviderRow(sc types.SiteConfig) (types.GitProvider, bool) {
	for _, row := range gitProviderRows(sc) {
		if row.Kind == types.GitProviderAzureDevOps && !row.Disabled {
			return row, true
		}
	}
	return types.GitProvider{}, false
}

// adoEntraRowConfig resolves s.cfg.ADOEntra and reports ok=true only when it
// names THIS row: a deployment's ADOEntra source can only ever describe one
// row (ADOEntraSource takes no row argument, ado_entra.go), so a row id
// mismatch means this particular row is not the per-user one.
func (s *Server) adoEntraRowConfig(ctx context.Context, rowID string) (ADOEntraConfig, bool) {
	if s.cfg.ADOEntra == nil {
		return ADOEntraConfig{}, false
	}
	cfg, found, err := s.cfg.ADOEntra(ctx)
	if err != nil || !found || cfg.RowID != rowID {
		return ADOEntraConfig{}, false
	}
	if err := cfg.validate(); err != nil || !cfg.isLoginApplication() {
		return ADOEntraConfig{}, false
	}
	return cfg, true
}

// gitSharedCredentialPresent reports whether a STORED credential for row's
// host(s) exists in the shared (operator) secret namespace — the
// `git-pat-<slug>` / `ssh-key-<slug>` convention scmProviderCheck already
// grades deployment-wide, scoped here to the one row's own hosts and lanes.
func (s *Server) gitSharedCredentialPresent(ctx context.Context, row types.GitProvider) bool {
	present := s.presentSecretNamesFor(ctx, "")
	for _, base := range row.BaseURLs {
		u, err := url.Parse(base)
		if err != nil || u.Host == "" {
			continue
		}
		slug := slugHost(u.Host)
		if laneAllowed(row, types.GitLanePAT) && present["git-pat-"+slug] {
			return true
		}
		if laneAllowed(row, types.GitLaneSSH) && present["ssh-key-"+slug] {
			return true
		}
	}
	return false
}

// scmAccessSourceFor maps a stored blob's Source (ado_entra_store.go) onto
// the wire value the copy renders.
func scmAccessSourceFor(blobSource string) string {
	switch blobSource {
	case adoEntraSourceLogin:
		return scmAccessSourceOrg
	case adoEntraSourceSignIn:
		return scmAccessSourceSeparate
	default:
		return ""
	}
}

// computeSCMAccess is THIS REQUEST's SCMAccess answer for subject (the OIDC
// human subject — "" for a mechanism caller, the same subject ado_entra.go
// binds a capture to). ok=false: no Azure DevOps row is configured, or the
// site config could not be read — the absent-row doctrine; every caller
// renders exactly what it does today.
func (s *Server) computeSCMAccess(ctx context.Context, subject string) (SCMAccess, bool) {
	if s.cfg.Store == nil {
		return SCMAccess{}, false
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return SCMAccess{}, false
	}
	return s.computeSCMAccessFor(ctx, sc, subject)
}

// computeSCMAccessFor is computeSCMAccess over an already-read site config —
// the shape requestRepoProviderRefusals needs, which already holds one.
func (s *Server) computeSCMAccessFor(ctx context.Context, sc types.SiteConfig, subject string) (SCMAccess, bool) {
	row, rowOK := adoProviderRow(sc)
	if !rowOK {
		return SCMAccess{}, false
	}
	cfg, entraOK := s.adoEntraRowConfig(ctx, row.ID)
	isMechanism := subject == ""
	var found bool
	var source string
	if entraOK && !isMechanism {
		if blob, ok, _ := s.readADOEntraBlob(ctx, subject, cfg.RowID); ok {
			found = true
			source = scmAccessSourceFor(blob.Source)
		}
	}
	sharedPresent := false
	if !entraOK {
		sharedPresent = s.gitSharedCredentialPresent(ctx, row)
	}
	state := adoAccessState(true, entraOK, isMechanism, found, sharedPresent)
	if state == "" {
		return SCMAccess{}, false
	}
	out := SCMAccess{State: state, Org: adoOrgDisplay(row)}
	if state == modelAccessLive && entraOK {
		out.Source = source
	}
	if state == modelAccessNotConfigured {
		out.Cause = scmAccessCauseRowIsNewer
	}
	return out, true
}

// scmAccessValue is computeSCMAccessFor with the ok bool dropped — the shape
// SetupStatus.SCMAccess (setup.go) wants inline in its struct literal, at
// setup.go's own size cap (see this file's #386 comment for why setup-row
// code lives here instead).
func (s *Server) scmAccessValue(ctx context.Context, sc types.SiteConfig, subject string) SCMAccess {
	v, _ := s.computeSCMAccessFor(ctx, sc, subject)
	return v
}

// adoOrgDisplay is the row's own address, for the {org} the connect/launch
// dialogs name — the row's first base URL, "" when it somehow carries none.
func adoOrgDisplay(row types.GitProvider) string {
	if len(row.BaseURLs) == 0 {
		return ""
	}
	return row.BaseURLs[0]
}

// gitCredentialRefusalReason is the 422 `reason` the New Run rail recognises
// (the issue's one new value on the 0.7.7 relaunch path, §1: "This round adds
// one `reason` value, `git_credential`, and reuses the path").
const gitCredentialRefusalReason = "git_credential"

// gitCredentialNotConnectedRefusal is §7.1's composed sentence for the ONE
// cause this file can actually tell apart today (adoAccessState's doc
// comment): the person has never captured a session for the row. The doc
// also freezes a second sentence for "your connection ended" — that needs a
// live-failure signal this issue's merged code does not yet produce (see the
// file doc comment), so it is not emitted here; a future caller that learns
// a credential died can add that branch without moving this one.
const gitCredentialNotConnectedRefusal = "you are not connected to Azure DevOps — connect and start the run again"

// gitCredentialRefusal is the git_credential 422 gate (#386's launch door): a
// repository admitted onto a per-user Azure DevOps row, with no usable
// captured sign-in for the caller, refuses the create/preflight door here —
// the SAME function launch and Review share (requestRepoProviderRefusals), so
// the two doors cannot answer differently.
//
// It runs AFTER admission (a repo not admitted at all never reaches this),
// so it only ever needs to resolve the row that already admitted the repo,
// never re-derive admission itself.
func (s *Server) gitCredentialRefusal(w http.ResponseWriter, r *http.Request, repos ...string) bool {
	repos = presentRepos(repos)
	if len(repos) == 0 || s.cfg.Store == nil {
		return false
	}
	ctx := r.Context()
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil || !providersConfigured(sc) {
		return false // a read failure or legacy open mode: admitRepoSources already answered, or there are no rows to grade
	}
	subject := oidcHumanFromContext(ctx)
	if subject == "" {
		return false // no session to bind a repair to — the same caller admission already let through
	}
	for _, repo := range repos {
		for _, row := range admittingRows(sc, repoCloneURL(repo)) {
			if row.Kind != types.GitProviderAzureDevOps {
				continue
			}
			cfg, entraOK := s.adoEntraRowConfig(ctx, row.ID)
			if !entraOK {
				continue // a shared row: nothing per-person for this caller to connect
			}
			if _, found, _ := s.readADOEntraBlob(ctx, subject, cfg.RowID); found {
				continue
			}
			// Not audited under authz.denied: that action's `reason` is a
			// documented CLOSED enum (docs/OPERATIONS.md), and this create-time
			// 422 follows writeLLMRefusal's own precedent (runs_dispatch_llm_mechanism.go)
			// — the sibling model_credential refusal at this same door is
			// likewise unaudited; only a run that actually DISPATCHES and then
			// fails audits, under run.create.
			writeJSON(w, http.StatusUnprocessableEntity, errorBody{
				Error: gitCredentialNotConnectedRefusal, Reason: gitCredentialRefusalReason,
			})
			return true
		}
	}
	return false
}

// handleGetSCMAccess serves GET /api/v1/me/scm-access: this caller's own
// Azure DevOps access state. Member-safe by construction — scoped entirely to
// the caller's own OIDC subject, the same self-service shape /me/ssh-keys is
// (routes.go).
func (s *Server) handleGetSCMAccess(w http.ResponseWriter, r *http.Request) {
	subject := oidcHumanFromContext(r.Context())
	access, ok := s.computeSCMAccess(r.Context(), subject)
	if !ok {
		writeJSON(w, http.StatusOK, SCMAccess{})
		return
	}
	writeJSON(w, http.StatusOK, access)
}
