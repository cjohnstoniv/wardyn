// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// A run's gateway client certificate. When a gateway on one of the run's
// identity routes asks for a client certificate during the TLS handshake, the
// proxy presents one wardynd signed for this run: an X.509 leaf whose one URI
// SAN is the run's SPIFFE ID. The key is made here and never leaves this
// process; wardynd sees its public half alone. A gateway that does not ask is
// sent no certificate, and the identity token is sent either way. When there is
// no usable certificate, or the gateway names only other CAs, the proxy sends
// none: a gateway that requires one refuses the connection, and a TLS front that
// only asks lets the call through as it did before this certificate existed.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
)

// gatewayCertMintTimeout bounds one certificate mint. It runs inside a TLS
// handshake, so it stays under tlsHandshakeTimeout.
const gatewayCertMintTimeout = 10 * time.Second

// newGatewayHolders makes a token holder per identity route and, when the run
// has any route, its one client-certificate holder.
func newGatewayHolders(run uuid.UUID, routes []GatewayIdentityRoute) (map[string]*gatewayTokenHolder, *gatewayCertHolder) {
	holders := map[string]*gatewayTokenHolder{}
	for _, rt := range routes {
		holders[rt.holderKey()] = &gatewayTokenHolder{providerID: rt.ProviderID, mcpKey: rt.MCPKey, audience: rt.Audience, timeout: 15 * time.Second}
	}
	if len(routes) == 0 {
		return holders, nil
	}
	return holders, &gatewayCertHolder{run: run.String()}
}

// offerGatewayClientCert makes cfg, one handshake's own config, present the
// run's client certificate if addr is one of the run's identity routes and the
// server asks for a certificate. crypto/tls calls GetClientCertificate only
// when the server sends a certificate request, so a gateway that does not ask
// costs no mint.
func (p *Proxy) offerGatewayClientCert(cfg *tls.Config, addr string) {
	if cfg == nil || p.gatewayCert == nil || !p.gatewayCertFor(addr) {
		return
	}
	cfg.GetClientCertificate = func(cri *tls.CertificateRequestInfo) (*tls.Certificate, error) {
		cert, err := p.gatewayCert.get(p)
		if err != nil || cri.SupportsCertificate(cert) != nil {
			return &tls.Certificate{}, nil // send none; never end the handshake here
		}
		return cert, nil
	}
}

// gatewayCertFor reports whether addr, a dial's host:port, is one of the run's
// identity routes. The path is not known at the handshake, so any route on the
// host and port counts.
func (p *Proxy) gatewayCertFor(addr string) bool {
	host, port := splitHostPort(addr, 443)
	return slices.ContainsFunc(p.gatewayRoutes, func(rt GatewayIdentityRoute) bool {
		return rt.Port == port && hostKey(rt.Host) == hostKey(host)
	})
}

// gatewayCertHolder is the run's current client certificate. Handshakes share
// it and take turns at a mint. A held certificate serves until half its life;
// from then on a handshake mints a new one, and if that fails the held one
// serves to its expiry. A failed mint with nothing usable held is asked again
// only after lastGoodRetry, so a sandbox retrying does not turn into a mint, and
// an audit row, per connection.
type gatewayCertHolder struct {
	run string // the run id the leaf must name

	mu      sync.Mutex
	cert    *tls.Certificate
	expires time.Time
	renewAt time.Time
	nextAsk time.Time
	lastErr error
}

// get returns the certificate to present, or why there is none; the caller
// then sends no certificate, and the proxy's log line names the cause.
// The mint runs on its own context: a handshake that is given up must not
// leave a failure the next one reads.
func (h *gatewayCertHolder) get(p *Proxy) (*tls.Certificate, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := p.now()
	held := h.cert != nil && now.Before(h.expires)
	if held && (now.Before(h.renewAt) || now.Before(h.nextAsk)) {
		return h.cert, nil
	}
	if !held && h.lastErr != nil && now.Before(h.nextAsk) {
		return nil, h.lastErr
	}
	ctx, cancel := context.WithTimeout(context.Background(), gatewayCertMintTimeout)
	defer cancel()
	cert, exp, drop, err := h.mint(ctx, p)
	now = p.now()
	if err != nil {
		slog.Warn("proxy: gateway client certificate mint failed", "err", err)
		if drop {
			h.cert = nil
		}
		h.lastErr, h.nextAsk = err, now.Add(lastGoodRetry)
		if h.cert != nil && now.Before(h.expires) {
			return h.cert, nil
		}
		return nil, err
	}
	h.cert, h.expires, h.renewAt, h.lastErr, h.nextAsk = cert, exp, now.Add(exp.Sub(now)/2), nil, time.Time{}
	return cert, nil
}

