// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"crypto/x509"
	"log/slog"
	"net/http"
	"time"

	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// InternalMTLS turns on the run-certificate check on the internal surface.
// nil (the default) leaves internalAuth exactly as it was.
type InternalMTLS struct {
	Bundles     x509bundle.Source // the SPIRE trust bundle source (svidsource.Holder)
	TrustDomain string            // this install's trust domain, as the identity provider has it
}

// refuseUnverifiedPeer is internalAuth's certificate half, run only after the bearer, the run's liveness and
// the relay binding have passed, so a certificate never admits on its own. With InternalMTLS set, a request
// that did not come through a runner's relay (refuseRelayBinding binds those to the relaying runner) must
// present a certificate that verifies against the SPIRE bundle, on this server's clock, and names the run its
// token names. It writes its own refusal and returns true once it has.
func (s *Server) refuseUnverifiedPeer(w http.ResponseWriter, r *http.Request, claims *identity.Claims) bool {
	m := s.cfg.InternalMTLS
	if m == nil {
		return false
	}
	if _, relayed := relayRunner(r.Context()); relayed {
		return false
	}
	now := s.cfg.Now()
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return s.denyPeer(w, r, http.StatusUnauthorized, reasonInternalPeerRequired, "this listener requires the run's certificate")
	}
	id, _, err := x509svid.Verify(r.TLS.PeerCertificates, m.Bundles, x509svid.WithTime(now))
	if err != nil {
		// The connection's leaf is judged on every request, so a fresh dial is the only way past this.
		w.Header().Set("Connection", "close")
		return s.denyPeer(w, r, http.StatusUnauthorized, reasonInternalPeerInvalid, "the run's certificate did not verify")
	}
	if _, err := identity.RunIDFromSPIFFEID(id.String(), m.TrustDomain); err != nil {
		s.warnPeer(r, "api: internal peer certificate is not a run's", id.String(), claims)
		return s.denyPeer(w, r, http.StatusForbidden, reasonInternalPeerNotARun, "the certificate is not a run's")
	}
	if id.String() != claims.SPIFFEID {
		s.warnPeer(r, "api: internal peer certificate is another run's", id.String(), claims)
		return s.denyPeer(w, r, http.StatusForbidden, reasonInternalPeerMismatch, "the certificate is another run's")
	}
	if peerNearExpiry(r.TLS.PeerCertificates[0], now) {
		// The proxy re-dials with its rotated certificate for the next request.
		w.Header().Set("Connection", "close")
	}
	return false
}

// proxyPeerID is the SPIFFE ID a run's proxy presents to the control plane as its client certificate: the
// run's own, only with WARDYN_INTERNAL_MTLS on and only for a sandbox this daemon's own Kubernetes runner
// creates (runOnKubernetes with no runner placing the run). Every other proxy gets "" and presents none.
func (s *Server) proxyPeerID(run types.AgentRun) string {
	if s.cfg.InternalMTLS == nil || run.RunnerID != nil || !s.runOnKubernetes(run) {
		return ""
	}
	return run.SPIFFEID
}

// denyPeer audits and writes one refusal of refuseUnverifiedPeer.
func (s *Server) denyPeer(w http.ResponseWriter, r *http.Request, status int, reason, msg string) bool {
	s.auditAuthFailedAs(r, internalAuthActor, reason)
	writeErrorReason(w, status, reason, msg)
	return true
}

// warnPeer logs a refused peer by its SPIFFE ID and the token's run, and nothing else of the certificate.
func (s *Server) warnPeer(r *http.Request, msg, peerID string, claims *identity.Claims) {
	slog.WarnContext(r.Context(), msg, slog.String("peer_spiffe_id", peerID), slog.String("run_id", claims.RunID.String()))
}

// peerExpiryWindow is how long before its expiry a leaf's connection is closed: a quarter of its lifetime,
// at least 15s and at most 10 minutes, and never more than half of the lifetime.
func peerExpiryWindow(leaf *x509.Certificate) time.Duration {
	ttl := leaf.NotAfter.Sub(leaf.NotBefore)
	return min(10*time.Minute, max(15*time.Second, ttl/4), ttl/2)
}

// peerNearExpiry reports whether leaf is inside its window at now. A leaf with no lifetime always is.
func peerNearExpiry(leaf *x509.Certificate, now time.Time) bool {
	return !leaf.NotAfter.After(leaf.NotBefore) || leaf.NotAfter.Sub(now) < peerExpiryWindow(leaf)
}
