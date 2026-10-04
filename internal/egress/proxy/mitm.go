// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"log/slog"
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

// leafCertTTL is the validity window of a minted per-host leaf, measured from
// NotBefore = now-1h (so 25h of wall clock). Short-lived by design.
const leafCertTTL = 24 * time.Hour

// leafRenewBefore is how long before NotAfter a cached leaf stops being reused
// and is re-minted. Runs can outlive leafCertTTL's 24h (the per-run CA lasts a
// year), so the margin keeps a leaf picked up near NotAfter from failing the
// sandbox's TLS handshake mid-flight.
const leafRenewBefore = time.Hour

// leafUsableAt reports whether a cached leaf parsed and won't expire within
// leafRenewBefore; unparsed (cert.Leaf == nil) counts as unusable so the cache
// never serves a cert it can't validate.
func leafUsableAt(c *tls.Certificate, now time.Time) bool {
	return c != nil && c.Leaf != nil && now.Before(c.Leaf.NotAfter.Add(-leafRenewBefore))
}

// certAuthority mints per-host leaf certs signed by a Wardyn CA so the proxy can
// terminate TLS for a known LLM host and inspect plaintext inside an otherwise
// opaque CONNECT tunnel. The CA private key lives ONLY in proxy memory; only its
// public cert is trusted in the sandbox. A nil *certAuthority disables MITM.
type certAuthority struct {
	caCert *x509.Certificate
	caKey  crypto.Signer

	mu     sync.Mutex
	leaves map[string]*tls.Certificate

	// now is the clock leafFor mints and expires against. Injectable so a test
	// can fast-forward past leafCertTTL without sleeping; nil means time.Now.
	now func() time.Time
}

