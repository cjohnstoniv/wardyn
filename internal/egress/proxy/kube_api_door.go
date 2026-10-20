// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// The kubectl door: a run that attaches a kubectl row reaches the row's
// Kubernetes API server through this gate and nowhere else. The sandbox holds a
// kubeconfig whose token is an inert sentinel; every request to the server is
// judged here and forwarded with a short-lived service-account token the
// control plane mints for the run and this proxy alone holds.
//
// THIS IS THE BOUNDARY: the token is the organisation's service account's, so
// the namespace and method check on the request line, together with the
// account's own RBAC on the cluster, is what holds a run to its row. It runs on
// the terminated tunnel only; the plain forward lane and a tunnel on any other
// port refuse every request to the host.
//
// What the door refuses beyond the row's namespaces and access, because each
// would put a cluster credential, or a shell, inside the sandbox: core Secrets,
// a service account's token request, exec, attach, port-forward, the API
// server's proxy, ephemeral containers, any protocol upgrade, and any
// impersonation header.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	ruleSourceKubeAPI        = "brokered:kube-api"
	ruleSourceKubeAPIRefused = "brokered:kube-api:refused"

	// The `wardyn` codes of the door's two kinds of refusal.
	kubeAPIRefused               = "kube_api_refused"
	kubeAPICredentialUnavailable = "kube_api_credential_unavailable"

	kubeAPIRefusedPrefix = "Wardyn refused this Kubernetes request: "
	kubeAPINoCredential  = kubeAPIRefusedPrefix + "this run has no cluster credential to add to it."
	kubeAPIMintFailed    = "Wardyn could not get this run's cluster token."
)

// KubeAPIGrant is the run's kubectl grant: the API server's host and port, the
// CA bundle its serving certificate chains to ("" = the egress trust set), the
// namespaces the row pins and whether the row lets kubectl change them.
type KubeAPIGrant struct {
	Host       string   `json:"host"`
	Port       int      `json:"port"`
	CAPEM      string   `json:"ca_pem,omitempty"`
	Namespaces []string `json:"namespaces"`
	Write      bool     `json:"write,omitempty"`
}

// kubeAPIDoor is the compiled grant.
type kubeAPIDoor struct {
	host       string
	namespaces []string
	write      bool
	roots      *x509.CertPool // nil: the egress transport's own trust
	rt         http.RoundTripper
}

// newKubeAPIDoor compiles the grant and adds the server to the MITM-eligible
// set on its one port. Nil without a grant. A CA bundle that does not parse
// was refused at validate.
func newKubeAPIDoor(g *KubeAPIGrant, mitmHosts map[string]bool, mitmPorts map[string]int) *kubeAPIDoor {
	if g == nil {
		return nil
	}
	h := patAPIHost(g.Host)
	mitmHosts[h], mitmPorts[h] = true, g.Port
	d := &kubeAPIDoor{host: h, namespaces: slices.Clone(g.Namespaces), write: g.Write}
	if g.CAPEM != "" {
		d.roots = x509.NewCertPool()
		d.roots.AppendCertsFromPEM([]byte(g.CAPEM))
	}
	return d
}

// bindTransport gives the door its upstream: the egress transport itself, or a
// copy of it that trusts the cluster's CA bundle and nothing else. The copy
// keeps the egress dial, so the vetted address and any corporate upstream
// proxy still apply, and speaks HTTP/1.1, which every API server serves: the
// egress transport's own TLS dial (offerHTTP2) carries the egress trust set.
func (d *kubeAPIDoor) bindTransport(base *http.Transport) {
	if d == nil || d.roots == nil {
		return
	}
	t := base.Clone()
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if t.TLSClientConfig != nil {
		cfg = t.TLSClientConfig.Clone()
	}
	cfg.RootCAs, cfg.NextProtos = d.roots, nil
	t.TLSClientConfig, t.DialTLSContext, t.ForceAttemptHTTP2 = cfg, nil, false
	d.rt = t
}

// isKubeAPIHost reports whether host is the door's on a run that has one.
func (p *Proxy) isKubeAPIHost(host string) bool {
	return p.kube != nil && patAPIHost(host) == p.kube.host
}

// kubeSubresourcesRefused are the subresources the door never forwards: each
// opens a shell, a stream or a tunnel into the cluster, or adds a container.
var kubeSubresourcesRefused = []string{"exec", "attach", "portforward", "proxy", "ephemeralcontainers"}

var kubeWriteMethods = []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}

