// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// workloadVerifier is the part of the issuer verifier the exchange uses (internal/auth/oidc portal_workload.go).
// cfg.PortalIssuers holds *oidc.PortalIssuers, built at every boot; asserting the wider interface here leaves
// server.go and cmd/wardynd as they are. A server without one answers the exchange 503.
type workloadVerifier interface {
	VerifyWorkload(ctx context.Context, reg oidc.WorkloadRegistration, raw string) (oidc.WorkloadSubject, oidc.PortalRefusal)
}

func workloadRegistration(t types.InboundTrust) oidc.WorkloadRegistration {
	return oidc.WorkloadRegistration{Issuer: t.Issuer, InternalIssuer: t.InternalIssuer, Audience: t.Audience,
		Subject: t.Subject, RequiredClaim: t.RequiredClaim, RequiredClaimValue: t.RequiredClaimValue,
		MaxTokenAge: time.Duration(t.MaxTokenAgeSeconds) * time.Second}
}

// inboundExchangeState bounds the one anonymous inbound door, POST /inbound/exchange, per client address (peerKey).
// A successful exchange is refunded, so only refused attempts drain the bucket. Embedded in Server, as
// deviceRouteState is.
type inboundExchangeState struct {
	inboundExchangeLimiter principalLimiter
}

func newInboundExchangeState() inboundExchangeState {
	return inboundExchangeState{inboundExchangeLimiter: principalLimiter{rate: enrolRatePerSec, burst: enrolBurst, max: enrolLimiterMaxPeers}}
}

// maxInboundExchangeBody caps the exchange's body: a subject token and a lifetime.
const maxInboundExchangeBody = 16 << 10

type inboundExchangeRequest struct {
	SubjectToken string `json:"subject_token"`
	TTLSeconds   int64  `json:"ttl_seconds,omitempty"`
}

type inboundExchangeResponse struct {
	Token           string    `json:"token"`
	ExpiresAt       time.Time `json:"expires_at"`
	ServiceIdentity string    `json:"service_identity"`
}

// inboundIssuerUnavailableMsg answers an exchange whose trusted provider could not be checked.
const inboundIssuerUnavailableMsg = "The provider token could not be checked right now"

// mountInboundExchangeRoute registers the exchange: a provider token, read from the body, is the only credential.
func (s *Server) mountInboundExchangeRoute(r chi.Router) {
	r.Post("/inbound/exchange", s.handleInboundExchange)
}

// handleInboundExchange is POST /api/v1/inbound/exchange: an organization's own provider token, matched to one
// trust and verified against it, swapped for a short-lived inbound token of the trust's service identity. The
// trust, never the caller, names the identity. Every refused subject token gets one answer.
func (s *Server) handleInboundExchange(w http.ResponseWriter, r *http.Request) {
	now := s.cfg.Now().UTC()
	peer := peerKey(r.RemoteAddr)
	its, req, ok := s.inboundExchangeDoor(w, r, peer, now)
	if !ok {
		return
	}
	trust, subj, ok := s.inboundExchangeVerify(w, r, its, req)
	if !ok {
		return
	}
	si, issue, ok := s.inboundExchangeLateChecks(w, r, &trust, now)
	if !ok {
		return
	}
	life, clampedFrom := s.inboundExchangeLifetime(trust, req.TTLSeconds, subj.Expiry, now)
	expires := now.Add(life)
	row := types.APIToken{
		ID: uuid.New(), Principal: si.Principal(), Role: oidc.RoleUser, UserType: si.UserType, Groups: []string{},
		GroupsTruncated: new(bool), Name: "exchange:" + trust.Name, CreatedAt: now, ExpiresAt: &expires,
		RevSeq: issue.Seq, Kind: types.APITokenKindInbound,
	}
	plaintext := newBearer(apiTokenPrefix)
	// No admission cap on this context: the store caps an exchange per trust.
	created, err := its.CreateExchangedToken(r.Context(), trust, row, plaintext)
	if err != nil {
		s.inboundExchangeStoreRefusal(w, r, &trust, si, err)
		return
	}
	s.inboundExchangeLimiter.refund(peer, now)
	data := map[string]any{"trust": trust.ID.String(), "trust_name": trust.Name, "issuer": trust.Issuer,
		"subject": trust.Subject, "service_identity": si.Name, "sponsor": si.Sponsor, "expires_at": created.ExpiresAt}
	if clampedFrom > 0 {
		data["ttl_clamped_from_seconds"] = clampedFrom
	}
	// The context has no service request's sponsor stamp, so the sponsor is written in the data.
	ev := s.auditEvent(nil, types.ActorService, si.Principal(), "inbound.exchange", created.ID.String(), "success", mustJSON(data))
	ev.SourceIP = r.RemoteAddr
	s.recordAudit(r.Context(), ev)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, inboundExchangeResponse{Token: plaintext, ExpiresAt: *created.ExpiresAt, ServiceIdentity: si.Name})
}

