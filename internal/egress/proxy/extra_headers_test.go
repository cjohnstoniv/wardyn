// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// An admin can set non-secret headers on a custom_endpoint provider (Portkey's x-portkey-provider, or any
// gateway that routes on a header). The proxy sets them on every request to that provider's gateway host,
// after the credential, so a sandbox header of the same name is overwritten and never reaches the gateway.

func TestExtraHeaders_RefusedNames(t *testing.T) {
	for _, name := range []string{
		"", "Bad Name", "X-Bad\r\nName", "X-Has:Colon",
		// the sandbox credential headers the proxy strips, and the proxy's own
		"Authorization", "X-Api-Key", "Api-Key", "X-Auth-Token", "Anthropic-Api-Key", "Cookie", "Private-Token",
		"X-Access-Token", "X-Amz-Security-Token", "X-Functions-Key", "X-Goog-Api-Key", "Proxy-Authorization",
		// hop-by-hop
		"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "TE", "Trailer", "Upgrade",
		// framing and content negotiation the proxy owns
		"Host", "Content-Length", "Content-Type", "Content-Encoding", "Transfer-Encoding", "Expect",
		"Accept-Encoding", "Range", "If-Range",
		// Wardyn's own, in any case
		"X-Wardyn-Run", "x-wardyn-call-id", "X-WARDYN-Identity",
		// names that read as a credential
		"X-Portkey-Api-Key", "X-Team-Token", "X-Client-Secret", "X-Auth-Scope", "X-Password-Hint", "X-Session-Cookie", "X-Credential-Id",
		"x-my-KEY",
	} {
		if err := ReservedExtraHeader(name); err == nil {
			t.Errorf("ReservedExtraHeader(%q) = nil, want a refusal", name)
		}
	}
}

func TestExtraHeaders_AllowedNames(t *testing.T) {
	for _, name := range []string{"X-Portkey-Provider", "X-Portkey-Config", "X-Portkey-Custom-Host", "x-litellm-tags", "Anthropic-Beta", "OpenAI-Project", "X-Team"} {
		if err := ReservedExtraHeader(name); err != nil {
			t.Errorf("ReservedExtraHeader(%q) = %v, want it allowed", name, err)
		}
	}
}

// The provider's own credential header, and any harness's, is the credential's: another provider-owned name
// is refused even when it would pass the fixed list.
func TestExtraHeaders_OwnAuthHeaderIsRefused(t *testing.T) {
	if err := ReservedExtraHeader("X-Gateway-Pass"); err != nil {
		t.Fatalf("X-Gateway-Pass alone: %v, want it allowed", err)
	}
	for _, own := range [][]string{{"x-gateway-pass"}, {"Other", "X-GATEWAY-PASS"}} {
		if err := ReservedExtraHeader("X-Gateway-Pass", own...); err == nil {
			t.Errorf("ReservedExtraHeader with own %v = nil, want a refusal", own)
		}
	}
}

func TestExtraHeaders_Values(t *testing.T) {
	bad := map[string]string{
		"empty":            "",
		"too long":         strings.Repeat("a", MaxExtraHeaderValue+1),
		"CR LF":            "a\r\nX-Injected: 1",
		"NUL":              "a\x00b",
		"control":          "a\x01b",
		"github token":     "ghp_" + strings.Repeat("a", 36),
		"aws key id":       "AKIA" + strings.Repeat("A", 16),
		"jwt":              "eyJ" + "hbGciOiJIUzI1NiJ9." + "eyJ" + "zdWIiOiIxMjM0NTY3ODkwIn0." + "abcdefghijk",
		"sk- key":          "sk-" + strings.Repeat("a", 24),
		"sk- key in text":  "provider sk-" + strings.Repeat("A1_-", 6),
		"private key head": "-----BEGIN RSA PRIVATE KEY-----",
	}
	for name, v := range bad {
		if err := ExtraHeaderValueRefusal(v); err == nil {
			t.Errorf("%s: ExtraHeaderValueRefusal(%q) = nil, want a refusal", name, v)
		}
	}
	for name, v := range map[string]string{
		"provider slug": "@openai-prod",
		"config id":     "pc-team-1a2b3c",
		"url":           "https://api.openai.com/v1",
		"short sk":      "sk-short",
		"max length":    strings.Repeat("a", MaxExtraHeaderValue),
		"with tab":      "a\tb",
	} {
		if err := ExtraHeaderValueRefusal(v); err != nil {
			t.Errorf("%s: ExtraHeaderValueRefusal(%q) = %v, want it allowed", name, v, err)
		}
	}
	// A refusal never echoes the value back: it can be a pasted secret.
	if err := ExtraHeaderValueRefusal("ghp_" + strings.Repeat("z", 36)); err == nil || strings.Contains(err.Error(), "zzzz") {
		t.Errorf("refusal %v must not carry the value", err)
	}
}

