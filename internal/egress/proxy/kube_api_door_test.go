// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	kubeHost     = "example.com" // a name the httptest certificate carries
	kubePort     = 6443
	kubeToken    = "Bearer minted-cluster-token"
	kubeSentinel = "Bearer wardyn-proxy-injects-kubectl-token"
)

// kubeHarness is a proxy whose run attaches a kubectl row pinned to the team-a
// and team-b namespaces, a fake API server behind it that records what
// arrives, and a fake control plane that counts every token mint. The
// injector's entry is EXPIRED, so a request that reaches the injector mints.
type kubeHarness struct {
	p      *Proxy
	mu     sync.Mutex
	seen   []string // "METHOD raw-target authorization"
	minted atomic.Int32
	log    func() string
}

type kubeOpts struct {
	write   bool
	caPEM   bool // the grant carries the API server's own certificate as its CA bundle
	trustCA bool // the egress transport skips verification (the other harnesses' default)
	allowed []string
}

func newKubeHarness(t *testing.T, o kubeOpts) *kubeHarness {
	t.Helper()
	h := &kubeHarness{}
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.seen = append(h.seen, r.Method+" "+r.RequestURI+" "+r.Header.Get("Authorization"))
		h.mu.Unlock()
		_, _ = io.WriteString(w, `{"kind":"PodList","items":[]}`)
	}))
	t.Cleanup(upstream.Close)
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		h.minted.Add(1)
		_ = json.NewEncoder(w).Encode(types.ResolvedInjection{
			Header: "Authorization", Value: kubeToken, JTI: "kube-jti", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
		})
	}))
	t.Cleanup(cp.Close)
	inj := &injector{
		byHost: map[string]*injEntry{kubeHost: {
			grantID:    uuid.New(),
			header:     injectedHeader{name: "Authorization", value: "Bearer stale"},
			expiresAt:  time.Now().Add(-time.Hour).UnixMilli(),
			requireTLS: true,
			rule:       egress.InjectionRule{RequireTLS: true},
		}},
		base: cp.URL, token: newTokenSource("RUNTOK"), client: cp.Client(),
	}
	if o.allowed == nil {
		o.allowed = []string{kubeHost + ":6443"}
	}
	grant := &KubeAPIGrant{Host: kubeHost, Port: kubePort, Namespaces: []string{"team-a", "team-b"}, Write: o.write}
	if o.caPEM {
		grant.CAPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstream.Certificate().Raw}))
	}
	var tlsCfg *tls.Config
	if o.trustCA {
		tlsCfg = testInsecureTLSConfig
	}
	buf := &bytes.Buffer{}
	h.p = newProxy(Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(types.RunPolicySpec{AllowedDomains: o.allowed}),
		Injector:        inj,
		Sink:            &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)},
		Resolver:        publicResolver{},
		Dial:            redirectDial(upstreamAddr(upstream)),
		RunToken:        newTokenSource("RUNTOK"),
		TLSClientConfig: tlsCfg,
		KubeAPI:         grant,
	})
	h.log = func() string {
		_ = h.p.sink.close(context.Background())
		return buf.String()
	}
	return h
}

func (h *kubeHarness) upstreamSaw() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.seen...)
}

func (h *kubeHarness) tunnel(t *testing.T, method, target string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	raw := method + " " + target + " HTTP/1.1\r\nHost: " + kubeHost + ":6443\r\nAuthorization: " + kubeSentinel + "\r\n"
	for _, l := range hdr {
		raw += l + "\r\n"
	}
	req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw + "\r\n")))
	if err != nil {
		t.Fatalf("build %s %s: %v", method, target, err)
	}
	rec := httptest.NewRecorder()
	h.p.serveMITMRequest(rec, req, kubeHost, kubePort)
	return rec
}

// mustRefuse asserts a 403 Kubernetes Status carrying code, that nothing
// reached the API server and that no token was minted.
func (h *kubeHarness) mustRefuse(t *testing.T, label string, rec *httptest.ResponseRecorder, code string) {
	t.Helper()
	body := rec.Body.String()
	if rec.Code != http.StatusForbidden || !strings.Contains(body, `"wardyn":"`+code+`"`) || !strings.Contains(body, `"kind":"Status"`) {
		t.Errorf("%s: status %d body %s, want 403 Status %s", label, rec.Code, body, code)
	}
	if seen := h.upstreamSaw(); len(seen) != 0 {
		t.Errorf("%s: the API server saw %q", label, seen)
	}
	if n := h.minted.Load(); n != 0 {
		t.Errorf("%s: %d token(s) minted for a refused request", label, n)
	}
}

