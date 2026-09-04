// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"testing"
)

// stubListenLookup replaces the boot classifier's resolver for one test, so
// these assertions never depend on the test host's DNS or /etc/hosts.
func stubListenLookup(t *testing.T, byHost map[string][]string) {
	t.Helper()
	prev := lookupListenIPs
	t.Cleanup(func() { lookupListenIPs = prev })
	lookupListenIPs = func(_ context.Context, host string) ([]net.IPAddr, error) {
		addrs, ok := byHost[host]
		if !ok {
			return nil, errors.New("no such host")
		}
		out := make([]net.IPAddr, 0, len(addrs))
		for _, a := range addrs {
			out = append(out, net.IPAddr{IP: net.ParseIP(a)})
		}
		return out, nil
	}
}

// TestListenClassifiersResolveHostnames pins the boot classifier that two
// refusals depend on: -local-trust-forwarder (which DISABLES the unspoofable
// loopback-peer gate on the unauthenticated local surface) and the
// plaintext-listen refusal (which stops cookies travelling in cleartext to LAN
// peers). Both ask one question — does this address bind a specific non-loopback
// interface a LAN peer can reach — and both used to answer "no" for ANY
// hostname, on the reasoning "a hostname we can't classify — don't refuse". So
// naming the LAN interface instead of numbering it skipped the refusal:
// WARDYN_LISTEN=lan-host.corp:8080 booted with the peer gate off.
//
// The table is the whole rule, and the SILENT rows matter as much as the loud
// one: a boot guard that fires on a merely unusual configuration gets filtered
// out of the logs within a week, and is then missing for the deployment that
// needed it.
func TestListenClassifiersResolveHostnames(t *testing.T) {
	stubListenLookup(t, map[string][]string{
		"lan-host.corp":   {"192.168.1.50"},
		"loop-alias.corp": {"127.0.0.1"},
		"dual.corp":       {"::1", "10.0.0.7"}, // loopback AND a LAN address
	})

	for _, c := range []struct {
		name         string
		listen       string
		wantRoutable bool
		wantLoopback bool
		why          string
	}{
		// The defect: a hostname naming the very interface the refusal exists for.
		{"hostname resolving to a LAN address", "lan-host.corp:8080", true, false,
			"binds the specific LAN interface the -local-trust-forwarder refusal exists to catch"},
		// Same answer as the literal it resolves to — the point of the fix.
		{"the literal it resolves to", "192.168.1.50:8080", true, false, "unchanged"},
		// Quiet: safe, merely unusual.
		{"hostname resolving only to loopback", "loop-alias.corp:8080", false, true,
			"an /etc/hosts alias for 127.0.0.1 is exactly as safe as the literal"},
		// Fail closed on the routable question, and NOT loopback-only.
		{"dual-stack: one loopback, one LAN", "dual.corp:8080", true, false,
			"one LAN-reachable address is enough to re-open the surface; loopback-only is a claim about the whole set"},
		// Quiet: cannot classify, and the bind will fail seconds later anyway.
		{"hostname that does not resolve", "nope.invalid:8080", false, false,
			"refusing boot on a resolver blip is the false alarm that gets a guard ignored"},
		// The pre-existing rows, unchanged.
		{"loopback literal", "127.0.0.1:8080", false, true, "unchanged"},
		{"unspecified bind", "0.0.0.0:8080", false, false, "indistinguishable from the safe compose publish"},
		{"localhost", "localhost:8080", false, true, "unchanged"},
		{"empty", "", false, false, "unchanged"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := listenBindsSpecificRoutable(c.listen); got != c.wantRoutable {
				t.Errorf("listenBindsSpecificRoutable(%q) = %v, want %v — %s", c.listen, got, c.wantRoutable, c.why)
			}
			if got := listenIsLoopback(c.listen); got != c.wantLoopback {
				t.Errorf("listenIsLoopback(%q) = %v, want %v — %s", c.listen, got, c.wantLoopback, c.why)
			}
		})
	}
}

