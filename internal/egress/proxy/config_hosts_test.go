// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// configHostSpelling is one host spelling step 0 refuses in every request: as a list entry or map key
// writes it (v), as a URL's host is written (auth, "" when url.Parse refuses every URL with it, so the
// field's own check refuses that config) and as url.URL.Host reads that back (entry), what the load rule
// says is wrong with it and the form it names.
type configHostSpelling struct {
	v, auth, entry, what, form string
	numeric                    bool
}

const (
	numericWhat = "writes an IP address in a non-standard spelling"
	colonWhat   = "is written with a colon or a percent sign"
	asciiWhat   = "has a character outside ASCII"
	twoReadings = "its standard form is 8.0.0.1 or 10.0.0.1, as programs differ on which address it names"
)

func numericSpelling(v, form string) configHostSpelling {
	return configHostSpelling{v: v, auth: v, entry: v, what: numericWhat, form: form, numeric: true}
}

var configHostSpellings = []configHostSpelling{
	numericSpelling("134744072", "its standard form is 8.8.8.8"),
	numericSpelling("0x8080808", "its standard form is 8.8.8.8"),
	numericSpelling("010.0.0.1", twoReadings),
	numericSpelling("8.8.8.008", "its standard form is 8.8.8.8"),
	numericSpelling("0x7f.1", "its standard form is 127.0.0.1"),
	numericSpelling("0x", "it has no standard form, so write the address you mean as four decimal numbers from 0 to 255 with no leading zeros"),
	numericSpelling("1.2.3", "its standard form is 1.2.0.3"),
	numericSpelling("010.0.0.1.", twoReadings),
	// Every trailing dot is taken off, so both readings are still named.
	numericSpelling("010.0.0.1..", twoReadings),
	numericSpelling("0X7F.1", "its standard form is 127.0.0.1"),
	{v: "::ffff:010.0.0.1", what: colonWhat, form: twoReadings},
	{v: "2001:db8::1%eth0", auth: "[2001:db8::1%25eth0]", entry: "[2001:db8::1%eth0]", what: colonWhat, form: "its standard form is 2001:db8::1"},
	{v: "[2001:4860:4860::8888%25eth0]", auth: "[2001:4860:4860::8888%25eth0]", entry: "[2001:4860:4860::8888%eth0]", what: colonWhat,
		form: "its standard form is 2001:4860:4860::8888"},
	{v: "bücher.de", auth: "bücher.de", entry: "bücher.de", what: asciiWhat, form: "its ASCII form is xn--bcher-kva.de"},
	{v: "ｅｘａｍｐｌｅ.com", auth: "ｅｘａｍｐｌｅ.com", entry: "ｅｘａｍｐｌｅ.com", what: asciiWhat, form: "its ASCII form is example.com"},
	{v: "８.８.８.８", auth: "８.８.８.８", entry: "８.８.８.８", what: asciiWhat, form: "its ASCII form is 8.8.8.8"},
	// A config is JSON, which carries a byte that is not UTF-8 ("b\xfccher.de") as U+FFFD: no character, so no form.
	{v: "b\uFFFDcher.de", auth: "b\uFFFDcher.de", entry: "b\uFFFDcher.de", what: asciiWhat,
		form: "it has no ASCII form, so write the host you mean in ASCII: a name as its xn-- form, an IP address in standard form"},
}

// configHostControls are spellings step 0 takes, as a list entry and as a URL's host.
var configHostControls = [][2]string{
	{"8.8.8.8", "8.8.8.8"}, {"::ffff:1.2.3.4", "[::ffff:1.2.3.4]"}, {"2001:db8::1", "[2001:db8::1]"}, {"example.com", "example.com"},
	{"0x8.example.com", "0x8.example.com"}, {"123.example.com", "123.example.com"}, {"xn--bcher-kva.de", "xn--bcher-kva.de"},
}

// configHostDoor is one configured host the load rule reads: its config path for an entry, the config that
// puts v (a list entry or map key) or a URL with host auth in it, and whether a door dials it.
type configHostDoor struct {
	field  func(v string) string
	extra  func(v, auth string) map[string]any
	url    bool
	dialed bool
}

func at(f string) func(string) string { return func(string) string { return f } }

func keyed(f string) func(string) string {
	return func(v string) string { return fmt.Sprintf("%s[%q]", f, v) }
}

