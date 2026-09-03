// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/contentscan"
	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// maxLeafCerts bounds the per-host leaf-cert cache (one entry per LLM host;
// realistically <5). Past the cap, leaves are minted but not cached.
const maxLeafCerts = 256

// leafCertTTL is the validity window of a minted per-host leaf. Short-lived: the
// cert is regenerated whenever the proxy restarts.
const leafCertTTL = 24 * time.Hour

// certAuthority mints per-host leaf certificates signed by a Wardyn CA so the proxy
// can terminate TLS for a known LLM host and inspect the plaintext request inside
// an otherwise-opaque CONNECT tunnel (the subscription-OAuth path). The CA PRIVATE
// KEY lives ONLY here in proxy memory; only the CA's PUBLIC cert is trusted inside
// the sandbox. A nil *certAuthority means MITM is disabled (opaque passthrough).
type certAuthority struct {
	caCert *x509.Certificate
	caKey  crypto.Signer

	mu     sync.Mutex
	leaves map[string]*tls.Certificate
}

// newCertAuthority parses a CA cert+key (PEM) into a leaf minter.
func newCertAuthority(certPEM, keyPEM []byte) (*certAuthority, error) {
	kp, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("mitm: parse CA keypair: %w", err)
	}
	caCert, err := x509.ParseCertificate(kp.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("mitm: parse CA cert: %w", err)
	}
	if !caCert.IsCA {
		return nil, fmt.Errorf("mitm: configured certificate is not a CA")
	}
	signer, ok := kp.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("mitm: CA private key is not a crypto.Signer")
	}
	return &certAuthority{
		caCert: caCert,
		caKey:  signer,
		leaves: make(map[string]*tls.Certificate),
	}, nil
}

// leafFor returns (minting + caching on first use) a leaf certificate for host
// signed by the CA, so a sandbox that trusts the CA accepts the proxy's
// termination of TLS to host.
//
// host may be a LITERAL IP, and then the SAN has to be an IP SAN: crypto/x509's
// VerifyHostname (like OpenSSL/curl and Node) matches an IP target ONLY against
// IPAddresses, so a leaf carrying "10.40.2.11" as a DNSName is rejected with
// "doesn't contain any IP SANs". That is not a corner case -- a private-endpoint
// mirror named by an EgressRedirect To is a literal IP by construction and
// isMITMHost admits it for token injection (substituteArtifactEgress writes the
// address onto mitmHosts), so a DNS-only leaf left the token-injection lane for
// such a mirror dead on the data path while test-redirect -- which never
// MITMs -- still reported the redirect as reached.
func (a *certAuthority) leafFor(host string) (*tls.Certificate, error) {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	a.mu.Lock()
	if c, ok := a.leaves[host]; ok {
		a.mu.Unlock()
		return c, nil
	}
	a.mu.Unlock()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    now.Add(-1 * time.Hour),
		NotAfter:     now.Add(leafCertTTL),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	// Exactly one SAN kind, chosen by what the CONNECT host actually is: an IP
	// SAN for a literal (the only SAN a client verifying an IP target consults),
	// a DNS SAN otherwise. Never both -- a hostname is never a valid IP SAN, and
	// an IP spelled as a DNSName is the bug this branch exists to prevent.
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.caCert, &key.PublicKey, a.caKey)
	if err != nil {
		return nil, err
	}
	cert := &tls.Certificate{
		Certificate: [][]byte{der, a.caCert.Raw}, // leaf then CA
		PrivateKey:  key,
	}
	if leaf, perr := x509.ParseCertificate(der); perr == nil {
		cert.Leaf = leaf
	}
	a.mu.Lock()
	if len(a.leaves) < maxLeafCerts {
		a.leaves[host] = cert
	}
	a.mu.Unlock()
	return cert, nil
}

// isMITMHost reports whether host is an upstream the proxy will TLS-MITM.
//
// TRUST BOUNDARY (security-sensitive — read before widening further): TLS-MITM
// lets the proxy read the plaintext of an otherwise-opaque HTTPS request and swap
// its credential header. It is permitted for exactly two, tightly-scoped reasons:
//
//  1. the built-in LLM hosts (Anthropic/OpenAI) — the subscription-OAuth /
//     content-inspection path. Bedrock is excluded: it authenticates with
//     client-side SigV4 a terminate-and-reforward proxy would invalidate.
//  2. OPERATOR-CONFIGURED corp artifact hosts (p.mitmHosts, compiled at dispatch
//     from the site-config artifact overrides) — so the operator's OWN corporate
//     registry token can be injected on the wire and the sandbox never holds it.
//
// The expanded surface (#2) is bounded on every axis: the set is authored by an
// admin via the site-config API (NOT the sandbox, NOT the agent, NOT the run
// request — a prompt-injected agent cannot add a host); it is an EXACT-hostname
// allowlist, never a wildcard or suffix match — and, since W13-S1-5, exact on
// PORT too when the authored entry names one (mitmPorts; see handleConnect's
// corp-MITM branch) so a mirror configured at a non-443 port cannot have its
// tunnel matched by hostname alone and then dialed at the wrong port; a host
// only lands here when it also carries a paired injection rule for the
// operator's own token (config-only redirects never MITM); and the CA private
// key stays in proxy memory exactly as for the LLM path. What it costs: for
// those named hosts the proxy sees the plaintext of the sandbox's requests (as
// it already does for the LLM hosts) — the operator is trusting their own
// proxy with traffic to their own registry.
func (p *Proxy) isMITMHost(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	if h == anthropicHost || h == openaiHost {
		return true
	}
	return p.mitmHosts[h]
}