// inboundExchangeDoor is the exchange's first checks, before anything is presented, so none writes a row: the
// per-address limiter before the store or the body is read, the switch, the body and the lifetime's bound.
func (s *Server) inboundExchangeDoor(w http.ResponseWriter, r *http.Request, peer string, now time.Time) (store.InboundTrustStore, inboundExchangeRequest, bool) {
	var req inboundExchangeRequest
	if !s.inboundExchangeLimiter.allow(peer, now) {
		w.Header().Set("Retry-After", "10")
		writeErrorReason(w, http.StatusTooManyRequests, reasonInboundExchangeRateLimited, "too many exchange attempts from this address; retry later")
		return nil, req, false
	}
	its, ok := inboundTrustStore(s)
	if !ok || s.cfg.LocalMode {
		writeErrorReason(w, http.StatusNotFound, reasonInboundFederationOff, inboundFederationOffMsg)
		return nil, req, false
	}
	f, err := its.GetInboundFederation(r.Context())
	if err != nil {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonTokenLookupUnavailable, "inbound federation lookup failed")
		return nil, req, false
	}
	if !f.Enabled {
		writeErrorReason(w, http.StatusNotFound, reasonInboundFederationOff, inboundFederationOffMsg)
		return nil, req, false
	}
	if err := types.DecodeOneJSON(http.MaxBytesReader(w, r.Body, maxInboundExchangeBody), &req, true); err != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidRequestBody, "invalid request body: "+err.Error())
		return nil, req, false
	}
	if req.TTLSeconds < 0 || req.TTLSeconds > apiTokenMaxTTLSeconds {
		writeErrorReason(w, http.StatusBadRequest, reasonAPITokenTTLInvalid, fmt.Sprintf("ttl_seconds: must be between 0 and %d", apiTokenMaxTTLSeconds))
		return nil, req, false
	}
	return its, req, true
}