// configHostDoors are the configured hosts the load rule reads beyond the two domain lists, live_denies,
// host rules and MCP server URLs. The dialed ones are each door's own host: the git broker's forge
// (pat_grants, ado_grant.hosts), the git_pat API door's (pat_grants), the model route's (llm_upstreams,
// local_models), the AWS door's sign-in portal and the corp proxy every door goes through.
var configHostDoors = []configHostDoor{
	{field: at("injection[0].host"), extra: func(v, _ string) map[string]any {
		return map[string]any{"injection": []map[string]any{{"host": v}}}
	}},
	{field: at("mitm_hosts[0]"), extra: func(v, _ string) map[string]any { return map[string]any{"mitm_hosts": []string{v}} }},
	{field: keyed("git_push_withheld"), extra: func(v, _ string) map[string]any {
		return map[string]any{"git_push_withheld": map[string]string{v: "withheld"}}
	}},
	{field: keyed("pat_grants"), dialed: true, extra: func(v, _ string) map[string]any {
		return map[string]any{"pat_grants": map[string]any{v: map[string]any{}}}
	}},
	{field: at("ado_grant.hosts[0]"), dialed: true, extra: func(v, _ string) map[string]any {
		return map[string]any{"ado_grant": map[string]any{"organization": "acme", "hosts": []string{v}}}
	}},
	{field: at("azure_gates[0].host"), extra: func(v, _ string) map[string]any {
		return map[string]any{"azure_gates": []map[string]any{{"host": v}}}
	}},
	{field: at("aws_sigv4.portal_host"), dialed: true, extra: func(v, _ string) map[string]any {
		return map[string]any{"aws_sigv4": map[string]any{"portal_host": v}}
	}},
	{field: at("aws_sigv4.hosts[0]"), extra: func(v, _ string) map[string]any {
		return map[string]any{"aws_sigv4": map[string]any{"portal_host": "portal.example.com", "hosts": []string{v}}}
	}},
	{field: at("aws_sigv4.gateways[0].host"), extra: func(v, _ string) map[string]any {
		return map[string]any{"aws_sigv4": map[string]any{"portal_host": "portal.example.com", "gateways": []map[string]any{{"host": v}}}}
	}},
	{field: at("kube_api.host"), extra: func(v, _ string) map[string]any { return map[string]any{"kube_api": map[string]any{"host": v}} }},
	{field: at("upstream_proxy_url"), url: true, dialed: true, extra: func(_, auth string) map[string]any {
		return map[string]any{"upstream_proxy_url": "http://user:secret@" + auth}
	}},
	{field: at("upstream_proxy_no_proxy[0]"), extra: func(v, _ string) map[string]any {
		return map[string]any{"upstream_proxy_no_proxy": []string{v}}
	}},
	{field: at("internal_hosts[0].host_suffix"), extra: func(v, _ string) map[string]any {
		return map[string]any{"internal_hosts": []map[string]any{{"host_suffix": v}}}
	}},
	{field: at(`llm_upstreams["anthropic"]`), url: true, dialed: true, extra: func(_, auth string) map[string]any {
		return map[string]any{"llm_upstreams": map[string]string{"anthropic": "https://" + auth + "/v1?key=secret"}}
	}},
	{field: keyed("llm_channel_hosts"), extra: func(v, _ string) map[string]any {
		return map[string]any{"llm_channel_hosts": map[string]string{v: "anthropic"}}
	}},
	{field: keyed("llm_headers"), extra: func(v, _ string) map[string]any {
		return map[string]any{"llm_headers": map[string]any{v: map[string]string{"x-team": "a"}}}
	}},
	{field: func(string) string { return "" }, url: true, dialed: true, extra: func(_, auth string) map[string]any {
		return map[string]any{"local_models": map[string]any{"http://" + auth + ":11434": map[string]any{}}}
	}},
	{field: keyed("bedrock_routes"), extra: func(v, _ string) map[string]any {
		return map[string]any{"bedrock_routes": map[string]string{v: "us-east-1"}}
	}},
	{field: at("gateway_identity[0].host"), extra: func(v, _ string) map[string]any {
		return map[string]any{"gateway_identity": []map[string]any{{"host": v}}}
	}},
	{field: at("forge_hosts[0]"), extra: func(v, _ string) map[string]any { return map[string]any{"forge_hosts": []string{v}} }},
	{field: at("via_org_routes[0].host"), extra: func(v, _ string) map[string]any {
		return map[string]any{"via_org_routes": []map[string]any{{"grant_id": uuid.NewString(), "host": v}}}
	}},
	{field: at("resident_mints[0].host"), extra: func(v, _ string) map[string]any {
		return map[string]any{"resident_mints": []map[string]any{{"grant_id": uuid.NewString(), "host": v}}}
	}},
}

