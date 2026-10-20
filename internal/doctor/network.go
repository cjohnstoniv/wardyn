// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package doctor

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// stageBound is how long one stage waits for the network.
const stageBound = 10 * time.Second

// Network is the connection checks both commands share: the organisation URL, name resolution, the proxy
// route, the TLS handshake, /healthz, and (when Identity is set) who /api/v1/me says you are. It reads the
// environment only through ProxyFor and the fields below, sends nothing but GETs, and writes nothing.
type Network struct {
	// OrgURL is the address to check, Source names where it came from ("--url", "WARDYN_URL"...), and
	// OrgSetting is the setting that fixes it.
	OrgURL, Source, OrgSetting string
	// Strict applies setup's own rule for a runner's organisation URL (federation.CheckOrgURL).
	Strict bool
	// Identity adds the /api/v1/me stage. Token is never printed; TokenSource names where it came from.
	Identity           bool
	Token, TokenSource string
	LookupHost         func(ctx context.Context, host string) ([]string, error)
	ProxyFor           func(*http.Request) (*url.URL, error)
	// SystemRoots is the trust store the command uses (nil: the system's). ExtraCA is a PEM bundle named by
	// ExtraCASource (a setting's name); ExtraCATrusted says whether the command being diagnosed trusts it:
	// the runner does, the CLI does not, and a certificate only that bundle verifies is a failure there.
	SystemRoots, ExtraCA *x509.CertPool
	ExtraCASource        string
	ExtraCATrusted       bool
	// ProxyNote is appended to the proxy stage, for what the diagnosed command's own environment lacks.
	ProxyNote string
	// ServerVersion is what /healthz reported, "" when it did not.
	ServerVersion string
}

// Run runs the stages in order; a failed stage skips the ones that need it.
func (n *Network) Run(ctx context.Context) []Stage {
	if n.LookupHost == nil {
		n.LookupHost = net.DefaultResolver.LookupHost
	}
	if n.ProxyFor == nil {
		n.ProxyFor = http.ProxyFromEnvironment
	}
	secrets := []string{n.Token}
	var out []Stage
	add := func(s Stage) { out = append(out, s.scrub(secrets)) }
	rest := func(why string, names ...string) {
		if n.Identity {
			names = append(names, "identity")
		}
		for _, name := range names {
			add(SkipStage(name, why))
		}
	}
	u, st := n.orgStage()
	add(st)
	if st.Status != StatusOK {
		rest("not run: the organisation URL is not usable", "dns", "proxy", "tls", "healthz")
		return out
	}
	proxy, proxyStage := n.proxyStage(u)
	if proxy != nil && proxy.User != nil {
		secrets = append(secrets, proxy.User.Username(), pw(proxy))
	}
	dnsStage := n.dnsStage(ctx, u, proxy)
	add(dnsStage)
	add(proxyStage)
	if dnsStage.Status == StatusFail || proxyStage.Status == StatusFail {
		rest("not run: the route to the organisation is not usable", "tls", "healthz")
		return out
	}
	p := &probe{n: n}
	tr := p.transport()
	resp, err := p.get(ctx, tr, u, "/healthz")
	tlsSt := n.tlsStage(p, u, proxy, err)
	add(tlsSt)
	healthSt := SkipStage("healthz", "not reached: the connection failed")
	if tlsSt.Status != StatusFail {
		healthSt = n.healthStage(u, proxy, p, resp, err)
	}
	add(healthSt)
	if n.Identity {
		if healthSt.Status != StatusOK {
			add(SkipStage("identity", "not run: /healthz did not answer"))
		} else {
			add(n.identityStage(ctx, u, tr))
		}
	}
	return out
}

func pw(u *url.URL) string {
	p, _ := u.User.Password()
	return p
}