// TestLocalTrustForwarderRefusesAHostnameBind is the refusal itself, driven
// through resolveLocalMode rather than the classifier — the classifier is the
// mechanism, this is the boot outcome an operator actually gets.
func TestLocalTrustForwarderRefusesAHostnameBind(t *testing.T) {
	stubListenLookup(t, map[string][]string{
		"lan-host.corp":   {"192.168.1.50"},
		"loop-alias.corp": {"127.0.0.1"},
	})

	for _, c := range []struct {
		name       string
		listen     string
		wantRefuse bool
	}{
		{"hostname on a LAN interface refuses", "lan-host.corp:8080", true},
		{"hostname on loopback boots", "loop-alias.corp:8080", false},
		{"unresolvable hostname boots (warn-only)", "nope.invalid:8080", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := testLocalModeFlags(c.listen, true)
			_, err := resolveLocalMode(f)
			if c.wantRefuse && err == nil {
				t.Fatalf("-local-trust-forwarder with listen %q booted; want a refusal — the loopback-peer gate is "+
					"disabled and this binds a LAN interface", c.listen)
			}
			if !c.wantRefuse && err != nil {
				t.Fatalf("-local-trust-forwarder with listen %q refused: %v; want boot", c.listen, err)
			}
		})
	}
}

// testLocalModeFlags builds the minimal bootFlags resolveLocalMode reads: local
// mode ON with no other auth configured, so the -local-trust-forwarder branch is
// the only thing the assertions above vary.
func testLocalModeFlags(listen string, trustFwd bool) *bootFlags {
	l, tf, lmOn := listen, trustFwd, true
	return &bootFlags{
		listen:        &l,
		adminToken:    new(string),
		localMode:     &lmOn,
		localOperator: new(string),
		oidcIssuer:    new(string),
		localTrustFwd: &tf,
		// resolveLocalMode continues past the local-mode branch into the
		// host-mode Bedrock auto-detect; these three are read there. Without
		// them the function nil-derefs and the panic stands in for the
		// assertion, which is a green that means nothing.
		bedrockRegion: new(string),
		bedrockModel:  new(string),
		bedrockAWSDir: new(string),
	}
}

// TestSecondHumanBootWarningFiresOnlyForTheBrokenCombination pins the boot
// warning R1 owes from the four-eyes fix: WARDYN_EGRESS_SECOND_HUMAN cannot be
// enforced in local mode (that mode authenticates nobody, so requireSecondHuman
// refuses outright with a 503 per decision), and an operator who set both would
// otherwise discover it when their first approval hangs.
//
// THE SILENT ROWS ARE THE TEST. A boot warning that fires on a merely unusual
// configuration gets filtered out of the logs within a week, and is then missing
// for the deployment that needed it — so this asserts the two half-configurations
// stay quiet as hard as it asserts the broken one speaks.
func TestSecondHumanBootWarningFiresOnlyForTheBrokenCombination(t *testing.T) {
	stubListenLookup(t, map[string][]string{})

	for _, c := range []struct {
		name      string
		localMode bool
		switchOn  bool
		wantWarn  bool
	}{
		{"local mode + switch on: the broken combination", true, true, true},
		{"local mode, switch off: nothing to say", true, false, false},
		{"switch on, NOT local mode: the switch works normally", false, true, false},
		{"neither", false, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.switchOn {
				t.Setenv("WARDYN_EGRESS_SECOND_HUMAN", "1")
			} else {
				t.Setenv("WARDYN_EGRESS_SECOND_HUMAN", "")
			}
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			f := testLocalModeFlags("127.0.0.1:8080", false)
			*f.localMode = c.localMode
			if !c.localMode {
				// A configured admin token is what keeps local mode OFF (and off
				// the auto-enable heuristic) without changing anything else.
				*f.adminToken = "tok"
			}
			if _, err := resolveLocalMode(f); err != nil {
				t.Fatalf("resolveLocalMode: %v", err)
			}
			got := strings.Contains(buf.String(), "four-eyes gate cannot be enforced")
			if got != c.wantWarn {
				t.Errorf("boot warning fired = %v, want %v\nlog:\n%s", got, c.wantWarn, buf.String())
			}
			if c.wantWarn && !strings.Contains(buf.String(), "Configure SSO") {
				t.Errorf("the warning must name the remedy; log:\n%s", buf.String())
			}
		})
	}
}

