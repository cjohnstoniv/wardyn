// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// A service grant's audit rows and its status: one row per (provider, replica) in provider_service_grant_status,
// which GET /model-providers and a service identity's GET /setup/status read, and which the create door reads
// to refuse a run the grant cannot serve. Nothing here holds a token, a secret or the identity provider's words.

import (
	"context"
	"log/slog"
	"net/url"
	"os"
	"slices"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	serviceGrantStateOK     = "ok"
	serviceGrantStateFailed = "failing"
	serviceGrantStateNotYet = "not_yet"

	auditServiceGrantMintAction = "model_provider.service_grant.mint"

	// serviceGrantStatusWriteBound bounds the detached status write of one attempt.
	serviceGrantStatusWriteBound = 5 * time.Second

	// mpServiceGrantRefusal is the create and dispatch refusal for a grant that cannot be minted.
	mpServiceGrantRefusal = "Wardyn could not get a token for this provider's service grant (%s); an administrator must check its client credentials"
	// mpServiceGrantNotListed is the sentence of a service identity the grant does not list.
	mpServiceGrantNotListed = "This provider's service grant is not available to this service identity; an operator lists the service identities that may use it"
)

// serviceGrantDefinitive reports whether a recorded reason is one a retry would repeat.
func serviceGrantDefinitive(reason string) bool {
	return reason != svcReasonUnavailable && reason != ""
}

// serviceGrantHost is this process's host name, for the status row's display column.
func serviceGrantHost() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown"
	}
	return h
}

// writeServiceGrantStatus records this replica's outcome for the row: tok non-nil is a success, otherwise e is
// the failure. A discarded mint writes nothing (the caller never gets here).
func (s *Server) writeServiceGrantStatus(ctx context.Context, p types.ModelProvider, digest string, at time.Time, tok *serviceGrantMinted, e *serviceGrantError) {
	gs, ok := s.serviceGrantStore()
	if !ok {
		return
	}
	row := store.ServiceGrantStatusRow{
		ProviderUID: p.UID, ReplicaID: s.svcGrant.replica(), ReplicaHost: serviceGrantHost(), GrantDigest: digest,
		ClientID: p.OIDC.ServiceGrant.ClientID, AttemptedAt: at.UTC(),
	}
	if tok != nil {
		row.Outcome, row.Egress = "ok", tok.egress
		okAt, until := at.UTC(), tok.expiresAt.UTC()
		row.LastOKAt, row.ValidUntil = &okAt, &until
	} else if e != nil {
		row.Outcome, row.Reason, row.OAuthError, row.HTTPStatus, row.Egress = "failed", e.reason, e.oauthError, e.httpStatus, e.egress
	}
	// Detached from the caller, and bounded: a status write that hangs must not hold the caller's lock or pass.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), serviceGrantStatusWriteBound)
	defer cancel()
	if err := gs.UpsertServiceGrantStatus(wctx, row); err != nil {
		slog.WarnContext(ctx, "wardynd: could not record a service grant's status", slog.String("provider", p.ID), slog.Any("err", err))
	}
}

// auditServiceGrantMint writes the row for one request sent: a success, a failure, or a success marked
// superseded when a rotation or an edit overtook it. It never carries the token, the secret,
// error_description or the response body.
func (s *Server) auditServiceGrantMint(ctx context.Context, p types.ModelProvider, trigger string, sent time.Time, tok *serviceGrantMinted, e *serviceGrantError, superseded bool) {
	g := p.OIDC.ServiceGrant
	host := ""
	if u, err := url.Parse(g.TokenURL); err == nil {
		host = u.Hostname()
	}
	data := map[string]any{
		"provider": p.ID, "provider_uid": p.UID, "client_id": g.ClientID, "token_host": host, "scope": g.Scope,
		"audience": g.Audience, "trigger": trigger, "replica": s.svcGrant.replica(),
	}
	outcome := "success"
	switch {
	case tok != nil:
		data["expires_at"] = tok.expiresAt.UTC().Format(time.RFC3339)
		data["egress"] = tok.egress
		if tok.scope != "" {
			data["scope"] = tok.scope
		}
		if superseded {
			data["superseded"] = true
		}
	case e != nil:
		outcome = "failure"
		data["reason"], data["oauth_error"], data["http_status"], data["egress"] = e.reason, e.oauthError, e.httpStatus, e.egress
		s.svcGrant.mu.Lock()
		if prev, ok := s.svcGrant.cache[p.UID]; ok && prev.expiresAt.After(sent) {
			data["previous_valid_until"] = prev.expiresAt.UTC().Format(time.RFC3339)
		}
		s.svcGrant.mu.Unlock()
	}
	s.recordAudit(context.WithoutCancel(ctx), s.auditEvent(nil, types.ActorSystem, serviceGrantActor,
		auditServiceGrantMintAction, p.ID, outcome, mustJSON(data)))
}

