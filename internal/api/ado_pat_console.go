// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// What the per-person Azure DevOps token console reads (#1428): the facts
// /me/scm-access adds to a row, a person's disconnect, and the tokens a run
// held. None of it carries a token value or an authorization id.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// adoPATAuditDisconnect is a person disconnecting their Azure DevOps sign-in.
const adoPATAuditDisconnect = "ado_pat.disconnect"

// ADOPATAccess is what /me/scm-access adds to a per-user row for the token
// console (the TS ADOPATAccess mirror). SCMAccess embeds it, so its fields sit
// beside SCMAccess's own on the wire. token_mode is SCMAccess's own field: a
// minted row reads "minted_pat" there, as an own-token row reads "own_pat".
type ADOPATAccess struct {
	// LastToken is the newest token Wardyn created for this person on this
	// row (minted_pat only), for the card's "last token" line.
	LastToken *adoLastToken `json:"last_token,omitempty"`
	// DefaultProfile is what a run on the row gets when its policy names no
	// Azure DevOps access: the row's default_profile, or
	// adoscope.ProfileDefault when it names none.
	DefaultProfile []adoscope.Capability `json:"default_profile,omitempty"`
}

// adoLastToken is when a person's newest token was created and, once its
// record is closed, when that was.
type adoLastToken struct {
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// adoPATAccessFor fills the token console's facts on out, the row's SCMAccess:
// the default profile always, and for a minted row token_mode "minted_pat" and
// the last token. The last token is a display line, so a record that could not
// be read leaves it out rather than failing the whole answer.
func (s *Server) adoPATAccessFor(ctx context.Context, pr perUserADORow, subject string, minted bool, out *SCMAccess) {
	out.DefaultProfile = pr.row.Entra.Profile()
	if !minted {
		return
	}
	out.TokenMode = string(types.ADOTokenModeMintedPAT)
	st, ok := s.cfg.Store.(store.RunPATReader)
	if !ok || subject == "" {
		return
	}
	last, found, err := st.LastRunPAT(ctx, subject, pr.row.ID)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: reading a person's last Azure DevOps token failed; the line is left out",
			slog.Any("err", err))
		return
	}
	if found {
		out.LastToken = &adoLastToken{CreatedAt: last.CreatedAt, RevokedAt: last.RevokedAt}
	}
}