// TestListenIsRoutablePublicResolvesHostnames closes the third classifier's
// hole (F067). listenIsLoopback and listenBindsSpecificRoutable both resolve a
// hostname through listenHostIPs; listenIsRoutablePublic alone still did
// net.ParseIP and returned false for anything that was not a literal. It is the
// fail-closed gate for -local-mode — "a no-auth public API must never be served
// on a public IP" — so naming the public interface instead of numbering it
// booted an UNAUTHENTICATED admin API on the internet with only a warning.
//
// ANY, not all, exactly like listenBindsSpecificRoutable: one publicly-routable
// address is enough to be serving no-auth on the internet.
func TestListenIsRoutablePublicResolvesHostnames(t *testing.T) {
	stubListenLookup(t, map[string][]string{
		"public-host.example.com": {"203.0.113.10"},
		"lan-host.corp":           {"192.168.1.50"},
		"loop-alias.corp":         {"127.0.0.1"},
		"split.example.com":       {"10.0.0.7", "198.51.100.9"}, // private AND public
	})

	for _, c := range []struct {
		name   string
		listen string
		want   bool
		why    string
	}{
		{"hostname resolving to a public IP", "public-host.example.com:8080", true,
			"the -local-mode refusal exists for exactly this bind"},
		{"the literal it resolves to", "203.0.113.10:8080", true, "unchanged"},
		{"hostname resolving to a private IP", "lan-host.corp:8080", false,
			"private is the -local-trust-forwarder classifier's business, not this one"},
		{"hostname resolving only to loopback", "loop-alias.corp:8080", false, "safe"},
		{"split private/public", "split.example.com:8080", true,
			"ANY: one public address is enough to be serving no-auth on the internet"},
		{"hostname that does not resolve", "nope.invalid:8080", false,
			"a resolver blip must not refuse boot"},
		{"loopback literal", "127.0.0.1:8080", false, "unchanged"},
		{"private literal", "10.0.0.5:8080", false, "unchanged"},
		{"unspecified bind", "0.0.0.0:8080", false, "warned, never refused"},
		{"empty host", ":8080", false, "unchanged"},
		{"localhost", "localhost:8080", false, "unchanged"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := listenIsRoutablePublic(c.listen); got != c.want {
				t.Errorf("listenIsRoutablePublic(%q) = %v, want %v — %s", c.listen, got, c.want, c.why)
			}
		})
	}
}

// TestEmptyListenNeverReachesTheClassifiers closes F011. WARDYN_LISTEN="" (a
// `docker run -e WARDYN_LISTEN` with no value, a compose `WARDYN_LISTEN=`
// passthrough, or `-listen=`) used to survive all the way to net/http, whose
// Server.Addr == "" means ":http" — 0.0.0.0:80. Every classifier read "" as
// "cannot classify" and skipped ALL THREE listen-based boot refusals on the
// way there. An empty bind states no intent, so it falls back to the same
// default the -listen usage string advertises.
func TestEmptyListenNeverReachesTheClassifiers(t *testing.T) {
	for _, raw := range []string{"", "   ", "\t"} {
		got := normalizeListenAddr(raw)
		if got != defaultListenAddr {
			t.Errorf("normalizeListenAddr(%q) = %q, want the documented default %q",
				raw, got, defaultListenAddr)
		}
	}
	for _, raw := range []string{":8080", "127.0.0.1:9000", "0.0.0.0:80", "lan-host.corp:8080"} {
		if got := normalizeListenAddr(raw); got != raw {
			t.Errorf("normalizeListenAddr(%q) = %q, want it left alone", raw, got)
		}
	}
}