// thisReplicaServiceGrantFailure is this replica's latest recorded outcome for the row under its live digest:
// the failed row's reason, or "" when there is none or it succeeded. It reads the status table, never mints.
func (s *Server) thisReplicaServiceGrantFailure(ctx context.Context, p types.ModelProvider) (string, error) {
	gs, ok := s.serviceGrantStore()
	if !ok {
		return "", nil
	}
	rows, err := gs.ServiceGrantStatuses(ctx, []string{p.UID})
	if err != nil {
		return "", err
	}
	digest := serviceGrantDigest(p)
	for _, r := range rows {
		if r.ReplicaID == s.svcGrant.replica() && r.GrantDigest == digest && r.Outcome == "failed" && serviceGrantDefinitive(r.Reason) {
			return r.Reason, nil
		}
	}
	return "", nil
}

// serviceGrantStatuses builds GET /model-providers' service_grants: for each provider holding a grant, the
// client, the derived secret name and what every replica last saw. State reads the live replicas' rows under
// the live digest only.
func (s *Server) serviceGrantStatuses(ctx context.Context, block *types.ModelProviders) (map[string]types.ServiceGrantStatus, error) {
	if block == nil {
		return nil, nil
	}
	var uids []string
	for _, p := range block.Providers {
		if p.OIDC != nil && p.OIDC.ServiceGrant != nil {
			uids = append(uids, p.UID)
		}
	}
	if len(uids) == 0 {
		return nil, nil
	}
	var rows []store.ServiceGrantStatusRow
	if gs, ok := s.serviceGrantStore(); ok {
		var err error
		if rows, err = gs.ServiceGrantStatuses(ctx, uids); err != nil {
			return nil, err
		}
	}
	out := map[string]types.ServiceGrantStatus{}
	for _, p := range block.Providers {
		if p.OIDC == nil || p.OIDC.ServiceGrant == nil {
			continue
		}
		out[p.ID] = serviceGrantStatusOf(p, rows)
	}
	return out, nil
}

// serviceGrantStatusOf aggregates one provider's rows: failing if any live replica's latest attempt failed, ok
// if every live replica's succeeded however long ago, not_yet if none has attempted.
func serviceGrantStatusOf(p types.ModelProvider, rows []store.ServiceGrantStatusRow) types.ServiceGrantStatus {
	digest := serviceGrantDigest(p)
	st := types.ServiceGrantStatus{
		ClientID: p.OIDC.ServiceGrant.ClientID, ClientSecretRef: serviceGrantSecretName(p.UID),
		State: serviceGrantStateNotYet, Replicas: []types.ServiceGrantReplicaStatus{},
	}
	live, failing := 0, false
	for _, r := range rows {
		if r.ProviderUID != p.UID || r.GrantDigest != digest {
			continue
		}
		rs := types.ServiceGrantReplicaStatus{
			Host: r.ReplicaHost, Live: r.Live, State: serviceGrantStateOK, AttemptedAt: r.AttemptedAt,
			LastOKAt: r.LastOKAt, ValidUntil: r.ValidUntil, Reason: r.Reason, OAuthError: r.OAuthError,
			HTTPStatus: r.HTTPStatus, Egress: r.Egress,
		}
		if r.Outcome == "failed" {
			rs.State = serviceGrantStateFailed
		}
		st.Replicas = append(st.Replicas, rs)
		if r.Live {
			live++
			failing = failing || rs.State == serviceGrantStateFailed
		}
	}
	switch {
	case failing:
		st.State = serviceGrantStateFailed
	case live > 0:
		st.State = serviceGrantStateOK
	}
	return st
}

// gradeProviderServiceGrant is a service identity's /setup/status row on a service-grant row: live, or
// not_configured with cause service_grant_failing while the aggregate is failing.
func (s *Server) gradeProviderServiceGrant(ctx context.Context, row *SetupProviderAccess, p types.ModelProvider) {
	row.State = modelAccessLive
	gs, ok := s.serviceGrantStore()
	if !ok {
		return
	}
	rows, err := gs.ServiceGrantStatuses(ctx, []string{p.UID})
	if err != nil {
		unreadableProvider(row)
		return
	}
	if serviceGrantStatusOf(p, rows).State == serviceGrantStateFailed {
		row.State, row.Cause = modelAccessNotConfigured, reasonServiceGrantFailing
		row.Action = "An administrator must check this provider's client credentials"
	}
}

// serviceGrantAppliesTo reports whether owner is a service identity the row's grant lists.
func serviceGrantAppliesTo(p types.ModelProvider, owner string) bool {
	return p.OIDC != nil && p.OIDC.ServiceGrant != nil && isServicePrincipal(owner)
}

// sortedCleanupUIDs orders claim uids for a stable listing.
func sortedCleanupUIDs(m map[string]store.ServiceGrantCleanup) []string {
	uids := make([]string, 0, len(m))
	for uid := range m {
		uids = append(uids, uid)
	}
	slices.Sort(uids)
	return uids
}
