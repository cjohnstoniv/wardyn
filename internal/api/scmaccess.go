// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// scmaccess.go is modelaccess.go's sibling for Azure DevOps: whose credential
// clones a run, and what state it is in, for the calling person. It grades in
// modelaccess.go's six-value vocabulary (docs/design/ado-entra-prompt.md §1).
//
// Only per-user (Entra) rows are graded. A row is per-user when s.cfg.ADOEntra
// resolves a config for it; any other row, a shared PAT row included, gets no
// fact at all, so a working PAT row never reads as expired. The provider write
// boundary admits at most one enabled per-user row (validateOneEntraRow).
//
// Entra publishes no refresh-token expiry, so `expiring` is unreachable. A
// sign-in is `expired_signin` when a renewal recorded it dead (DeadAt) or it
// no longer covers the row's default profile; otherwise a valid stored sign-in
// is `live` and none is `not_configured`.

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
// Nothing in this file produces `live` with an EMPTY Source, but every reader
// must still treat that combination as "makes no per-person claim" (§7.5's
// ACCESS_SHARED_NOTE) rather than assume Source is always present.
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
	// NO ROW ID. workspace_admission.go's disclosure rule ("the kind, never
	// the row id") applies here as it does to admitSSHHostLevel's 201 warning;
	// Org is the one deliberate exception the frozen copy needs
	// (LAUNCH_DIALOG_BODY, CONNECT_DIALOG_BODY, PANEL_ORG interpolate {org}
	// verbatim). Org + Kind is the stable key over the /me/scm-access array.
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
// file doc comment for why.
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
// mismatch means this particular row is not the per-user one. A source that
// could not be read is an error, never "not per-user": that answer would wave
// the row through the launch gate ungraded.
func (s *Server) adoEntraRowConfig(ctx context.Context, rowID string) (ADOEntraConfig, bool, error) {
	if s.cfg.ADOEntra == nil {
		return ADOEntraConfig{}, false, nil
	}
	cfg, found, err := s.cfg.ADOEntra(ctx)
	if err != nil {
		return ADOEntraConfig{}, false, fmt.Errorf("read the azure devops sign-in configuration: %w", err)
	}
	if !found || cfg.RowID != rowID {
		return ADOEntraConfig{}, false, nil
	}
	if err := cfg.validate(); err != nil || !cfg.isLoginApplication() {
		return ADOEntraConfig{}, false, nil
	}
	return cfg, true, nil
}

// perUserADORow is an enabled Azure DevOps row together with the per-user
// config it resolved to, so no caller resolves the same row twice.
type perUserADORow struct {
	row types.GitProvider
	cfg ADOEntraConfig
}

// perUserADORows is every ENABLED azure_devops row this deployment's
// ADOEntra source covers — at most one (see the file doc comment).
func (s *Server) perUserADORows(ctx context.Context, sc types.SiteConfig) ([]perUserADORow, error) {
	var out []perUserADORow
	for _, row := range gitProviderRows(sc) {
		if row.Kind != types.GitProviderAzureDevOps || row.Disabled {
			continue
		}
		cfg, ok, err := s.adoEntraRowConfig(ctx, row.ID)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, perUserADORow{row, cfg})
		}
	}
	return out, nil
}

