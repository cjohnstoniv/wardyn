// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/contentscan"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A channel host is classified as its vendor's dialect and nothing more: it is a model host to
// isLLMHost and channelForHost, and it is neither a gateway (which would enrol it in the relaxed
// private-address vet) nor an llmUpstreams entry (which would let /wardyn/llm/* forward to it).
func TestChannelHosts_ClassifyAsTheirVendorWithoutBecomingGateways(t *testing.T) {
	p := newProxy(Options{
		RunID: uuid.New(), Policy: CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{azHost + ":443"}}),
		LLMChannelHosts: map[string]string{"Foundry.Test.": anthropicHost, "responses.test": openaiHost},
	})
	for host, want := range map[string]contentscan.Channel{
		"foundry.test":   contentscan.ChannelAnthropicMessages,
		"FOUNDRY.test.":  contentscan.ChannelAnthropicMessages,
		"responses.test": contentscan.ChannelOpenAIChat,
		"other.test":     contentscan.ChannelGeneric,
	} {
		if got := p.channelForHost(host); got != want {
			t.Errorf("channelForHost(%q) = %v, want %v", host, got, want)
		}
	}
	if !p.isLLMHost("foundry.test") || !p.isLLMHost("responses.test") || p.isLLMHost("other.test") {
		t.Error("isLLMHost does not follow the channel hosts")
	}
	if len(p.gatewayVendor) != 0 || len(p.llmUpstreams) != 0 {
		t.Errorf("a channel host leaked into the gateway tables: gatewayVendor=%v llmUpstreams=%v", p.gatewayVendor, p.llmUpstreams)
	}
}

// Inspection follows the dialect: the Anthropic-dialect messages calls are scanned, and the Responses
// calls, which carry text in a shape the scanner does not parse yet, are honestly uninspected rather than
// silently forwarded.
func TestAzureChannel_InspectionCoverageFollowsTheDialect(t *testing.T) {
	p := newProxy(Options{
		RunID: uuid.New(), Policy: CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{azHost + ":443"}}),
		LLMChannelHosts: map[string]string{"foundry.test": anthropicHost, "responses.test": openaiHost},
	})
	for _, tc := range []struct {
		host, rest string
		want       int
	}{
		{"foundry.test", "anthropic/v1/messages", scanMessages},
		{"foundry.test", "anthropic/v1/messages/count_tokens", scanMessages},
		{"responses.test", "openai/v1/responses", scanOpaque},
		{"responses.test", "openai/v1/responses/compact", scanOpaque},
	} {
		if got := classifyLLM(p.channelForHost(tc.host), http.MethodPost, tc.rest); got != tc.want {
			t.Errorf("%s %s: classifyLLM = %d, want %d", tc.host, tc.rest, got, tc.want)
		}
	}
}

func TestConfig_LLMChannelHostsAreValidated(t *testing.T) {
	for name, tc := range map[string]struct {
		hosts map[string]string
		ok    bool
	}{
		"anthropic":      {map[string]string{azHost: anthropicHost}, true},
		"openai":         {map[string]string{azHost: openaiHost}, true},
		"none":           {nil, true},
		"another vendor": {map[string]string{azHost: "api.example.com"}, false},
		"host with port": {map[string]string{azHost + ":443": anthropicHost}, false},
		"host with path": {map[string]string{azHost + "/x": anthropicHost}, false},
		"empty host":     {map[string]string{" ": anthropicHost}, false},
	} {
		c := &Config{LLMChannelHosts: tc.hosts}
		if err := c.validateChannelHosts(); (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", name, err, tc.ok)
		}
	}
	// The key rides the config JSON, and its absence leaves the older shape unchanged.
	cfg, err := LoadConfigBytes(baseConfigJSON(t, map[string]any{"llm_channel_hosts": map[string]string{azHost: anthropicHost}}))
	if err != nil || cfg.LLMChannelHosts[azHost] != anthropicHost {
		t.Fatalf("LoadConfigBytes with llm_channel_hosts: %+v, %v", cfg, err)
	}
	if _, err := LoadConfigBytes(baseConfigJSON(t, map[string]any{"llm_channel_hosts": map[string]string{azHost: "x.test"}})); err == nil {
		t.Error("a channel host whose vendor is not a scanner dialect loaded")
	}
}

// An Azure host that answers with a private address is refused on the MITM lane while no InternalHosts
// entry lifts the guard: the person's token is never sent to whatever answers at an RFC1918 address a
// resolver was made to return. Nothing reaches the upstream and the token is never redeemed.
func TestAzureMITM_PrivateAnswerWithoutInternalHostsIsRefused(t *testing.T) {
	h := newAzureHarness(t, 0)
	h.p.res = fakeResolver{m: map[string][]net.IP{azHost: ips("10.1.2.3")}}
	rec := h.do(t, http.MethodPost, azMessages, azBody(azMain), nil)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "rfc1918") {
		t.Fatalf("a request to a private answer: %d %s, want the private-address refusal (502, rfc1918)", rec.Code, rec.Body.String())
	}
	if seen := h.upstreamSaw(); len(seen) != 0 {
		t.Errorf("the upstream saw %+v", seen)
	}
	if n := h.cpHits.Load(); n != 0 {
		t.Errorf("the token was redeemed %d time(s) for a refused dial", n)
	}
	if l := h.log(); !strings.Contains(l, "deny") {
		t.Errorf("no deny decision for the refused dial: %s", l)
	}
	// The control: the same request on a public answer completes, so the refusal above was the address.
	h = newAzureHarness(t, 0)
	if rec := h.do(t, http.MethodPost, azMessages, azBody(azMain), nil); rec.Code != http.StatusOK {
		t.Errorf("control: %d %s, want 200", rec.Code, rec.Body.String())
	}
}

// A credential the control plane refuses to resolve is answered in the Azure shape, 403 with the code and
// the control plane's own sentence, never the AWS 401 a spent hold used to get, and never forwarded
// without the token.
func TestAzureMITM_CredentialRefusalAnswersWithTheAzureCode(t *testing.T) {
	h := newAzureHarness(t, 0)
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"Sign in to Azure again, then relaunch","reason":"dead_credential"}`))
	}))
	t.Cleanup(cp.Close)
	h.p.inject.base, h.p.inject.client = cp.URL, cp.Client()
	rec := h.do(t, http.MethodPost, azMessages, azBody(azMain), nil)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"wardyn":"`+azureCredentialUnavailable+`"`) ||
		!strings.Contains(rec.Body.String(), "Sign in to Azure again") {
		t.Fatalf("status %d body %s, want 403 %s carrying the control plane's sentence", rec.Code, rec.Body.String(), azureCredentialUnavailable)
	}
	if seen := h.upstreamSaw(); len(seen) != 0 {
		t.Errorf("the upstream saw %+v after a credential refusal", seen)
	}
	if l := h.log(); !strings.Contains(l, `"`+ruleSourceAzureDenied+`"`) {
		t.Errorf("no %s row: %s", ruleSourceAzureDenied, l)
	}
}