func extraHeaderProxy(up *capturedUpstream, host string, gateway bool, inj *injector, extra map[string]map[string]string) *Proxy {
	inj.setLLMHeaders(extra)
	return portkeyProxy(up, host, gateway, inj)
}

func sandboxExtraHeaders() http.Header {
	h := portkeySandboxHeaders()
	h.Set("X-Team", "sandbox-team")
	h.Add("x-team", "sandbox-team-2")
	return h
}

// Both lanes (the brokered route and the terminated tunnel) and the plain forward lane: the configured
// header arrives once with the configured value, the sandbox's copy of it does not, and the credential
// is still the brokered one.
func TestExtraHeaders_ArriveAndOverwriteTheSandbox(t *testing.T) {
	for _, lane := range portkeyLanes {
		t.Run(lane.name, func(t *testing.T) {
			up := captureUpstream(t, true, `{"ok":true}`)
			inj := staticInj(map[string]injectedHeader{portkeyGatewayHost: {name: "Authorization", value: "Bearer BROKERED"}})
			p := extraHeaderProxy(up, portkeyGatewayHost, true, inj, map[string]map[string]string{
				portkeyGatewayHost: {"X-Portkey-Provider": "@openai-prod", "X-Team": "platform"},
			})

			rec := lane.send(t, p, portkeyGatewayHost, sandboxExtraHeaders())
			if rec.Code != http.StatusOK || !up.reached {
				t.Fatalf("status = %d reached = %v body = %q", rec.Code, up.reached, rec.Body.String())
			}
			if got := up.header.Values("X-Portkey-Provider"); len(got) != 1 || got[0] != "@openai-prod" {
				t.Errorf("x-portkey-provider = %q, want exactly the configured value", got)
			}
			if got := up.header.Values("X-Team"); len(got) != 1 || got[0] != "platform" {
				t.Errorf("x-team = %q, want the configured value alone: the sandbox's copies are overwritten", got)
			}
			if got := up.header.Get("Authorization"); got != "Bearer BROKERED" {
				t.Errorf("Authorization = %q, want the brokered credential", got)
			}
			// The other sandbox routing headers are still dropped (the gateway strip runs first).
			for _, k := range []string{"X-Portkey-Custom-Host", "X-Portkey-Config"} {
				if got := up.header.Values(k); len(got) != 0 {
					t.Errorf("gateway saw sandbox %s = %q, want it dropped", k, got)
				}
			}
		})
	}
}