// admit judges one request: its headers, method and raw path.
func (d *kubeAPIDoor) admit(method, raw string, h http.Header) error {
	for name := range h {
		if strings.HasPrefix(strings.ToLower(name), "impersonate-") {
			return errors.New("this run's kubectl acts as its organisation's service account and cannot impersonate another")
		}
	}
	if h.Get("Upgrade") != "" || headerHasToken(h, "Connection", "upgrade") {
		return errors.New("exec, attach and port-forward are not allowed for this run's kubectl")
	}
	if method != http.MethodGet && !slices.Contains(kubeWriteMethods, method) {
		return fmt.Errorf("the method %s is not one kubectl sends", method)
	}
	if !strings.HasPrefix(raw, "/") || strings.Contains(raw, "%") {
		return errors.New("the path is not a plain Kubernetes API path")
	}
	segs := strings.Split(raw[1:], "/")
	for _, s := range segs {
		if s == "" || s == "." || s == ".." {
			return errors.New("the path is not a plain Kubernetes API path")
		}
	}
	if kubeDiscovery(segs) {
		if method != http.MethodGet {
			return errors.New("discovery paths are read only")
		}
		return nil
	}
	ns, rest, ok := kubeNamespaced(segs)
	if !ok {
		return fmt.Errorf("this run's kubectl reaches only the namespaces %s, not cluster-wide resources", strings.Join(d.namespaces, ", "))
	}
	if !slices.Contains(d.namespaces, ns) {
		return fmt.Errorf("the namespace %q is not one this run's kubectl may reach (%s)", ns, strings.Join(d.namespaces, ", "))
	}
	switch {
	case len(rest) == 0 && method != http.MethodGet,
		len(rest) == 1 && (rest[0] == "finalize" || rest[0] == "status"):
		return errors.New("this run's kubectl cannot change or delete a namespace itself")
	case len(rest) > 3:
		return errors.New("the path is deeper than a Kubernetes resource and subresource")
	case len(rest) >= 1 && segs[0] == "api" && rest[0] == "secrets":
		return errors.New("Secrets are not readable or writable through this run's kubectl")
	case len(rest) == 3 && slices.Contains(kubeSubresourcesRefused, rest[2]):
		return fmt.Errorf("%s is not allowed for this run's kubectl", rest[2])
	case len(rest) == 3 && rest[0] == "serviceaccounts" && rest[2] == "token":
		return errors.New("this run's kubectl cannot request a service account token")
	case method != http.MethodGet && !d.write:
		return errors.New("this run's kubectl has read access only")
	}
	return nil
}

// kubeDiscovery reports whether segs is one of the discovery paths kubectl
// reads before anything else.
func kubeDiscovery(segs []string) bool {
	switch {
	case len(segs) == 1:
		return segs[0] == "version" || segs[0] == "api" || segs[0] == "apis"
	case segs[0] == "api":
		return len(segs) == 2
	case segs[0] == "apis":
		return len(segs) <= 3
	case segs[0] == "openapi":
		return (len(segs) == 2 && segs[1] == "v2") || segs[1] == "v3"
	}
	return false
}

// kubeNamespaced splits a namespaced path, /api/<v>/namespaces/<ns>/... or
// /apis/<g>/<v>/namespaces/<ns>/..., into the namespace and what follows it.
func kubeNamespaced(segs []string) (ns string, rest []string, ok bool) {
	at := -1
	switch {
	case segs[0] == "api" && len(segs) >= 4:
		at = 2
	case segs[0] == "apis" && len(segs) >= 5:
		at = 3
	}
	if at < 0 || segs[at] != "namespaces" {
		return "", nil, false
	}
	return segs[at+1], segs[at+2:], true
}

// headerHasToken reports whether the comma-separated header name carries token.
func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

// serveKubeAPI is the door: judge, resolve, forward.
func (p *Proxy) serveKubeAPI(w http.ResponseWriter, r *http.Request, host string, port int) {
	if err := p.kube.admit(r.Method, adoRawPath(r), r.Header); err != nil {
		p.refuseKubeAPI(w, r, host, port, kubeAPIRefused, kubeAPIRefusedPrefix+err.Error()+".")
		return
	}
	// After the gate, so a refused request never mints a token.
	hdr, ok, err := p.inject.resolveCtx(r.Context(), host)
	switch {
	case err != nil && r.Context().Err() != nil:
		return // the client hung up; nothing was refused
	case err != nil:
		slog.Warn("proxy: a kubectl cluster token could not be resolved", "host", host, "err", err)
		p.refuseKubeAPI(w, r, host, port, kubeAPICredentialUnavailable, adoControlPlaneRefusal(err, kubeAPIMintFailed))
		return
	case !ok:
		p.refuseKubeAPI(w, r, host, port, kubeAPICredentialUnavailable, kubeAPINoCredential)
		return
	}
	rearmBodyDeadline(w)
	p.forwardAPIDoor(w, r, host, port, apiDoor{allow: ruleSourceKubeAPI, denied: ruleSourceKubeAPIRefused,
		mintFailed: "cluster credential", upstream: "kubernetes", rt: p.kube.rt,
		credential: func(context.Context) (string, string, error) { return hdr.name, hdr.value, nil }})
}