// clock returns the authority's time source (time.Now unless a test injected one).
func (a *certAuthority) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
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
// signed by the CA, so a sandbox trusting the CA accepts the proxy's TLS
// termination.
//
// host may be a LITERAL IP, which needs an IP SAN: crypto/x509's VerifyHostname
// matches an IP target only against IPAddresses, so a leaf carrying an IP as a
// DNSName is rejected. Not a corner case — an EgressRedirect mirror is a
// literal IP by construction and isMITMHost admits it for token injection, so a
// DNS-only leaf silently killed that lane while test-redirect (which never
// MITMs) still reported the redirect reached.
func (a *certAuthority) leafFor(host string) (*tls.Certificate, error) {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	now := a.clock()
	a.mu.Lock()
	if c, ok := a.leaves[host]; ok && leafUsableAt(c, now) {
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
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    now.Add(-1 * time.Hour),
		NotAfter:     now.Add(leafCertTTL),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	// Exactly one SAN kind, by what the CONNECT host actually is: IP SAN for a
	// literal (the only kind an IP-verifying client consults), DNS SAN
	// otherwise — an IP spelled as a DNSName is the bug this prevents.
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
// TRUST BOUNDARY (read before widening): TLS-MITM lets the proxy read plaintext and swap the
// credential header. Permitted for exactly three reasons:
//  1. built-in LLM hosts (Anthropic/OpenAI) — subscription-OAuth / content-inspection. Bedrock is
//     excluded: its client-side SigV4 auth would be invalidated by terminate-and-reforward.
//  2. operator-configured corp artifact hosts (p.mitmHosts, compiled at dispatch from site-config
//     overrides) — so the operator's own registry token can be injected without the sandbox holding it.
//  3. the run's own AWS IAM Identity Center portal host, authored at DISPATCH from the run's own
//     captured SSO credential (never sandbox/request/policy), when WARDYN_AWS_SSO_PROXY_INJECT is on.
//     Rides p.mitmHosts with #2, bounded the same way, so the SSO token is set as
//     x-amz-sso_bearer_token instead of written into the sandbox. Bedrock's data plane stays excluded
//     per #1.
//
// #2/#3's surface is bounded on every axis: admin-authored only (never sandbox/agent/request); exact
// hostname, never wildcard/suffix; exact port when the entry names one (mitmPorts), so a non-443
// mirror can't be dialed at the wrong port via hostname match alone; paired with an injection rule for
// the operator's own token; CA key stays in proxy memory. Cost: for those hosts the proxy sees
// plaintext, same as for LLM hosts — the operator trusts their own proxy with their own registry.
func (p *Proxy) isMITMHost(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	if h == anthropicHost || h == openaiHost {
		return true
	}
	return p.mitmHosts[h]
}

// mitmLLMHost reports whether a CONNECT to a built-in LLM host should be TLS-terminated: needs both
// a CA and this run's MITM-LLM intent (p.mitmLLM) — a CA minted only for artifact-token injection
// must not terminate a direct Anthropic/OpenAI CONNECT. Applies the same mitmPortAllowed clamp as
// isCorpMITMHost so a port-mismatched entry can't be re-admitted here.
func (p *Proxy) mitmLLMHost(host string, port int) bool {
	return p.ca != nil && p.mitmLLM && p.isMITMHost(host) && p.mitmPortAllowed(host, port)
}

// mitmPortAllowed reports whether an authored mitmHosts entry for host covers port.
//
// TRUST BOUNDARY: eligibility must never be decidable without the port — both isCorpMITMHost and
// mitmLLMHost route through here, so a mismatch can't fall through to a branch that TLS-terminates
// and credential-injects on a port the operator never configured. Keys on the normalized host (as
// compileMITMHosts does) so case/trailing-dot can't miss the entry and read as any-port. 0 means the
// entry named no port and stays any-port (legacy).
func (p *Proxy) mitmPortAllowed(host string, port int) bool {
	cport := p.mitmPorts[strings.TrimSuffix(strings.ToLower(host), ".")]
	return cport == 0 || cport == port
}

// isCorpMITMHost reports whether host is an operator-configured corp artifact host, MITM-eligible
// but not a built-in LLM host.
//
// Reserved LLM hostnames are excluded explicitly (defense in depth): handleConnect dispatches this
// branch before the isLLMHost/mitmLLMHost gate, so if mitmHosts were ever misconfigured with
// api.anthropic.com/api.openai.com, this check alone stops a corp artifact token from silently
// landing on real Anthropic/OpenAI traffic, bypassing the mitmLLM intent gate.
func (p *Proxy) isCorpMITMHost(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	if h == anthropicHost || h == openaiHost {
		return false
	}
	return p.mitmHosts[h]
}

// channelForHost maps a model host to its request schema for inspection. A non-LLM host (e.g. a
// corp artifact host MITM'd only for token injection) maps to ChannelGeneric, so the content scanner
// never runs against package-registry traffic. A method, not a free function, so a configured
// gateway host classifies as its vendor's channel, not generic.
func (p *Proxy) channelForHost(host string) contentscan.Channel {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	if vendor, ok := p.gatewayVendor[h]; ok {
		h = vendor
	} else if vendor, ok := p.channelHosts[h]; ok {
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

// mitmConnect intercepts a CONNECT tunnel to a known LLM host: terminates TLS with a CA-signed
// leaf, then serves decrypted HTTP/1.1 through the inspect-only handler (serveMITMRequest). The
// caller has already evaluated and allowed the CONNECT; per-request scan decisions are emitted
// inside. port is the REAL CONNECT port, carried through to serveMITMRequest's dial — never assume
// 443, a corp artifact mirror may listen elsewhere.
// tlsRecordHandshake is the TLS ClientHello's first byte (content type 22) — the
// one byte that tells a terminated tunnel's two possible clients apart.
const tlsRecordHandshake = 0x16

// clientSpeaksTLS peeks the first byte the client sends inside the tunnel without consuming it. A
// peek, not a flag: the entry's scheme says what the ORIGIN speaks, but this asks what the CLIENT
// speaks, and they differ (AWS SDK sends plaintext into an http:// tunnel; curl -k etc. still send a
// ClientHello). A read error answers TLS, so the unchanged path handles it as before.
func clientSpeaksTLS(br *bufio.Reader) bool {
	b, err := br.Peek(1)
	return err != nil || b[0] == tlsRecordHandshake
}

// readerConn is a net.Conn whose reads come from r — the hijacked connection's
// own buffered reader, so bytes already buffered (or peeked) are served rather
// than lost.
type readerConn struct {
	net.Conn
	r io.Reader
}

func (c *readerConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func (p *Proxy) mitmConnect(w http.ResponseWriter, r *http.Request, host string, port int) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
		return
	}
	clientConn, brw, err := hj.Hijack()
	if err != nil {
		return
	}
	if _, err := clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		_ = clientConn.Close()
		return
	}
	// The client leg usually speaks TLS like the upstream leg, but aws-sdk-js sends PLAINTEXT into
	// an `http://` tunnel (first byte 0x47, not 0x16) — handshaking there dropped the connection and
	// the SDK retried dozens of times with nothing forwarded (measured:
	// TestMITMConnect_PlaintextClientInsideTheTunnelIsServed). Still terminated (so the proxy can
	// substitute the real token); no confidentiality lost since http:// only appears under
	// WARDYN_AWS_SSO_ENDPOINT_OVERRIDE + WARDYN_ALLOW_TEST_ENDPOINTS.
	//
	// Every read goes through the hijack's own buffered reader, so nothing already sent is lost —
	// the peek below can put its byte back.
	buffered := p.countActivity(&readerConn{Conn: clientConn, r: brw.Reader})
	// Order matters: a TLS entry must never reach the peek — peeking waits for a byte the client
	// hasn't sent, while a TLS client waits for the server to go first.
	served := net.Conn(buffered)
	if !p.mitmPlaintextUpstream(host, port) || clientSpeaksTLS(brw.Reader) {
		tlsConn := tls.Server(buffered, &tls.Config{
			MinVersion: tls.VersionTLS12,
			GetCertificate: func(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
				// Mint for the VALIDATED CONNECT host, not the agent-chosen SNI: bounds the leaf
				// cache and stops an agent forcing a fresh keygen+sign per request via unique SNIs.
				return p.ca.leafFor(host)
			},
		})
		if err := tlsConn.Handshake(); err != nil {
			_ = clientConn.Close()
			return
		}
		served = tlsConn
	}
	// Otherwise the client speaks plaintext and `served` stays raw; the same strip-inject-forward
	// path runs below either way, via a real http.Server for correct framing/keep-alive/timeouts.
	srv := &http.Server{
		Handler: http.HandlerFunc(func(rw http.ResponseWriter, rr *http.Request) {
			p.serveMITMRequest(rw, rr, host, port)
		}),
		ReadHeaderTimeout: 30 * time.Second,
		// ReadTimeout bounds the whole request incl. body (anti slow-loris); WriteTimeout stays 0
		// since model responses legitimately stream for a long time.
		ReadTimeout: mitmReadTimeout,
		IdleTimeout: 90 * time.Second,
	}
	_ = srv.Serve(&oneConnListener{conn: served})
}

// mitmReadTimeout is the inner server's per-request read bound. A var only so
// a test can shorten it.
var mitmReadTimeout = 5 * time.Minute

// rearmBodyDeadline gives the request body a fresh mitmReadTimeout from now.
//
// ReadTimeout counts from when headers arrived and isn't reset once read (measured: an unread body
// past the deadline fails with i/o timeout even mid-handler), so a request held on an ADO or
// sign-in hold would have its body cut short by time already spent waiting. Called after each point
// that can hold, before the body is read. A writer without deadline support (a test recorder) has
// none to re-arm.
func rearmBodyDeadline(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(mitmReadTimeout))
}

// serveMITMRequest serves a MITM-terminated request: inspects the plaintext and forwards it, and —
// when an injection rule exists — STRIPS the sandbox's credential headers and injects the
// brokered/live one (same discipline as proxyLLMRequest; sandbox holds only an inert sentinel).
// With no rule, the agent's own credential is preserved (inspect-only). A confident block refuses
// the request; a credential that can't be refreshed fails closed rather than forwarding stale.
// port is the REAL CONNECT port — the dial below must use it, not assume 443, or the operator's
// credential reaches whatever answers at the wrong port.
func (p *Proxy) serveMITMRequest(w http.ResponseWriter, r *http.Request, host string, port int) {
	// allowed_methods applies to inner requests too, not just the opening CONNECT: the sandbox
	// chooses each one, so the method restriction must be re-applied per request. The APPROVAL half
	// stays per-CONNECT (docs/POLICIES.md "'One connection' is the honest word") since re-approving
	// per inner request would multiply approvals; the method check costs nothing to re-apply.
	if !p.evaluator.MethodAllowed(r.Method) {
		log := decisionLog(p.reqOf(r, host, port), egress.Deny, "policy:method")
		if p.sink != nil {
			p.sink.emit(log)
		}
		// memoed=false: built right here (policy:method on an inner request), never from the
		// private-IP memo.
		p.writeEgressDeny(w, host, port, &log, false)
		return
	}

	rest := strings.TrimPrefix(r.URL.Path, "/")
	channel := p.channelForHost(host)
	// Decision-log source: distinguishes LLM inspection/injection from corp
	// artifact-token injection (no scan coverage).
	mitmSource := ruleSourceLLMMITM
	if !p.isLLMHost(host) {
		mitmSource = ruleSourceArtifactMITM
	}
	mitmSource, nsOff := p.gateADO(w, r, host, port, mitmSource)
	if mitmSource == "" {
		return
	}
	// The Azure gate runs before the injector resolves, so a refused request never redeems the token.
	mitmSource, releaseAzure, ok := p.gateAzure(w, r, host, port, mitmSource)
	if !ok {
		return
	}
	defer releaseAzure()
	rearmBodyDeadline(w) // the gate may have held the request (awaitADOCapability)

	// Dial target: through the corp proxy (by hostname) when configured — egressDial chains
	// CONNECT+TLS end-to-end — else the vetted IP directly (upstream mode relaxes the vetted-IP pin
	// for this hop; see dialThroughUpstream).
	target, _, terr := p.egressTarget(host, port)
	if terr != nil {
		p.emitLLMDecision(r, host, port, egress.Deny, mitmSource, nil)
		// AWS lane: modelled, valid-JSON error body — MITM-terminated sibling of
		// llm_routes.go's vet-failed site.
		p.httpErrorAWSAware(w, host, "llm upstream vet failed", terr, false, http.StatusInternalServerError, "InternalServerException")
		return
	}

	// Pick the inspection core by whether the body is PARSEABLE (the channel), never by whether the
	// host is an LLM: a ChannelGeneric host (corp mirror, or Bedrock) is one classifyLLM treats as
	// scanNone, so inspectLLM would silently skip it even when inspect_forward_egress opted generic
	// bodies in. Route it through the same scanBufferedBody core the non-MITM forward path uses.
	//
	// Gated on CHANNEL, not mitm source, so a host-matcher change moving a host between branches
	// can't silently drop scan coverage — an auditor must never read a bare `scan:mitm` allow as
	// "inspected" for a turn that wasn't.
	var bodyReader io.Reader
	var scanSummary *egress.ScanSummary
	var blocked bool
	// The inspected body stays charged to maxRetainedScanBytes until this
	// request's own round trip has consumed it.
	releaseBody := func() {}
	defer func() { releaseBody() }()
	if channel == contentscan.ChannelGeneric && p.scanner != nil &&
		p.scanner.InspectForwardEgress() && p.scanner.Mode() != contentscan.ModeOff && hasScannableBody(r) {
		bodyReader, scanSummary, releaseBody, blocked = p.inspectForwardBody(w, r, host, port)
	} else {
		bodyReader, scanSummary, releaseBody, blocked = p.inspectLLM(w, r, host, port, rest, channel)
		// Honest coverage: say what happened instead of a bare scan:mitm allow for a channel we
		// can't parse — same uninspected_channel skip the brokered route already emits.
		if scanSummary == nil && channel == contentscan.ChannelGeneric && p.scanner != nil &&
			p.scanner.Mode() != contentscan.ModeOff && hasScannableBody(r) {
			scanSummary = p.skipSummary("skip", "uninspected_channel", channel)
		}
	}
	if blocked {
		return
	}

	// Uses the caller's ctx, not a background one: a mid-run credential re-auth PARKS this request
	// (credhold.go), and a disconnecting SDK must release the per-host single flight rather than
	// pin it for the whole hold budget.
	hdr, ok, ierr := p.inject.resolveCtx(r.Context(), host)
	if ierr != nil {
		if r.Context().Err() != nil {
			// SECURITY: client gone via its own cancellation was never refused — writing a 401/deny
			// here would log an expiry that didn't happen. The hold itself continues (it belongs to
			// the workflow), so the owner's sign-in still lands for whoever is left.
			return
		}
		if p.isAzureLane(host) {
			p.refuseAzureCredential(w, r, host, port, ierr)
			return
		}
		if p.isADOLane(host) {
			// Azure DevOps answers in its own error shape, never the AWS one.
			p.refuseADOCredential(w, r, host, port, ierr)
			return
		}
		if errors.Is(ierr, errReauthNoCredential) {
			// Hold ended with no credential: 401 + UnauthorizedException, not the 502 below — both
			// AWS SDKs treat 502 as retryable transport error (three more full holds for one lapse).
			//
			// SECURITY: the decision row is written only when the hold's own BUDGET expired ("the
			// owner had the whole window and didn't sign in"); a shutdown, killed run, answered
			// request, or per-run cap gets the honest sentence instead.
			//
			// ONE row per hold, not per retry: at ~30s SDK cadence a ten-minute expiry would
			// otherwise deny-log twenty times. errReauthTimedOut goes to the first observer,
			// errReauthTimedOutAgain to the rest; only the first is recorded.
			switch {
			case !errors.Is(ierr, errReauthTimedOut):
				slog.Warn("proxy: a held AWS SSO credential request ended", "host", host, "err", reauthHoldError(ierr))
				writeSSOUnauthorized(w, reauthEndedSentence)
			case errors.Is(ierr, errReauthTimedOutAgain):
				writeSSOUnauthorized(w, reauthTimedOutSentence)
			default:
				p.emitLLMDecision(r, host, port, egress.Deny, ruleSourceCredentialReauthTimeout, nil)
				slog.Warn("proxy: a held AWS SSO credential request expired", "host", host, "err", reauthHoldError(ierr))
				writeSSOUnauthorized(w, reauthTimedOutSentence)
			}
			return
		}
		p.emitLLMDecision(r, host, port, egress.Deny, mitmSource, nil)
		// AWS lane: 401 UnauthorizedException, generalizing writeSSOUnauthorized's precedent above —
		// a credential resolve failure is non-retryable the same way a spent hold is.
		p.httpErrorAWSAware(w, host, "llm credential refresh failed", ierr, false, http.StatusUnauthorized, "UnauthorizedException")
		return
	}
	rearmBodyDeadline(w) // the resolve may have held the request (credhold.go)
	var injectHdr *injectedHeader
	if ok {
		injectHdr = &hdr
	}
	// SECURITY (the pin): a rule may narrow its credential to ONE request shape; other requests to
	// the host forward without it, unauthenticated. For captured-AWS-SSO this stops the injected
	// session riding `POST /logout` (AWS: invalidates the owner's sign-in for every run) or a
	// GetRoleCredentials for another account/role. Unpinned rules are unaffected.
	//
	// The STRIP still runs (forwardInspectedLLM) — a withheld injection must not fall through to the
	// "no rule at all" branch, which would forward the sandbox's own header on exactly the requests
	// the pin exists to narrow.
	// The header this host's rule owns, known even when the pin withholds it.
	ownedHeader := ""
	if ok {
		ownedHeader = hdr.name
	}
	if injectHdr != nil && !p.inject.allowsInjection(host, r.Method, r.URL.Path, r.URL.RawQuery) {
		injectHdr = nil
	}
	// Forwards over the pinned transport; DialContext dials the vetted target from
	// the request context, so the host is never re-resolved.
	p.forwardInspectedLLM(w, r, host, port, rest, target, injectHdr, ownedHeader, mitmSource, nsOff, bodyReader, scanSummary)
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