// An admitted request reaches the API server with the minted token in place
// of the sandbox's sentinel, on the path the gate compared.
func TestKubeAPI_AdmitsInsideTheRow(t *testing.T) {
	for _, target := range []string{
		"/api/v1/namespaces/team-a/pods?limit=500",
		"/apis/apps/v1/namespaces/team-b/deployments/web",
		"/api/v1/namespaces/team-a/pods/web-1/log?container=app",
		"/api/v1/namespaces/team-a",
		"/api", "/apis", "/api/v1", "/apis/apps", "/apis/apps/v1", "/version", "/openapi/v2", "/openapi/v3/apis/apps/v1",
	} {
		t.Run(target, func(t *testing.T) {
			h := newKubeHarness(t, kubeOpts{trustCA: true})
			rec := h.tunnel(t, http.MethodGet, target)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d body %s", rec.Code, rec.Body)
			}
			if seen := h.upstreamSaw(); len(seen) != 1 || seen[0] != "GET "+target+" "+kubeToken {
				t.Fatalf("the API server saw %q", seen)
			}
			if l := h.log(); !strings.Contains(l, `"`+ruleSourceKubeAPI+`"`) {
				t.Errorf("decision log lacks %s: %s", ruleSourceKubeAPI, l)
			}
		})
	}
}

// Everything outside the row, and every path that would put a credential or a
// shell in the sandbox, is refused before a token is minted.
func TestKubeAPI_RefusesOutsideTheRow(t *testing.T) {
	type tc struct {
		name, method, target string
		write                bool
		hdr                  []string
	}
	for _, c := range []tc{
		{"a change under read", http.MethodPost, "/api/v1/namespaces/team-a/configmaps", false, nil},
		{"a delete under read", http.MethodDelete, "/apis/apps/v1/namespaces/team-a/deployments/web", false, nil},
		{"another namespace", http.MethodGet, "/api/v1/namespaces/kube-system/pods", true, nil},
		{"a group path in another namespace", http.MethodGet, "/apis/apps/v1/namespaces/prod/deployments", true, nil},
		{"every namespace", http.MethodGet, "/api/v1/pods", true, nil},
		{"the namespace list", http.MethodGet, "/api/v1/namespaces", true, nil},
		{"nodes", http.MethodGet, "/api/v1/nodes", true, nil},
		{"cluster roles", http.MethodGet, "/apis/rbac.authorization.k8s.io/v1/clusterroles", true, nil},
		{"deleting the namespace", http.MethodDelete, "/api/v1/namespaces/team-a", true, nil},
		{"finalizing the namespace", http.MethodPut, "/api/v1/namespaces/team-a/finalize", true, nil},
		{"the namespace's status", http.MethodPut, "/api/v1/namespaces/team-a/status", true, nil},
		{"patching the namespace's status", http.MethodPatch, "/api/v1/namespaces/team-a/status", true, nil},
		{"reading the namespace's status", http.MethodGet, "/api/v1/namespaces/team-a/status", true, nil},
		{"secrets", http.MethodGet, "/api/v1/namespaces/team-a/secrets", true, nil},
		{"one secret", http.MethodGet, "/api/v1/namespaces/team-a/secrets/db", true, nil},
		{"a token request", http.MethodPost, "/api/v1/namespaces/team-a/serviceaccounts/default/token", true, nil},
		{"exec", http.MethodPost, "/api/v1/namespaces/team-a/pods/web-1/exec?command=sh", true, nil},
		{"exec over GET", http.MethodGet, "/api/v1/namespaces/team-a/pods/web-1/exec?command=sh", true, nil},
		{"attach", http.MethodPost, "/api/v1/namespaces/team-a/pods/web-1/attach", true, nil},
		{"port-forward", http.MethodPost, "/api/v1/namespaces/team-a/pods/web-1/portforward", true, nil},
		{"the pod proxy", http.MethodGet, "/api/v1/namespaces/team-a/pods/web-1/proxy", true, nil},
		{"the pod proxy with a path", http.MethodGet, "/api/v1/namespaces/team-a/services/web/proxy/admin", true, nil},
		{"ephemeral containers", http.MethodPatch, "/api/v1/namespaces/team-a/pods/web-1/ephemeralcontainers", true, nil},
		{"legacy watch", http.MethodGet, "/api/v1/watch/namespaces/team-a/pods", true, nil},
		{"an upgrade", http.MethodGet, "/api/v1/namespaces/team-a/pods/web-1/log", true, []string{"Connection: Upgrade", "Upgrade: websocket"}},
		{"impersonation", http.MethodGet, "/api/v1/namespaces/team-a/pods", true, []string{"Impersonate-User: system:admin"}},
		{"impersonated groups", http.MethodGet, "/api/v1/namespaces/team-a/pods", true, []string{"Impersonate-Group: system:masters"}},
		{"an encoded slash", http.MethodGet, "/api/v1/namespaces/team-a%2f..%2fkube-system/pods", true, nil},
		{"a dot-dot segment", http.MethodGet, "/api/v1/namespaces/team-a/../kube-system/pods", true, nil},
		{"an empty segment", http.MethodGet, "/api/v1/namespaces//pods", true, nil},
		{"a discovery write", http.MethodPost, "/apis", true, nil},
		{"another method", "OPTIONS", "/api/v1/namespaces/team-a/pods", true, nil},
		{"health", http.MethodGet, "/healthz", true, nil},
		{"the root", http.MethodGet, "/", true, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newKubeHarness(t, kubeOpts{write: c.write, trustCA: true})
			h.mustRefuse(t, c.name, h.tunnel(t, c.method, c.target, c.hdr...), kubeAPIRefused)
			if l := h.log(); !strings.Contains(l, `"`+ruleSourceKubeAPIRefused+`"`) {
				t.Errorf("decision log lacks %s: %s", ruleSourceKubeAPIRefused, l)
			}
		})
	}
}

