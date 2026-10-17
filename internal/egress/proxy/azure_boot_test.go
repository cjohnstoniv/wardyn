// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
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

	"github.com/cjohnstoniv/wardyn/internal/contentscan"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestAzureConfig_BootedProxyHoldsEveryDoor boots the proxy the way a sidecar does, from the config JSON a
// dispatch of an azure_foundry run writes (the gate, the pinned TLS-only rule, the endpoint as a MITM host,
// the channel host and the per-run CA), and drives every door against a fake upstream: an allowed inference
// call completes with the person's token, and a disallowed route or deployment, the plain forward lane and
// the brokered route all get no token. The token is redeemed exactly once, at boot.
func TestAzureConfig_BootedProxyHoldsEveryDoor(t *testing.T) {
	certPEM, keyPEM := genTestCA(t)
	var cpHits atomic.Int32
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/internal/injection/") {
			w.WriteHeader(http.StatusOK)
			return
		}
		cpHits.Add(1)
		_ = json.NewEncoder(w).Encode(types.ResolvedInjection{
			Header: "Authorization", Value: azToken, JTI: "azure-jti", ExpiresAt: time.Now().Add(24 * time.Hour).UnixMilli(),
		})
	}))
	t.Cleanup(cp.Close)
	var mu sync.Mutex
	var seen []azSeen
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		mu.Lock()
		seen = append(seen, azSeen{r.Method, r.URL.Path, r.Header.Get("Authorization"), int(n)})
		mu.Unlock()
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(upstream.Close)

	cfg, err := LoadConfigBytes(baseConfigJSON(t, map[string]any{
		"control_plane_url":    cp.URL,
		"control_plane_ca_pem": "",
		"policy":               types.RunPolicySpec{AllowedDomains: []string{azHost + ":443"}},
		"mitm_ca_cert_pem":     string(certPEM),
		"mitm_ca_key_pem":      string(keyPEM),
		"mitm_hosts":           []string{azHost + ":443"},
		"llm_channel_hosts":    map[string]string{azHost: anthropicHost},
		"azure_gates":          []AzureGateConfig{{Host: azHost, Route: types.AzureRouteAnthropic, Models: []string{azMain, azFast}}},
		"injection": []InjectionConfig{{
			InjectionRule: egress.InjectionRule{
				Host: azHost, Header: "Authorization", Format: "Bearer %s", SecretName: "wardyn-provider-x-entra",
				RequireTLS: true, PinRoutes: AzureRoutePins(types.AzureRouteAnthropic),
			},
			GrantID: uuid.New(),
		}},
	}))
	if err != nil {
		t.Fatalf("LoadConfigBytes over a dispatched Azure config: %v", err)
	}
	cfg.Listen = "127.0.0.1:0"
	srv, err := NewServer(context.Background(), cfg, cp.Client(), nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	})
	if n := cpHits.Load(); n != 1 {
		t.Fatalf("the token was redeemed %d times at boot, want once", n)
	}
	p := srv.proxy
	p.res = publicResolver{}
	p.dial = redirectDial(strings.TrimPrefix(upstream.URL, "http://"))
	p.mitmPlaintext = map[string]bool{plaintextKey(azHost, 443): true} // the fake origin speaks plain HTTP
	upstreamSaw := func() []azSeen {
		mu.Lock()
		defer mu.Unlock()
		return append([]azSeen(nil), seen...)
	}

	if !p.isLLMHost(azHost) || p.channelForHost(azHost) != contentscan.ChannelAnthropicMessages || len(p.gatewayVendor) != 0 || len(p.llmUpstreams) != 0 {
		t.Fatalf("the host classifies as llm=%v channel=%v, gateways=%v upstreams=%v: want an Anthropic channel host and no gateway",
			p.isLLMHost(azHost), p.channelForHost(azHost), p.gatewayVendor, p.llmUpstreams)
	}

	doMITM := func(method, target, body string, hdr map[string]string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		p.serveMITMRequest(rec, azRequest(t, method, target, body, hdr), azHost, 443)
		return rec
	}
	refused := func(label string, rec *httptest.ResponseRecorder) {
		t.Helper()
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), azureRouteRefused) {
			t.Errorf("%s: %d %s, want 403 %s", label, rec.Code, rec.Body.String(), azureRouteRefused)
		}
	}

	// The terminated door.
	if rec := doMITM(http.MethodPost, azMessages, azBody(azMain), map[string]string{"Authorization": "Bearer sandbox", "x-api-key": "sk-sandbox"}); rec.Code != http.StatusOK {
		t.Fatalf("an allowed call: %d %s", rec.Code, rec.Body.String())
	}
	if got := upstreamSaw(); len(got) != 1 || got[0].auth != azToken {
		t.Fatalf("upstream saw %+v, want one request carrying the person's token and not the sandbox's", got)
	}
	refused("another API on the host", doMITM(http.MethodPost, "/openai/files", azBody(azMain), nil))
	refused("a deployment outside the pinned set", doMITM(http.MethodPost, azMessages, azBody("deploy-other"), nil))
	refused("a deployment named in the path", doMITM(http.MethodPost, "/openai/deployments/deploy-other/chat/completions", azBody(azMain), nil))
	refused("a websocket upgrade", doMITM(http.MethodGet, azMessages, "", map[string]string{"Upgrade": "websocket", "Connection": "Upgrade"}))

	// The plain forward lane and the brokered route.
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://"+azHost+azMessages, strings.NewReader(azBody(azMain))))
	refused("the plain forward lane", rec)
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost, "/wardyn/llm/anthropic/v1/messages", strings.NewReader(azBody(azMain))))
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "no_llm_credential") {
		t.Errorf("the brokered route: %d %s, want 404 no_llm_credential", rec.Code, rec.Body.String())
	}

	if got := upstreamSaw(); len(got) != 1 {
		t.Errorf("a refused request reached the upstream: %+v", got)
	}
	if n := cpHits.Load(); n != 1 {
		t.Errorf("the token was redeemed %d times, want once: a refusal must never redeem it", n)
	}
}