// loadRecord is one record the load logs.
type loadRecord struct {
	Level, Msg, Field, Entry string
	StandardForm             string `json:"standard_form"`
}

// captureLoadLog records what slog logs until the test ends.
func captureLoadLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

func loadRecords(t *testing.T, buf *bytes.Buffer) []loadRecord {
	t.Helper()
	var out []loadRecord
	for dec := json.NewDecoder(buf); ; {
		var r loadRecord
		if err := dec.Decode(&r); err == io.EOF {
			return out
		} else if err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
}

// Every configured host a door dials or a request is matched against, stored in a spelling step 0 refuses
// in every request (an address in a non-standard spelling, all-numeric or with a colon or a percent sign,
// or a character outside ASCII), stops its config from loading. The error and one warning name the field,
// the entry and its standard or ASCII form, and a URL by its host alone. A host a door dials is said to
// be one the proxy does not dial.
func TestEveryConfiguredHostInARefusedSpellingDoesNotLoad(t *testing.T) {
	for _, s := range configHostSpellings {
		for _, d := range configHostDoors {
			entry, field := s.v, d.field(s.v)
			if d.url && s.auth == "" {
				continue
			}
			if d.url {
				entry = s.entry
			}
			if field == "" { // local_models: keyed by the URL, which names no secret
				field = fmt.Sprintf("local_models[%q]", "http://"+s.auth+":11434")
				entry = s.entry + ":11434"
			}
			consequence, warning := "which the proxy refuses in every request, so the entry matches none", configHostEntryWarning
			if d.dialed {
				consequence = "which programs read differently, so the proxy does not dial it"
			} else if s.numeric {
				warning = ipSpellingEntryWarning
			}
			logs := captureLoadLog(t)
			_, err := LoadConfigBytes(baseConfigJSON(t, d.extra(s.v, s.auth)))
			want := fmt.Sprintf("config: %s: %q %s, %s; %s", field, entry, s.what, consequence, s.form)
			if err == nil || err.Error() != want {
				t.Errorf("%s = %q:\n got %v\nwant %s", field, s.v, err, want)
			}
			recs := loadRecords(t, logs)
			if len(recs) != 1 || recs[0] != (loadRecord{Level: "WARN", Msg: warning, Field: field, Entry: entry, StandardForm: s.form}) {
				t.Errorf("%s = %q: logged %+v, want one %q warning naming the entry and its form", field, s.v, recs, warning)
			}
			if err != nil && strings.Contains(err.Error()+logs.String(), "secret") {
				t.Errorf("%s = %q: a URL's secret is named", field, s.v)
			}
		}
	}
}

// The two domain lists, live_denies, host rules and MCP server URLs refuse the colon and percent forms
// and a character outside ASCII as they refuse an all-numeric spelling.
func TestADomainHostRuleOrMCPEntryInAnyRefusedSpellingDoesNotLoad(t *testing.T) {
	doors := append(ipSpellingDoors[:len(ipSpellingDoors):len(ipSpellingDoors)], struct {
		field string
		extra func(v string) map[string]any
	}{"live_denies[0]", func(v string) map[string]any { return map[string]any{"live_denies": []string{v}} }})
	for _, s := range configHostSpellings {
		if s.numeric {
			continue // TestAConfigEntryInANonStandardIPSpellingDoesNotLoad
		}
		for _, d := range doors {
			v, entry := s.v, s.v
			if d.field == "mcp_servers[0].url" {
				if s.auth == "" {
					continue
				}
				v, entry = s.auth, s.entry
			}
			logs := captureLoadLog(t)
			_, err := LoadConfigBytes(baseConfigJSON(t, d.extra(v)))
			want := fmt.Sprintf("config: %s: %q %s, which the proxy refuses in every request, so the entry matches none; %s",
				d.field, entry, s.what, s.form)
			if err == nil || err.Error() != want {
				t.Errorf("%s = %q:\n got %v\nwant %s", d.field, v, err, want)
			}
			recs := loadRecords(t, logs)
			if len(recs) != 1 || recs[0] != (loadRecord{Level: "WARN", Msg: configHostEntryWarning, Field: d.field, Entry: entry, StandardForm: s.form}) {
				t.Errorf("%s = %q: logged %+v", d.field, v, recs)
			}
		}
	}
}

// The spellings step 0 takes pass the load rule in every field.
func TestEveryConfiguredHostInAStandardSpellingPassesTheLoadRule(t *testing.T) {
	for _, s := range configHostControls {
		for _, d := range configHostDoors {
			var c Config
			if err := json.Unmarshal(baseConfigJSON(t, d.extra(s[0], s[1])), &c); err != nil {
				t.Fatal(err)
			}
			logs := captureLoadLog(t)
			if err := c.refuseIPSpellingEntries(); err != nil || logs.Len() != 0 {
				t.Errorf("%s = %q: %v, logged %q; want it to pass", d.field(s[0]), s[0], err, logs)
			}
		}
	}
}

// The write doors in this package hold a host to the load rule's own words: ValidConfigHost, the no_proxy
// entry and the upstream proxy URL. A domain entry with a character outside ASCII names its ASCII form
// where IDNA gives one that step 0 takes.
func TestConfigHostWriteDoorsRefuseWhatLoadRefuses(t *testing.T) {
	for _, s := range append(configHostSpellings, configHostSpelling{v: "b\xfccher.de", what: asciiWhat,
		form: "it has no ASCII form, so write the host you mean in ASCII: a name as its xn-- form, an IP address in standard form"}) {
		want := fmt.Sprintf("%q %s, which the proxy refuses in every request; %s", s.v, s.what, s.form)
		if err := ValidConfigHost(s.v); err == nil || err.Error() != want {
			t.Errorf("ValidConfigHost(%q):\n got %v\nwant %s", s.v, err, want)
		}
		if ValidNoProxyEntry(s.v) {
			t.Errorf("ValidNoProxyEntry(%q) = true", s.v)
		}
	}
	for _, s := range configHostControls {
		if err := ValidConfigHost(s[0]); err != nil {
			t.Errorf("ValidConfigHost(%q) = %v", s[0], err)
		}
		if err := ValidConfigHost(s[1] + ":443"); err != nil {
			t.Errorf("ValidConfigHost(%q) = %v", s[1]+":443", err)
		}
	}
	for _, e := range []string{"8.8.8.8", "example.com", ".corp.example.com", "10.0.0.0/8"} {
		if !ValidNoProxyEntry(e) {
			t.Errorf("ValidNoProxyEntry(%q) = false", e)
		}
	}
	err := ValidUpstreamProxyURL("http://user:secret@134744072:3128")
	if want := `upstream proxy url: "134744072" writes an IP address in a non-standard spelling, which the proxy refuses in every request; its standard form is 8.8.8.8`; err == nil || err.Error() != want {
		t.Errorf("ValidUpstreamProxyURL:\n got %v\nwant %s", err, want)
	}
	if err := ValidUpstreamProxyURL("http://user:secret@proxy.corp.example.com:3128"); err != nil {
		t.Errorf("a standard upstream proxy URL: %v", err)
	}
	for d, want := range map[string]string{
		"bücher.de":    charsetWhy + "; its ASCII form is xn--bcher-kva.de",
		"*.bücher.de":  charsetWhy + "; its ASCII form is *.xn--bcher-kva.de",
		"８.８.８.８:443":  charsetWhy + "; its ASCII form is 8.8.8.8",
		"０１０.0.0.1":    charsetWhy,
		"b\xfccher.de": charsetWhy,
	} {
		err := ValidDomainEntry(d)
		if want := fmt.Sprintf("domain %q never matches any request (%s)", d, want); err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("ValidDomainEntry(%q):\n got %v\nwant %s…", d, err, want)
		}
	}
}

