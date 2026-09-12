// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// V1-D2 + V1-D7. Two defects, one file, because they are the same defect seen
// from two sides.
//
// D2: every post-resolution guard refusal collapsed into builtin:private-ip AND
// into ONE detail sentence — "declare it under internal_hosts … to lift the
// guard", plus B2's "start a new run", plus X-Wardyn-Egress-Retry: never. Only
// blockPrivate (RFC1918/ULA/CGNAT) is liftable: vetHostLift offers its lift
// predicate to that kind alone. A host resolving to loopback, to link-local /
// 169.254.169.254, to a NAT64- or ::/96-embedded blocked v4, or to any other
// reserved range was therefore INSTRUCTED into a kill-and-redispatch cycle that
// cannot succeed, and into widening an SSRF control that would not have helped.
//
// D7: none of those bodies was ever asserted byte-for-byte. The old test checked
// strings.Contains(detail, egressInternalHostsRemedy) — the constants against
// themselves — so the separators, the order, and three of the four sentences
// could be reworded with every gate green. Golden literals below: a reworded
// constant reds HERE, which is exactly what a DRAFT string awaiting a canon
// sitting needs.
//
// The rule SOURCE stays builtin:private-ip on both variants (its AUDIT-ACTIONS
// row is an exact-line citation, and it names the RULE, which is the same rule).
// The retry header stays `never` on both: both are truly final.

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The two composed detail sentences, written out rather than re-joined from the
// constants. See the file header for why.
const (
	goldenLiftableDetail = "this host resolves into a private/reserved address range, which the built-in guard denies regardless of policy; " +
		"declare it in site config under internal_hosts (host_suffix; leave cidrs empty unless you know the ranges the SANDBOX resolves into) to lift the guard for it. " +
		"Leave `cidrs` empty unless you know the addresses the sandbox resolves. What your own machine sees for a private endpoint is usually not what the cluster sees. " +
		"Site config is read at run start, so change it and start a new run — this one will keep being refused."

	goldenLoopbackDetail = "this host's address is a loopback, link-local/metadata, multicast or unspecified address, " +
		"which the proxy denies unconditionally — loopback, link-local (including the 169.254.169.254 metadata address), " +
		"multicast, NAT64- and IPv4-compatible-embedded and the other reserved ranges are never reachable from a sandbox, " +
		"and no internal_hosts entry lifts them. There is nothing to change in site config."

	goldenResolveFailedDetail = "this host did not resolve (DNS failure, no such name, or no address records), so no address " +
		"could be vetted; this is a name-resolution fault, not the private-address guard — check the sandbox's resolver, " +
		"not the allowlist"

	goldenRequireTLSBody = "credential injection for allowed.test requires TLS: this rule sets require_tls and the request was plain HTTP"
)

// denyBody is the 403 body without http.Error's trailing newline.
func denyBody(rec *httptest.ResponseRecorder) string {
	return strings.TrimSuffix(rec.Body.String(), "\n")
}

// driveDeny resolves host to addr and CONNECTs to it, returning the recorder.
func driveDeny(t *testing.T, host, addr string) *httptest.ResponseRecorder {
	t.Helper()
	p, _ := newInternalHostsProxy(t,
		types.RunPolicySpec{AllowedDomains: []string{host}},
		fakeResolver{m: map[string][]net.IP{host: ips(addr)}}, nil, nil, nil, "127.0.0.1:1")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, connectReq(t, host+":443"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("%s -> %s: status = %d, want 403", host, addr, rec.Code)
	}
	return rec
}

// TestLiftableHostnameDenialBodyIsGolden is the variant that KEEPS today's text,
// byte for byte: docs/OPERATIONS.md quotes siteInternalHostsCIDRHint verbatim and
// a guard in cmd/wardynd holds the two together, so this one moving silently
// would break the doc without breaking a test.
func TestLiftableHostnameDenialBodyIsGolden(t *testing.T) {
	rec := driveDeny(t, "priv.internal.test", "10.9.8.7")
	if got := denyBody(rec); got != "egress denied: "+goldenLiftableDetail {
		t.Errorf("RFC1918 hostname 403 body drifted.\n got: %q\nwant: %q", got, "egress denied: "+goldenLiftableDetail)
	}
	if got := rec.Header().Get(egressHeaderDetail); got != goldenLiftableDetail {
		t.Errorf("%s drifted.\n got: %q\nwant: %q", egressHeaderDetail, got, goldenLiftableDetail)
	}
	if got := rec.Header().Get(egressHeaderRetry); got != egressRetryNever {
		t.Errorf("%s = %q, want %q", egressHeaderRetry, got, egressRetryNever)
	}
	if got := rec.Header().Get(egressHeaderReason); got != "builtin:private-ip" {
		t.Errorf("%s = %q, want builtin:private-ip — the rule source is unchanged by this split", egressHeaderReason, got)
	}
}

// TestNeverLiftableHostnameDenialBodyIsGolden is the new variant, byte for byte.
func TestNeverLiftableHostnameDenialBodyIsGolden(t *testing.T) {
	rec := driveDeny(t, "loop.internal.test", "127.0.0.1")
	if got := denyBody(rec); got != "egress denied: "+goldenLoopbackDetail {
		t.Errorf("loopback hostname 403 body drifted.\n got: %q\nwant: %q", got, "egress denied: "+goldenLoopbackDetail)
	}
	if got := rec.Header().Get(egressHeaderRetry); got != egressRetryNever {
		t.Errorf("%s = %q, want %q — a never-liftable refusal is MORE final, not less", egressHeaderRetry, got, egressRetryNever)
	}
	if got := rec.Header().Get(egressHeaderReason); got != "builtin:private-ip" {
		t.Errorf("%s = %q, want builtin:private-ip", egressHeaderReason, got)
	}
}