// inboundExchangeVerify matches the presented token to one trust by its unverified issuer, audience and subject,
// and verifies it against that trust. Nothing is fetched for an issuer no trust names.
func (s *Server) inboundExchangeVerify(w http.ResponseWriter, r *http.Request, its store.InboundTrustStore, req inboundExchangeRequest) (types.InboundTrust, oidc.WorkloadSubject, bool) {
	if req.SubjectToken == "" {
		s.inboundExchangeDeny(w, r, nil, "subject_token_missing", nil)
		return types.InboundTrust{}, oidc.WorkloadSubject{}, false
	}
	iss, aud, sub, ok := oidc.WorkloadCandidate(req.SubjectToken)
	if !ok {
		s.inboundExchangeDeny(w, r, nil, "subject_token_shape", nil)
		return types.InboundTrust{}, oidc.WorkloadSubject{}, false
	}
	trust, err := its.MatchInboundTrust(r.Context(), iss, aud, sub)
	if errors.Is(err, store.ErrNotFound) {
		s.inboundExchangeDeny(w, r, nil, "no_trust", nil)
		return types.InboundTrust{}, oidc.WorkloadSubject{}, false
	}
	if err != nil {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonTokenLookupUnavailable, "inbound federation lookup failed")
		return types.InboundTrust{}, oidc.WorkloadSubject{}, false
	}
	// The own-address rule again, for a trust made before this deployment's address changed.
	if !s.ownAudience(trust.Audience) {
		s.inboundExchangeDeny(w, r, &trust, "audience_not_this_deployment", nil)
		return types.InboundTrust{}, oidc.WorkloadSubject{}, false
	}
	v, ok := s.cfg.PortalIssuers.(workloadVerifier)
	if !ok {
		w.Header().Set("Retry-After", "30")
		writeErrorReason(w, http.StatusServiceUnavailable, reasonInboundIssuerUnavailable, inboundIssuerUnavailableMsg)
		s.inboundExchangeRefused(r, &trust, "verifier_unavailable", nil)
		return types.InboundTrust{}, oidc.WorkloadSubject{}, false
	}
	subj, refusal := v.VerifyWorkload(r.Context(), workloadRegistration(trust), req.SubjectToken)
	if refusal.Class == oidc.PortalRefusedUnavailable {
		// An accepted residual: during an outage a caller learns a trust exists for this exact triple.
		retry := int(math.Ceil(refusal.RetryAfter.Seconds()))
		if retry <= 0 {
			retry = 30
		}
		w.Header().Set("Retry-After", strconv.Itoa(retry))
		writeErrorReason(w, http.StatusServiceUnavailable, reasonInboundIssuerUnavailable, inboundIssuerUnavailableMsg)
		s.inboundExchangeRefused(r, &trust, "issuer_unavailable", map[string]any{"cause": refusal.Cause})
		return types.InboundTrust{}, oidc.WorkloadSubject{}, false
	}
	if !refusal.OK() {
		s.inboundExchangeDeny(w, r, &trust, refusal.Reason, nil)
		return types.InboundTrust{}, oidc.WorkloadSubject{}, false
	}
	return trust, subj, true
}

// inboundExchangeLateChecks reads the trust's identity and the issue the token records, then checks the identity
// is active and the sponsor has not been revoked since the trust was last confirmed. The issue is read first, as
// mintSessionLive does: a revoke of the sponsor that commits before it is seen here, and one after it draws a
// greater sequence than the token records, so the token is refused at its first use.
func (s *Server) inboundExchangeLateChecks(w http.ResponseWriter, r *http.Request, trust *types.InboundTrust, now time.Time) (types.ServiceIdentity, oidc.Issue, bool) {
	unavailable := func(extra map[string]any) (types.ServiceIdentity, oidc.Issue, bool) {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonTokenLookupUnavailable, "inbound federation lookup failed")
		s.inboundExchangeRefused(r, trust, "store_unavailable", extra)
		return types.ServiceIdentity{}, oidc.Issue{}, false
	}
	ss, ok := s.cfg.Store.(store.ServiceIdentityStore)
	if !ok {
		return unavailable(nil)
	}
	si, err := ss.GetServiceIdentity(r.Context(), trust.ServiceIdentity)
	if err != nil {
		return unavailable(nil)
	}
	sponsor := map[string]any{"sponsor": si.Sponsor}
	if s.cfg.SessionRevocations == nil {
		writeErrorReason(w, http.StatusConflict, reasonServiceIdentityInactive, "this deployment cannot check the sponsor's session, so an exchanged token would never authenticate")
		s.inboundExchangeRefused(r, trust, "identity_inactive", sponsor)
		return types.ServiceIdentity{}, oidc.Issue{}, false
	}
	// The anonymous request speaks for nobody, so this is the current sequence.
	issue, err := s.mintIssue(r.Context(), si.Principal(), now)
	if err != nil {
		return unavailable(sponsor)
	}
	if err := s.withServiceState(r, &si); err != nil {
		return unavailable(sponsor)
	}
	if si.State != types.ServiceIdentityActive {
		writeErrorReason(w, http.StatusConflict, reasonServiceIdentityInactive, "this service identity cannot take calls now")
		s.inboundExchangeRefused(r, trust, "identity_inactive", sponsor)
		return types.ServiceIdentity{}, oidc.Issue{}, false
	}
	// Epoch -1 leaves a sign-out out, as for the sponsor's own tokens.
	status, err := oidc.CheckSession(r.Context(), s.cfg.SessionRevocations, si.Sponsor, "",
		oidc.Issue{At: trust.ConfirmedAt, Seq: trust.ConfirmedSeq}, -1)
	if err != nil {
		return unavailable(sponsor)
	}
	if status != oidc.SessionLive {
		writeErrorReason(w, http.StatusConflict, reasonInboundTrustUnconfirmed, inboundTrustUnconfirmedMsg)
		s.inboundExchangeRefused(r, trust, "trust_unconfirmed", sponsor)
		return types.ServiceIdentity{}, oidc.Issue{}, false
	}
	return si, issue, true
}