// perUserADORowsAdmitting is perUserADORows narrowed to the rows that ADMIT
// at least one of repos (admittingRows' own verdict) — the one row selection
// both gitCredentialRefusal (the gate) and gitCredentialFactForRepos
// (preflight's informational fact) use, so the two never disagree about
// which row is in play.
func (s *Server) perUserADORowsAdmitting(ctx context.Context, sc types.SiteConfig, repos []string) ([]perUserADORow, error) {
	var out []perUserADORow
	seen := map[string]bool{}
	for _, repo := range presentRepos(repos) {
		for _, row := range admittingRows(sc, repoCloneURL(repo)) {
			if row.Kind != types.GitProviderAzureDevOps || row.Disabled || seen[row.ID] {
				continue
			}
			cfg, ok, err := s.adoEntraRowConfig(ctx, row.ID)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue // a row this file cannot prove per-user carries no fact
			}
			seen[row.ID] = true
			out = append(out, perUserADORow{row, cfg})
		}
	}
	return out, nil
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
// (perUserADORows / perUserADORowsAdmitting), for subject. A secret store that
// could not be read is an error, never `not_configured`: grading an outage as
// "you are not connected" sends the person to reconnect a sign-in that is
// sitting there intact.
func (s *Server) scmAccessForRow(ctx context.Context, pr perUserADORow, subject string) (SCMAccess, error) {
	row := pr.row
	isMechanism := subject == ""
	var blob adoEntraBlob
	var found bool
	if !isMechanism {
		var err error
		if blob, found, err = s.readADOEntraBlob(ctx, subject, pr.cfg.RowID); err != nil {
			return SCMAccess{}, err
		}
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
	return out, nil
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

// computeSCMAccessRows is GET /me/scm-access's answer: one SCMAccess per
// per-user Azure DevOps row. Empty — never nil-vs-absent distinguished on the
// wire, see handleGetSCMAccess — when there is no such row, as on every
// deployment whose only Azure DevOps row is `shared`. A site config that
// could not be read is an error, never "no rows".
func (s *Server) computeSCMAccessRows(ctx context.Context, subject string) ([]SCMAccess, error) {
	if s.cfg.Store == nil {
		return nil, nil
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("read site config: %w", err)
	}
	return s.computeSCMAccessRowsFor(ctx, sc, subject)
}

// computeSCMAccessRowsFor is computeSCMAccessRows over an already-read site
// config.
func (s *Server) computeSCMAccessRowsFor(ctx context.Context, sc types.SiteConfig, subject string) ([]SCMAccess, error) {
	rows, err := s.perUserADORows(ctx, sc)
	if err != nil {
		return nil, err
	}
	out := make([]SCMAccess, 0, len(rows))
	for _, row := range rows {
		access, err := s.scmAccessForRow(ctx, row, subject)
		if err != nil {
			return nil, err
		}
		out = append(out, access)
	}
	return out, nil
}

// scmAccessValue is the FIRST row of computeSCMAccessRowsFor (at most one
// exists, see the file doc comment) — the shape SetupStatus.SCMAccess
// (setup.go) wants inline; it lives here because setup.go is at its size cap.
// The zero SCMAccess{} when there is none — setup.go's `omitzero` then drops
// the field — and when the rows could not be read: setup status is a summary,
// so an unreadable fact is left out rather than failing the whole page or
// being graded into a state it is not.
func (s *Server) scmAccessValue(ctx context.Context, sc types.SiteConfig, subject string) SCMAccess {
	rows, err := s.computeSCMAccessRowsFor(ctx, sc, subject)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: reading Azure DevOps access for setup status failed; the fact is left out",
			slog.Any("err", err))
		return SCMAccess{}
	}
	if len(rows) == 0 {
		return SCMAccess{}
	}
	return rows[0]
}

