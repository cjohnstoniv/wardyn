// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// CLI sessions: the wdc_ refresh secret a CLI sign-in hands the CLI, bound to its Ed25519 device key,
// renewing a 1-hour cli_access token. Every refresh rotates the secret and re-checks every cut; a
// refresh with a valid proof and a spent secret is a reuse and ends the session. Every session cut
// (a revoke, Sign out everywhere, the console's Sign out, SCIM) ends the person's CLI sessions in the
// cut's own transaction (store.ExecRevocationCut), so the refresh-time check is the second line.
package api

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// cliProofValid checks a device proof: pub signed types.CLIProofMessage for this route, id and secret,
// at a time within types.CLIProofSkew of now.
func (s *Server) cliProofValid(pub []byte, route string, id uuid.UUID, secret string, p types.CLIProof, now time.Time) bool {
	if len(pub) != ed25519.PublicKeySize || secret == "" {
		return false
	}
	at := time.Unix(p.Time, 0)
	if at.Before(now.Add(-types.CLIProofSkew)) || at.After(now.Add(types.CLIProofSkew)) {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(p.Signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(pub, types.CLIProofMessage(s.cfg.CLISignIn.Origin, route, id, secret, p.Time), sig)
}

// cliIdentityRequest is r carrying id's snapshot as the request's human, the way apiTokenAuth publishes
// a token's: prepareMint, the capability seam and the late revocation re-check then read it.
func cliIdentityRequest(r *http.Request, id store.CLIIdentity) *http.Request {
	ctx := withHumanIdentity(r.Context(), id.Principal, id.Email, id.Role, id.UserType, id.Groups,
		id.GroupsTruncated == nil || *id.GroupsTruncated)
	ctx = oidc.WithAuthorityEpoch(oidc.WithRevocationSeq(ctx, id.Principal, id.RevSeq), id.AuthorityEpoch)
	return r.WithContext(ctx)
}

// mintCLIToken is the CLI's one path into the mint core: prepareMint from id's snapshot (the
// per-person cap, the snapshot, the late revocation re-check), then the CLI kind and session. It
// returns the request that carries the snapshot, for the store write's identity guard.
func (s *Server) mintCLIToken(w http.ResponseWriter, r *http.Request, id store.CLIIdentity, authorizedAt time.Time,
	kind, machine string, session uuid.UUID, life time.Duration) (types.APIToken, *http.Request, bool) {
	r = cliIdentityRequest(r, id)
	name := "CLI: " + machine
	if kind == types.APITokenKindCLIAdmin {
		name = "CLI admin: " + machine
	}
	row, ok := s.prepareMint(w, r, id.Principal, authorizedAt, name, life)
	row.ID, row.Kind, row.CLISessionID = uuid.New(), kind, &session
	return row, r, ok
}

// cliGrantTimes fills a grant's session and idle ends for sess as of now.
func (s *Server) cliGrantTimes(g types.CLISessionGrant, sessionExpires, lastRefresh time.Time, st string) types.CLISessionGrant {
	g.SessionExpiresAt = sessionExpires
	g.IdleExpiresAt = minTime(lastRefresh.Add(s.cfg.CLISignIn.sessionIdle(st)), sessionExpires)
	g.Store = st
	return g
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// cliWriteFailed answers a store write of a CLI credential that failed: a deactivated owner is the
// generic refusal, anything else a 500.
func cliWriteFailed(w http.ResponseWriter, r *http.Request, err error, invalid func(http.ResponseWriter)) {
	if errors.Is(err, store.ErrIdentityDeactivated) || errors.Is(err, store.ErrNotFound) {
		invalid(w)
		return
	}
	writeServerError(w, r, "write cli session", err)
}

// createCLISession is the token call's ordinary arm: a new session and its first access token, both
// timed from the approval.
func (s *Server) createCLISession(w http.ResponseWriter, r *http.Request, si store.CLISignIn) {
	authorizedAt := *si.AuthorizedAt
	expires := authorizedAt.Add(s.cfg.CLISignIn.sessionTTL(si.Store))
	sessID := uuid.New()
	tok, mr, ok := s.mintCLIToken(w, r, si.Approver, authorizedAt, types.APITokenKindCLIAccess, si.Machine, sessID,
		min(s.cliAccessLife(), expires.Sub(authorizedAt)))
	if !ok {
		return
	}
	secret, raw := newBearer(types.CLISessionSecretPrefix), newBearer(apiTokenPrefix)
	row := store.CLISessionRow{ID: sessID, Identity: si.Approver, IdentityStampedAt: authorizedAt, Machine: si.Machine,
		Store: si.Store, PublicKey: si.PublicKey, CreatedAt: authorizedAt, ExpiresAt: expires}
	created, err := s.cliStore().CreateCLISession(guardOwner(mr.Context(), si.Approver.Principal), row, secret, tok, raw)
	if err != nil {
		cliWriteFailed(w, r, err, cliSignInInvalid)
		return
	}
	who := si.Approver.Principal
	ev := s.auditEvent(nil, types.ActorHuman, who, "cli.session.create", sessID.String(), "success",
		mustJSON(map[string]any{"machine": si.Machine, "store": si.Store, "sign_in": si.ID.String(), "expires_at": expires}))
	ev.SourceIP = r.RemoteAddr
	s.recordAudit(r.Context(), ev)
	s.auditCLITokenCreate(r, created)
	writeJSON(w, http.StatusCreated, s.cliGrantTimes(types.CLISessionGrant{SessionID: sessID, Secret: secret, AccessToken: raw,
		Kind: created.Kind, AccessExpiresAt: *tok.ExpiresAt}, expires, authorizedAt, si.Store))
}

// auditCLITokenCreate writes the token.create row of a CLI token.
func (s *Server) auditCLITokenCreate(r *http.Request, created types.APIToken) {
	detail := mintAuditDetail(created, 0, nil)
	detail["cli_session"] = created.CLISessionID.String()
	ev := s.auditEvent(nil, types.ActorHuman, created.Principal, "token.create", created.ID.String(), "success", mustJSON(detail))
	ev.SourceIP = r.RemoteAddr
	s.recordAudit(r.Context(), ev)
}

// cliSessionLive reports whether sess is one si's approver holds, with si's device key, and that has not
// ended or expired.
func (s *Server) cliSessionLive(sess store.CLISessionRow, principal string, key []byte, now time.Time) bool {
	return sess.EndedAt == nil && now.Before(sess.ExpiresAt) && sess.Identity.Principal == principal &&
		ed25519.PublicKey(sess.PublicKey).Equal(ed25519.PublicKey(key))
}

// mintCLIAdmin is the token call's --admin arm: a cli_admin token with the approver's real role, 1 hour,
// never renewed, belonging to the computer's live session so it ends with it.
func (s *Server) mintCLIAdmin(w http.ResponseWriter, r *http.Request, si store.CLISignIn) {
	sess, err := s.cliStore().GetCLISession(r.Context(), *si.SessionID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeServerError(w, r, "read cli session", err)
		return
	}
	authorizedAt := *si.AuthorizedAt
	if err != nil || !s.cliSessionLive(sess, si.Approver.Principal, si.PublicKey, s.cfg.Now()) {
		cliSignInInvalid(w)
		return
	}
	tok, mr, ok := s.mintCLIToken(w, r, si.Approver, authorizedAt, types.APITokenKindCLIAdmin, sess.Machine, sess.ID,
		min(s.cliAccessLife(), sess.ExpiresAt.Sub(authorizedAt)))
	if !ok {
		return
	}
	raw := newBearer(apiTokenPrefix)
	created, err := s.cliStore().CreateCLIAdminToken(guardOwner(mr.Context(), si.Approver.Principal), sess.ID, tok, raw)
	if err != nil {
		cliWriteFailed(w, r, err, cliSignInInvalid)
		return
	}
	s.auditCLITokenCreate(r, created)
	writeJSON(w, http.StatusCreated, s.cliGrantTimes(types.CLISessionGrant{SessionID: sess.ID, AccessToken: raw,
		Kind: created.Kind, AccessExpiresAt: *tok.ExpiresAt}, sess.ExpiresAt, sess.LastRefreshAt, sess.Store))
}

// cliSessionInvalid is the one 401 a refresh gives for an unknown, ended, expired, idle, cut or reused
// session and for a bad proof, so the route is not an oracle for which.
func cliSessionInvalid(w http.ResponseWriter) {
	writeErrorReason(w, http.StatusUnauthorized, reasonCLISessionInvalid, "this CLI sign-in has ended: run wardyn login again")
}

// auditRefreshRefused writes one cli.session.refresh_refused row.
func (s *Server) auditRefreshRefused(r *http.Request, sess store.CLISessionRow, reason string) {
	ev := s.auditEvent(nil, types.ActorHuman, sess.Identity.Principal, "cli.session.refresh_refused", sess.ID.String(), "denied",
		mustJSON(map[string]any{"reason": reason, "machine": sess.Machine, "address": requestAddress(r)}))
	ev.SourceIP = r.RemoteAddr
	s.recordAudit(r.Context(), ev)
}

// endCLISessionAt ends sess for reason, ignoring one that already ended.
func (s *Server) endCLISessionAt(r *http.Request, sess store.CLISessionRow, reason string, now time.Time) error {
	_, err := s.cliStore().EndCLISession(r.Context(), sess.ID, "", reason, now)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}

// cliSessionEnded is why sess can no longer refresh as of now on its own clock ("" when it can):
// ended already, past its absolute lifetime, or idle past its idle lifetime.
func (s *Server) cliSessionEnded(sess store.CLISessionRow, now time.Time) string {
	switch {
	case sess.EndedAt != nil:
		return "ended"
	case !now.Before(sess.ExpiresAt):
		return types.CLISessionEndExpired
	case !now.Before(sess.LastRefreshAt.Add(s.cfg.CLISignIn.sessionIdle(sess.Store))):
		return types.CLISessionEndIdle
	}
	return ""
}

// handleCLISessionRefresh is POST /api/v1/cli/sessions/{id}/refresh, anonymous: authenticated by the
// session secret and the device proof, rate-limited per client address and per session. The proof is
// checked first, so nothing about the session changes for a caller without the device key.
func (s *Server) handleCLISessionRefresh(w http.ResponseWriter, r *http.Request) {
	now := s.cfg.Now().UTC()
	id, ok := parseIDParam(w, r, "id", "session")
	if !ok {
		return
	}
	if !s.cliRefreshPeerLimiter.allow(peerKey(r.RemoteAddr), now) || !s.cliRefreshSessionLimiter.allow(id.String(), now) {
		w.Header().Set("Retry-After", "60")
		writeErrorReason(w, http.StatusTooManyRequests, reasonCLISignInRateLimited, "too many refreshes; retry later")
		return
	}
	var req types.CLISessionRefreshRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	sess, err := s.cliStore().GetCLISession(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		cliSessionInvalid(w)
		return
	}
	if err != nil {
		writeServerError(w, r, "read cli session", err)
		return
	}
	if !s.cliProofValid(sess.PublicKey, "/api/v1/cli/sessions/"+id.String()+"/refresh", id, req.Secret, req.Proof, now) {
		s.auditRefreshRefused(r, sess, "proof_invalid")
		cliSessionInvalid(w)
		return
	}
	if reason := s.cliSessionEnded(sess, now); reason != "" {
		s.refuseRefresh(w, r, sess, reason, now)
		return
	}
	if !sess.CLISecretMatches(req.Secret) {
		s.cliReuseDetected(w, r, sess, now)
		return
	}
	if !s.cliSessionCutsPass(w, r, sess, now) {
		return
	}
	s.rotateCLISession(w, r, sess, now)
}

// refuseRefresh ends sess for reason when that is an end reason it does not carry yet, writes the
// refusal row and answers the generic 401.
func (s *Server) refuseRefresh(w http.ResponseWriter, r *http.Request, sess store.CLISessionRow, reason string, now time.Time) {
	if sess.EndedAt == nil && reason != "ended" {
		if err := s.endCLISessionAt(r, sess, reason, now); err != nil {
			writeServerError(w, r, "end cli session", err)
			return
		}
	}
	s.auditRefreshRefused(r, sess, reason)
	cliSessionInvalid(w)
}

// cliReuseDetected ends sess, revoking its tokens, after a refresh with a valid device proof presented a
// secret that is not the current one, or lost the rotation race to one that did.
func (s *Server) cliReuseDetected(w http.ResponseWriter, r *http.Request, sess store.CLISessionRow, now time.Time) {
	if err := s.endCLISessionAt(r, sess, types.CLISessionEndReuseDetected, now); err != nil {
		writeServerError(w, r, "end cli session", err)
		return
	}
	ev := s.auditEvent(nil, types.ActorHuman, sess.Identity.Principal, "cli.session.reuse_detected", sess.ID.String(), "denied",
		mustJSON(map[string]any{"machine": sess.Machine, "address": requestAddress(r), "generation": sess.Generation}))
	ev.SourceIP = r.RemoteAddr
	s.recordAudit(r.Context(), ev)
	cliSessionInvalid(w)
}

// cliSessionCutsPass is the refresh's checks on the person, each refusing until it passes: the owner
// check with the session's issue and authority epoch (a revoke, deactivation, a sessions-only cut), the
// role stamp's age under WARDYN_ROLE_STAMP_TTL, and the api_token feature. A cut or deactivation ends
// the session; a stale stamp or a missing feature leaves it, since a browser sign-in or a grant fixes it.
func (s *Server) cliSessionCutsPass(w http.ResponseWriter, r *http.Request, sess store.CLISessionRow, now time.Time) bool {
	id := sess.Identity
	if rev := s.cfg.SessionRevocations; rev != nil {
		st, err := oidc.CheckSession(r.Context(), rev, id.Principal, id.Email, oidc.Issue{At: sess.CreatedAt, Seq: id.RevSeq}, id.AuthorityEpoch)
		if err != nil {
			writeErrorReason(w, http.StatusServiceUnavailable, reasonTokenLookupUnavailable, "cli session check failed")
			return false
		}
		if st != oidc.SessionLive {
			reason := types.CLISessionEndRevoked
			if st == oidc.SessionDeactivated {
				reason = types.CLISessionEndDeactivated
			}
			s.refuseRefresh(w, r, sess, reason, now)
			return false
		}
	}
	if s.cfg.RoleStampTTL > 0 && !sshRoleFresh(&sess.IdentityStampedAt, now, s.cfg.RoleStampTTL) {
		s.auditRefreshRefused(r, sess, string(authz.ReasonRoleStampStale))
		writeErrorReason(w, http.StatusUnauthorized, string(authz.ReasonRoleStampStale),
			"your role in Wardyn is out of date: sign in to the console in a browser, then retry")
		return false
	}
	if s.denyUserCapability(w, cliIdentityRequest(r, id), capFeature, featureAPIToken, "cli.sessions.refresh", apiTokenFeatureRefusal) {
		s.auditRefreshRefused(r, sess, string(authz.ReasonCapabilityFeature))
		return false
	}
	return true
}

// rotateCLISession is a refresh that passed: a new secret and a new access token, written together.
func (s *Server) rotateCLISession(w http.ResponseWriter, r *http.Request, sess store.CLISessionRow, now time.Time) {
	r = r.WithContext(withCLIRotation(r.Context(), sess.ID))
	tok, mr, ok := s.mintCLIToken(w, r, sess.Identity, now, types.APITokenKindCLIAccess, sess.Machine, sess.ID,
		min(s.cliAccessLife(), sess.ExpiresAt.Sub(now)))
	if !ok {
		return
	}
	secret, raw := newBearer(types.CLISessionSecretPrefix), newBearer(apiTokenPrefix)
	created, err := s.cliStore().RotateCLISession(guardOwner(mr.Context(), sess.Identity.Principal), sess.ID, sess.Generation, secret, now, tok, raw)
	if errors.Is(err, store.ErrConflict) {
		s.rotateConflict(w, r, sess.ID, now)
		return
	}
	if err != nil {
		cliWriteFailed(w, r, err, cliSessionInvalid)
		return
	}
	writeJSON(w, http.StatusOK, s.cliGrantTimes(types.CLISessionGrant{SessionID: sess.ID, Secret: secret, AccessToken: raw,
		Kind: created.Kind, AccessExpiresAt: *tok.ExpiresAt}, sess.ExpiresAt, now, sess.Store))
}

// rotateConflict answers a rotation whose compare-and-set lost: the session ended (or expired) since it
// was read, which is a refused refresh, or another refresh moved its secret on, which is a reuse.
func (s *Server) rotateConflict(w http.ResponseWriter, r *http.Request, id uuid.UUID, now time.Time) {
	sess, err := s.cliStore().GetCLISession(r.Context(), id)
	if err != nil {
		cliWriteFailed(w, r, err, cliSessionInvalid)
		return
	}
	if reason := s.cliSessionEnded(sess, now); reason != "" {
		s.refuseRefresh(w, r, sess, reason, now)
		return
	}
	s.cliReuseDetected(w, r, sess, now)
}

// handleListCLISessions is GET /api/v1/me/cli-sessions: the caller's live CLI sessions, never a secret.
func (s *Server) handleListCLISessions(w http.ResponseWriter, r *http.Request) {
	sub := oidcHumanFromContext(r.Context())
	if sub == "" {
		writeErrorReason(w, http.StatusForbidden, reasonAPITokenNoHuman, apiTokenNoHumanRefusal)
		return
	}
	now := s.cfg.Now().UTC()
	rows, err := s.cliStore().ListCLISessions(r.Context(), sub, now)
	if err != nil {
		writeServerError(w, r, "list cli sessions", err)
		return
	}
	out := []types.CLISession{}
	for _, row := range rows {
		if s.cliSessionEnded(row, now) != "" {
			continue
		}
		out = append(out, types.CLISession{ID: row.ID, Machine: row.Machine, Store: row.Store, CreatedAt: row.CreatedAt,
			LastRefreshAt: row.LastRefreshAt, ExpiresAt: row.ExpiresAt,
			IdleExpiresAt:  minTime(row.LastRefreshAt.Add(s.cfg.CLISignIn.sessionIdle(row.Store)), row.ExpiresAt),
			AdminExpiresAt: row.AdminExpiresAt})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleEndCLISession is DELETE /api/v1/me/cli-sessions/{id}: Sign out on one computer's row, and
// wardyn logout. Someone else's session is the same 404 as none.
func (s *Server) handleEndCLISession(w http.ResponseWriter, r *http.Request) {
	sub := oidcHumanFromContext(r.Context())
	if sub == "" {
		writeErrorReason(w, http.StatusForbidden, reasonAPITokenNoHuman, apiTokenNoHumanRefusal)
		return
	}
	id, ok := parseIDParam(w, r, "id", "session")
	if !ok {
		return
	}
	ended, err := s.cliStore().EndCLISession(r.Context(), id, sub, types.CLISessionEndSignedOut, s.cfg.Now().UTC())
	if errors.Is(err, store.ErrNotFound) {
		writeErrorReason(w, http.StatusNotFound, reasonCLISessionNotFound, "no such CLI sign-in")
		return
	}
	if err != nil {
		writeServerError(w, r, "end cli session", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r), "cli.session.revoke", id.String(), "success",
		mustJSON(map[string]any{"machine": ended.Machine, "reason": types.CLISessionEndSignedOut})))
	w.WriteHeader(http.StatusNoContent)
}

// liveCLISessions counts the live CLI sessions a cut naming name is about to end ("" = every one), for
// the cli_sessions_ended field of its session.revoke row; 0 when CLI sign-in is off or the count fails.
// The cut itself ends them (store.ExecRevocationCut), so a failed count costs only the field.
func (s *Server) liveCLISessions(ctx context.Context, name string) int {
	cs := s.cliStore()
	if cs == nil {
		return 0
	}
	n, err := cs.CountLiveCLISessions(ctx, name)
	if err != nil {
		slog.WarnContext(ctx, "api: count cli sessions for a session.revoke row failed", "error", err)
	}
	return n
}

type cliRotationKey struct{}

// withCLIRotation marks a mint as session's rotation: its new access token replaces the one the
// session holds (RotateCLISession revokes it), so the per-person cap does not count that one.
func withCLIRotation(ctx context.Context, session uuid.UUID) context.Context {
	return context.WithValue(ctx, cliRotationKey{}, session)
}

// replacedByRotation reports whether t is the access token the mint on ctx replaces.
func replacedByRotation(ctx context.Context, t types.APIToken) bool {
	session, ok := ctx.Value(cliRotationKey{}).(uuid.UUID)
	return ok && t.Kind == types.APITokenKindCLIAccess && t.CLISessionID != nil && *t.CLISessionID == session
}
