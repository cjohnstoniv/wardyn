// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// credential_erase.go is least retention for stored credentials
// (credential-storage design §2.5, §2.7, CS-5): an admin erases a person's
// credentials as one audited act, a sign-in the authority has refused for good
// is deleted at once, and one whose expiry has passed is deleted by the daily
// sweep. Each deletion removes the value from an external store before its row.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// eraseUnresolvedMsg and eraseAmbiguousMsg (design F-5 finding, packet F
// canon) are resolveSecretOwner's ?owner= refusals (secretOwnerUnresolvedMsg,
// secretOwnerAmbiguousMsg), reworded for THIS route: those two both open
// "?owner= names…", but the erase route takes no ?owner= — it names the
// person in the path. eraseRefusalMsg is the one place that reworks a
// refusal, mirroring resolveSSHKeyOwner's own translation (sshkeys_admin.go)
// for the same underlying resolvePrincipal answers.
const (
	eraseUnresolvedMsg = "That email address doesn't match anyone this deployment knows, so nothing was erased. " +
		"Use the person's subject, as the Audit log shows it."
	eraseAmbiguousMsg = "That matches more than one person, so nothing was erased. Use the person's subject exactly."
)

// eraseRefusalMsg reworks one of resolveSecretOwner's ?owner= refusals into
// this route's own wording (see the constants' comment).
func eraseRefusalMsg(refusal string) string {
	if refusal == secretOwnerAmbiguousMsg {
		return eraseAmbiguousMsg
	}
	return eraseUnresolvedMsg
}

// handleErasePersonCredentials deletes every credential in one person's
// namespace: DELETE /people/{principal}/credentials, on the security tier.
// Removing reach is the tier's job, and it returns no credential material.
// The principal resolves as ?owner= does on the secrets routes. It never
// answers success with a credential left behind, and it never erases the
// operator namespace, which holds the platform keys.
func (s *Server) handleErasePersonCredentials(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(principalParam(r))
	if raw == "" {
		s.auditOwnerRefusal(r, "credential.erase", "", "blank_principal")
		writeErrorReason(w, http.StatusBadRequest, reasonCredentialErasePrincipalRequired, "name the person whose credentials to erase")
		return
	}
	owner, known, refusal := s.resolveSecretOwner(r.Context(), raw)
	if refusal != "" {
		reason := ownerRefusalReason(refusal)
		s.auditOwnerRefusal(r, "credential.erase", raw, reason)
		writeErrorReason(w, http.StatusUnprocessableEntity, reason, eraseRefusalMsg(refusal))
		return
	}
	// Revoke the person's live Azure DevOps tokens first: the erase takes the
	// sign-in that revoking them needs.
	s.revokeOwnerRunPATs(r.Context(), owner, adoPATRevokeOffboarding)
	rep, err := secretstore.EraseOwner(r.Context(), s.cfg.Secrets, owner)
	// Even on a partial erase, which may have removed the sign-in.
	s.adoEntraTokens.forget(owner)
	data := map[string]any{"count": rep.Count}
	if rep.Store != "" {
		data["store"], data["purged"] = rep.Store, rep.Purged
		if !rep.Purged && rep.RecoverableDays > 0 {
			data["recoverable_days"] = rep.RecoverableDays
		}
	}
	resp := maps.Clone(data)
	if err != nil {
		s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
			"credential.erase", owner, "failure", withSecretOwner(data, owner, known)))
		if errors.Is(err, secretstore.ErrOperatorNamespace) {
			writeErrorReason(w, http.StatusBadRequest, reasonCredentialEraseOperatorNamespace, "that names the operator namespace, which is not a person's")
			return
		}
		writeServerError(w, r, "erase credentials", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"credential.erase", owner, "success", withSecretOwner(data, owner, known)))
	writeJSON(w, http.StatusOK, resp)
}

// expiredSweeper is the store that can delete the rows whose expiry has
// passed (secretstore/pg Store.DeleteExpired).
type expiredSweeper interface {
	DeleteExpired(ctx context.Context) ([]secretstore.Expired, error)
}

