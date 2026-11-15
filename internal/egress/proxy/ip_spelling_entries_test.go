// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"fmt"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ipSpellingDoors are the config entries that name a destination by host, each with the config that puts
// v in its first entry.
var ipSpellingDoors = []struct {
	field string
	extra func(v string) map[string]any
}{
	{"policy.allowed_domains[0]", func(v string) map[string]any {
		return map[string]any{"policy": types.RunPolicySpec{AllowedDomains: []string{v}}}
	}},
	{"policy.denied_domains[0]", func(v string) map[string]any {
		return map[string]any{"policy": types.RunPolicySpec{AllowAllEgress: true, DeniedDomains: []string{v}}}
	}},
	{"policy.host_rules[0].rules[0].host", func(v string) map[string]any {
		return map[string]any{"policy": types.RunPolicySpec{AllowAllEgress: true,
			HostRules: []types.HostRuleSet{{Rules: []types.HostRule{{Host: v, Paths: []string{"/ok"}, Effect: types.HostRuleAllow}}}}}}
	}},
	{"mcp_servers[0].url", func(v string) map[string]any {
		return map[string]any{"mcp_servers": []MCPServerConfig{{Key: "m", URL: "https://" + v + "/mcp?key=secret"}}}
	}},
}

// A config stored before the write-time refusals, with a destination written in a non-standard IP
// spelling in either domain list, a host rule or an MCP server URL, does not load, so its run does not
// start on a deny or a refusal that binds nothing: each entry is named in a warning, and the error names it
// with its standard form, both readings when programs differ. An MCP server is named by its host alone.
func TestAConfigEntryInANonStandardIPSpellingDoesNotLoad(t *testing.T) {
	for v, std := range map[string]string{
		"134744072": "its standard form is 8.8.8.8",
		"0x7f.1":    "its standard form is 127.0.0.1",
		"010.0.0.1": "its standard form is 8.0.0.1 or 10.0.0.1, as programs differ on which address it names",
		// A trailing dot and an upper-case 0X are the same spellings: both readings are still named.
		"010.0.0.1.": "its standard form is 8.0.0.1 or 10.0.0.1, as programs differ on which address it names",
		"0X7F.1":     "its standard form is 127.0.0.1",
		"8.8.8.008":  "its standard form is 8.8.8.8",
		"1.2.3.4.5":  "it has no standard form, so write the address you mean as four decimal numbers from 0 to 255 with no leading zeros",
	} {
		for _, d := range ipSpellingDoors {
			logs := captureSlog(t)
			_, err := LoadConfigBytes(baseConfigJSON(t, d.extra(v)))
			want := fmt.Sprintf("config: %s: %q writes an IP address in a non-standard spelling, which the proxy "+
				"refuses in every request, so the entry matches none; %s", d.field, v, std)
			if err == nil || err.Error() != want {
				t.Errorf("%s = %q:\n got %v\nwant %s", d.field, v, err, want)
			}
			got := logs.String()
			if strings.Count(got, `level=WARN msg="`+ipSpellingEntryWarning+`"`) != 1 || !strings.Contains(got, d.field) || !strings.Contains(got, "entry="+v+" ") ||
				!strings.Contains(got, std) || strings.Contains(got, "secret") {
				t.Errorf("%s = %q: the load warning is %q, want one naming the entry and its standard form", d.field, v, got)
			}
		}
	}

	// Each such entry is named, in the warnings and the error; the others load as before (a malformed
	// port is the domain list's own dead entry, reported at sidecar boot).
	logs := captureSlog(t)
	_, err := LoadConfigBytes(baseConfigJSON(t, map[string]any{"policy": types.RunPolicySpec{AllowAllEgress: true,
		DeniedDomains: []string{"134744072", "8.8.8.8", "0x8080808:443"}}}))
	if err == nil || strings.Count(err.Error(), "non-standard spelling") != 2 || !strings.Contains(err.Error(), `denied_domains[0]: "134744072"`) ||
		!strings.Contains(err.Error(), `denied_domains[2]: "0x8080808:443"`) {
		t.Errorf("two entries: %v", err)
	}
	if n := strings.Count(logs.String(), `level=WARN msg="`+ipSpellingEntryWarning+`"`); n != 2 {
		t.Errorf("two entries: %d warnings, want 2", n)
	}
	for _, v := range []string{"8.8.8.8", "0x8.example.com", "123.example.com", "1e3", "example.com:0"} {
		for _, d := range ipSpellingDoors {
			if v == "example.com:0" && !strings.Contains(d.field, "domains") {
				continue // a host rule or an MCP URL refuses that port in its own check
			}
			logs := captureSlog(t)
			if _, err := LoadConfigBytes(baseConfigJSON(t, d.extra(v))); err != nil {
				t.Errorf("%s = %q: %v, want it to load", d.field, v, err)
			}
			if strings.Contains(logs.String(), ipSpellingEntryWarning) {
				t.Errorf("%s = %q: warned %q", d.field, v, logs.String())
			}
		}
	}
}

// A live deny stored in a non-standard IP spelling binds nothing either, so its config does not load.
func TestALiveDenyInANonStandardIPSpellingDoesNotLoad(t *testing.T) {
	logs := captureSlog(t)
	_, err := LoadConfigBytes(baseConfigJSON(t, map[string]any{"policy": types.RunPolicySpec{AllowAllEgress: true}, "live_denies": []string{"example.com", "134744072"}}))
	want := `config: live_denies[1]: "134744072" writes an IP address in a non-standard spelling, which the proxy ` +
		"refuses in every request, so the entry matches none; its standard form is 8.8.8.8"
	if err == nil || err.Error() != want {
		t.Errorf("got %v\nwant %s", err, want)
	}
	if n := strings.Count(logs.String(), `level=WARN msg="`+ipSpellingEntryWarning+`"`); n != 1 {
		t.Errorf("%d warnings, want 1: %q", n, logs.String())
	}
	if _, err := LoadConfigBytes(baseConfigJSON(t, map[string]any{"live_denies": []string{"example.com", "8.8.8.8"}})); err != nil {
		t.Errorf("a standard live deny list: %v", err)
	}
	// What a live edit relies on: a live deny ValidDomainEntry refuses for its spelling does not load, so a
	// recorded one that loads still passes NormalizeLiveDenies.
	for _, v := range []string{"134744072", "0x7f.1", "010.0.0.1", "010.0.0.1.", "8.8.8.008", "1.2.3", "0x", "0x8080808:443"} {
		if err := ValidDomainEntry(v); err == nil || !strings.Contains(err.Error(), ipSpellingWhy) {
			t.Errorf("ValidDomainEntry(%q) = %v, want the spelling refusal", v, err)
		}
		if _, err := LoadConfigBytes(baseConfigJSON(t, map[string]any{"live_denies": []string{v}})); err == nil {
			t.Errorf("live_denies [%q] loads, and NormalizeLiveDenies refuses it", v)
		}
	}
}