// mitmLLMHost reports whether a CONNECT to a BUILT-IN LLM host should be
// TLS-terminated: a CA is present AND MITM of the LLM hosts is actually intended
// for this run (p.mitmLLM — subscription credential injection or intercept_tls).
// The per-run CA may instead have been minted purely for artifact-token injection
// (mitmHosts), so "a CA exists" alone must NOT terminate a direct CONNECT to
// Anthropic/OpenAI: an artifact-only run leaves the LLM hosts opaque passthrough.
// (Corp artifact hosts are handled earlier via isCorpMITMHost, so isMITMHost here
// is effectively the LLM-host membership check.)
func (p *Proxy) mitmLLMHost(host string) bool {
	return p.ca != nil && p.mitmLLM && p.isMITMHost(host)
}

// isCorpMITMHost reports whether host is an operator-configured corp artifact
// host (i.e. MITM-eligible but NOT a built-in LLM host). The CONNECT path uses it
// to route corp hosts into the MITM tunnel without touching the LLM-specific
// blind-coverage bookkeeping.
//
// The reserved LLM hostnames are excluded here explicitly (defense in depth):
// handleConnect dispatches this branch BEFORE the isLLMHost/mitmLLMHost gate, so
// if mitmHosts were ever populated with api.anthropic.com/api.openai.com — e.g. a
// misconfigured or admin-authored EgressRedirect targeting one of them — this
// check alone stands between that entry and silently swapping a corp artifact
// token onto real Anthropic/OpenAI traffic, bypassing the mitmLLM intent gate
// entirely. No caller today populates mitmHosts with either host, but the
// ordering in handleConnect gives this map veto power over the LLM gate, so it
// must never trust the map blindly.
func (p *Proxy) isCorpMITMHost(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	if h == anthropicHost || h == openaiHost {
		return false
	}
	return p.mitmHosts[h]
}

// channelForHost maps a model host to its request schema for inspection. A host
// that is not a recognised LLM API (e.g. an operator corp artifact host being
// MITM'd only for token injection) maps to ChannelGeneric, which classifyLLM
// treats as not-prompt-bearing — so the LLM content scanner never runs against
// package-registry traffic. A method (not a free function) so a configured
// internal gateway's host classifies as its vendor's own channel, not generic.
func (p *Proxy) channelForHost(host string) contentscan.Channel {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	if vendor, ok := p.gatewayVendor[h]; ok {
		h = vendor
	}
	switch h {
	case openaiHost:
		return contentscan.ChannelOpenAIChat
	case anthropicHost:
		return contentscan.ChannelAnthropicMessages
	default:
		return contentscan.ChannelGeneric
	}
}

// mitmConnect intercepts a CONNECT tunnel to a known LLM host: it terminates TLS
// with a CA-signed leaf, then serves the decrypted HTTP/1.1 requests through the
// INSPECT-ONLY passthrough handler (serveMITMRequest). This makes the otherwise-
// opaque subscription-OAuth path inspectable. The caller has already evaluated +
// allowed the CONNECT (its egress.allow decision is recorded); per-request scan
// decisions are emitted inside. port is the REAL CONNECT port (handleConnect's
// parsed target) carried through to serveMITMRequest's dial (W13-S1-5) — never
// assume 443, a corp artifact mirror may listen elsewhere.
func (p *Proxy) mitmConnect(w http.ResponseWriter, r *http.Request, host string, port int) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hj.Hijack()
	if err != nil {
		return
	}
	if _, err := clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		_ = clientConn.Close()
		return
	}
	tlsConn := tls.Server(clientConn, &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
			// Mint for the VALIDATED CONNECT host, NOT the agent-chosen SNI: this
			// bounds the leaf cache to the few real LLM hosts and prevents an agent
			// from forcing a fresh keygen+sign per request via unique SNIs. An agent
			// that sends a mismatched SNI simply fails its own validation.
			return p.ca.leafFor(host)
		},
	})
	if err := tlsConn.Handshake(); err != nil {
		_ = clientConn.Close()
		return
	}
	// Serve the decrypted connection with a real http.Server (correct HTTP/1.1
	// framing + keep-alive + timeouts) over a one-shot listener. Serve returns
	// as soon as the listener yields the single conn; the per-conn goroutine
	// keeps serving requests until the client closes or IdleTimeout fires.
	srv := &http.Server{
		Handler: http.HandlerFunc(func(rw http.ResponseWriter, rr *http.Request) {
			p.serveMITMRequest(rw, rr, host, port)
		}),
		ReadHeaderTimeout: 30 * time.Second,
		// ReadTimeout bounds the whole request incl. body so a slow-loris body
		// can't pin a goroutine + scan buffer indefinitely. WriteTimeout stays 0:
		// model RESPONSES legitimately stream for a long time.
		ReadTimeout: 5 * time.Minute,
		IdleTimeout: 90 * time.Second,
	}
	_ = srv.Serve(&oneConnListener{conn: tlsConn})
}

