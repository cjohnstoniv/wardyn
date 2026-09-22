// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"

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
// ONLY PER-USER (ENTRA) ROWS ARE EVER GRADED (review finding F4). An earlier
// version of this file graded a `shared` row too, inferring "live" from a
// stored git-pat-/ssh-key- secret's mere presence and "shared_expired" from
// its absence — that made a deployment with a plain, working PAT row report
// `shared_expired`, which is false: nothing about that row is expired, it was
// simply never asked to be per-user. #383 (not yet landed) is what will make
// `credential_source: shared` an authored fact rather than a guess; until
// then, a row this file cannot prove is per-user (s.cfg.ADOEntra does not
// resolve it) gets NO fact at all — the absent-row doctrine, and byte-identical
// to every deployment before this issue.
//
// WHAT IS AND ISN'T KNOWABLE TODAY, for a row this file CAN grade. Microsoft
// Entra publishes no refresh-token expiry (ado_entra_store.go's
// adoEntraBlob.ExpiresAt doc comment: "Entra publishes none"), and this
// package captures no live "known dead" signal the way AWS SSO's
// ssoRefreshSpent map does — that lands with dispatch-time redemption
// (#387+). So `expiring` and `expired_signin` are part of the vocabulary
// this file exports but are NOT REACHABLE from today's local signals: only
// `live` (a found, valid blob) and `not_configured` (none) are. A future
// caller that learns a credential actually died (a failed redemption) can
// widen adoAccessState's inputs without changing its shape or its callers.
//
// THE ADMIN ROW MODEL THIS READS AGAINST (`entra` lane, `credential_source`
// on GitProvider) IS #383's, not yet landed. This file never assumes those
// fields exist: "does per-user Azure DevOps apply to this row" is answered by
// whether s.cfg.ADOEntra resolves a config FOR IT (the seam #385 already
// wired and #383 will populate from the row), never by reading a
// `credential_source` field that is not there yet. ADOEntraSource itself
// names no row of its own (ado_entra.go) — today's deployments therefore
// carry at most ONE per-user row — so every "rows" plural below is 0 or 1
// entries in practice; the plumbing is row-shaped for when #383 changes that.

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

// SCMAccess is THIS PRINCIPAL's Azure DevOps access answer for ONE row — the
// shape the Getting-started chip, the Settings connected panel, the New Run
// rail's preflight line, and GET /me/scm-access all read instead of each
// re-deriving it from the provider row and the stored blob.
//
// State is `live` with an EMPTY Source only when a future caller (#383+)
// grades a genuinely shared row `live` — nothing in THIS file produces that
// combination today (see the file doc comment), but every reader of this
// struct must still treat an empty Source on `live` as "makes no per-person
// claim" (§7.5's ACCESS_SHARED_NOTE) rather than assume Source is always
// present — see ado-connection.tsx's own fix (review finding F3).
type SCMAccess struct {
	// RowID names which Azure DevOps GitProvider row this answer is about —
	// meaningful once a deployment can carry more than one per-user row
	// (#383); "" from no caller in this codebase today.
	RowID string `json:"row_id,omitempty"`
	// State is one of the six modelaccess.go values.
	State string `json:"state"`
	// Source is "org" or "separate" — set only for a live per-user connection.
	Source string `json:"source,omitempty"`
	// Cause narrows `not_configured` — see scmAccessCauseRowIsNewer.
	Cause string `json:"cause,omitempty"`
	// Org is the Azure DevOps address this row clones from (the row's first
	// base URL) — the {org} the connect and launch dialogs name.
	Org string `json:"org,omitempty"`
}