// handleADODisconnect is DELETE /scm/azure-devops/connection: the caller
// disconnects their own Azure DevOps sign-in. It revokes every live run token
// created in their name, forgets the stored sign-in, audits
// ado_pat.disconnect and answers 204, also when nothing was stored. A
// deployment with no per-user row, and a row the caller may not use, answer
// the same 404 (D-6).
//
// A sign-in captured at console login (source "org") is captured again at the
// person's next login (CaptureLoginGrant, ado_entra_login.go); disconnect
// does not stop that.
func (s *Server) handleADODisconnect(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	subject := oidcHumanFromContext(ctx)
	if subject == "" {
		writeErrorReason(w, http.StatusForbidden, reasonADOSignInNoSession, adoSignInNoSessionRefusal)
		return
	}
	rowID, ok, err := s.adoDisconnectRow(ctx)
	if err != nil {
		writeServerError(w, r, "read the Azure DevOps sign-in configuration", err)
		return
	}
	if !ok {
		writeErrorReason(w, http.StatusNotFound, reasonADOSignInUnconfigured, adoSignInUnconfiguredRefusal)
		return
	}
	// Revoke first: revoking a token redeems the sign-in this then forgets.
	// A token created while this runs revokes itself (adoSignInEnds).
	// end runs even on a panic, or the person's mints would self-revoke until restart.
	removed, err := func() (bool, error) {
		defer s.adoSignInEnds.begin(subject, adoPATRevokeDisconnect)()
		s.revokeOwnerRunPATs(ctx, subject, adoPATRevokeDisconnect)
		return s.forgetADOSignIn(ctx, subject, rowID)
	}()
	data, outcome := map[string]any{"provider_row": rowID, "removed": removed}, "success"
	if err != nil {
		data["error"], outcome = err.Error(), "failure"
	}
	s.recordAudit(ctx, s.auditEvent(nil, types.ActorHuman, subject, adoPATAuditDisconnect, rowID, outcome, mustJSON(data)))
	if err != nil {
		writeServerError(w, r, "forget the Azure DevOps sign-in", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// adoDisconnectRow is the Entra sign-in row the caller may use; ok=false when
// there is none. An own-token row is never it: its stored token is deleted on
// its own route, and choosing it would forget the wrong secret.
func (s *Server) adoDisconnectRow(ctx context.Context) (string, bool, error) {
	if s.cfg.Store == nil {
		return "", false, nil
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return "", false, err
	}
	all, err := s.perUserADORows(ctx, sc)
	if err != nil {
		return "", false, err
	}
	all = slices.DeleteFunc(all, func(pr perUserADORow) bool { return isADOOwnTokenRow(pr.row) })
	rows := capVisible(ctx, s, capWorkspaceProvider, all, func(pr perUserADORow) string { return pr.row.ID })
	if len(rows) == 0 {
		return "", false, nil
	}
	return rows[0].row.ID, true, nil
}

// forgetADOSignIn deletes owner's stored sign-in for rowID and drops any
// access token held for them. removed reports whether one was stored.
func (s *Server) forgetADOSignIn(ctx context.Context, owner, rowID string) (removed bool, err error) {
	err = s.eraseADOSignIn(owner, rowID, func() error {
		if s.cfg.Secrets == nil {
			return nil
		}
		st, name := s.cfg.Secrets.For(owner), adoEntraSecretName(rowID)
		own, err := st.List(ctx)
		if err != nil || !slices.Contains(own, name) {
			return err
		}
		if err := st.Delete(ctx, name); err != nil && !errors.Is(err, secretstore.ErrNotFound) {
			return err
		}
		removed = true
		return nil
	})
	return removed, err
}

// eraseADOSignIn runs del, which deletes owner's stored Azure DevOps sign-in,
// under the redemption lock for rowID, and forgets the access tokens cached
// for owner before releasing it. A redemption in flight either finishes first
// (and del deletes its rotated refresh token, the forget its cached token) or
// starts after, reads the store under the lock and finds nothing. The lock is
// taken even when rowID is "" (no sign-in row configured): the key is then
// merely unshared, and every caller takes it in the same place in the order
// (see eraseLocked).
func (s *Server) eraseADOSignIn(owner, rowID string, del func() error) error {
	unlock := s.adoEntra.lock(owner, rowID)
	defer unlock()
	defer s.adoEntraTokens.forget(owner) // runs before the unlock
	return del()
}

// adoSignInRowID is the row this deployment's Azure DevOps sign-in redeems
// for: "" with no error when none is configured, and an error when the
// configuration cannot be read, which an erase must not proceed past.
func (s *Server) adoSignInRowID(ctx context.Context) (string, error) {
	if s.cfg.ADOEntra == nil {
		return "", nil
	}
	cfg, found, err := s.cfg.ADOEntra(ctx)
	if err != nil || !found {
		return "", err
	}
	return cfg.RowID, nil
}

// adoSignInEnds counts, per person, the disconnects and erases of their
// Azure DevOps sign-in. revokeOwnerRunPATs revokes the tokens recorded when it
// lists them; a mint that redeemed before the sign-in was deleted can record
// its token after that. mintRunPAT reads the count before it redeems and
// checks it after it records: a count that moved, or an end still running,
// means the token was created across an end, and the mint revokes it itself.
//
// In memory, per process: on a deployment with more than one replica a mint
// on another replica keeps this gap, bounded by the token's valid_to.
type adoSignInEnds struct {
	mu sync.Mutex
	m  map[string]*adoSignInEnd
}

// adoSignInEnd is one person's count: gen moves at each end's start and
// finish, active counts ends still running, reason is the last one's revoke
// reason. An entry is never removed, so a count read earlier is never
// mistaken for an unchanged one.
type adoSignInEnd struct {
	gen    uint64
	active int
	reason string
}

// begin marks the start of an end of owner's sign-in; the returned func marks
// its finish.
func (c *adoSignInEnds) begin(owner, reason string) func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]*adoSignInEnd{}
	}
	e := c.m[owner]
	if e == nil {
		e = &adoSignInEnd{}
		c.m[owner] = e
	}
	e.gen++
	e.active++
	e.reason = reason
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		e.gen++
		e.active--
	}
}