// TestNeverLiftableClassesPrescribeNothing is the case-per-class table the
// field report needed: whatever the class, the body must not send the operator
// to site config and must not promise a new run will behave differently.
func TestNeverLiftableClassesPrescribeNothing(t *testing.T) {
	for _, tc := range []struct {
		name, addr, class string
	}{
		{"loopback", "127.0.0.1", "loopback, link-local/metadata, multicast or unspecified address"},
		{"link-local metadata", "169.254.169.254", "loopback, link-local/metadata, multicast or unspecified address"},
		{"ipv6 loopback", "::1", "loopback, link-local/metadata, multicast or unspecified address"},
		{"ipv6 link-local", "fe80::1", "loopback, link-local/metadata, multicast or unspecified address"},
		{"nat64-embedded metadata", "64:ff9b::a9fe:a9fe", "NAT64-embedded address"},
		{"ipv4-compatible loopback", "::127.0.0.1", "IPv4-compatible-embedded address"},
		{"reserved (6to4)", "2002:7f00:1::1", "reserved address"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := driveDeny(t, "svc.internal.test", tc.addr)
			body := denyBody(rec)
			if !strings.Contains(body, tc.class) {
				t.Errorf("body does not name the guard class %q:\n\t%s", tc.class, body)
			}
			for _, forbidden := range []string{
				egressInternalHostsRemedy, // there is no site-config entry that lifts this
				siteInternalHostsCIDRHint, // …so there are no cidrs to write either
				egressDenialSuffix,        // …and no new run behaves differently
				"allowed_domains",         // trustsExactLiteralIP gates on blockPrivate
				"egress_redirects",
			} {
				if strings.Contains(body, forbidden) {
					t.Errorf("body prescribes a remedy that cannot work (%q):\n\t%s", forbidden, body)
				}
			}
			if got := rec.Header().Get(egressHeaderRetry); got != egressRetryNever {
				t.Errorf("%s = %q, want %q", egressHeaderRetry, got, egressRetryNever)
			}
		})
	}
}

// TestNeverLiftableLiteralArmPrescribesNothing is the same lie on the LITERAL
// arm, which told a 127.0.0.1 / metadata / NAT64 literal that "only an EXACT
// allowed_domains entry for that address can reach it" — it cannot:
// trustsExactLiteralIP admits blockPrivate and nothing else.
func TestNeverLiftableLiteralArmPrescribesNothing(t *testing.T) {
	pol := CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"127.0.0.1", "169.254.169.254"}})
	for _, host := range []string{"127.0.0.1", "169.254.169.254", "::1", "64:ff9b::a9fe:a9fe"} {
		got := literalIPDenialDetail(host, 443, pol, blockNone)
		if !strings.Contains(got, "never reachable from a sandbox") {
			t.Errorf("%s: detail does not say the address is unreachable:\n\t%s", host, got)
		}
		for _, forbidden := range []string{"allowed_domains", "egress_redirects", egressInternalHostsRemedy} {
			if strings.Contains(got, forbidden) {
				t.Errorf("%s: detail prescribes %q, which cannot reach this address:\n\t%s", host, forbidden, got)
			}
		}
	}
	// The liftable literal keeps its own (accurate) advice.
	if got := literalIPDenialDetail("100.64.5.7", 443, pol, blockNone); !strings.Contains(got, "allowed_domains") {
		t.Errorf("a CGNAT literal lost the remedy that does work:\n\t%s", got)
	}
}

// TestResolveFailedDenialBodyIsGolden and TestRequireTLSDenialBodyIsGolden close
// D7's other half: the two remaining composed refusal bodies, asserted as text
// rather than against the constant that produced them.
func TestResolveFailedDenialBodyIsGolden(t *testing.T) {
	p, _ := newInternalHostsProxy(t,
		types.RunPolicySpec{AllowedDomains: []string{allowedHost}},
		fakeResolver{m: map[string][]net.IP{}}, nil, nil, nil, "127.0.0.1:1")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "http://"+allowedHost+"/"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if got := denyBody(rec); got != "egress denied: "+goldenResolveFailedDetail {
		t.Errorf("resolve-failed 403 body drifted.\n got: %q\nwant: %q", got, "egress denied: "+goldenResolveFailedDetail)
	}
	if got := rec.Header().Get(egressHeaderRetry); got != "" {
		t.Errorf("%s = %q, want unset — a DNS fault may clear on the next attempt", egressHeaderRetry, got)
	}
}

func TestRequireTLSDenialBodyIsGolden(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer upstream.Close()
	p, _ := newTestProxy(t, types.RunPolicySpec{AllowedDomains: []string{"allowed.test"}},
		upstreamAddr(upstream), nil, requireTLSInj("allowed.test", injectedHeader{name: "X-Api-Key", value: "SECRET-KEY"}))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "http://allowed.test/path"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if got := denyBody(rec); got != goldenRequireTLSBody {
		t.Errorf("require-tls 403 body drifted.\n got: %q\nwant: %q", got, goldenRequireTLSBody)
	}
}
