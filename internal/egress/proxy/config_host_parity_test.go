// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A bypass list entry is read in one form at write and at load (normalizeNoProxyEntry), so a leading dot
// or a space carries an address in a non-standard spelling past neither.
func TestABypassEntryIsReadInOneFormAtWriteAndAtLoad(t *testing.T) {
	load := func(e string) error {
		_, err := LoadConfigBytes(baseConfigJSON(t, map[string]any{
			"upstream_proxy_url": "http://proxy.corp.example.com:3128", "upstream_proxy_no_proxy": []string{e},
		}))
		return err
	}
	for _, e := range []string{".010.0.0.1", " .134744072 ", ".0x7f.1."} {
		if ValidNoProxyEntry(e) {
			t.Errorf("ValidNoProxyEntry(%q) = true", e)
		}
		want := "config: upstream_proxy_no_proxy[0]: " + strconv.Quote(e) + " writes an IP address in a non-standard spelling, which the proxy refuses in every request"
		if err := load(e); err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("a stored bypass entry %q:\n got %v\nwant %s…", e, err, want)
		}
	}
	for _, e := range []string{".corp.example.com", " .10.example.com. ", "10.0.0.1"} {
		if !ValidNoProxyEntry(e) {
			t.Errorf("ValidNoProxyEntry(%q) = false", e)
		}
		if err := load(e); err != nil {
			t.Errorf("a stored bypass entry %q: %v", e, err)
		}
	}
}

// A host with a character outside ASCII and a colon or a percent sign is named for the first, in step 0's order.
func TestAHostOutsideASCIIWithAColonIsNamedForItsCharacter(t *testing.T) {
	const host = "[2001:db8::1%ｅth0]"
	want := strconv.Quote(host) + " has a character outside ASCII, which the proxy refuses in every request; it has no ASCII form, " +
		"so write the host you mean in ASCII: a name as its xn-- form, an IP address in standard form"
	if err := ValidConfigHost(host); err == nil || err.Error() != want {
		t.Errorf("ValidConfigHost(%q):\n got %v\nwant %s", host, err, want)
	}
}

// Config load gives a host step 0's verdict (nonASCIIHost, nonStandardIPSpelling): it passes every
// spelling step 0 passes, an IPv6 authority with a trailing dot such as [::1]. among them, and refuses
// every spelling step 0 refuses. A port does not change the verdict.
func TestConfigHostRuleGivesStepZerosVerdict(t *testing.T) {
	for _, h := range []string{
		"example.com", "example.com.", "EXAMPLE.com", "a-b.example", "xn--bcher-kva.de", "10.0.0.1", "10.0.0.1.",
		"010.0.0.1", "010.0.0.1..", "134744072", "0x7f.1", "1.2.3.077", "::1", "[::1]", "[::1].", "[::1]..",
		"::ffff:1.2.3.4", "[::ffff:1.2.3.4].", "::ffff:010.0.0.1", "2001:db8::1%eth0", "[2001:db8::1%25eth0]",
		"1.2.3.4%eth0", "bücher.de", "８.８.８.８", "8.8.8.8。",
	} {
		step0 := nonASCIIHost(h) || nonStandardIPSpelling(h)
		if _, refused := configHostRefusal(h); refused != step0 {
			t.Errorf("%q: config load refuses it = %v, step 0 refuses it = %v", h, refused, step0)
		}
		if !strings.Contains(h, ":") || strings.HasSuffix(h, "]") {
			if _, refused := configHostRefusal(h + ":8443"); refused != step0 {
				t.Errorf("%q with a port: config load refuses it = %v, step 0 refuses the host = %v", h, refused, step0)
			}
		}
	}
}