// Two entries of one table the proxy keys as one host do not load: the table would keep one of them, and
// which one depends on map order. Both are named as written.
func TestTwoEntriesWithOneHostKeyDoNotLoad(t *testing.T) {
	for _, tc := range []struct {
		extra map[string]any
		want  string
	}{
		{map[string]any{"pat_grants": map[string]any{"1.2.3.7": map[string]any{}, "::ffff:1.2.3.7": map[string]any{}}},
			`config: pat_grants: "1.2.3.7" and "::ffff:1.2.3.7" name one host; keep one`},
		{map[string]any{"pat_grants": map[string]any{"GitLab.example.com": map[string]any{}, "gitlab.example.com.": map[string]any{}}},
			`config: pat_grants: "GitLab.example.com" and "gitlab.example.com." name one host; keep one`},
		{map[string]any{"git_push_withheld": map[string]string{"1.2.3.7": "a", "::ffff:1.2.3.7": "b"}},
			`config: git_push_withheld: "1.2.3.7" and "::ffff:1.2.3.7" name one host; keep one`},
		{map[string]any{"llm_channel_hosts": map[string]string{"1.2.3.7": "anthropic", "::ffff:1.2.3.7": "openai"}},
			`config: llm_channel_hosts: "1.2.3.7" and "::ffff:1.2.3.7" name one host; keep one`},
		{map[string]any{"llm_headers": map[string]any{"1.2.3.7": map[string]string{"x": "a"}, "::ffff:1.2.3.7": map[string]string{"x": "b"}}},
			`config: llm_headers: "1.2.3.7" and "::ffff:1.2.3.7" name one host; keep one`},
		{map[string]any{"bedrock_routes": map[string]string{"1.2.3.7": "us-east-1", "::ffff:1.2.3.7": "eu-west-1"}},
			`config: bedrock_routes: "1.2.3.7" and "::ffff:1.2.3.7" name one host; keep one`},
		{map[string]any{"local_models": map[string]any{"http://[::ffff:1.2.3.6]:11434": map[string]any{}, "http://1.2.3.6:11434": map[string]any{}}},
			`config: local_models: "http://1.2.3.6:11434" and "http://[::ffff:1.2.3.6]:11434" name one host and port; keep one`},
		{map[string]any{"local_models": map[string]any{"https://1.2.3.6": map[string]any{}, "https://1.2.3.6:443": map[string]any{}}},
			`config: local_models: "https://1.2.3.6" and "https://1.2.3.6:443" name one host and port; keep one`},
		{map[string]any{"via_org_routes": []map[string]any{{"grant_id": uuid.NewString(), "host": "1.2.3.7"}, {"grant_id": uuid.NewString(), "host": "::ffff:1.2.3.7"}}},
			`config: via_org_routes[0].host "1.2.3.7" and via_org_routes[1].host "::ffff:1.2.3.7" name one host for two grants; keep one`},
	} {
		if _, err := LoadConfigBytes(baseConfigJSON(t, tc.extra)); err == nil || err.Error() != tc.want {
			t.Errorf("got %v\nwant %s", err, tc.want)
		}
	}
	// Different hosts, another port, and one grant routed twice do not collide.
	g := uuid.NewString()
	for _, extra := range []map[string]any{
		{"pat_grants": map[string]any{"1.2.3.7": map[string]any{}, "1.2.3.8": map[string]any{}}},
		{"local_models": map[string]any{"http://1.2.3.6:11434": map[string]any{}, "http://1.2.3.6:11435": map[string]any{}, "https://1.2.3.6": map[string]any{}}},
		{"via_org_routes": []map[string]any{{"grant_id": g, "host": "1.2.3.7"}, {"grant_id": g, "host": "::ffff:1.2.3.7"}}},
	} {
		var c Config
		if err := json.Unmarshal(baseConfigJSON(t, extra), &c); err != nil {
			t.Fatal(err)
		}
		if err := c.refuseHostKeyCollisions(); err != nil {
			t.Errorf("%v: %v", extra, err)
		}
	}
}

// The git broker finds a run's Azure DevOps grant by the key its table is built with, so every spelling
// of a covered address reaches it: here the grant's organisation refusal, which only that lane gives.
func TestGitBrokerFindsTheAzureDevOpsGrantByItsHostKey(t *testing.T) {
	p := newProxy(Options{
		RunID:     uuid.New(),
		Policy:    CompilePolicy(types.RunPolicySpec{}),
		Sink:      &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 16)},
		Resolver:  publicResolver{},
		RunToken:  newTokenSource("RUNTOK"),
		ADOGrants: newADOGrantsByHost(&ADOGrantConfig{Organization: "acme", Hosts: []string{"1.2.3.11"}}),
	})
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	for host, want := range map[string]string{
		"1.2.3.11":        `this run is granted the "acme" Azure DevOps organisation only`,
		"::ffff:1.2.3.11": `this run is granted the "acme" Azure DevOps organisation only`,
		"1.2.3.12":        "host not granted to this run",
	} {
		resp, err := http.Get(srv.URL + routePATBroker + host + "/evil/proj/_git/app/info/refs?service=git-upload-pack")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if !strings.Contains(string(body), want) {
			t.Errorf("%s: %d %q, want %q", host, resp.StatusCode, body, want)
		}
	}
}