// inboundExchangeLifetime is the exchanged token's lifetime: the trust's, lowered to the deployment's cap, lowered
// to the one asked for, and never past the provider token's exp. clampedFrom is the lifetime asked for, in seconds,
// when it was above the limit; the cap at exp is not a clamp of the request.
func (s *Server) inboundExchangeLifetime(trust types.InboundTrust, requested int64, exp, now time.Time) (life time.Duration, clampedFrom int64) {
	limit := time.Duration(trust.TokenTTLSeconds) * time.Second
	if c := s.cfg.APITokenMaxTTL; c > 0 && c < limit {
		limit = c
	}
	limitSeconds := int64(limit / time.Second)
	life = limit
	if requested > 0 && requested < limitSeconds {
		life = time.Duration(requested) * time.Second
	}
	if left := exp.Sub(now); left < life {
		life = left
	}
	if requested > limitSeconds {
		clampedFrom = requested
	}
	return life, clampedFrom
}

// inboundExchangeStoreRefusal answers the store's refusal of the mint.
func (s *Server) inboundExchangeStoreRefusal(w http.ResponseWriter, r *http.Request, trust *types.InboundTrust, si types.ServiceIdentity, err error) {
	sponsor := map[string]any{"sponsor": si.Sponsor}
	switch {
	case errors.Is(err, store.ErrInboundFederationOff):
		writeErrorReason(w, http.StatusNotFound, reasonInboundFederationOff, inboundFederationOffMsg)
		s.inboundExchangeRefused(r, trust, "federation_off", sponsor)
	case errors.Is(err, store.ErrNotFound):
		s.inboundExchangeDeny(w, r, trust, "trust_gone", sponsor)
	case errors.Is(err, store.ErrServiceIdentityInactive):
		writeErrorReason(w, http.StatusConflict, reasonServiceIdentityInactive, "this service identity cannot take calls now")
		s.inboundExchangeRefused(r, trust, "identity_inactive", sponsor)
	case errors.Is(err, store.ErrInboundTrustCap):
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonAPITokenCapReached, "this trust holds its most live exchanged tokens; retry once one expires")
		s.inboundExchangeRefused(r, trust, "trust_cap", sponsor)
	default:
		writeErrorReason(w, http.StatusServiceUnavailable, reasonTokenLookupUnavailable, "inbound federation lookup failed")
		s.inboundExchangeRefused(r, trust, "store_unavailable", sponsor)
	}
}

// inboundExchangeDeny writes the one answer every refused subject token gets, and its row.
func (s *Server) inboundExchangeDeny(w http.ResponseWriter, r *http.Request, trust *types.InboundTrust, word string, extra map[string]any) {
	writeErrorReason(w, http.StatusUnauthorized, reasonInboundSubjectTokenInvalid, inboundSubjectTokenInvalidMsg)
	s.inboundExchangeRefused(r, trust, word, extra)
}

// inboundExchangeRefused is the only writer of a refused exchange's audit row, within authFailedAllowed's bound.
// The row names the matched trust from its own row, never an unverified claim, and carries the caller's address.
func (s *Server) inboundExchangeRefused(r *http.Request, trust *types.InboundTrust, word string, extra map[string]any) {
	if !s.authFailedAllowed() {
		return
	}
	data := map[string]any{"reason": word}
	target := ""
	if trust != nil {
		target = trust.ID.String()
		data["trust"], data["trust_name"], data["issuer"] = target, trust.Name, trust.Issuer
		data["subject"], data["service_identity"] = trust.Subject, trust.ServiceIdentity
	}
	for k, v := range extra {
		data[k] = v
	}
	ev := s.auditEvent(nil, types.ActorSystem, "inbound-exchange", "inbound.exchange", target, "denied", mustJSON(data))
	ev.SourceIP = r.RemoteAddr
	s.recordAudit(r.Context(), ev)
}