// An upstream proxy URL that ValidUpstreamProxyURL refuses for its host's spelling alone names a real
// proxy, which the control plane delivers so the run's proxy refuses it; no other refusal is that one.
func TestUpstreamProxyURLHostRefusedIsTheSpellingRefusalAlone(t *testing.T) {
	for raw, want := range map[string]bool{
		"http://010.0.0.1:3128": true, "http://user:pass@134744072:3128": true, "http://[2001:db8::1%25eth0]:3128": true,
		"http://bücher.de:3128": true, "http://proxy.corp.example:3128": false, "http://10.0.0.1:3128": false,
		"http://proxy.corp.example:0": false, "http://010.0.0.1:0": false, "https://010.0.0.1:3128": false, "http://": false, "": false,
		"http://[::ffff:010.0.0.1]:3128": false, // url.Parse refuses it: malformed, as before
	} {
		if got := UpstreamProxyURLHostRefused(raw); got != want {
			t.Errorf("UpstreamProxyURLHostRefused(%q) = %v, want %v", raw, got, want)
		}
		if want && ValidUpstreamProxyURL(raw) == nil {
			t.Errorf("ValidUpstreamProxyURL(%q) = nil, want the host's refusal", raw)
		}
	}
}

// The refusal reads the host without its brackets or trailing dots, however many there are: an address is
// named in standard form, an all-numeric spelling and a host outside ASCII each by their own arm.
func TestARefusedBracketedHostIsNamedWithoutItsBracketsOrDots(t *testing.T) {
	for host, want := range map[string]string{
		"[::1]..":     "is written with a colon or a percent sign, which the proxy refuses in every request; its standard form is ::1",
		"[010.0.0.1]": "writes an IP address in a non-standard spelling, which the proxy refuses in every request; its standard form is 8.0.0.1 or 10.0.0.1, as programs differ on which address it names",
		"[bücher.de]": "has a character outside ASCII, which the proxy refuses in every request; its ASCII form is xn--bcher-kva.de",
	} {
		if err := ValidConfigHost(host); err == nil || err.Error() != `"`+host+`" `+want {
			t.Errorf("ValidConfigHost(%q):\n got %v\nwant %q %s", host, err, host, want)
		}
	}
}

// The upstream proxy's host is held to the rule where a server is built too, for a Config whose URL was set
// after its load, as the organisation's via_org engine's is: no engine is built to dial such a host.
func TestNoViaOrgEngineIsBuiltOnAnUpstreamProxyHostConfigLoadRefuses(t *testing.T) {
	for raw, refused := range map[string]bool{
		"http://user:pass@010.0.0.1:3128": true, "http://bücher.de:3128": true, " http://134744072:3128": true,
		"http://10.0.0.1:3128": false, "http://proxy.corp.example:3128": false,
	} {
		cfg := &Config{RunID: uuid.New(), ControlPlaneURL: "https://wardynd.test", RunToken: "RUNTOK", DecisionBufferSize: defaultBufferSize,
			Policy: types.RunPolicySpec{AllowAllEgress: true}, UpstreamProxyURL: raw}
		e, err := NewViaOrgEngine(context.Background(), cfg, ViaOrgEngineOptions{Client: &http.Client{Transport: &fakeOrg{}}})
		if e != nil {
			_ = e.Close(context.Background())
		}
		if (err != nil) != refused || (refused && (!strings.HasPrefix(err.Error(), "upstream proxy: \"") || strings.Contains(err.Error(), "user:pass"))) {
			t.Errorf("an engine on upstream %q: %v, want refused = %v, naming the host and no credential", raw, err, refused)
		}
		// The refusal is told apart from every other build failure: it is permanent, so no retry helps.
		if errors.Is(err, ErrUpstreamProxyHostRefused) != refused {
			t.Errorf("an engine on upstream %q: errors.Is(%v, ErrUpstreamProxyHostRefused) = %v, want %v", raw, err, !refused, refused)
		}
	}
	// A URL refused for another reason is not that refusal.
	cfg := &Config{RunID: uuid.New(), ControlPlaneURL: "https://wardynd.test", RunToken: "RUNTOK", DecisionBufferSize: defaultBufferSize,
		Policy: types.RunPolicySpec{AllowAllEgress: true}, UpstreamProxyURL: "http://proxy.corp.example:0"}
	if _, err := NewViaOrgEngine(context.Background(), cfg, ViaOrgEngineOptions{Client: &http.Client{Transport: &fakeOrg{}}}); err == nil ||
		errors.Is(err, ErrUpstreamProxyHostRefused) {
		t.Errorf("an engine on an upstream whose port is refused: %v, want a build failure that is not ErrUpstreamProxyHostRefused", err)
	}
}