// refuseKubeAPI is the door's one refusal point: a decision row under
// brokered:kube-api:refused and a 403 Kubernetes Status, whose message kubectl
// prints. Never 401, which kubectl reads as "sign in again".
func (p *Proxy) refuseKubeAPI(w http.ResponseWriter, r *http.Request, host string, port int, code, message string) {
	p.emitPATAPIDecision(r, host, port, egress.Deny, ruleSourceKubeAPIRefused)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"kind": "Status", "apiVersion": "v1", "metadata": map[string]any{}, "status": "Failure",
		"message": message, "reason": "Forbidden", "code": http.StatusForbidden, "wardyn": code,
	})
}

// refuseKubeAPIPlain refuses, on the plain forward lane, every request to the
// door's host, and reports whether it did. That lane never runs the door.
func (p *Proxy) refuseKubeAPIPlain(w http.ResponseWriter, r *http.Request) bool {
	if p.kube == nil || r.URL == nil {
		return false
	}
	host, port := splitHostPort(r.URL.Host, defaultPortForScheme(r.URL.Scheme))
	if !p.isKubeAPIHost(host) {
		return false
	}
	p.refuseKubeAPI(w, r, host, port, kubeAPIRefused, kubeAPIRefusedPrefix+
		"this host is reached only through Wardyn's checked HTTPS tunnel (CONNECT), not as a plain proxy request.")
	return true
}

// refuseKubeAPITunnel refuses a CONNECT to the door's host that the proxy will
// not terminate (another port): a blind tunnel would carry it past the gate.
func (p *Proxy) refuseKubeAPITunnel(w http.ResponseWriter, r *http.Request, host string, port int) bool {
	if !p.isKubeAPIHost(host) {
		return false
	}
	p.refuseKubeAPI(w, r, host, port, kubeAPIRefused, kubeAPIRefusedPrefix+
		"this run reaches this cluster only through Wardyn's checked HTTPS door on the port its kubectl row names.")
	return true
}

// validateKubeAPI refuses a config whose kubectl grant the proxy could not
// honour, at boot: no MITM CA, a host, port or namespace that is not one, a CA
// bundle that does not parse, or anything but exactly one TLS-only, unpinned
// injection rule on the server's host — the door is that rule's only gate. It
// also refuses a kubectl CLI token rule with no door in front of it.
func (c *Config) validateKubeAPI() error {
	g := c.KubeAPI
	for i, inj := range c.Injection {
		if types.CLIKubectlTokenSecret(inj.SecretName) && (g == nil || patAPIHost(strings.TrimSpace(inj.Host)) != patAPIHost(g.Host)) {
			return fmt.Errorf("config: injection[%d]: a kubectl cluster token is injected only on the kube_api door's host", i)
		}
	}
	if g == nil {
		return nil
	}
	if c.MITMCACertPEM == "" || c.MITMCAKeyPEM == "" {
		return fmt.Errorf("config: kube_api requires mitm_ca_cert_pem and mitm_ca_key_pem — the kubectl door runs only on a terminated connection")
	}
	if !types.CLIKubectlHost(g.Host) {
		return fmt.Errorf("config: kube_api.host %q is not a host name or an IP address", g.Host)
	}
	if g.Port < 1 || g.Port > 65535 {
		return fmt.Errorf("config: kube_api.port %d is not a port", g.Port)
	}
	if len(g.Namespaces) == 0 {
		return fmt.Errorf("config: kube_api.namespaces must name at least one namespace")
	}
	for i, ns := range g.Namespaces {
		if !types.CLIKubectlNamespace(ns) || slices.Contains(g.Namespaces[:i], ns) {
			return fmt.Errorf("config: kube_api.namespaces[%d]: %q is not a namespace name listed once", i, ns)
		}
	}
	if g.CAPEM != "" && !x509.NewCertPool().AppendCertsFromPEM([]byte(g.CAPEM)) {
		return fmt.Errorf("config: kube_api.ca_pem holds no PEM certificate")
	}
	rules := 0
	for i, inj := range c.Injection {
		if patAPIHost(strings.TrimSpace(inj.Host)) != patAPIHost(g.Host) {
			continue
		}
		if !inj.RequireTLS || len(inj.PinRoutes) > 0 || inj.PinPath != "" {
			return fmt.Errorf("config: injection[%d]: the rule on %s must set require_tls and no pin — the kubectl door is its gate", i, g.Host)
		}
		rules++
	}
	if rules != 1 {
		return fmt.Errorf("config: kube_api requires exactly one injection rule on %s, found %d", g.Host, rules)
	}
	return nil
}
