// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// DRAFT (M2 canon pending)
const (
	providerKeyNotRecorded = "a model provider's key is injected only through the grant Wardyn authors when a run launches on " +
		"that provider, which records whose key the run uses; this grant carries no such record"
	providerKeyNotOwn = "a model provider's key is injected only from the run owner's own namespace; this grant names another"
	providerKeyAbsent = "your own key or token for this run's model provider is not stored — add it again from Getting " +
		"started in the console; no other person's credential stands in for it"
	providerKeyRefused = "Wardyn's store refused your model provider credential (it is gone, bound to another row, or " +
		"Wardyn's access to it was revoked). Nothing was substituted; save it again or ask an admin."
	providerKeyChanged = "this run's model provider was removed, turned off, re-pointed or changed after the run " +
		"started, so its key is no longer injected"
	providerKeyRecordUnreadable = "Wardyn couldn't read this run's model provider just now, so its key is not injected"
)

// providerKeyRecheck is how long a resolved provider key is good for. The
// proxy re-resolves 5 minutes before expiry (injectRefreshMargin), so a key is
// re-checked against the provider record about every 10 minutes while the run
// makes model calls; each re-check is one credential.mint and one secret.read
// audit row. A provider removed, turned off or re-pointed mid-run therefore
// fails closed within that window rather than keep its startup copy, and the
// sidecar's exact-host allowlist, fixed at dispatch, already refuses a new
// address in the meantime.
const providerKeyRecheck = 15 * time.Minute

// providerKeyDestination is where the record says p's key goes for agent, and
// how it rides: the key and endpoint arm's lane, or a Bedrock key provider's
// own data-plane host as a bearer. ok=false for any other kind — a
// wardyn-provider-<uid>-key name is never resolved for a provider with no key.
func providerKeyDestination(p types.ModelProvider, agent string) (host, header, format string, ok bool) {
	if p.Kind == types.ModelProviderBedrockBearer {
		return providerBedrockRuntimeHost(p), "Authorization", "Bearer %s", true
	}
	lane, ok := providerKeyLaneFor(p, agent)
	return lane.host, lane.header, lane.format, ok
}

// providerStoreReadRefusal is storeReadRefusal's split — 503 only when the
// store did not answer, so the proxy drops the header at once on a refusal —
// in the words of a person's own model provider credential.
func providerStoreReadRefusal(name string, err error) (status int, reason, body string) {
	status, _, _ = storeReadRefusal(name, err)
	switch {
	case status == http.StatusServiceUnavailable:
		return status, "store_unavailable", sinkStoreUnreachable
	case errors.Is(err, secretstore.ErrNotFound):
		return status, "own_key_absent", providerKeyAbsent
	default:
		return status, "store_refused", providerKeyRefused
	}
}

// resolveProviderKeyInjection is the wardyn-provider-<uid>-key arm of
// handleInternalInjection, for every kind a person brings a key or token to:
// the key and endpoint kinds and a Bedrock key (bedrock_bearer) share the one
// secret name, so they share this one sink — two sinks keyed by the same name
// would let the first refuse the other's grant. handled=false means the grant
// names another secret.
//
// It resolves only a grant dispatch authored (authorProviderKeyInjection, or
// authorBedrockBearerInjection on a provider run): the grant's snapshot must
// name the key it carries and the run's OWN subject — the run token, not the
// grant, is authority for who that is — and the key is read from that
// namespace strictly (ownSecret), never through the operator fallback the
// generic arm below takes.
//
// It trusts the provider record, never the grant, for where the key goes: the
// provider is re-read by UID on every resolve and must still be the run's
// choice, on, serving its agent and of a kind with a key; the host, header and
// format it derives must equal the grant's. Every miss fails closed.
func (s *Server) resolveProviderKeyInjection(w http.ResponseWriter, r *http.Request,
	claims *identity.Claims, minted broker.Minted, grantID uuid.UUID,
) bool {
	name := minted.Injection.SecretName
	if !strings.HasPrefix(name, providerSecretPrefix) {
		return false
	}
	ctx := r.Context()
	rctx, row := secretstore.SiteAudited(ctx)
	fail := func(status int, reason, body string) bool {
		s.recordAudit(ctx, s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
			"secret.read", name, "failure", mustJSON(withStoreRow(map[string]any{"reason": reason, "grant_id": grantID, "source": "provider"}, row))))
		writeError(w, status, body)
		return true
	}
	var rec providerGrantSnapshot
	if !s.grantSnapshot(ctx, claims.RunID, grantID, &rec) || rec.ProviderUID == "" ||
		name != providerSecretName(rec.ProviderUID, providerKeyPart) {
		return fail(http.StatusForbidden, "missing_scope_snapshot", providerKeyNotRecorded)
	}
	if rec.OwnerSubject == "" || rec.OwnerSubject != claims.Sub {
		return fail(http.StatusForbidden, "owner_mismatch", providerKeyNotOwn)
	}
	run, err := s.cfg.Store.GetRun(ctx, claims.RunID)
	if err != nil {
		return fail(http.StatusServiceUnavailable, "run_unreadable", providerKeyRecordUnreadable)
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return fail(http.StatusServiceUnavailable, "providers_unreadable", providerKeyRecordUnreadable)
	}
	p, found := modelProviderByID(sc.ModelProviders, run.ModelProviderID)
	if !found || p.UID != rec.ProviderUID || p.Disabled || !p.Serves(run.Agent) {
		return fail(http.StatusForbidden, "provider_changed", providerKeyChanged)
	}
	host, header, format, ok := providerKeyDestination(p, run.Agent)
	if !ok || !hostEqual(minted.Injection.Host, host) ||
		minted.Injection.Header != header || minted.Injection.Format != format {
		return fail(http.StatusForbidden, "provider_changed", providerKeyChanged)
	}
	secret, found, err := s.ownSecret(rctx, rec.OwnerSubject, name)
	switch {
	case err != nil:
		return fail(providerStoreReadRefusal(name, err))
	case !found || len(bytes.TrimSpace(secret)) == 0:
		return fail(http.StatusFailedDependency, "own_key_absent", providerKeyAbsent)
	}
	formatted := formatInjectionValue(format, secret)
	if s.cfg.MaskRegistry != nil {
		s.cfg.MaskRegistry.Add(claims.RunID, secret)
		if formatted != string(secret) {
			s.cfg.MaskRegistry.Add(claims.RunID, []byte(formatted))
		}
	}
	s.recordAudit(ctx, s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
		"secret.read", name, "success", mustJSON(withStoreRow(map[string]any{
			"purpose": "proxy-injection", "grant_id": grantID, "jti": minted.JTI, "source": "provider",
			"owner": rec.OwnerSubject, "provider": p.ID, "provider_uid": rec.ProviderUID,
		}, row))))
	writeJSON(w, http.StatusOK, injectionResponse{
		Host: host, Header: header, Value: formatted, JTI: minted.JTI,
		ExpiresAt: time.Now().Add(providerKeyRecheck).UnixMilli(),
	})
	return true
}