// mint makes a key, asks the control plane to certify its public half, and
// checks the answer. drop says the answer was definite, so a held certificate
// must go too.
func (h *gatewayCertHolder) mint(ctx context.Context, p *Proxy) (*tls.Certificate, time.Time, bool, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, time.Time{}, false, fmt.Errorf("gateway client certificate: generate key: %w", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, time.Time{}, false, fmt.Errorf("gateway client certificate: encode key: %w", err)
	}
	body, err := json.Marshal(map[string]string{"public_key": base64.StdEncoding.EncodeToString(der)})
	if err != nil {
		return nil, time.Time{}, false, err
	}
	resp, err := p.forwardToControlPlane(ctx, http.MethodPost, "/api/v1/internal/gateway-identity", body, "application/json")
	if err != nil {
		return nil, time.Time{}, false, fmt.Errorf("the control plane could not be reached for this run's gateway client certificate: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		return nil, time.Time{}, false, fmt.Errorf("the control plane answered %d for this run's gateway client certificate", resp.StatusCode)
	default:
		return nil, time.Time{}, true, fmt.Errorf("the control plane refused this run's gateway client certificate (HTTP %d); if the provider was changed or turned off, start a new run", resp.StatusCode)
	}
	var answer struct {
		Certificate string    `json:"certificate"`
		ExpiresAt   time.Time `json:"expires_at"`
	}
	if resp.Header.Get("Content-Encoding") != "" || json.NewDecoder(io.LimitReader(resp.Body, maxBrokeredBody)).Decode(&answer) != nil {
		return nil, time.Time{}, true, errors.New("the control plane's gateway client certificate answer could not be read")
	}
	leaf, exp, err := h.check(answer.Certificate, answer.ExpiresAt, &key.PublicKey, p.now())
	if err != nil {
		return nil, time.Time{}, true, err
	}
	return &tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: key, Leaf: leaf}, exp, false, nil
}

// check refuses a leaf this proxy would not itself accept as this run's: one
// for another key, one naming another run, or one already expired. Its life is
// judged against the control plane's expires_at, or its NotAfter if that is
// earlier, and held no longer than ten minutes, the cap on a token's life.
func (h *gatewayCertHolder) check(certPEM string, expiresAt time.Time, pub *ecdsa.PublicKey, now time.Time) (*x509.Certificate, time.Time, error) {
	unusable := errors.New("the control plane's gateway client certificate is malformed")
	block, rest := pem.Decode([]byte(certPEM))
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, time.Time{}, unusable
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, time.Time{}, unusable
	}
	if !pub.Equal(leaf.PublicKey) {
		return nil, time.Time{}, errors.New("the control plane's gateway client certificate is for another key than this proxy's")
	}
	if len(leaf.URIs) != 1 || leaf.URIs[0].Scheme != "spiffe" || !h.names(leaf.URIs[0].Path) {
		return nil, time.Time{}, errors.New("the control plane's gateway client certificate names another run")
	}
	exp := leaf.NotAfter
	if expiresAt.Before(exp) {
		exp = expiresAt
	}
	if !now.Before(exp) {
		return nil, time.Time{}, errors.New("the control plane's gateway client certificate has already expired by this proxy's clock")
	}
	if limit := now.Add(10 * time.Minute); exp.After(limit) {
		exp = limit
	}
	return leaf, exp, nil
}

// names reports whether path is a SPIFFE ID path of this holder's run, either form identity.RunPath
// writes: the run's own, or the run under the workload that launched it.
func (h *gatewayCertHolder) names(path string) bool {
	_, run, err := identity.ParseRunPath(path)
	return err == nil && run.String() == h.run
}
