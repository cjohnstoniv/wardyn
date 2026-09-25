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
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// handleErasePersonCredentials deletes every credential in one person's
// namespace: DELETE /people/{principal}/credentials, on the security tier.
// Removing reach is the tier's job, and it returns no credential material.
// The principal resolves as ?owner= does on the secrets routes. It never
// answers success with a credential left behind, and it never erases the
// operator namespace, which holds the platform keys.
func (s *Server) handleErasePersonCredentials(w http.ResponseWriter, r *http.Request) {
	raw := chi.URLParam(r, "principal")
	if r.URL.RawPath != "" {
		if u, err := url.PathUnescape(raw); err == nil {
			raw = u
		}
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		writeError(w, http.StatusBadRequest, "name the person whose credentials to erase")
		return
	}
	owner, known, refusal := s.resolveSecretOwner(r.Context(), raw)
	if refusal != "" {
		writeError(w, http.StatusUnprocessableEntity, refusal)
		return
	}
	rep, err := secretstore.EraseOwner(r.Context(), s.cfg.Secrets, owner)
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
			writeError(w, http.StatusBadRequest, "that names the operator namespace, which is not a person's")
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
// passed, audits each as credential.expired_deleted, and returns how many it
// deleted. cmd/wardynd calls it daily. A row it could not delete is kept, and
// the next sweep tries it again.
func (s *Server) SweepExpiredCredentials(ctx context.Context) int {
	sw, ok := s.cfg.Secrets.(expiredSweeper)
	if !ok {
		noSweepOnce.Do(func() {
			slog.ErrorContext(ctx, "wardynd: the secret store has no expiry sweep; expired stored credentials are NOT being deleted",
				slog.String("store", fmt.Sprintf("%T", s.cfg.Secrets)))
		})
		return 0
	}
	gone, err := sw.DeleteExpired(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: deleting expired credentials left some behind; the next sweep retries them", slog.Any("err", err))
	}
	for _, e := range gone {
		s.recordAudit(ctx, s.auditEvent(nil, types.ActorSystem, "wardynd", "credential.expired_deleted", e.Name, "success",
			withSecretOwner(map[string]any{"reason": "expired", "expires_at": e.ExpiresAt.UTC().Format(time.RFC3339)}, e.Owner, true)))
	}
	return len(gone)
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
	s.recordAudit(ctx, s.auditEvent(nil, types.ActorSystem, "wardynd", "credential.expired_deleted", name, outcome,
		withSecretOwner(data, owner, true)))
}