// gitCredentialFactForRepos is preflight's INFORMATIONAL, per-run
// git_credential fact — never a refusal, and never a
// deployment-wide guess: the per-user row (if any) that ADMITS one of repos,
// graded for subject, using the SAME row-selection perUserADORowsAdmitting
// gitCredentialRefusal itself uses, so a preflight fact and a launch refusal
// can never point at different rows. nil when no per-user row admits any of
// these repos — a GitHub-only run, a deployment with no Azure DevOps row at
// all, or one whose only Azure DevOps row is shared, all read identically:
// nothing to say. Also nil when the row or the sign-in could not be read: an
// informational fact is left out rather than graded from a failed read; the
// launch gate is what answers that read failure.
func (s *Server) gitCredentialFactForRepos(ctx context.Context, subject string, repos []string) *SCMAccess {
	if s.cfg.Store == nil {
		return nil
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil || !providersConfigured(sc) {
		return nil
	}
	rows, err := s.perUserADORowsAdmitting(ctx, sc, repos)
	if err != nil || len(rows) == 0 {
		return nil
	}
	out, err := s.scmAccessForRow(ctx, rows[0], subject)
	if err != nil {
		return nil
	}
	return &out
}

// gitCredentialErrorBody is the git_credential 422's body: the frozen
// sentence + reason, plus the org the LAUNCH DOOR names in its dialog before
// any preflight verdict exists — a 422 can be the very FIRST thing this
// caller hears about the row, so the dialog cannot depend on a
// `git_credential` preflight fact having already landed. No row id — see
// SCMAccess's own doc comment.
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
// including its "git_credential: " prefix (pinned by
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

// errGitCredentialRefused is gitCredentialRefusalForLauncher's sentinel —
// errRepoNotAdmitted's shape (workspace_admission.go), for the launchers
// that hold no ResponseWriter (record.go's launchRecordRun). A caller matches it with errors.As, on the concrete
// *gitCredentialRefusalError below, to recover the org for the 422 body.
var errGitCredentialRefused = errors.New(gitCredentialRefusalReason)

// gitCredentialRefusalError carries the one extra fact
// gitCredentialErrorBody needs (the org) through an error return — Unwrap
// makes it match errGitCredentialRefused for a caller that only wants to
// know WHICH refusal this is, errors.As for one that wants the org too.
type gitCredentialRefusalError struct{ Org, Sentence string }

func (e *gitCredentialRefusalError) Error() string { return e.Sentence }
func (e *gitCredentialRefusalError) Unwrap() error { return errGitCredentialRefused }

// errGitCredentialUnreadable is the gate's answer when the site config, the
// Azure DevOps sign-in configuration or the person's stored sign-in could not
// be read. It is a daemon fault, not a refusal the person can repair, so it
// is never graded into "you are not connected" and never admits the launch.
var errGitCredentialUnreadable = errors.New("git_credential: could not read the Azure DevOps sign-in state")

// gitCredentialUnreadableReason is the 503's `reason`, the same one the
// credential resolver answers for the identical read failure (injection_ado.go).
const gitCredentialUnreadableReason = "roster_unreadable"

// writeGitCredentialUnreadable answers errGitCredentialUnreadable at an HTTP
// door: 503 with adoResolveRosterUnreadable, the cause logged, never sent.
func writeGitCredentialUnreadable(w http.ResponseWriter, r *http.Request, err error) {
	slog.ErrorContext(r.Context(), "api: git_credential gate could not read the Azure DevOps sign-in state",
		slog.String("method", r.Method), slog.String("path", r.URL.Path), slog.Any("err", err))
	writeJSON(w, http.StatusServiceUnavailable, gitCredentialErrorBody{
		Error: adoResolveRosterUnreadable, Reason: gitCredentialUnreadableReason,
	})
}

// gitCredentialRefusalForLauncher is the git_credential launch GATE for a
// caller with no ResponseWriter — a SERVER-SIDE launcher
// (recordLaunchRefusals, called from launchRecordRun). nil when admitted; a *gitCredentialRefusalError
// (wrapped) when refused; errGitCredentialUnreadable (wrapped) when a read it
// needed failed — for the caller to map to its own door's 422 or 503 shape —
// record.go's own chain of errors.Is/errors.As mappings is the precedent
// (errWorkspaceSourceTarget, errAgentNotEnabled, ...).
//
// subject == "" (no OIDC human — an admin-token or local-mode caller) never
// refuses: no sign-in it could complete, the same rule the HTTP-facing
// gitCredentialRefusal follows for the identical reason.
func (s *Server) gitCredentialRefusalForLauncher(ctx context.Context, subject string, repos ...string) error {
	repos = presentRepos(repos)
	if len(repos) == 0 || s.cfg.Store == nil || subject == "" {
		return nil
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return fmt.Errorf("%w: read site config: %w", errGitCredentialUnreadable, err)
	}
	if !providersConfigured(sc) {
		return nil // legacy open mode: there are no rows to grade
	}
	rows, err := s.perUserADORowsAdmitting(ctx, sc, repos)
	if err != nil {
		return fmt.Errorf("%w: %w", errGitCredentialUnreadable, err)
	}
	for _, row := range rows {
		access, err := s.scmAccessForRow(ctx, row, subject)
		if err != nil {
			return fmt.Errorf("%w: %w", errGitCredentialUnreadable, err)
		}
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
// handleBuildWorkspace and handleScanWorkspace directly — but NEVER from preflight, which reads the same row through
// gitCredentialFactForRepos instead and never refuses.
//
// It runs AFTER admission (a repo not admitted at all never reaches this),
// so it only ever needs to resolve the row that already admitted the repo,
// never re-derive admission itself.
func (s *Server) gitCredentialRefusal(w http.ResponseWriter, r *http.Request, repos ...string) bool {
	err := s.gitCredentialRefusalForLauncher(r.Context(), oidcHumanFromContext(r.Context()), repos...)
	if errors.Is(err, errGitCredentialUnreadable) {
		writeGitCredentialUnreadable(w, r, err)
		return true
	}
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
// Azure DevOps access, one entry per per-user row.
// Member-safe by construction — scoped entirely to the caller's own OIDC
// subject, the same self-service shape /me/ssh-keys is (routes.go).
func (s *Server) handleGetSCMAccess(w http.ResponseWriter, r *http.Request) {
	subject := oidcHumanFromContext(r.Context())
	rows, err := s.computeSCMAccessRows(r.Context(), subject)
	if err != nil {
		writeServerError(w, r, "read Azure DevOps access", err)
		return
	}
	if rows == nil {
		rows = []SCMAccess{}
	}
	writeJSON(w, http.StatusOK, rows)
}
