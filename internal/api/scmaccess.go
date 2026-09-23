// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
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
// adoEntraBlob.ExpiresAt doc comment: "Entra publishes none"), so `expiring`
// is not reachable. `expired_signin` is, from two stored facts: a renewal
// recorded the sign-in dead or blocked by Conditional Access
// (adoEntraBlob.DeadAt), or the sign-in does not cover the scopes a run on
// the row's default profile needs (an administrator widened the row after
// the person signed in). Otherwise a found, valid blob is `live` and none is
// `not_configured`.
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

// The two `expired_signin` causes. scmAccessCauseEnded: a renewal found the
// stored sign-in dead or blocked by Conditional Access (adoEntraBlob.DeadAt).
// scmAccessCauseConsentNeeded: the sign-in was captured before an
// administrator widened the row, and no longer covers the access a run on the
// row's default profile is dispatched with.
const (
	scmAccessCauseEnded         = "ended"
	scmAccessCauseConsentNeeded = "consent_needed"
)

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
	// State is one of the six modelaccess.go values.
	State string `json:"state"`
	// Source is "org" or "separate" — set for a per-user connection that is
	// live or expired_signin (which door to go back through).
	Source string `json:"source,omitempty"`
	// Cause narrows `not_configured` (scmAccessCauseRowIsNewer) and
	// `expired_signin` (scmAccessCauseEnded, scmAccessCauseConsentNeeded).
	Cause string `json:"cause,omitempty"`
	// Org is the Azure DevOps address this row clones from (the row's first
	// base URL) — the {org} the connect and launch dialogs name.
	//
	// NO ROW ID (review follow-up N3). workspace_admission.go's own
	// disclosure rule ("the kind, never the row id") applies here the same
	// way it applies to admitSSHHostLevel's 201 warning; Org is kept as the
	// one deliberate exception the frozen copy needs (LAUNCH_DIALOG_BODY,
	// CONNECT_DIALOG_BODY, PANEL_ORG all interpolate {org} verbatim) — Kind
	// below is what a caller that needs a stable key over the /me/scm-access
	// array uses instead of a row id (org + kind is unique per row today,
	// since a deployment carries at most one Azure DevOps org).
	Org string `json:"org,omitempty"`
	// Kind is the provider kind this row is ("azure_devops", always, today —
	// scmaccess.go grades Azure DevOps rows only) — paired with Org as the
	// stable key a list of these needs, in place of a row id.
	Kind string `json:"kind,omitempty"`
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
	var blob adoEntraBlob
	var found bool
	if !isMechanism {
		blob, found, _ = s.readADOEntraBlob(ctx, subject, cfg.RowID)
	}
	out := SCMAccess{State: adoAccessState(isMechanism, found), Org: adoOrgDisplay(row), Kind: string(row.Kind)}
	switch {
	case out.State == modelAccessNotConfigured:
		out.Cause = scmAccessCauseRowIsNewer
	case out.State != modelAccessLive:
	case blob.signInEnded():
		out.State, out.Cause = modelAccessExpiredSignin, scmAccessCauseEnded
	case !adoBlobCoversBaseline(blob, row):
		out.State, out.Cause = modelAccessExpiredSignin, scmAccessCauseConsentNeeded
	}
	if out.State == modelAccessLive || out.State == modelAccessExpiredSignin {
		out.Source = scmAccessSourceFor(blob.Source)
	}
	return out
}

