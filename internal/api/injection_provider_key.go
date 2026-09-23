// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// DRAFT (M2 canon pending)
const (
	providerKeyNotRecorded = "a model provider's key is injected only through the grant Wardyn authors when a run launches on " +
		"that provider, which records whose key the run uses; this grant carries no such record"
	providerKeyNotOwn = "a model provider's key is injected only from the run owner's own namespace; this grant names another"
	providerKeyAbsent = "your own key or token for this run's model provider is not stored — add it again from Getting " +
		"started in the console; no other person's credential stands in for it"
	providerKeyUnreadable = "Wardyn couldn't read your model provider credential just now"
)

// resolveProviderKeyInjection is the wardyn-provider-<uid>-key arm of
// handleInternalInjection: a person's own key or token for a key or endpoint
// model provider. handled=false means the grant names another secret.
//
// It resolves only a grant dispatch authored (authorProviderKeyInjection):
// the grant's snapshot must name the key it carries and the run's OWN subject
// — the run token, not the grant, is authority for who that is — and the key
// is read from that namespace strictly (ownSecret), never through the
// operator fallback the generic arm below takes. Every miss fails closed.
func (s *Server) resolveProviderKeyInjection(w http.ResponseWriter, r *http.Request,
	claims *identity.Claims, minted broker.Minted, grantID uuid.UUID,
) bool {
	name := minted.Injection.SecretName
	if !strings.HasPrefix(name, providerSecretPrefix) {
		return false
	}
	ctx := r.Context()
	fail := func(status int, reason, body string) bool {
		s.recordAudit(ctx, s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
			"secret.read", name, "failure", mustJSON(map[string]any{"reason": reason, "grant_id": grantID})))
		writeError(w, status, body)
		return true
	}
	var rec providerKeySnapshot
	if !s.grantSnapshot(ctx, claims.RunID, grantID, &rec) || rec.ProviderUID == "" ||
		name != providerSecretName(rec.ProviderUID, providerKeyPart) {
		return fail(http.StatusForbidden, "missing_scope_snapshot", providerKeyNotRecorded)
	}
	if rec.OwnerSubject == "" || rec.OwnerSubject != claims.Sub {
		return fail(http.StatusForbidden, "owner_mismatch", providerKeyNotOwn)
	}
	secret, found, err := s.ownSecret(ctx, claims.Sub, name)
	switch {
	case err != nil:
		return fail(http.StatusServiceUnavailable, "store_unreadable", providerKeyUnreadable)
	case !found || len(secret) == 0:
		return fail(http.StatusFailedDependency, "own_key_absent", providerKeyAbsent)
	}
	formatted := formatInjectionValue(minted.Injection.Format, secret)
	if s.cfg.MaskRegistry != nil {
		s.cfg.MaskRegistry.Add(claims.RunID, secret)
		if formatted != string(secret) {
			s.cfg.MaskRegistry.Add(claims.RunID, []byte(formatted))
		}
	}
	s.recordAudit(ctx, s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
		"secret.read", name, "success", mustJSON(map[string]any{
			"purpose": "proxy-injection", "grant_id": grantID, "jti": minted.JTI,
			"owner": claims.Sub, "provider_uid": rec.ProviderUID,
		})))
	writeJSON(w, http.StatusOK, injectionResponse{
		Host: minted.Injection.Host, Header: minted.Injection.Header, Value: formatted, JTI: minted.JTI,
	})
	return true
}