// noSweepOnce logs, once per process, that the configured store cannot sweep:
// least retention must never be off without a word.
var noSweepOnce sync.Once

// SweepExpiredCredentials deletes every stored credential whose expiry has
// passed, audits each as credential.expired.delete, and returns how many it
// deleted. cmd/wardynd calls it daily. A row it could not delete is kept, and
// the next sweep tries it again. A store that cannot sweep — no DeleteExpired,
// or a wrapper answering secretstore.ErrNoExpirySweep — is logged at Error once.
func (s *Server) SweepExpiredCredentials(ctx context.Context) int {
	if s.cfg.Secrets == nil {
		return 0 // no store, so nothing stored to expire
	}
	noSweep := func() int {
		noSweepOnce.Do(func() {
			slog.ErrorContext(ctx, "wardynd: the secret store has no expiry sweep; expired stored credentials are NOT being deleted",
				slog.String("store", fmt.Sprintf("%T", s.cfg.Secrets)), slog.String("store_name", s.cfg.Secrets.Name()))
		})
		return 0
	}
	sw, ok := s.cfg.Secrets.(expiredSweeper)
	if !ok {
		return noSweep()
	}
	gone, err := sw.DeleteExpired(ctx)
	if errors.Is(err, secretstore.ErrNoExpirySweep) {
		return noSweep()
	}
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: deleting expired credentials left some behind; the next sweep retries them", slog.Any("err", err))
		s.auditSweepFailure(ctx, err)
	}
	for _, e := range gone {
		s.adoEntraTokens.forget(e.Owner)
		s.recordAudit(ctx, s.auditEvent(nil, types.ActorSystem, "wardynd", "credential.expired.delete", e.Name, "success",
			withSecretOwner(map[string]any{"reason": "expired", "expires_at": e.ExpiresAt.UTC().Format(time.RFC3339)}, e.Owner, true)))
	}
	return len(gone)
}

// auditSweepFailure records what a sweep could not do: a failure row for each
// row it kept (secretstore.ExpiredKept), so a row the store refuses every day
// is on the record every day, and one row with no target for a sweep that
// failed as a whole (its scan).
func (s *Server) auditSweepFailure(ctx context.Context, err error) {
	errs := []error{err}
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		errs = j.Unwrap()
	}
	for _, e := range errs {
		var kept *secretstore.ExpiredKept
		if !errors.As(e, &kept) {
			s.recordAudit(ctx, s.auditEvent(nil, types.ActorSystem, "wardynd", "credential.expired.delete", "", "failure",
				mustJSON(map[string]any{"reason": "expired", "error": e.Error()})))
			continue
		}
		s.recordAudit(ctx, s.auditEvent(nil, types.ActorSystem, "wardynd", "credential.expired.delete", kept.Name, "failure",
			withSecretOwner(map[string]any{"reason": "expired", "error": kept.Err.Error()}, kept.Owner, true)))
	}
}

// deleteDeadCredential deletes a stored sign-in the authority has refused for
// good (invalid_grant and its siblings): nothing can renew it, so keeping it
// only keeps a secret. st is the owner's own view. Best-effort for the caller,
// whose answer is already a refusal; a failed delete is audited and the sign-in
// is left for the next refusal or the person's next capture to replace.
func (s *Server) deleteDeadCredential(ctx context.Context, st secretstore.Store, owner, name, provider string) {
	data := map[string]any{"reason": "invalid_grant", "provider": provider}
	outcome := "success"
	if err := st.Delete(ctx, name); err != nil {
		slog.WarnContext(ctx, "wardynd: deleting a sign-in the authority refused failed", slog.String("provider", provider), slog.Any("err", err))
		outcome, data["error"] = "failure", err.Error()
	}
	s.adoEntraTokens.forget(owner)
	s.recordAudit(ctx, s.auditEvent(nil, types.ActorSystem, "wardynd", "credential.expired.delete", name, outcome,
		withSecretOwner(data, owner, true)))
}