// serveMITMRequest serves a MITM-terminated request. It inspects the plaintext
// and forwards it, and — when an injection rule exists for the host — STRIPS the
// sandbox's credential headers and injects the brokered/live one (same discipline
// as proxyLLMRequest). This is how a subscription run is credentialed proxy-side:
// the sandbox holds only an inert sentinel and the proxy injects the operator's
// live, host-refreshed OAuth token here. When NO rule exists, the agent's own
// resident credential is preserved (inspect-only). A confident block refuses the
// request (written by inspectLLM); a rotating credential that cannot be refreshed
// fails closed rather than forwarding a stale token. port is the REAL CONNECT
// port (from mitmConnect) — the dial below MUST use it rather than assume 443:
// a tokened tunnel dialed to the wrong port presents the operator's credential
// to whatever answers there instead of the intended mirror (W13-S1-5).
func (p *Proxy) serveMITMRequest(w http.ResponseWriter, r *http.Request, host string, port int) {
	rest := strings.TrimPrefix(r.URL.Path, "/")
	channel := p.channelForHost(host)
	// Decision-log source: honest about WHY this tunnel was terminated — LLM
	// inspection/injection vs corp artifact-token injection (no scan coverage).
	mitmSource := ruleSourceLLMMITM
	if !p.isLLMHost(host) {
		mitmSource = ruleSourceArtifactMITM
	}

	// Dial target: through the corp proxy (by hostname) when an upstream is
	// configured — the transport's egressDial chains the CONNECT and TLS then
	// runs end-to-end proxy<->host — else the vetted IP (direct). Upstream mode
	// relaxes the vetted-IP pin for this hop (see dialThroughUpstream).
	target, _, terr := p.egressTarget(host, port)
	if terr != nil {
		p.emitLLMDecision(r, host, port, egress.Deny, mitmSource, nil)
		p.httpError(w, "llm upstream vet failed", terr, http.StatusBadGateway)
		return
	}

	// LLM hosts go through inspectLLM's per-endpoint classifier as always. A
	// corp artifact host, though, is ALWAYS ChannelGeneric — which classifyLLM
	// unconditionally treats as scanNone (not prompt-bearing) — so inspectLLM
	// would silently stream it through unscanned even when the operator has
	// opted every generic forward body into inspection via
	// inspect_forward_egress. That flag already extends scanning to the plain
	// (non-MITM) forward path (handlePlain/inspectForwardBody); route the
	// artifact-MITM body through the SAME scanBufferedBody core here so the
	// flag's promise holds on this path too (W19-W19d-1: a MITM'd non-LLM
	// tunnel used to be unconditionally unscanned, no matter the policy).
	var bodyReader io.Reader
	var scanSummary *egress.ScanSummary
	var blocked bool
	if mitmSource == ruleSourceArtifactMITM && p.scanner != nil &&
		p.scanner.InspectForwardEgress() && p.scanner.Mode() != contentscan.ModeOff && hasScannableBody(r) {
		bodyReader, scanSummary, blocked = p.inspectForwardBody(w, r, host, port)
	} else {
		bodyReader, scanSummary, blocked = p.inspectLLM(w, r, host, port, rest, channel)
	}
	if blocked {
		return
	}

	// Credential handling: if an injection rule exists for this host, STRIP every
	// sandbox-supplied credential header and inject the brokered/live one (the
	// subscription OAuth token path); otherwise PRESERVE the agent's own resident
	// credential (inspect-only, hdr==nil below). A rule whose rotating credential
	// cannot be refreshed fails closed — never forward a stale token.
	hdr, ok, ierr := p.inject.resolve(host)
	if ierr != nil {
		p.emitLLMDecision(r, host, port, egress.Deny, mitmSource, nil)
		p.httpError(w, "llm credential refresh failed", ierr, http.StatusBadGateway)
		return
	}
	var injectHdr *injectedHeader
	if ok {
		injectHdr = &hdr
	}
	// Forwards over the pinned transport; DialContext dials the vetted target from
	// the request context, so the host is never re-resolved.
	p.forwardInspectedLLM(w, r, host, port, rest, target, injectHdr, mitmSource, bodyReader, scanSummary)
}

// oneConnListener hands a single already-accepted conn to http.Server.Serve and
// then reports EOF so Serve returns; the conn's own goroutine keeps serving.
type oneConnListener struct {
	conn net.Conn
	mu   sync.Mutex
	used bool
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.used {
		return nil, io.EOF
	}
	l.used = true
	return l.conn, nil
}

func (l *oneConnListener) Close() error   { return nil }
func (l *oneConnListener) Addr() net.Addr { return l.conn.LocalAddr() }
