// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/armscope"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	armTenant   = "00000000-0000-4000-8000-0000000000a1"
	armSub      = "1111aaaa-2222-3333-4444-5555bbbbcccc"
	armOther    = "99999999-8888-7777-6666-555555555555"
	armToken    = "Bearer arm-token-of-the-person"
	armSentinel = "Bearer wardyn-proxy-injects-azure-token"
	armGroups   = "/subscriptions/" + armSub + "/resourcegroups"
)

// armHarness is a proxy whose run attaches an Azure CLI row pinned to armSub,
// a fake Resource Manager behind it that records what arrives, and a fake
// control plane that counts every token redemption. The injector's entry is
// EXPIRED, so a request that reaches the injector redeems: a refusal that
// happens before it leaves redeemed at zero.
type armHarness struct {
	p        *Proxy
	mu       sync.Mutex
	seen     []string // "METHOD raw-target authorization"
	redeemed atomic.Int32
	cpStatus atomic.Int32 // the control plane's answer; 0 is 200
	log      func() string
}

func newARMHarness(t *testing.T, write bool, allowed ...string) *armHarness {
	t.Helper()
	h := &armHarness{}
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.seen = append(h.seen, r.Method+" "+r.RequestURI+" "+r.Header.Get("Authorization"))
		h.mu.Unlock()
		_, _ = io.WriteString(w, `{"value":[]}`)
	}))
	t.Cleanup(upstream.Close)
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		h.redeemed.Add(1)
		if s := h.cpStatus.Load(); s != 0 {
			w.WriteHeader(int(s))
			_, _ = io.WriteString(w, `{"error":"The Azure sign-in behind this run can no longer be renewed","reason":"dead_credential"}`)
			return
		}
		_ = json.NewEncoder(w).Encode(types.ResolvedInjection{
			Header: "Authorization", Value: armToken, JTI: "arm-jti", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
		})
	}))
	t.Cleanup(cp.Close)
	inj := &injector{
		byHost: map[string]*injEntry{armscope.Host: {
			grantID:    uuid.New(),
			header:     injectedHeader{name: "Authorization", value: "Bearer stale"},
			expiresAt:  time.Now().Add(-time.Hour).UnixMilli(),
			requireTLS: true,
			rule:       egress.InjectionRule{RequireTLS: true},
		}},
		base: cp.URL, token: newTokenSource("RUNTOK"), client: cp.Client(),
	}
	if len(allowed) == 0 {
		allowed = []string{armscope.Host + ":443", AzureLoginHost + ":443"}
	}
	buf := &bytes.Buffer{}
	h.p = newProxy(Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(types.RunPolicySpec{AllowedDomains: allowed}),
		Injector:        inj,
		Sink:            &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)},
		Resolver:        publicResolver{},
		Dial:            redirectDial(upstreamAddr(upstream)),
		RunToken:        newTokenSource("RUNTOK"),
		TLSClientConfig: testInsecureTLSConfig,
		AzureARM:        &AzureARMGrant{TenantID: armTenant, Subscriptions: []string{armSub}, Write: write},
	})
	h.log = func() string {
		_ = h.p.sink.close(context.Background())
		return buf.String()
	}
	return h
}

func (h *armHarness) upstreamSaw() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.seen...)
}

// armRequest is a request as the terminated tunnel reads it, carrying the
// sandbox's inert token.
func armRequest(t *testing.T, method, target, host string) *http.Request {
	t.Helper()
	raw := method + " " + target + " HTTP/1.1\r\nHost: " + host + "\r\nAuthorization: " + armSentinel + "\r\n\r\n"
	req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw)))
	if err != nil {
		t.Fatalf("build %s %s: %v", method, target, err)
	}
	return req
}

func (h *armHarness) tunnel(t *testing.T, method, target, host string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.p.serveMITMRequest(rec, armRequest(t, method, target, host), host, 443)
	return rec
}