// adoAccessState grades one PER-USER row's captured sign-in into the
// six-value vocabulary. Pure over its arguments, mirroring
// awsSSOCredentialState's shape. Callers must have already proven the row IS
// per-user (adoEntraRowConfig ok) — there is no "shared" arm here; see the
// file doc comment for why (review finding F4).
//
//   - isMechanism: the caller carries no identity-provider subject (an
//     admin-token or local-mode caller) — no sign-in it could complete, and
//     under this lane no capture is ever stored for one either (ado_entra.go
//     refuses the capture outright, unlike AWS SSO's admin-bearer mechanism
//     namespace).
//   - found: a valid captured blob exists for (this person, this row).
func adoAccessState(isMechanism, found bool) string {
	if isMechanism {
		return modelAccessNotApplicable
	}
	if found {
		return modelAccessLive
	}
	return modelAccessNotConfigured
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

// perUserADORows is every ENABLED azure_devops GitProvider row this
// deployment's ADOEntra source actually covers (review finding F6) — today
// at most one (see the file doc comment); a future #383 that makes the
// source row-aware widens this loop for free, no caller changes.
func (s *Server) perUserADORows(ctx context.Context, sc types.SiteConfig) []types.GitProvider {
	var out []types.GitProvider
	for _, row := range gitProviderRows(sc) {
		if row.Kind != types.GitProviderAzureDevOps || row.Disabled {
			continue
		}
		if _, ok := s.adoEntraRowConfig(ctx, row.ID); ok {
			out = append(out, row)
		}
	}
	return out
}

// perUserADORowsAdmitting is perUserADORows narrowed to the rows that ADMIT
// at least one of repos (admittingRows' own verdict) — the SAME
// row-selection helper both gitCredentialRefusal (the gate) and
// gitCredentialFactForRepos (preflight's informational fact) use, so the two
// can never disagree about which row is in play (review finding F6).
func (s *Server) perUserADORowsAdmitting(ctx context.Context, sc types.SiteConfig, repos []string) []types.GitProvider {
	var out []types.GitProvider
	seen := map[string]bool{}
	for _, repo := range presentRepos(repos) {
		for _, row := range admittingRows(sc, repoCloneURL(repo)) {
			if row.Kind != types.GitProviderAzureDevOps || row.Disabled || seen[row.ID] {
				continue
			}
			if _, ok := s.adoEntraRowConfig(ctx, row.ID); !ok {
				continue // a row this file cannot prove per-user carries no fact (F4)
			}
			seen[row.ID] = true
			out = append(out, row)
		}
	}
	return out
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

// scmAccessForRow grades ONE row, already proven per-user by the caller
// (perUserADORows / perUserADORowsAdmitting), for subject.
func (s *Server) scmAccessForRow(ctx context.Context, row types.GitProvider, cfg ADOEntraConfig, subject string) SCMAccess {
	isMechanism := subject == ""
	var found bool
	var source string
	if !isMechanism {
		if blob, ok, _ := s.readADOEntraBlob(ctx, subject, cfg.RowID); ok {
			found = true
			source = scmAccessSourceFor(blob.Source)
		}
	}
	state := adoAccessState(isMechanism, found)
	out := SCMAccess{RowID: row.ID, State: state, Org: adoOrgDisplay(row)}
	if state == modelAccessLive {
		out.Source = source
	}
	if state == modelAccessNotConfigured {
		out.Cause = scmAccessCauseRowIsNewer
	}
	return out
}

// adoOrgDisplay is the row's own address, for the {org} the connect/launch
// dialogs name — the row's first base URL, "" when it somehow carries none.
func adoOrgDisplay(row types.GitProvider) string {
	if len(row.BaseURLs) == 0 {
		return ""
	}
	return row.BaseURLs[0]
}

// computeSCMAccessRows is GET /me/scm-access's answer (review finding F6):
// one SCMAccess per per-user Azure DevOps row. Empty — never nil-vs-absent
// distinguished on the wire, see handleGetSCMAccess — when there is no such
// row, which is every deployment before this issue and every deployment
// whose only Azure DevOps row is `shared` (F4's byte-identical requirement).
func (s *Server) computeSCMAccessRows(ctx context.Context, subject string) []SCMAccess {
	if s.cfg.Store == nil {
		return nil
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return nil
	}
	return s.computeSCMAccessRowsFor(ctx, sc, subject)
}

// computeSCMAccessRowsFor is computeSCMAccessRows over an already-read site
// config.
func (s *Server) computeSCMAccessRowsFor(ctx context.Context, sc types.SiteConfig, subject string) []SCMAccess {
	rows := s.perUserADORows(ctx, sc)
	out := make([]SCMAccess, 0, len(rows))
	for _, row := range rows {
		cfg, ok := s.adoEntraRowConfig(ctx, row.ID)
		if !ok {
			continue // perUserADORows already filtered to this; defensive only
		}
		out = append(out, s.scmAccessForRow(ctx, row, cfg, subject))
	}
	return out
}

// scmAccessValue is the FIRST row of computeSCMAccessRowsFor (today at most
// one row ever exists, see the file doc comment) — the shape
// SetupStatus.SCMAccess (setup.go) and preflight's deployment-agnostic
// callers want inline, at setup.go's own size cap (see this file's #386
// comment for why setup-row code lives here instead). The zero SCMAccess{}
// when there is none — setup.go's own `omitzero` then drops the field.
func (s *Server) scmAccessValue(ctx context.Context, sc types.SiteConfig, subject string) SCMAccess {
	rows := s.computeSCMAccessRowsFor(ctx, sc, subject)
	if len(rows) == 0 {
		return SCMAccess{}
	}
	return rows[0]
}

// gitCredentialFactForRepos is preflight's INFORMATIONAL, per-run
// git_credential fact (review finding F2) — never a refusal, and never a
// deployment-wide guess: the per-user row (if any) that ADMITS one of repos,
// graded for subject, using the SAME row-selection perUserADORowsAdmitting
// gitCredentialRefusal itself uses, so a preflight fact and a launch refusal
// can never point at different rows. nil when no per-user row admits any of
// these repos — a GitHub-only run, a deployment with no Azure DevOps row at
// all, or one whose only Azure DevOps row is shared, all read identically:
// nothing to say.
func (s *Server) gitCredentialFactForRepos(ctx context.Context, subject string, repos []string) *SCMAccess {
	if s.cfg.Store == nil {
		return nil
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil || !providersConfigured(sc) {
		return nil
	}
	rows := s.perUserADORowsAdmitting(ctx, sc, repos)
	if len(rows) == 0 {
		return nil
	}
	cfg, ok := s.adoEntraRowConfig(ctx, rows[0].ID)
	if !ok {
		return nil
	}
	out := s.scmAccessForRow(ctx, rows[0], cfg, subject)
	return &out
}

// gitCredentialErrorBody is the git_credential 422's body (review finding
// F1): the frozen sentence + reason, plus the org and row id the LAUNCH
// DOOR names in its dialog before any preflight verdict exists — a 422 can
// be the very FIRST thing this caller hears about the row, so the dialog
// cannot depend on a `git_credential` preflight fact having already landed.
type gitCredentialErrorBody struct {
	Error  string `json:"error"`
	Reason string `json:"reason"`
	Org    string `json:"org,omitempty"`
	RowID  string `json:"row_id,omitempty"`
}

// gitCredentialRefusalReason is the 422 `reason` the New Run rail recognises
// (the issue's one new value on the 0.7.7 relaunch path, §1: "This round adds
// one `reason` value, `git_credential`, and reuses the path").
const gitCredentialRefusalReason = "git_credential"

// gitCredentialNotConnectedRefusal is §7.1's composed sentence, BYTE-EXACT
// including its "git_credential: " prefix (review finding F7 — pinned by
// TestGitCredentialRefusalMatchesCanon, which parses the canon table the way
// ado-entra-copy.test.ts parses §7). It is the ONE cause this file can
// actually tell apart today (adoAccessState's doc comment): the person has
// never captured a session for the row. The doc also freezes a second
// sentence for "your connection ended" — that needs a live-failure signal
// this issue's merged code does not yet produce (see the file doc comment),
// so it is not emitted here; a future caller that learns a credential died
// can add that branch without moving this one.
const gitCredentialNotConnectedRefusal = "git_credential: you are not connected to Azure DevOps — connect and start the run again"

// gitCredentialRefusal is the git_credential 422 GATE (#386's launch door,
// review finding F5): a repository on a per-user Azure DevOps row, with no
// usable captured sign-in for the caller, refuses here. It is called from
// EVERY door a repository can reach a run through that this deployment can
// actually launch from — requestRepoProviderRefusals (the two free-text
// fields) and seedAndAdmitWorkspace (the resolved spec's WorkspaceRepos,
// which is what a workspace_id, a second workspace, a stored policy or an
// inline policy all fold into) — but NEVER from preflight, which reads the
// same row through gitCredentialFactForRepos instead and never refuses.
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
	for _, row := range s.perUserADORowsAdmitting(ctx, sc, repos) {
		cfg, ok := s.adoEntraRowConfig(ctx, row.ID)
		if !ok {
			continue // defensive only — perUserADORowsAdmitting already proved this
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
		writeJSON(w, http.StatusUnprocessableEntity, gitCredentialErrorBody{
			Error: gitCredentialNotConnectedRefusal, Reason: gitCredentialRefusalReason,
			Org: adoOrgDisplay(row), RowID: row.ID,
		})
		return true
	}
	return false
}

// handleGetSCMAccess serves GET /api/v1/me/scm-access: this caller's own
// Azure DevOps access, one entry per per-user row (review finding F6).
// Member-safe by construction — scoped entirely to the caller's own OIDC
// subject, the same self-service shape /me/ssh-keys is (routes.go).
func (s *Server) handleGetSCMAccess(w http.ResponseWriter, r *http.Request) {
	subject := oidcHumanFromContext(r.Context())
	rows := s.computeSCMAccessRows(r.Context(), subject)
	if rows == nil {
		rows = []SCMAccess{}
	}
	writeJSON(w, http.StatusOK, rows)
}
