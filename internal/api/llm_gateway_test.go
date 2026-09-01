// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "testing"

// TestValidateLLMGateways exercises the seven boot-time rejection rules a
// WARDYN_ANTHROPIC_BASE_URL / WARDYN_OPENAI_BASE_URL value must pass, plus the
// unset -> nil map baseline (byte-identical to today).
func TestValidateLLMGateways(t *testing.T) {
	cases := []struct {
		name        string
		anthropic   string
		openai      string
		ok          bool
		wantHost    string // if ok, the parsed gateway host (via gatewayHost)
		checkVendor string
	}{
		{"both unset -> nil map", "", "", true, "", ""},
		{"good anthropic gateway", "https://llm-gateway.corp.internal", "", true, "llm-gateway.corp.internal", "api.anthropic.com"},
		{"good anthropic gateway, RFC1918 literal", "https://10.40.1.5:8443/v1", "", true, "10.40.1.5", "api.anthropic.com"},
		{"good openai gateway", "", "https://oai-gateway.corp.internal", true, "oai-gateway.corp.internal", "api.openai.com"},
		{"rule 1: http refused", "http://llm-gateway.corp.internal", "", false, "", ""},
		{"rule 2: userinfo refused", "https://user:pass@llm-gateway.corp.internal", "", false, "", ""},
		{"rule 3: empty host refused", "https:///path", "", false, "", ""},
		{"rule 4: loopback literal refused", "https://127.0.0.1", "", false, "", ""},
		{"rule 4: link-local literal refused", "https://169.254.1.1", "", false, "", ""},
		{"rule 4: metadata literal refused", "https://169.254.169.254", "", false, "", ""},
		{"rule 4: unspecified literal refused", "https://0.0.0.0", "", false, "", ""},
		{"rule 4: multicast literal refused", "https://224.0.0.1", "", false, "", ""},
		{"rule 4: nat64-embedded refused", "https://[64:ff9b::a9fe:a9fe]", "", false, "", ""},
		{"rule 4 exception: RFC1918 literal allowed", "https://10.0.0.5", "", true, "10.0.0.5", "api.anthropic.com"},
		{"rule 4 exception: CGNAT literal allowed", "https://100.64.0.5", "", true, "100.64.0.5", "api.anthropic.com"},
		{"rule 5: equals the public host refused", "https://api.anthropic.com", "", false, "", ""},
		{"rule 5 case-insensitive: equals the public host refused", "https://API.ANTHROPIC.COM", "", false, "", ""},
		{"rule 5 trailing-dot: equals the public host refused", "https://api.anthropic.com.", "", false, "", ""},
		{"rule 6: query refused", "https://llm-gateway.corp.internal?x=1", "", false, "", ""},
		{"rule 7: fragment refused", "https://llm-gateway.corp.internal#x", "", false, "", ""},
		{"malformed URL refused", "https://[::", "", false, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := ValidateLLMGateways(c.anthropic, c.openai)
			if c.ok && err != nil {
				t.Fatalf("expected valid, got error: %v", err)
			}
			if !c.ok && err == nil {
				t.Fatalf("expected an error, got nil (out=%v)", out)
			}
			if c.name == "both unset -> nil map" && out != nil {
				t.Fatalf("unset must yield a nil map, got %v", out)
			}
			if c.wantHost != "" {
				base, has := out[c.checkVendor]
				if !has {
					t.Fatalf("expected an entry for %q, got %v", c.checkVendor, out)
				}
				if h := gatewayHost(base); h != c.wantHost {
					t.Fatalf("gatewayHost(%q) = %q, want %q", base, h, c.wantHost)
				}
			}
		})
	}
}

// TestValidateLLMGateways_PathPrefixAndPortPreserved: the normalized base URL
// keeps a non-default port and a path prefix — the proxy's LLMUpstreams
// wiring depends on both surviving validation intact.
func TestValidateLLMGateways_PathPrefixAndPortPreserved(t *testing.T) {
	out, err := ValidateLLMGateways("https://llm-gateway.corp.internal:8443/v1/", "")
	if err != nil {
		t.Fatalf("ValidateLLMGateways: %v", err)
	}
	got := out["api.anthropic.com"]
	want := "https://llm-gateway.corp.internal:8443/v1"
	if got != want {
		t.Fatalf("got %q, want %q (one trailing slash trimmed, port+prefix preserved)", got, want)
	}
}

// TestValidateBedrockBaseURL pins WARDYN_BEDROCK_BASE_URL's own boot gate: it
// inherits validateOneLLMGateway's seven rules (so it cannot drift from the
// gateway knobs), with rule 5 measured against the REGIONAL Bedrock data-plane
// host rather than a vendor's public host. The CGNAT row is the load-bearing
// one — that is what an AWS PrivateLink endpoint resolves into, so a floor that
// refused it would refuse the whole feature.
func TestValidateBedrockBaseURL(t *testing.T) {
	const region = "us-east-1"
	cases := []struct {
		name, raw string
		ok        bool
		wantHost  string
	}{
		{"unset -> empty, byte-identical to today", "", true, ""},
		{"vpc endpoint hostname", "https://vpce-0abc-bedrock-runtime.us-east-1.vpce.amazonaws.com", true, "vpce-0abc-bedrock-runtime.us-east-1.vpce.amazonaws.com"},
		{"rule 1: http refused", "http://vpce-0abc.vpce.amazonaws.com", false, ""},
		{"rule 2: userinfo refused", "https://user:pass@vpce-0abc.vpce.amazonaws.com", false, ""},
		{"rule 3: empty host refused", "https:///path", false, ""},
		{"rule 4: metadata literal refused", "https://169.254.169.254", false, ""},
		{"rule 4: loopback literal refused", "https://127.0.0.1", false, ""},
		{"rule 4 exception: CGNAT literal ACCEPTED (the PrivateLink case)", "https://100.64.1.5", true, "100.64.1.5"},
		{"rule 4 exception: RFC1918 literal ACCEPTED", "https://10.40.1.5:8443", true, "10.40.1.5"},
		{"rule 5: equals the regional public host refused", "https://bedrock-runtime.us-east-1.amazonaws.com", false, ""},
		{"rule 6: query refused", "https://vpce-0abc.vpce.amazonaws.com?x=1", false, ""},
		{"rule 7: fragment refused", "https://vpce-0abc.vpce.amazonaws.com#x", false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ValidateBedrockBaseURL(c.raw, region)
			if c.ok && err != nil {
				t.Fatalf("expected valid, got error: %v", err)
			}
			if !c.ok {
				if err == nil {
					t.Fatalf("expected an error, got nil (out=%q)", got)
				}
				if got != "" {
					t.Fatalf("a refused value must return %q, got %q", "", got)
				}
				return
			}
			if h := gatewayHost(got); h != c.wantHost {
				t.Fatalf("gatewayHost(%q) = %q, want %q", got, h, c.wantHost)
			}
		})
	}
}
