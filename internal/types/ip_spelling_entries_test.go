// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"strconv"
	"strings"
	"testing"
)

// A host rule or an MCP server URL that writes an address in a non-standard spelling is refused: the
// proxy refuses every request written that way, and a request for the standard spelling is another
// host, so the rule or the brokered-only refusal would bind nothing.
func TestHostRuleAndMCPURLRefuseANonStandardIPSpelling(t *testing.T) {
	const want = "four decimal numbers from 0 to 255 with no leading zeros"
	for _, h := range []string{"134744072", "0x8080808", "010.0.0.1", "8.8.8.008", "0x7f.1", "1.2.3", "0x"} {
		err := ValidateHostRuleSets([]HostRuleSet{{Rules: []HostRule{{Host: h, Paths: []string{"/ok"}, Effect: HostRuleAllow}}}})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("a host rule for %q = %v, want the spelling refusal", h, err)
		}
		if _, err := MCPURLDestination("https://" + h + "/mcp"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("an MCP URL at %q = %v, want the spelling refusal", h, err)
		}
	}
	for _, h := range []string{"8.8.8.8", "example.com", "0x8.example.com", "123.example.com"} {
		if err := ValidateHostRuleSets([]HostRuleSet{{Rules: []HostRule{{Host: h, Paths: []string{"/ok"}, Effect: HostRuleAllow}}}}); err != nil {
			t.Errorf("a host rule for %q = %v, want nil", h, err)
		}
		if _, err := MCPURLDestination("https://" + h + "/mcp"); err != nil {
			t.Errorf("an MCP URL at %q = %v, want nil", h, err)
		}
	}
}

// A host rule or an MCP server URL with a character outside ASCII is refused, naming the ASCII form IDNA
// gives it where there is one: the proxy refuses every request host with such a character.
func TestHostRuleAndMCPURLRefuseANonASCIIHost(t *testing.T) {
	const none = "a host is written in ASCII: a name as its xn-- form, an IP address in standard form"
	for h, why := range map[string]string{
		"bücher.de":      "a host is written in ASCII: its ASCII form is xn--bcher-kva.de",
		"ｅｘａｍｐｌｅ.com":    "a host is written in ASCII: its ASCII form is example.com",
		"Ｅｘａｍｐｌｅ.com":    "a host is written in ASCII: its ASCII form is example.com",
		"８.８.８.８":        "a host is written in ASCII: its ASCII form is 8.8.8.8",
		"０１０.0.0.1":      none, // its ASCII form writes an address in a non-standard spelling
		"b\xfccher.de":   none,
		"b\uFFFDcher.de": none,
	} {
		err := ValidateHostRuleSets([]HostRuleSet{{Rules: []HostRule{{Host: h, Paths: []string{"/ok"}, Effect: HostRuleAllow}}}})
		if want := "host: " + strconv.Quote(h) + ": " + why; err == nil || !strings.HasSuffix(err.Error(), want) {
			t.Errorf("a host rule for %q:\n got %v\nwant …%s", h, err, want)
		}
		if strings.Contains(h, "\xfc") || strings.ContainsRune(h, 'Ｅ') {
			continue // a URL is not written with a byte that is not UTF-8; a host with a capital is the rule's own case
		}
		_, err = MCPURLDestination("https://" + h + "/mcp")
		if want := strconv.Quote(h) + ": " + why; err == nil || err.Error() != want {
			t.Errorf("an MCP URL at %q:\n got %v\nwant %s", h, err, want)
		}
	}
	if _, err := MCPURLDestination("https://xn--bcher-kva.de/mcp"); err != nil {
		t.Errorf("an MCP URL at its ASCII form: %v", err)
	}
}

// An MCP server URL whose host has a percent sign is refused as a host with a colon is: the proxy refuses
// every request host written with either, so config load refuses the URL too.
func TestMCPURLRefusesAHostWithAPercentSign(t *testing.T) {
	for raw, host := range map[string]string{
		"https://1.2.3.4%25eth0/mcp":       "1.2.3.4%eth0",
		"https://example.com%25eth0/mcp":   "example.com%eth0",
		"https://[fe80::1%25eth0]/mcp":     "[fe80::1%eth0]",
		"https://example.com%25eth0:8443/": "example.com%eth0:8443",
	} {
		_, err := MCPURLDestination(raw)
		if want := strconv.Quote(host) + " is not one host"; err == nil || err.Error() != want {
			t.Errorf("an MCP URL %q:\n got %v\nwant %s", raw, err, want)
		}
	}
	for _, raw := range []string{"https://example.com/mcp", "https://1.2.3.4:8443/mcp"} {
		if _, err := MCPURLDestination(raw); err != nil {
			t.Errorf("an MCP URL %q: %v", raw, err)
		}
	}
}

// The ASCII form is named without a trailing dot, as the proxy reads a host.
func TestASCIIHostFormDropsATrailingDot(t *testing.T) {
	for h, want := range map[string]string{"bücher.de.": "xn--bcher-kva.de", "*.bücher.de..": "*.xn--bcher-kva.de"} {
		if a, ok := ASCIIHostForm(h); !ok || a != want {
			t.Errorf("ASCIIHostForm(%q) = %q, %v; want %q", h, a, ok, want)
		}
	}
}
