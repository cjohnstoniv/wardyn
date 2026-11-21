// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// A service grant on a run's path: the create and dispatch check of a service identity's choice of a grant
// row, and the proxy's resolve. The grant stays the existing -oidc api_key grant; only its snapshot gains the
// grant's digest, and the resolve takes a minted token in place of redeeming a person's sign-in.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/subscription"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// providerServiceGrantRefusal is the liveness of a service grant row for the service identity owner. Not
// listed is refused at once. With refresh off (create, Review) it reads this replica's last recorded outcome
// and never mints: no row for the live digest, or a success, passes, and a definitive failure refuses until
// this replica's next attempt succeeds. With refresh on (dispatch) it mints, so a cold cache mints there and a
// first launch right after the row is configured needs no earlier mint.
func (s *Server) providerServiceGrantRefusal(ctx context.Context, p types.ModelProvider, owner string, refresh bool) (providerDenial, error) {
	if !serviceGrantListed(p.OIDC.ServiceGrant, owner) {
		d := stateDenial(p.ID, "it is not available to this service identity", "ask an administrator to list this service identity on its service grant.")
		d.notListed = true
		return d, nil
	}
	if refresh {
		if _, err := s.serviceGrantToken(ctx, p, "", serviceGrantTriggerDispatch); err != nil {
			return serviceGrantDenial(p, err)
		}
		return providerDenial{}, nil
	}
	reason, err := s.thisReplicaServiceGrantFailure(ctx, p)
	if err != nil {
		return providerDenial{}, err
	}
	if reason != "" {
		return providerDenial{msg: fmt.Sprintf(mpServiceGrantRefusal, reason), credential: true, serviceGrant: true}, nil
	}
	return providerDenial{}, nil
}

// serviceGrantDenial turns a failed mint into the refusal the create door and dispatch answer with.
func serviceGrantDenial(p types.ModelProvider, err error) (providerDenial, error) {
	var se *serviceGrantError
	switch {
	case errors.As(err, &se):
		return providerDenial{msg: fmt.Sprintf(mpServiceGrantRefusal, se.reason), credential: true, serviceGrant: true}, nil
	case errors.Is(err, errServiceGrantDrift):
		return stateDenial(p.ID, "it changed while this run was starting", mpRunRemedy), nil
	}
	return providerDenial{}, err
}

// serviceGrantSnapshotDigest is the digest a run's grant snapshot records: the grant's, when the run's owner
// is a service identity the row's grant lists, and "" for everyone else, so a person never takes this path.
func serviceGrantSnapshotDigest(p types.ModelProvider, owner string) string {
	if serviceGrantAppliesTo(p, owner) && serviceGrantListed(p.OIDC.ServiceGrant, owner) {
		return serviceGrantDigest(p)
	}
	return ""
}

// resolveServiceGrantFail is resolveEndpointOIDCInjection's refusal writer, which audits and answers.
type resolveServiceGrantFail func(status int, reason, body string, extra map[string]any) bool