// mustRefuse asserts a 403 carrying code, that nothing reached Resource
// Manager, that the token was never redeemed, and that neither the token nor a
// 401 reached the sandbox.
func (h *armHarness) mustRefuse(t *testing.T, label string, rec *httptest.ResponseRecorder, code string) {
	t.Helper()
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"wardyn":"`+code+`"`) {
		t.Errorf("%s: status %d body %s, want 403 %s", label, rec.Code, rec.Body, code)
	}
	if seen := h.upstreamSaw(); len(seen) != 0 {
		t.Errorf("%s: Resource Manager saw %q", label, seen)
	}
	if strings.Contains(rec.Body.String(), strings.TrimPrefix(armToken, "Bearer ")) {
		t.Errorf("%s: the refusal carries the token", label)
	}
}

// The gate holds on both lanes. Through the terminated tunnel an admitted
// request reaches Resource Manager with the person's token in place of the
// sandbox's sentinel, on the path the gate compared, and a request outside the
// row is refused before the token is redeemed. The same admitted request as an
// absolute-form request line on the plain lane, and a tunnel on another port,
// are refused before any redemption or any byte upstream.
func TestAzureARM_GateOnBothLanes(t *testing.T) {
	t.Run("tunnel admits inside the row", func(t *testing.T) {
		h := newARMHarness(t, false)
		target := "/subscriptions/" + strings.ToUpper(armSub) + "/resourceGroups/my%20rg?api-version=2024-11-01"
		rec := h.tunnel(t, http.MethodGet, target, armscope.Host)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d body %s", rec.Code, rec.Body)
		}
		seen := h.upstreamSaw()
		if want := "GET " + target + " " + armToken; len(seen) != 1 || seen[0] != want {
			t.Fatalf("Resource Manager saw %q, want %q", seen, want)
		}
		if n := h.redeemed.Load(); n != 1 {
			t.Errorf("redeemed %d time(s), want 1", n)
		}
		if l := h.log(); !strings.Contains(l, `"`+ruleSourceAzureARM+`"`) {
			t.Errorf("decision log lacks %s: %s", ruleSourceAzureARM, l)
		}
	})
	for _, tc := range []struct{ name, method, target string }{
		{"a change under read", http.MethodPost, "/subscriptions/" + armSub + "/resourceGroups/rg/providers/Microsoft.Storage/storageAccounts/a/listKeys?api-version=1"},
		{"another subscription", http.MethodGet, "/subscriptions/" + armOther + "/resourcegroups"},
		{"a tenant-level provider", http.MethodGet, "/providers/Microsoft.Management/managementGroups"},
		{"the batch door", http.MethodPost, "/batch?api-version=2020-06-01"},
		{"a dot segment", http.MethodGet, "/subscriptions/" + armSub + "/%2e%2e/" + armOther + "/resourcegroups"},
	} {
		t.Run("tunnel refuses "+tc.name, func(t *testing.T) {
			h := newARMHarness(t, false)
			h.mustRefuse(t, tc.name, h.tunnel(t, tc.method, tc.target, armscope.Host), azureARMRefused)
			if n := h.redeemed.Load(); n != 0 {
				t.Errorf("the token was redeemed %d time(s) for a refused request", n)
			}
			if l := h.log(); !strings.Contains(l, `"`+ruleSourceAzureARMRefused+`"`) {
				t.Errorf("decision log lacks %s: %s", ruleSourceAzureARMRefused, l)
			}
		})
	}
	for _, target := range []string{"https://" + armscope.Host + armGroups, "http://" + armscope.Host + armGroups, "http://" + armscope.Host + ":443" + armGroups} {
		t.Run("plain lane refuses "+target, func(t *testing.T) {
			h := newARMHarness(t, true)
			rec := httptest.NewRecorder()
			req := mustProxyReq(t, http.MethodGet, target)
			req.Header.Set("Authorization", armSentinel)
			h.p.servePlain(rec, req)
			h.mustRefuse(t, target, rec, azureARMRefused)
			if n := h.redeemed.Load(); n != 0 {
				t.Errorf("the token was redeemed %d time(s) on the plain lane", n)
			}
		})
	}
	t.Run("a tunnel on another port is refused", func(t *testing.T) {
		h := newARMHarness(t, true, armscope.Host+":8443")
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodConnect, "http://"+armscope.Host+":8443", nil)
		req.Host = armscope.Host + ":8443"
		h.p.handleConnect(rec, req)
		h.mustRefuse(t, "connect :8443", rec, azureARMRefused)
	})
}

// A row that says write may change its own subscriptions and nothing else.
func TestAzureARM_WriteStaysInsideTheRow(t *testing.T) {
	h := newARMHarness(t, true)
	if rec := h.tunnel(t, http.MethodPut, "/subscriptions/"+armSub+"/resourcegroups/rg?api-version=1", armscope.Host); rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
	h = newARMHarness(t, true)
	h.mustRefuse(t, "write elsewhere", h.tunnel(t, http.MethodPut, "/subscriptions/"+armOther+"/resourcegroups/rg", armscope.Host), azureARMRefused)
}

// The Entra host answers the run tenant's OpenID configuration from the canned
// copy, with no upstream call, and refuses every other request: a token
// request, instance discovery, a device code, another tenant's document.
func TestAzureARM_OIDCOnlyForRowTenant(t *testing.T) {
	h := newARMHarness(t, false)
	rec := h.tunnel(t, http.MethodGet, "/"+armTenant+"/v2.0/.well-known/openid-configuration", AzureLoginHost)
	var doc map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); rec.Code != http.StatusOK || err != nil {
		t.Fatalf("status %d body %s (%v)", rec.Code, rec.Body, err)
	}
	base := "https://login.microsoftonline.com/" + armTenant
	for key, want := range map[string]string{
		"issuer": base + "/v2.0", "authorization_endpoint": base + "/oauth2/v2.0/authorize",
		"token_endpoint": base + "/oauth2/v2.0/token", "device_authorization_endpoint": base + "/oauth2/v2.0/devicecode",
	} {
		if doc[key] != want {
			t.Errorf("%s = %q, want %q", key, doc[key], want)
		}
	}
	if seen := h.upstreamSaw(); len(seen) != 0 || h.redeemed.Load() != 0 {
		t.Fatalf("the canned document dialled out: upstream %q, redeemed %d", seen, h.redeemed.Load())
	}
	for _, tc := range []struct{ method, target string }{
		{http.MethodPost, "/" + armTenant + "/oauth2/v2.0/token"},
		{http.MethodPost, "/" + armTenant + "/oauth2/v2.0/devicecode"},
		{http.MethodGet, "/common/discovery/instance?api-version=1.1&authorization_endpoint=x"},
		{http.MethodGet, "/common/v2.0/.well-known/openid-configuration"},
		{http.MethodGet, "/" + armOther + "/v2.0/.well-known/openid-configuration"},
		{http.MethodPost, "/" + armTenant + "/v2.0/.well-known/openid-configuration"},
		{http.MethodGet, "/" + armTenant + "/oauth2/v2.0/authorize?client_id=x"},
	} {
		h := newARMHarness(t, false)
		rec := h.tunnel(t, tc.method, tc.target, AzureLoginHost)
		h.mustRefuse(t, tc.method+" "+tc.target, rec, azureARMRefused)
		if !strings.Contains(rec.Body.String(), `"error":"access_denied"`) {
			t.Errorf("%s %s: body %s is not an OAuth error", tc.method, tc.target, rec.Body)
		}
		if l := h.log(); !strings.Contains(l, `"`+ruleSourceAzureARMRefused+`"`) {
			t.Errorf("%s %s: decision log lacks %s: %s", tc.method, tc.target, ruleSourceAzureARMRefused, l)
		}
	}
	h = newARMHarness(t, false)
	plain := httptest.NewRecorder()
	h.p.servePlain(plain, mustProxyReq(t, http.MethodPost, "https://"+AzureLoginHost+"/"+armTenant+"/oauth2/v2.0/token"))
	h.mustRefuse(t, "plain lane token request", plain, azureARMRefused)
}

// A token the control plane will not renew is a 403 with the control plane's
// own sentence, never a 401 (which the CLI answers by asking Entra for a
// token) and never a request forwarded without a credential.
func TestAzureARM_CredentialFailureIs403(t *testing.T) {
	h := newARMHarness(t, false)
	h.cpStatus.Store(http.StatusForbidden)
	rec := h.tunnel(t, http.MethodGet, armGroups, armscope.Host)
	h.mustRefuse(t, "dead sign-in", rec, azureARMCredentialUnavailable)
	if !strings.Contains(rec.Body.String(), "can no longer be renewed") {
		t.Errorf("body %s lacks the control plane's sentence", rec.Body)
	}
	if l := h.log(); !strings.Contains(l, `"`+ruleSourceAzureARMRefused+`"`) {
		t.Errorf("decision log lacks %s: %s", ruleSourceAzureARMRefused, l)
	}

	// A door with no rule behind it has no token to add, and adds none.
	h = newARMHarness(t, false)
	h.p.inject = nil
	h.mustRefuse(t, "no rule", h.tunnel(t, http.MethodGet, armGroups, armscope.Host), azureARMCredentialUnavailable)
}

// A config whose grant the door could not honour is refused at boot.
func TestAzureARM_ConfigValidation(t *testing.T) {
	rule := func(host string, f func(*egress.InjectionRule)) InjectionConfig {
		r := egress.InjectionRule{Host: host, Header: "Authorization", SecretName: "wardyn-cli-x-entra", RequireTLS: true}
		if f != nil {
			f(&r)
		}
		return InjectionConfig{InjectionRule: r, GrantID: uuid.New()}
	}
	good := func() *Config {
		return &Config{MITMCACertPEM: "cert", MITMCAKeyPEM: "key",
			AzureARM:  &AzureARMGrant{TenantID: armTenant, Subscriptions: []string{armSub}},
			Injection: []InjectionConfig{rule(armscope.Host, nil)}}
	}
	if err := good().validateAzureARM(); err != nil {
		t.Fatalf("a well-formed grant = %v", err)
	}
	if err := (&Config{}).validateAzureARM(); err != nil {
		t.Fatalf("no grant = %v", err)
	}
	for name, mutate := range map[string]func(*Config){
		"no CA":                    func(c *Config) { c.MITMCAKeyPEM = "" },
		"no tenant":                func(c *Config) { c.AzureARM.TenantID = "" },
		"a tenant with a slash":    func(c *Config) { c.AzureARM.TenantID = "a/b" },
		"no subscription":          func(c *Config) { c.AzureARM.Subscriptions = nil },
		"an uppercase id":          func(c *Config) { c.AzureARM.Subscriptions = []string{strings.ToUpper(armSub)} },
		"an id twice":              func(c *Config) { c.AzureARM.Subscriptions = []string{armSub, armSub} },
		"not a GUID":               func(c *Config) { c.AzureARM.Subscriptions = []string{"nope"} },
		"no rule":                  func(c *Config) { c.Injection = nil },
		"two rules":                func(c *Config) { c.Injection = append(c.Injection, rule("Management.Azure.com.", nil)) },
		"a rule without TLS":       func(c *Config) { c.Injection[0].RequireTLS = false },
		"a pinned rule":            func(c *Config) { c.Injection[0].PinPath = "/subscriptions" },
		"a rule on the Entra host": func(c *Config) { c.Injection = append(c.Injection, rule(AzureLoginHost, nil)) },
	} {
		c := good()
		mutate(c)
		if err := c.validateAzureARM(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