// A row that says write may change its own namespaces.
func TestKubeAPI_WriteInsideTheRow(t *testing.T) {
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		h := newKubeHarness(t, kubeOpts{write: true, trustCA: true})
		if rec := h.tunnel(t, m, "/apis/apps/v1/namespaces/team-b/deployments/web"); rec.Code != http.StatusOK {
			t.Errorf("%s: status %d body %s", m, rec.Code, rec.Body)
		}
	}
}

// The plain lane and a tunnel on another port never reach the door.
func TestKubeAPI_PlainLaneAndOtherPortRefused(t *testing.T) {
	h := newKubeHarness(t, kubeOpts{write: true, trustCA: true})
	rec := httptest.NewRecorder()
	req := mustProxyReq(t, http.MethodGet, "http://"+kubeHost+":6443/api/v1/namespaces/team-a/pods")
	req.Header.Set("Authorization", kubeSentinel)
	h.p.servePlain(rec, req)
	h.mustRefuse(t, "plain", rec, kubeAPIRefused)

	h = newKubeHarness(t, kubeOpts{write: true, trustCA: true, allowed: []string{kubeHost + ":443"}})
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodConnect, "http://"+kubeHost+":443", nil)
	req.Host = kubeHost + ":443"
	h.p.handleConnect(rec, req)
	h.mustRefuse(t, "connect :443", rec, kubeAPIRefused)
}

// The door trusts the row's CA bundle: with it, a server the egress trust set
// does not know is reached; without it, the same server is not.
func TestKubeAPI_TrustsTheRowCA(t *testing.T) {
	h := newKubeHarness(t, kubeOpts{caPEM: true})
	if rec := h.tunnel(t, http.MethodGet, "/api/v1/namespaces/team-a/pods"); rec.Code != http.StatusOK {
		t.Fatalf("with the row's CA: status %d body %s", rec.Code, rec.Body)
	}
	h = newKubeHarness(t, kubeOpts{})
	if rec := h.tunnel(t, http.MethodGet, "/api/v1/namespaces/team-a/pods"); rec.Code == http.StatusOK {
		t.Fatalf("without the row's CA the unknown server was trusted")
	}
	if seen := h.upstreamSaw(); len(seen) != 0 {
		t.Errorf("the untrusted server saw %q", seen)
	}
}

func TestKubeAPI_ConfigValidation(t *testing.T) {
	const tokenName = "wardyn-cli-1b4e28ba-2fa1-11d2-883f-0016d3cca427-kubectl"
	rule := func(host string) InjectionConfig {
		return InjectionConfig{InjectionRule: egress.InjectionRule{Host: host, Header: "Authorization", SecretName: tokenName, RequireTLS: true}, GrantID: uuid.New()}
	}
	good := func() *Config {
		return &Config{MITMCACertPEM: "cert", MITMCAKeyPEM: "key",
			KubeAPI:   &KubeAPIGrant{Host: "k8s.example.com", Port: 6443, Namespaces: []string{"team-a"}},
			Injection: []InjectionConfig{rule("k8s.example.com")}}
	}
	// validateTerminatedGates runs every door's check, the Azure CLI's among them,
	// which must leave a kubectl token rule to this door.
	if err := good().validateTerminatedGates(); err != nil {
		t.Fatalf("a well-formed grant = %v", err)
	}
	if err := (&Config{Injection: []InjectionConfig{rule("k8s.example.com")}}).validateKubeAPI(); err == nil {
		t.Error("a cluster token rule with no kube_api was accepted")
	}
	for name, mutate := range map[string]func(*Config){
		"no CA":               func(c *Config) { c.MITMCAKeyPEM = "" },
		"no host":             func(c *Config) { c.KubeAPI.Host = "" },
		"a host with a path":  func(c *Config) { c.KubeAPI.Host = "k8s.example.com/x" },
		"no port":             func(c *Config) { c.KubeAPI.Port = 0 },
		"no namespace":        func(c *Config) { c.KubeAPI.Namespaces = nil },
		"a bad namespace":     func(c *Config) { c.KubeAPI.Namespaces = []string{"A"} },
		"a namespace twice":   func(c *Config) { c.KubeAPI.Namespaces = []string{"a", "a"} },
		"a CA that is not":    func(c *Config) { c.KubeAPI.CAPEM = "nope" },
		"no rule":             func(c *Config) { c.Injection = nil },
		"two rules":           func(c *Config) { c.Injection = append(c.Injection, rule("K8S.example.com.")) },
		"a rule without TLS":  func(c *Config) { c.Injection[0].RequireTLS = false },
		"a pinned rule":       func(c *Config) { c.Injection[0].PinPath = "/api" },
		"token on other host": func(c *Config) { c.Injection = append(c.Injection, rule("evil.example")) },
	} {
		c := good()
		mutate(c)
		if err := c.validateKubeAPI(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