func (n *Network) orgStage() (*url.URL, Stage) {
	detail := func(u *url.URL) string { return fmt.Sprintf("%s (from %s)", u.Redacted(), n.Source) }
	u, err := url.Parse(strings.TrimSpace(n.OrgURL))
	switch {
	case strings.TrimSpace(n.OrgURL) == "":
		return nil, FailStage("org-url", "no organisation address is set", n.OrgSetting, "your organisation's address, such as https://wardyn.example.com")
	case err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http"):
		return nil, FailStage("org-url", "the organisation URL ("+n.Source+") is not an absolute http(s) address", n.OrgSetting, "your organisation's address, such as https://wardyn.example.com")
	case n.Strict:
		if err := federation.CheckOrgURL(n.OrgURL); err != nil {
			return nil, FailStage("org-url", fmt.Sprintf("%s (from %s): %v", u.Redacted(), n.Source, err), n.OrgSetting, "your organisation's public HTTPS address")
		}
	case u.Scheme == "http" && !isLoopback(u.Hostname()):
		return u, OKStage("org-url", detail(u)+": plain http://, so a token sent to it travels in cleartext")
	}
	return u, OKStage("org-url", detail(u))
}

func isLoopback(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}

// proxyVar is the variable that names the proxy for this URL's scheme.
func proxyVar(u *url.URL) string {
	if u.Scheme == "http" {
		return "HTTP_PROXY"
	}
	return "HTTPS_PROXY"
}

// proxyStage is the route the connection takes, as Go's ProxyFromEnvironment resolves it, naming the proxy's
// host and port and never its credentials.
func (n *Network) proxyStage(u *url.URL) (*url.URL, Stage) {
	proxy, err := n.ProxyFor(&http.Request{URL: u})
	switch {
	case err != nil:
		return nil, FailStage("proxy", "the proxy settings cannot be read as a proxy address (the value is not repeated here: it may hold a credential)", proxyVar(u), "a proxy address such as http://proxy.example.com:3128, or unset it (NO_PROXY lists hosts to reach directly)")
	case proxy == nil:
		return nil, OKStage("proxy", "direct (no proxy applies to this address)")
	}
	return proxy, OKStage("proxy", fmt.Sprintf("via proxy %s (%s)%s", proxy.Host, proxyVar(u), n.ProxyNote))
}

// dnsStage resolves the name this computer itself dials: the proxy's when one applies, since the proxy
// resolves the organisation's name then.
func (n *Network) dnsStage(ctx context.Context, u, proxy *url.URL) Stage {
	host, setting, fix, what := u.Hostname(), n.OrgSetting, "a name that resolves from this computer (or fix its DNS or VPN)", ""
	if proxy != nil {
		host, setting, fix, what = proxy.Hostname(), proxyVar(u), "a proxy whose name resolves from this computer", "proxy "
	}
	if net.ParseIP(host) != nil {
		return OKStage("dns", what+host+" is an address: nothing to resolve")
	}
	ctx, cancel := context.WithTimeout(ctx, stageBound)
	defer cancel()
	addrs, err := n.LookupHost(ctx, host)
	if err != nil || len(addrs) == 0 {
		return FailStage("dns", fmt.Sprintf("%s%s does not resolve", what, host), setting, fix)
	}
	return OKStage("dns", fmt.Sprintf("%s%s resolves to %d address(es)", what, host, len(addrs)))
}

// probe is one connection's record: what the server presented, which trust store verified it, and what a
// proxy answered to CONNECT.
type probe struct {
	n         *Network
	leaf      *x509.Certificate
	store     string
	extraOnly bool
	verifyErr error
	connect   int
}

func (p *probe) transport() *http.Transport {
	return &http.Transport{
		Proxy:                 p.n.ProxyFor,
		DisableKeepAlives:     true,
		TLSHandshakeTimeout:   stageBound,
		ResponseHeaderTimeout: stageBound,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true, VerifyConnection: p.verify}, // #nosec G402 -- verify below does the verifying, so the leaf is known on failure
		OnProxyConnectResponse: func(_ context.Context, _ *url.URL, _ *http.Request, res *http.Response) error {
			p.connect = res.StatusCode
			return nil
		},
	}
}