// A configured header never replaces the rule's own credential header, even if a config names it.
func TestExtraHeaders_NeverOverwriteTheCredentialHeader(t *testing.T) {
	for _, lane := range portkeyLanes {
		t.Run(lane.name, func(t *testing.T) {
			up := captureUpstream(t, true, `{"ok":true}`)
			inj := staticInj(map[string]injectedHeader{portkeyGatewayHost: {name: "X-Gateway-Pass", value: "BROKERED"}})
			p := extraHeaderProxy(up, portkeyGatewayHost, true, inj, map[string]map[string]string{
				portkeyGatewayHost: {"x-gateway-pass": "configured", "X-Portkey-Provider": "@openai-prod"},
			})

			rec := lane.send(t, p, portkeyGatewayHost, portkeySandboxHeaders())
			if rec.Code != http.StatusOK || !up.reached {
				t.Fatalf("status = %d reached = %v body = %q", rec.Code, up.reached, rec.Body.String())
			}
			if got := up.header.Values("X-Gateway-Pass"); len(got) != 1 || got[0] != "BROKERED" {
				t.Errorf("x-gateway-pass = %q, want the brokered credential alone", got)
			}
			if got := up.header.Get("X-Portkey-Provider"); got != "@openai-prod" {
				t.Errorf("x-portkey-provider = %q, want the configured value", got)
			}
		})
	}
}

// Another host's headers are not sent, and a host that is no configured gateway gets none even if a config
// names it: the vendor host is never a place these headers go.
func TestExtraHeaders_OtherHostsUntouched(t *testing.T) {
	for _, lane := range portkeyLanes {
		t.Run(lane.name+"/other gateway", func(t *testing.T) {
			up := captureUpstream(t, true, `{"ok":true}`)
			inj := staticInj(map[string]injectedHeader{portkeyGatewayHost: {name: "Authorization", value: "Bearer BROKERED"}})
			p := extraHeaderProxy(up, portkeyGatewayHost, true, inj, map[string]map[string]string{
				"another.corp.test": {"X-Team": "platform"},
			})
			lane.send(t, p, portkeyGatewayHost, portkeySandboxHeaders())
			if got := up.header.Values("X-Team"); len(got) != 0 {
				t.Errorf("x-team = %q, want none: the headers belong to another host", got)
			}
		})
		t.Run(lane.name+"/vendor host", func(t *testing.T) {
			up := captureUpstream(t, true, `{"ok":true}`)
			inj := staticInj(map[string]injectedHeader{anthropicHost: {name: "X-Api-Key", value: "BROKERED"}})
			p := extraHeaderProxy(up, anthropicHost, false, inj, map[string]map[string]string{
				anthropicHost: {"X-Team": "platform"},
			})
			lane.send(t, p, anthropicHost, portkeySandboxHeaders())
			if got := up.header.Values("X-Team"); len(got) != 0 {
				t.Errorf("x-team = %q, want none: a vendor host is no gateway", got)
			}
		})
	}
}

// With nothing configured a gateway request is exactly what it was.
func TestExtraHeaders_NoneConfiguredChangesNothing(t *testing.T) {
	up := captureUpstream(t, true, `{"ok":true}`)
	inj := staticInj(map[string]injectedHeader{portkeyGatewayHost: {name: "Authorization", value: "Bearer BROKERED"}})
	p := extraHeaderProxy(up, portkeyGatewayHost, true, inj, nil)
	h := sandboxExtraHeaders()
	rec := portkeyLanes[0].send(t, p, portkeyGatewayHost, h)
	if rec.Code != http.StatusOK || !up.reached {
		t.Fatalf("status = %d reached = %v", rec.Code, up.reached)
	}
	if got := up.header.Values("X-Team"); len(got) != 2 {
		t.Errorf("x-team = %q, want the sandbox's own two values passed as before", got)
	}
}

func llmHeadersConfig(t *testing.T, upstreams map[string]string, headers map[string]map[string]string) []byte {
	t.Helper()
	extra := map[string]any{}
	if upstreams != nil {
		extra["llm_upstreams"] = upstreams
	}
	if headers != nil {
		extra["llm_headers"] = headers
	}
	return baseConfigJSON(t, extra)
}