// resolveServiceGrantInjection is the service-grant arm of the resolve: the owner must be a service identity
// the live row's grant still lists, the live grant's digest must be the snapshot's, then the token comes from
// serviceGrantToken. Before it answers it reads the settings again and repeats the checks, so a resolve that
// waited through an edit or a list removal returns no bearer made under the new settings.
func (s *Server) resolveServiceGrantInjection(w http.ResponseWriter, r *http.Request, claims *identity.Claims,
	minted broker.Minted, grantID uuid.UUID, rec providerGrantSnapshot, p types.ModelProvider, run types.AgentRun,
	lane providerKeyLane, fail resolveServiceGrantFail,
) bool {
	ctx := secretstore.WithPurpose(r.Context(), secretstore.PurposeProviderSignIn)
	if refusal := s.serviceGrantResolveRefusal(rec, p); refusal != nil {
		return fail(refusal.status, refusal.reason, refusal.msg, map[string]any{"drift": refusal.drift, "owner": rec.OwnerSubject})
	}
	tok, err := s.serviceGrantToken(ctx, p, rec.ServiceGrantDigest, serviceGrantTriggerResolve)
	if err != nil {
		return s.failServiceGrantResolve(err, rec, fail)
	}
	if again, ok := s.reReadServiceGrantRow(ctx, run, rec); !ok {
		return fail(http.StatusServiceUnavailable, reasonRosterUnreadable, endpointOIDCResolveUnreadable, nil)
	} else if refusal := s.serviceGrantResolveRefusal(rec, again); refusal != nil {
		return fail(refusal.status, refusal.reason, refusal.msg, map[string]any{"drift": refusal.drift, "owner": rec.OwnerSubject})
	}
	value := formatInjectionValue(lane.format, []byte(tok.token))
	if s.refuseUnmasked(w, r, claims, "injection.resolve", []byte(tok.token), []byte(value)) {
		return true
	}
	s.recordAudit(ctx, s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
		"secret.read", minted.Injection.SecretName, "success", mustJSON(map[string]any{
			"purpose": "proxy-injection-service-grant", "grant_id": grantID, "jti": minted.JTI, "source": "provider",
			"owner": rec.OwnerSubject, "provider": p.ID, "provider_uid": p.UID,
			"client_id": p.OIDC.ServiceGrant.ClientID, "expires_at": tok.expiresAt.UTC().Format(time.RFC3339),
		})))
	writeJSON(w, http.StatusOK, injectionResponse{
		Host: minted.Injection.Host, Header: lane.header, Value: value, JTI: minted.JTI,
		ExpiresAt:           s.subscriptionLease(minted, subscription.Token{ExpiresAt: tok.expiresAt}),
		CredentialExpiresAt: credentialExpiry(tok.expiresAt),
	})
	return true
}

type serviceGrantResolveRefused struct {
	status int
	reason string
	msg    string
	drift  string
}

// serviceGrantResolveRefusal repeats the owner and the grant checks against a row: the owner is a service
// identity still listed, and the row's grant digest is the snapshot's. nil passes.
func (s *Server) serviceGrantResolveRefusal(rec providerGrantSnapshot, p types.ModelProvider) *serviceGrantResolveRefused {
	switch {
	case !serviceGrantAppliesTo(p, rec.OwnerSubject) || !serviceGrantListed(p.OIDC.ServiceGrant, rec.OwnerSubject):
		return &serviceGrantResolveRefused{http.StatusForbidden, reasonServiceGrantNotListed, mpServiceGrantNotListed, "not_listed"}
	case serviceGrantDigest(p) != rec.ServiceGrantDigest:
		return &serviceGrantResolveRefused{http.StatusForbidden, reasonScopeChanged, endpointOIDCResolveChanged, "service_grant"}
	}
	return nil
}

// reReadServiceGrantRow reads the site configuration again and returns the run's provider for the snapshot's
// harness, after the same drift check the resolve began with. ok=false: it could not be read, or the row moved.
func (s *Server) reReadServiceGrantRow(ctx context.Context, run types.AgentRun, rec providerGrantSnapshot) (types.ModelProvider, bool) {
	siteCfg, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return types.ModelProvider{}, false
	}
	harness, providerID, launched, err := s.sinkHarnessProvider(ctx, run, rec.Harness)
	if err != nil {
		return types.ModelProvider{}, false
	}
	p, found := modelProviderByID(siteCfg.ModelProviders, providerID)
	if endpointOIDCDrift(rec, p, found && launched, harness) != "" {
		return types.ModelProvider{}, false
	}
	return p, true
}

// failServiceGrantResolve answers a failed mint: drift is 403 scope_changed, a transient failure is 503
// service_grant_unavailable, which the proxy rides out on its last good value, and a definitive one is 424
// service_grant_refused, after which the proxy drops the header at once.
func (s *Server) failServiceGrantResolve(err error, rec providerGrantSnapshot, fail resolveServiceGrantFail) bool {
	extra := map[string]any{"owner": rec.OwnerSubject}
	var se *serviceGrantError
	switch {
	case errors.Is(err, errServiceGrantDrift):
		extra["drift"] = "service_grant"
		return fail(http.StatusForbidden, reasonScopeChanged, endpointOIDCResolveChanged, extra)
	case errors.As(err, &se) && se.definitive:
		extra["failure"] = se.reason
		return fail(http.StatusFailedDependency, reasonServiceGrantRefused, fmt.Sprintf(mpServiceGrantRefusal, se.reason), extra)
	case errors.As(err, &se):
		extra["failure"] = se.reason
	}
	return fail(http.StatusServiceUnavailable, reasonServiceGrantUnavailable, "Wardyn could not get a token for this provider's service grant just now; it will be retried", extra)
}