// verify checks the presented chain against the command's trust store, then against the extra bundle, so the
// report can say which one vouched for it, and records the leaf even when neither did.
func (p *probe) verify(cs tls.ConnectionState) error {
	p.leaf = cs.PeerCertificates[0]
	inter := x509.NewCertPool()
	for _, c := range cs.PeerCertificates[1:] {
		inter.AddCert(c)
	}
	try := func(roots *x509.CertPool) error {
		_, err := p.leaf.Verify(x509.VerifyOptions{DNSName: cs.ServerName, Roots: roots, Intermediates: inter})
		return err
	}
	err := try(p.n.SystemRoots)
	if err == nil {
		p.store = "system"
		return nil
	}
	if p.n.ExtraCA != nil && try(p.n.ExtraCA) == nil {
		p.store = p.n.ExtraCASource
		if p.n.ExtraCATrusted {
			return nil
		}
		p.extraOnly = true
	}
	p.verifyErr = err
	return err
}

// get is a GET that does not follow a redirect, with a bounded wait, through tr.
func (p *probe) get(ctx context.Context, tr *http.Transport, u *url.URL, path string) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*stageBound)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(u.String(), "/")+path, nil)
	if err != nil {
		return nil, err
	}
	c := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body = io.NopCloser(strings.NewReader(string(body)))
	return resp, nil
}

func (n *Network) tlsStage(p *probe, u, proxy *url.URL, err error) Stage {
	switch {
	case u.Scheme != "https":
		return SkipStage("tls", "plain http://: there is no handshake to check")
	case p.verifyErr != nil:
		return n.certFailure(p, u)
	case err != nil && p.leaf == nil:
		return n.connFailure("tls", err, p, u, proxy)
	}
	return OKStage("tls", fmt.Sprintf("%s presented a certificate for %s issued by %s; verified by the %s trust store",
		u.Host, runnerwire.Distinguished(p.leaf.Subject), runnerwire.Distinguished(p.leaf.Issuer), p.store))
}

// certFailure names the certificate the server presented and who issued it, which tells a proxy that inspects
// TLS (its own issuer) from a wrong address or an expired certificate, and the setting that fixes each.
func (n *Network) certFailure(p *probe, u *url.URL) Stage {
	presented := fmt.Sprintf("%s presented a certificate for %s issued by %s", u.Host, runnerwire.Distinguished(p.leaf.Subject), runnerwire.Distinguished(p.leaf.Issuer))
	var unknown x509.UnknownAuthorityError
	var wrongHost x509.HostnameError
	var invalid x509.CertificateInvalidError
	switch {
	case p.extraOnly:
		return FailStage("tls", presented+"; only "+p.store+" verifies it, and this command does not read that setting", "SSL_CERT_FILE",
			"a PEM bundle holding that root (Linux), or add the root to the system trust store; wardyn-runnerd reads "+p.store+" instead")
	case errors.As(p.verifyErr, &unknown):
		return FailStage("tls", presented+", which no trusted root signed (a proxy that inspects TLS does this)", "WARDYN_TRUSTED_CA_FILE",
			"a PEM file holding that issuer's root CA for a runner (this CLI reads the system trust store: SSL_CERT_FILE on Linux, or add the root to it)")
	case errors.As(p.verifyErr, &wrongHost):
		return FailStage("tls", presented+", which does not cover "+u.Hostname(), n.OrgSetting, "the address that certificate covers")
	case errors.As(p.verifyErr, &invalid):
		return FailStage("tls", presented+", which is expired or not valid yet", "", "check this computer's clock; if it is right, the organisation's certificate needs renewing")
	}
	return FailStage("tls", presented+": "+p.verifyErr.Error(), n.OrgSetting, "an address whose certificate this computer trusts")
}