func TestConfig_LLMHeadersAreValidated(t *testing.T) {
	gw := map[string]string{anthropicHost: "https://" + portkeyGatewayHost + "/v1"}
	nine := map[string]string{}
	for _, n := range []string{"X-A", "X-B", "X-C", "X-D", "X-E", "X-F", "X-G", "X-H", "X-I"} {
		nine[n] = "v"
	}
	for name, tc := range map[string]struct {
		upstreams map[string]string
		headers   map[string]map[string]string
		want      string // "" accepts
	}{
		"gateway host":            {gw, map[string]map[string]string{portkeyGatewayHost: {"X-Portkey-Provider": "@openai"}}, ""},
		"host in other case":      {gw, map[string]map[string]string{"PortKey.Corp.Test.": {"X-Portkey-Provider": "@openai"}}, ""},
		"none":                    {gw, nil, ""},
		"not a gateway host":      {gw, map[string]map[string]string{"other.corp.test": {"X-Team": "t"}}, "llm_headers"},
		"vendor host":             {gw, map[string]map[string]string{anthropicHost: {"X-Team": "t"}}, "llm_headers"},
		"no gateways at all":      {nil, map[string]map[string]string{portkeyGatewayHost: {"X-Team": "t"}}, "llm_headers"},
		"host with a port":        {gw, map[string]map[string]string{portkeyGatewayHost + ":443": {"X-Team": "t"}}, "llm_headers"},
		"reserved name":           {gw, map[string]map[string]string{portkeyGatewayHost: {"Authorization": "x"}}, "Authorization"},
		"credential-like name":    {gw, map[string]map[string]string{portkeyGatewayHost: {"X-Portkey-Api-Key": "x"}}, "X-Portkey-Api-Key"},
		"secret-shaped value":     {gw, map[string]map[string]string{portkeyGatewayHost: {"X-Team": "ghp_" + strings.Repeat("a", 36)}}, "X-Team"},
		"empty value":             {gw, map[string]map[string]string{portkeyGatewayHost: {"X-Team": ""}}, "X-Team"},
		"names differing by case": {gw, map[string]map[string]string{portkeyGatewayHost: {"X-Team": "a", "x-team": "b"}}, "x-team"},
		"nine headers":            {gw, map[string]map[string]string{portkeyGatewayHost: nine}, "llm_headers"},
	} {
		t.Run(name, func(t *testing.T) {
			raw := llmHeadersConfig(t, tc.upstreams, tc.headers)
			cfg, err := LoadConfigBytes(raw)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("LoadConfigBytes = %v, want it accepted", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("LoadConfigBytes = %v, want a refusal naming %q", err, tc.want)
			}
			if err != nil && strings.Contains(err.Error(), "ghp_") {
				t.Errorf("the refusal %q carries the value", err)
			}
			if tc.want == "" && tc.headers != nil && len(cfg.LLMHeaders) != 1 {
				t.Errorf("LLMHeaders = %v, want the one host's headers loaded", cfg.LLMHeaders)
			}
		})
	}
}

// The config the sidecar boots from reaches the proxy's injector, keyed like every host lookup.
func TestNewServer_CarriesLLMHeadersToTheInjector(t *testing.T) {
	raw := llmHeadersConfig(t, map[string]string{anthropicHost: "https://" + portkeyGatewayHost + "/v1"},
		map[string]map[string]string{"PortKey.Corp.Test": {"X-Portkey-Provider": "@openai"}})
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["listen"] = "127.0.0.1:0"
	m["control_plane_url"] = "http://127.0.0.1:1"
	raw, _ = json.Marshal(m)
	cfg, err := LoadConfigBytes(raw)
	if err != nil {
		t.Fatalf("LoadConfigBytes: %v", err)
	}
	srv, err := newServer(context.Background(), cfg, &http.Client{Timeout: time.Second}, &bytes.Buffer{}, serverHooks{res: publicResolver{}})
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	got := srv.proxy.inject.llmHeadersFor(portkeyGatewayHost)
	if len(got) != 1 || got["X-Portkey-Provider"] != "@openai" {
		t.Fatalf("injector headers for the gateway = %v, want x-portkey-provider from the config", got)
	}
}