// read is owner's count now.
func (c *adoSignInEnds) read(owner string) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.m[owner]; e != nil {
		return e.gen
	}
	return 0
}

// since reports whether an end of owner's sign-in started, finished or is
// still running since read returned gen, and that end's revoke reason.
func (c *adoSignInEnds) since(owner string, gen uint64) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.m[owner]
	if e == nil || (e.gen == gen && e.active == 0) {
		return "", false
	}
	return e.reason, true
}

// revokeRunPATCreatedAcrossEnd revokes a token mintRunPAT recorded while its
// owner's sign-in was ending, with the access token the mint still holds (the
// stored sign-in may already be gone), and closes its row.
func (s *Server) revokeRunPATCreatedAcrossEnd(ctx context.Context, st store.RunPATStore, client adoPATClient,
	accessToken string, p store.RunPAT, reason string,
) {
	data := map[string]any{"reason": reason, "authorization_id": p.AuthorizationID, "owner": p.Owner,
		"provider_row": p.ProviderRowID, "organisation": p.Org}
	err := client.Revoke(ctx, p.Org, accessToken, p.AuthorizationID.String())
	var pe *adoPATError
	if err == nil || (errors.As(err, &pe) && pe.Status == http.StatusNotFound) {
		if s.closeRunPAT(ctx, st, p, reason, "") {
			s.recordAudit(ctx, s.auditEvent(&p.RunID, types.ActorSystem, "wardynd", adoPATAuditRevoke,
				p.AuthorizationID.String(), "success", mustJSON(data)))
		}
		return
	}
	// Nothing can redeem the sign-in again to retry: the token lives to its
	// valid_to, and its record says so.
	data["error"], data["abandoned"] = err.Error(), true
	s.recordAudit(ctx, s.auditEvent(&p.RunID, types.ActorSystem, "wardynd", adoPATAuditRevokeFailed,
		p.AuthorizationID.String(), "failure", mustJSON(data)))
	s.closeRunPAT(ctx, st, p, reason, err.Error())
}

// adoRunToken is one token a run held (GET /runs/{id}/ado-tokens): a row of
// ado_run_pats without its authorization id, owner or organisation.
type adoRunToken struct {
	CreatedAt time.Time `json:"created_at"`
	ValidTo   time.Time `json:"valid_to"`
	// Scope is the token's scope names as stored (ado_run_pats.scope split on
	// single spaces, e.g. ["vso.code","vso.project"]): what it could do, so a
	// reader can tell a token that a wider one replaced from a plain renewal.
	// Names only, never a token value.
	Scope     []string   `json:"scope"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	// RevokeReason is L2's revoke reason, as written: run_end, kill, pause,
	// drift, disconnect, offboarding, upstream_401, sweep, or expired (the
	// token reached valid_to; RevokedAt is then when its record was closed,
	// not a revoke).
	RevokeReason string `json:"revoke_reason,omitempty"`
	// RevokeFailed: the record was closed with a failed revoke on it, so the
	// token lives to ValidTo on its own.
	RevokeFailed bool `json:"revoke_failed,omitempty"`
}

// handleListRunADOTokens is GET /runs/{id}/ado-tokens: every token the run
// held, oldest first, [] for none. The owner or an admin, like GET /runs/{id},
// whose 404 everyone else gets (D-6).
func (s *Server) handleListRunADOTokens(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	run, ok := s.getRunAuthorized(w, r, id)
	if !ok {
		return
	}
	out := []adoRunToken{}
	st, ok := s.cfg.Store.(store.RunPATReader)
	if !ok {
		writeJSON(w, http.StatusOK, out)
		return
	}
	rows, err := st.ListRunPATs(r.Context(), run.ID)
	if err != nil {
		writeServerError(w, r, "read the run's Azure DevOps tokens", err)
		return
	}
	for _, p := range rows {
		out = append(out, adoRunToken{
			CreatedAt: p.CreatedAt, ValidTo: p.ValidTo, Scope: strings.Split(p.Scope, " "), RevokedAt: p.RevokedAt, RevokeReason: p.RevokeReason,
			RevokeFailed: p.RevokedAt != nil && p.LastError != "",
		})
	}
	writeJSON(w, http.StatusOK, out)
}