// connFailure classifies a connection that never reached a handshake or an answer.
func (n *Network) connFailure(name string, err error, p *probe, u, proxy *url.URL) Stage {
	variable := proxyVar(u)
	var op *net.OpError
	switch {
	case p.connect == http.StatusProxyAuthRequired:
		return FailStage(name, fmt.Sprintf("the proxy %s refused the connection (407: it wants proxy authentication)", proxy.Host), variable,
			"a proxy address that includes credentials it accepts, or add "+u.Hostname()+" to NO_PROXY")
	case p.connect == http.StatusForbidden:
		return FailStage(name, fmt.Sprintf("the proxy %s refused the connection (403: it does not allow this destination)", proxy.Host), variable,
			"a proxy that allows "+u.Hostname()+", or add it to NO_PROXY")
	case errors.As(err, &op) && op.Op == "proxyconnect":
		return FailStage(name, fmt.Sprintf("the proxy %s did not accept the connection", proxy.Host), variable, "the address of a proxy that is running, or unset it")
	case proxy != nil:
		return FailStage(name, fmt.Sprintf("no connection through the proxy %s: %v", proxy.Host, unwrapURL(err)), variable, "a proxy that reaches "+u.Host+", or add it to NO_PROXY")
	}
	return FailStage(name, fmt.Sprintf("no connection to %s: %v", u.Host, unwrapURL(err)), n.OrgSetting, "an address this computer reaches (check a VPN or firewall)")
}

func unwrapURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

func (n *Network) healthStage(u, proxy *url.URL, p *probe, resp *http.Response, err error) Stage {
	switch {
	case err != nil:
		return n.connFailure("healthz", err, p, u, proxy)
	case resp.StatusCode == http.StatusProxyAuthRequired:
		return FailStage("healthz", fmt.Sprintf("the proxy %s answered 407: it wants proxy authentication", proxy.Host), proxyVar(u), "a proxy address that includes credentials it accepts")
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		to := "elsewhere"
		if loc, err := resp.Location(); err == nil {
			to = loc.Host
		}
		return FailStage("healthz", fmt.Sprintf("redirected (%d) to %s", resp.StatusCode, to), n.OrgSetting, "the address it redirects to")
	case resp.StatusCode != http.StatusOK:
		return FailStage("healthz", fmt.Sprintf("answered %d: this address is not a healthy Wardyn control plane", resp.StatusCode), n.OrgSetting, "your organisation's Wardyn address")
	}
	var body struct{ Status, Version string }
	raw, _ := io.ReadAll(resp.Body)
	if json.Unmarshal(raw, &body) != nil || body.Version == "" {
		return FailStage("healthz", "answered 200 with something that is not Wardyn's health report (a proxy or portal may be answering)", n.OrgSetting, "your organisation's Wardyn address")
	}
	n.ServerVersion = body.Version
	return OKStage("healthz", fmt.Sprintf("status %s, server version %s", body.Status, body.Version))
}

// identityStage asks /api/v1/me through the CLI's own client. It prints the sign-in method and role the
// server reports, never the token or who it belongs to.
func (n *Network) identityStage(ctx context.Context, u *url.URL, tr *http.Transport) Stage {
	ctx, cancel := context.WithTimeout(ctx, 3*stageBound)
	defer cancel()
	c := &sdk.Client{BaseURL: u.String(), Token: n.Token, HTTPClient: &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	raw, err := c.Me(ctx)
	var ae *sdk.APIError
	switch {
	case errors.As(err, &ae) && (ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden):
		from := "no token was sent"
		if n.Token != "" {
			from = "the token from " + n.TokenSource + " was refused"
		}
		return FailStage("identity", fmt.Sprintf("/api/v1/me answered %d: %s", ae.Status, from), "WARDYN_TOKEN",
			"a personal API token: sign in to the console and create one under Your account (a deployment with single sign-on and no admin token takes nothing else)")
	case err != nil:
		return FailStage("identity", "/api/v1/me did not answer: "+unwrapURL(err).Error(), n.OrgSetting, "your organisation's Wardyn address")
	}
	var me struct{ Method, Role string }
	_ = json.Unmarshal(raw, &me)
	who := "no token sent (this server asked for none)"
	if n.Token != "" {
		who = "the token from " + n.TokenSource
	}
	return OKStage("identity", fmt.Sprintf("%s: method %s, role %s", who, me.Method, me.Role))
}