// adoBlobCoversBaseline reports whether a stored sign-in was consented for
// every scope a run on the row's default profile is dispatched with (Profile
// is nil-safe: no profile is the read profile). A profile the catalogue cannot
// map covers nothing honestly, so false.
func adoBlobCoversBaseline(blob adoEntraBlob, row types.GitProvider) bool {
	need, err := adoscope.ScopesFor(row.Entra.Profile())
	return err == nil && adoScopesWithin(need, blob.Scopes)
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
// F1): the frozen sentence + reason, plus the org the LAUNCH DOOR names in
// its dialog before any preflight verdict exists — a 422 can be the very
// FIRST thing this caller hears about the row, so the dialog cannot depend
// on a `git_credential` preflight fact having already landed. No row id
// (review follow-up N3) — see SCMAccess's own doc comment.
type gitCredentialErrorBody struct {
	Error  string `json:"error"`
	Reason string `json:"reason"`
	Org    string `json:"org,omitempty"`
}

// gitCredentialRefusalReason is the 422 `reason` the New Run rail recognises
// (the issue's one new value on the 0.7.7 relaunch path, §1: "This round adds
// one `reason` value, `git_credential`, and reuses the path").
const gitCredentialRefusalReason = "git_credential"

// gitCredentialNotConnectedRefusal is §7.1's composed sentence, BYTE-EXACT
// including its "git_credential: " prefix (review finding F7 — pinned by
// TestGitCredentialRefusalMatchesCanon, which parses the canon table the way
// ado-entra-copy.test.ts parses §7): the person has never captured a session
// for the row.
const gitCredentialNotConnectedRefusal = "git_credential: you are not connected to Azure DevOps — connect and start the run again"

// gitCredentialEndedRefusal / gitCredentialConsentRefusal are §7.1's two
// expired_signin sentences, by cause (scmAccessCauseEnded /
// scmAccessCauseConsentNeeded) — pinned by the same canon test.
const (
	gitCredentialEndedRefusal   = "git_credential: your Azure DevOps connection ended — connect and start the run again"
	gitCredentialConsentRefusal = "git_credential: your Azure DevOps connection doesn't cover the access this run needs — connect and start the run again"
)

// gitCredentialNoPersonRefusal answers a DEVICE caller (a hybrid laptop's
// wdd_ credential) whose run would need a per-person Azure DevOps credential.
// Not UI canon: no console renders it, since a device has no console session.
const gitCredentialNoPersonRefusal = "git_credential: this run needs a person's own Azure DevOps connection, and a device submitted it with no person — submit it as the person"

// errGitCredentialRefused is gitCredentialRefusalForLauncher's sentinel —
// errRepoNotAdmitted's shape (workspace_admission.go), for the launchers
// that hold no ResponseWriter (review follow-up N4: record.go's
// launchRecordRun). A caller matches it with errors.As, on the concrete
// *gitCredentialRefusalError below, to recover the org for the 422 body.
var errGitCredentialRefused = errors.New(gitCredentialRefusalReason)

// gitCredentialRefusalError carries the one extra fact
// gitCredentialErrorBody needs (the org) through an error return — Unwrap
// makes it match errGitCredentialRefused for a caller that only wants to
// know WHICH refusal this is, errors.As for one that wants the org too.
type gitCredentialRefusalError struct{ Org, Sentence string }

func (e *gitCredentialRefusalError) Error() string { return e.Sentence }
func (e *gitCredentialRefusalError) Unwrap() error { return errGitCredentialRefused }

// gitCredentialRefusalForLauncher is the git_credential GATE (#386's launch
// door, review finding F5) for a caller with no ResponseWriter — a
// SERVER-SIDE launcher (review follow-up N4: recordLaunchRefusals, called
// from launchRecordRun). nil when admitted; a *gitCredentialRefusalError
// (wrapped) otherwise, for the caller to map to its own door's 422 shape —
// record.go's own chain of errors.Is/errors.As mappings is the precedent
// (errWorkspaceSourceTarget, errAgentNotEnabled, ...).
//
// subject == "" (no OIDC human — an admin-token or local-mode caller) never
// refuses: no sign-in it could complete, the same rule the HTTP-facing
// gitCredentialRefusal follows for the identical reason.
//
// A DEVICE caller with no human is the exception, and it fails closed. A
// device context carries no OIDC human, so without this arm it would read as
// the admin token (isOperator refuses a device first for the same reason),
// skip this gate, and fail at dispatch with no credential stored under any
// person. A run a laptop submits to the organisation must carry the person.
func (s *Server) gitCredentialRefusalForLauncher(ctx context.Context, subject string, repos ...string) error {
	repos = presentRepos(repos)
	_, isDevice := deviceFromContext(ctx)
	noPerson := isDevice && subject == ""
	if len(repos) == 0 || s.cfg.Store == nil || (subject == "" && !noPerson) {
		return nil
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil && noPerson {
		return &gitCredentialRefusalError{Sentence: gitCredentialNoPersonRefusal}
	}
	if err != nil || !providersConfigured(sc) {
		return nil // a read failure or legacy open mode: the caller's own admission check already answered, or there are no rows to grade
	}
	for _, row := range s.perUserADORowsAdmitting(ctx, sc, repos) {
		if noPerson {
			return &gitCredentialRefusalError{Org: adoOrgDisplay(row), Sentence: gitCredentialNoPersonRefusal}
		}
		cfg, ok := s.adoEntraRowConfig(ctx, row.ID)
		if !ok {
			continue // defensive only — perUserADORowsAdmitting already proved this
		}
		access := s.scmAccessForRow(ctx, row, cfg, subject)
		switch {
		case access.State == modelAccessLive:
			continue
		case access.Cause == scmAccessCauseEnded:
			return &gitCredentialRefusalError{Org: access.Org, Sentence: gitCredentialEndedRefusal}
		case access.Cause == scmAccessCauseConsentNeeded:
			return &gitCredentialRefusalError{Org: access.Org, Sentence: gitCredentialConsentRefusal}
		}
		return &gitCredentialRefusalError{Org: access.Org, Sentence: gitCredentialNotConnectedRefusal}
	}
	return nil
}

// gitCredentialRefusal is gitCredentialRefusalForLauncher at an HTTP door
// that holds a ResponseWriter — requestRepoProviderRefusals (the two
// free-text fields) and seedAndAdmitWorkspace (the resolved spec's
// WorkspaceRepos, which is what a workspace_id, a second workspace, a
// stored policy or an inline policy all fold into), plus
// handleBuildWorkspace and handleScanWorkspace directly (review follow-up
// N4) — but NEVER from preflight, which reads the same row through
// gitCredentialFactForRepos instead and never refuses.
//
// It runs AFTER admission (a repo not admitted at all never reaches this),
// so it only ever needs to resolve the row that already admitted the repo,
// never re-derive admission itself.
func (s *Server) gitCredentialRefusal(w http.ResponseWriter, r *http.Request, repos ...string) bool {
	err := s.gitCredentialRefusalForLauncher(r.Context(), oidcHumanFromContext(r.Context()), repos...)
	var gcErr *gitCredentialRefusalError
	if !errors.As(err, &gcErr) {
		return false
	}
	// Not audited under authz.denied: that action's `reason` is a documented
	// CLOSED enum (docs/OPERATIONS.md), and this create-time 422 follows
	// writeLLMRefusal's own precedent (runs_dispatch_llm_mechanism.go) — the
	// sibling model_credential refusal at this same door is likewise
	// unaudited; only a run that actually DISPATCHES and then fails audits,
	// under run.create.
	writeJSON(w, http.StatusUnprocessableEntity, gitCredentialErrorBody{
		Error: gcErr.Sentence, Reason: gitCredentialRefusalReason, Org: gcErr.Org,
	})
	return true
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
