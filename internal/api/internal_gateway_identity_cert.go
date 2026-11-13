// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// A run's gateway client certificate: the run's proxy makes a key and asks here
// for a certificate over its public half, which it presents to a gateway on one
// of the run's identity routes that asks for one in the TLS handshake
// (internal/egress/proxy/gateway_client_cert.go). The certificate names the run
// alone, by its SPIFFE ID, the token's act.sub; which gateway a call is for is
// the token's audience, not the certificate's.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	// maxGatewayPublicKey bounds the public_key field: a P-256 key in PKIX DER is
	// 91 bytes, 124 in base64.
	maxGatewayPublicKey = 256
	// gatewayCertLifetime is the longest a client certificate lives, the cap on a
	// token's life in internal/gatewayidentity.
	gatewayCertLifetime = 10 * time.Minute
)

// handleGatewayClientCertificate signs the run's client certificate.
// internalAuth has refused a terminal, kept, queued or wrongly relayed run. A
// certificate is issued only while at least one of the run's sealed routes would
// get a token now (gatewayCertAdmit), and lives no longer than the shortest
// token among them.
func (s *Server) handleGatewayClientCertificate(w http.ResponseWriter, r *http.Request, claims *identity.Claims, publicKey string) {
	ctx := r.Context()
	record := func(outcome string, data map[string]any) {
		ev := s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID, "gateway_identity.certificate.mint", claims.SPIFFEID, outcome, mustJSON(data))
		ev.SourceIP = r.RemoteAddr
		s.recordAudit(ctx, ev)
	}
	refuse := func(ref *ownerRefusal) {
		record("denied", map[string]any{"reason": ref.reason})
		writeErrorReason(w, ref.status, ref.reason, ref.msg)
	}
	pub, err := parseGatewayPublicKey(publicKey)
	if err != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidRequestBody, err.Error())
		return
	}
	run, live := liveRunFromContext(ctx)
	if !live {
		refuse(gatewayUnavailable())
		return
	}
	lifetime, ref := s.gatewayCertAdmit(ctx, claims, run)
	if ref != nil {
		refuse(ref)
		return
	}
	cert, expires, err := s.issueGatewayCertificate(ctx, pub, claims.SPIFFEID, lifetime)
	if err != nil {
		slog.ErrorContext(ctx, "gateway identity: issue a client certificate", slog.String("run_id", claims.RunID.String()), slog.Any("err", err))
		refuse(gatewayUnavailable())
		return
	}
	exp := expires.UTC().Format(time.RFC3339)
	// One row for a run's first certificate on this replica, not one per renewal.
	if s.gatewayFirstMint(claims.RunID.String(), "cert\x00") {
		record("success", map[string]any{"expires_at": exp, "first": true})
	}
	writeJSON(w, http.StatusOK, map[string]string{"certificate": string(cert), "expires_at": exp})
}

// gatewayCertAdmit runs gatewayMintAdmit for each of the run's sealed routes and
// returns the shortest token lifetime among those admitted. A route changed since
// dispatch is passed over, as its own token would be refused; any other refusal
// (the person, the portal, a store fault) is the answer, since it holds for every
// route. No route admitted is gatewayChanged.
func (s *Server) gatewayCertAdmit(ctx context.Context, claims *identity.Claims, run types.AgentRun) (time.Duration, *ownerRefusal) {
	routes, err := s.sealedGatewayRoutes(ctx, run.ID)
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, errRunProxyConfigNotKept):
		return 0, gatewayChanged()
	case err != nil:
		slog.ErrorContext(ctx, "gateway identity: read the sealed route", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return 0, gatewayUnavailable()
	}
	lifetime := time.Duration(0)
	for _, rt := range routes {
		dest, _, ref := s.gatewayMintAdmit(ctx, claims, run, rt.ProviderID, rt.MCPKey)
		if ref != nil && ref.reason == reasonGatewayIdentityChanged {
			continue
		}
		if ref != nil {
			return 0, ref
		}
		life := dest.lifetime
		if life <= 0 || life > gatewayCertLifetime {
			life = gatewayCertLifetime
		}
		if lifetime == 0 || life < lifetime {
			lifetime = life
		}
	}
	if lifetime == 0 {
		return 0, gatewayChanged()
	}
	return lifetime, nil
}

// issueGatewayCertificate signs for an admitted run. The CA is read, never made
// here: the first enable makes it (refuseGatewayIdentityWrite), and boot makes it
// on an install whose signing key came first.
func (s *Server) issueGatewayCertificate(ctx context.Context, pub *ecdsa.PublicKey, spiffeID string, lifetime time.Duration) ([]byte, time.Time, error) {
	if s.cfg.GatewayClientCA == nil {
		return nil, time.Time{}, errors.New("no client CA is configured")
	}
	ca, err := s.cfg.GatewayClientCA(ctx, false)
	if err != nil {
		return nil, time.Time{}, err
	}
	if ca == nil {
		return nil, time.Time{}, errors.New("no client CA is stored")
	}
	return ca.Issue(pub, spiffeID, lifetime)
}

// parseGatewayPublicKey reads the public_key field: standard base64 of a PKIX
// P-256 public key.
func parseGatewayPublicKey(s string) (*ecdsa.PublicKey, error) {
	bad := errors.New("public_key must be a base64 PKIX P-256 public key")
	if len(s) > maxGatewayPublicKey {
		return nil, bad
	}
	der, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, bad
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, bad
	}
	pub, ok := parsed.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return nil, bad
	}
	return pub, nil
}
