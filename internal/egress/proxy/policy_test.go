// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"encoding/json"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestEvalHost(t *testing.T) {
	spec := types.RunPolicySpec{
		AllowedDomains: []string{"api.github.com", "*.example.com", "Example.ORG"},
		DeniedDomains:  []string{"evil.example.com", "*.blocked.example.com"},
	}
	p := CompilePolicy(spec)

	cases := []struct {
		host string
		want hostDecision
	}{
		{"api.github.com", hostAllow},          // exact
		{"API.GitHub.com", hostAllow},          // case-insensitive
		{"example.org", hostAllow},             // exact, configured mixed-case
		{"a.example.com", hostAllow},           // wildcard label match
		{"x.y.example.com", hostAllow},         // multi-label wildcard
		{"example.com", hostUnknown},           // bare apex NOT matched by *.example.com
		{"notexample.com", hostUnknown},        // suffix must be on label boundary
		{"fooexample.com", hostUnknown},        // no false suffix match
		{"evil.example.com", hostDeny},         // exact deny beats wildcard allow
		{"deep.blocked.example.com", hostDeny}, // wildcard deny beats wildcard allow
		{"github.com", hostUnknown},            // not in allowlist
		{"a.example.com.", hostAllow},          // trailing dot normalized
	}
	for _, c := range cases {
		if got := p.evalHost(c.host, 443); got != c.want {
			t.Errorf("evalHost(%q) = %q, want %q", c.host, got, c.want)
		}
	}
}

// TestEvalHostPortQualified asserts an "host:port" allow/deny entry is honored
// (matches ONLY that port) while a bare entry matches any port — ITEM 25. Before
// the fix, "api.test:443" was stored as the exact host "api.test:443" and never
// compared against the bare request host, so a port-qualified allow was silently
// dead (and a port-qualified deny never fired).
func TestEvalHostPortQualified(t *testing.T) {
	p := CompilePolicy(types.RunPolicySpec{
		AllowedDomains: []string{"api.test:443", "*.wild.test:8443", "any.test", "zero.test:0", "big.test:99999"},
		DeniedDomains:  []string{"api.test:80", "*.wild.test:80"},
	})
	cases := []struct {
		host string
		port int
		want hostDecision
	}{
		{"api.test", 443, hostAllow},      // port-qualified allow honored (was dead)
		{"api.test", 8080, hostUnknown},   // other ports NOT allowed by :443 entry
		{"api.test", 80, hostDeny},        // port-qualified deny honored (was dead)
		{"any.test", 22, hostAllow},       // bare entry matches ANY port
		{"any.test", 443, hostAllow},      // bare entry matches ANY port
		{"a.wild.test", 8443, hostAllow},  // port-qualified wildcard allow
		{"a.wild.test", 443, hostUnknown}, // wildcard only on its :8443 port
		{"a.wild.test", 80, hostDeny},     // port-qualified wildcard deny beats allow
		// An out-of-range port qualifier ("zero.test:0", "big.test:99999") must
		// leave the entry DEAD — never silently degrade to a bare any-port allow
		// (that would widen egress fail-open on malformed input).
		{"zero.test", 22, hostUnknown},
		{"zero.test", 0, hostUnknown},
		{"big.test", 99999, hostUnknown},
		{"big.test", 443, hostUnknown},
	}
	for _, c := range cases {
		if got := p.evalHost(c.host, c.port); got != c.want {
			t.Errorf("evalHost(%q, %d) = %q, want %q", c.host, c.port, got, c.want)
		}
	}
}

func TestDenyBeatsAllowSameHost(t *testing.T) {
	p := CompilePolicy(types.RunPolicySpec{
		AllowedDomains: []string{"dual.example.com"},
		DeniedDomains:  []string{"dual.example.com"},
	})
	if got := p.evalHost("dual.example.com", 443); got != hostDeny {
		t.Fatalf("deny must beat allow: got %q", got)
	}
}

func TestDefaultDeny(t *testing.T) {
	p := CompilePolicy(types.RunPolicySpec{})
	if got := p.evalHost("anything.com", 443); got != hostUnknown {
		t.Fatalf("empty policy host = %q, want unknown (default-deny at pipeline)", got)
	}
	// And an empty allowlist must not auto-allow via exact-host injection check.
	if p.AllowedExactHost("anything.com") {
		t.Fatalf("empty allowlist must not allow any host for injection")
	}
}

func TestMethodAllowed(t *testing.T) {
	none := CompilePolicy(types.RunPolicySpec{})
	if !none.methodAllowed("GET") || !none.methodAllowed("CONNECT") {
		t.Fatalf("empty method restriction must allow all methods")
	}
	restricted := CompilePolicy(types.RunPolicySpec{AllowedMethods: []string{"get", "POST"}})
	if !restricted.methodAllowed("GET") {
		t.Errorf("GET should be allowed (case-insensitive)")
	}
	if !restricted.methodAllowed("post") {
		t.Errorf("post should be allowed (case-insensitive)")
	}
	if restricted.methodAllowed("CONNECT") {
		t.Errorf("CONNECT must be denied when not in allowed methods")
	}
	if restricted.methodAllowed("DELETE") {
		t.Errorf("DELETE must be denied")
	}
}

func TestAllowedExactHostExcludesWildcard(t *testing.T) {
	p := CompilePolicy(types.RunPolicySpec{
		AllowedDomains: []string{"exact.example.com", "*.wild.example.com"},
		DeniedDomains:  []string{"bad.example.com"},
	})
	if !p.AllowedExactHost("exact.example.com") {
		t.Errorf("exact host must qualify for injection")
	}
	// Injection must NOT apply to a wildcard-matched host (no secret leak).
	if p.AllowedExactHost("a.wild.example.com") {
		t.Errorf("wildcard-matched host must NOT qualify for exact injection")
	}
	if p.AllowedExactHost("bad.example.com") {
		t.Errorf("denied host must never qualify")
	}
}

// TestAllowAllEgress exercises the "allow all (deny-list only)" mode: any
// non-denied PUBLIC host is allowed even when it is absent from
// allowed_domains, denied_domains STILL wins, and credential injection is NOT
// widened (AllowedExactHost stays false for an allow-all-only host).
func TestAllowAllEgress(t *testing.T) {
	p := CompilePolicy(types.RunPolicySpec{
		AllowAllEgress: true,
		// allowed_domains may be EMPTY under allow-all; the one exact entry
		// below exists only to prove injection is gated on it, not allow-all.
		AllowedDomains: []string{"inject.example.com"},
		DeniedDomains:  []string{"blocked.example.com", "*.deny.example.com"},
	})

	// Arbitrary host NOT in allowed_domains is allowed under allow-all.
	if got := p.evalHost("random.example.com", 443); got != hostAllow {
		t.Errorf("allow-all: random.example.com = %q, want hostAllow", got)
	}
	if got := p.evalHost("some-other-host.net", 443); got != hostAllow {
		t.Errorf("allow-all: arbitrary host = %q, want hostAllow", got)
	}

	// denied_domains STILL wins under allow-all (exact and wildcard).
	if got := p.evalHost("blocked.example.com", 443); got != hostDeny {
		t.Errorf("allow-all: exact denied host = %q, want hostDeny", got)
	}
	if got := p.evalHost("a.deny.example.com", 443); got != hostDeny {
		t.Errorf("allow-all: wildcard denied host = %q, want hostDeny", got)
	}

	// Credential injection must NOT be widened by allow-all: a host reachable
	// ONLY via allow-all does not qualify for exact-host injection. A secret
	// must never leak to an arbitrary host.
	if p.AllowedExactHost("random.example.com") {
		t.Errorf("allow-all must NOT widen injection: random.example.com qualified for exact injection")
	}
	// The explicit exact allowlist entry STILL qualifies for injection.
	if !p.AllowedExactHost("inject.example.com") {
		t.Errorf("explicit exact entry must still qualify for injection under allow-all")
	}
	// A denied host never qualifies, even under allow-all.
	if p.AllowedExactHost("blocked.example.com") {
		t.Errorf("denied host must never qualify for injection")
	}
}

// TestAllowAllEgressDoesNotBypassIPGuard asserts the SSRF/private-IP guard is
// unaffected by allow-all: VetHost still denies private/metadata IPs regardless
// of policy mode (allow-all reaches PUBLIC hosts only).
func TestAllowAllEgressDoesNotBypassIPGuard(t *testing.T) {
	// VetHost is policy-independent, but assert it here to lock the invariant
	// that allow-all is "public hosts only".
	for _, host := range []string{"169.254.169.254", "127.0.0.1", "10.1.2.3", "::1"} {
		if got := VetHost(host, nil); !got.Denied {
			t.Errorf("VetHost(%q) must stay denied under allow-all (public hosts only)", host)
		}
	}
	// A host that RESOLVES to the metadata address is denied even though
	// evalHost would allow it under allow-all.
	p := CompilePolicy(types.RunPolicySpec{AllowAllEgress: true})
	if got := p.evalHost("metadata.example.com", 443); got != hostAllow {
		t.Fatalf("precondition: allow-all should allow the name; got %q", got)
	}
	res := fakeResolver{m: map[string][]net.IP{"metadata.example.com": ips("169.254.169.254")}}
	if got := VetHost("metadata.example.com", res); !got.Denied {
		t.Errorf("allow-all host resolving to metadata IP must be denied by VetHost")
	}
}

// fakeResolver returns canned addresses for VetHost tests.
type fakeResolver struct {
	m   map[string][]net.IP
	err error
}

func (f fakeResolver) LookupIP(host string) ([]net.IP, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.m[host], nil
}

func ips(ss ...string) []net.IP {
	out := make([]net.IP, 0, len(ss))
	for _, s := range ss {
		out = append(out, net.ParseIP(s))
	}
	return out
}

func TestVetHostBlocksPrivateAndMetadata(t *testing.T) {
	cases := []struct {
		name       string
		host       string
		resolved   []net.IP
		wantDenied bool
	}{
		{"public literal", "93.184.216.34", nil, false},
		{"loopback literal", "127.0.0.1", nil, true},
		{"rfc1918 10", "10.1.2.3", nil, true},
		{"rfc1918 172", "172.16.5.5", nil, true},
		{"rfc1918 192", "192.168.1.1", nil, true},
		{"metadata", "169.254.169.254", nil, true},
		{"link-local", "169.254.1.1", nil, true},
		{"cgnat", "100.64.0.1", nil, true},
		{"ipv6 loopback", "::1", nil, true},
		{"ipv6 ula", "fc00::1", nil, true},
		{"ipv6 link-local", "fe80::1", nil, true},
		{"ipv4-mapped loopback", "::ffff:127.0.0.1", nil, true},
		{"nat64 metadata", "64:ff9b::a9fe:a9fe", nil, true}, // E2: 169.254.169.254 embedded
		{"nat64 rfc1918", "64:ff9b::0a00:0005", nil, true},  // E2: 10.0.0.5 embedded
		{"nat64 localuse metadata", "64:ff9b:1::a9fe:a9fe", nil, true},
		{"resolved public", "good.example.com", ips("93.184.216.34"), false},
		{"resolved private", "rebind.example.com", ips("10.0.0.5"), true},
		{"resolved mixed pub+priv", "mixed.example.com", ips("93.184.216.34", "127.0.0.1"), true},
		{"resolved metadata", "meta.example.com", ips("169.254.169.254"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := fakeResolver{m: map[string][]net.IP{c.host: c.resolved}}
			got := VetHost(c.host, res)
			if got.Denied != c.wantDenied {
				t.Fatalf("VetHost(%q) Denied=%v (reason=%q), want %v", c.host, got.Denied, got.Reason, c.wantDenied)
			}
			if !got.Denied && got.IP == nil {
				t.Fatalf("allowed host must return a dial IP")
			}
		})
	}
}

// TestNAT64EmbeddedNotOverblocking guards the E2 fix against over-blocking: the
// embedded-v4 check is scoped to NAT64 prefixes, so a legit public IPv6 whose
// low 32 bits happen to look like a reserved v4 (2606:4700:4700::1111 ends in
// 0.0.17.17, inside 0.0.0.0/8) must NOT be denied.
func TestNAT64EmbeddedNotOverblocking(t *testing.T) {
	if kind, why := isBlockedIP(net.ParseIP("2606:4700:4700::1111")); kind != blockNone {
		t.Fatalf("public IPv6 outside NAT64 prefixes must not be blocked (reason=%q)", why)
	}
	// And a NAT64 address embedding a PUBLIC v4 is still blocked wholesale (the
	// prefix is a by-IP bypass of hostname allowlisting): fail closed.
	if kind, _ := isBlockedIP(net.ParseIP("64:ff9b::5db8:d822")); kind == blockNone { // 93.184.216.34
		t.Fatalf("NAT64 prefix must be blocked wholesale (fail closed)")
	}
}

// TestIsBlockedIPKind guards the blockKind classification vetHostLift's lift
// predicate depends on: ONLY RFC1918/ULA/CGNAT (ipguard.Liftable) classify as
// blockPrivate — every other denied range must stay unconditional (blockLocal/
// blockReservedOther/blockNAT64), so an internal-host declaration can never
// lift loopback, link-local, metadata, or the non-liftable ReservedV4 entries.
func TestIsBlockedIPKind(t *testing.T) {
	cases := []struct {
		name string
		ip   string
		want blockKind
	}{
		{"public", "93.184.216.34", blockNone},
		{"loopback", "127.0.0.1", blockLocal},
		{"unspecified", "0.0.0.0", blockLocal},
		{"link-local", "169.254.1.1", blockLocal},
		{"metadata", "169.254.169.254", blockLocal},
		{"multicast", "224.0.0.1", blockLocal},
		{"ipv6 loopback", "::1", blockLocal},
		{"rfc1918 10", "10.1.2.3", blockPrivate},
		{"rfc1918 172", "172.16.5.5", blockPrivate},
		{"rfc1918 192", "192.168.1.1", blockPrivate},
		{"ipv6 ula", "fc00::1", blockPrivate},
		{"cgnat", "100.64.0.1", blockPrivate},
		{"reserved this-network", "0.5.5.5", blockReservedOther},
		{"reserved benchmarking", "198.18.0.1", blockReservedOther},
		{"nat64 metadata", "64:ff9b::a9fe:a9fe", blockNAT64},
		{"nat64 rfc1918", "64:ff9b::0a00:0005", blockNAT64},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if kind, why := isBlockedIP(net.ParseIP(c.ip)); kind != c.want {
				t.Fatalf("isBlockedIP(%q) kind = %v (reason=%q), want %v", c.ip, kind, why, c.want)
			}
		})
	}
}

func TestVetHostResolveFailureFailsClosed(t *testing.T) {
	res := fakeResolver{err: net.UnknownNetworkError("boom")}
	got := VetHost("whatever.example.com", res)
	if !got.Denied {
		t.Fatalf("resolve failure must fail closed (deny)")
	}
}

func TestVetHostEmptyAddressesFailsClosed(t *testing.T) {
	res := fakeResolver{m: map[string][]net.IP{"empty.example.com": {}}}
	got := VetHost("empty.example.com", res)
	if !got.Denied {
		t.Fatalf("no addresses must fail closed (deny)")
	}
}

// --- B6 / B7 / B2: the private-ip memo and the four sentences of its 403 ------

// countingResolver is fakeResolver with a lookup counter, so "the memo did not
// re-resolve" is asserted against the resolver itself rather than inferred.
type countingResolver struct {
	m      map[string][]net.IP
	lookup *int
}

func (c countingResolver) LookupIP(host string) ([]net.IP, error) {
	*c.lookup++
	return c.m[host], nil
}

// countDecisions returns how many decision logs the sink mirrored, and decodes
// them in order.
func countDecisions(t *testing.T, buf *bytes.Buffer) []egress.DecisionLog {
	t.Helper()
	var out []egress.DecisionLog
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var d egress.DecisionLog
		if err := json.Unmarshal([]byte(line), &d); err != nil {
			t.Fatalf("decode decision %q: %v", line, err)
		}
		out = append(out, d)
	}
	return out
}

// flattenHeader renders a header block as one comparable map, so "byte-identical
// 403" is asserted over the WHOLE block rather than the two headers a test
// happened to think of.
func flattenHeader(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		out[k] = strings.Join(v, "\x00")
	}
	return out
}

func connectReq(t *testing.T, hostport string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodConnect, "http://"+hostport+"/", nil)
	r.Host = hostport
	return r
}

// TestPrivateIPMemo_IdenticalRefusalsCostOneDecisionRow is B6.
//
// The field report is of a run burning all ten of the agent CLI's retries
// against a denial the guard will never change its mind about, and of an
// evidence rail showing that one denial ten times. Every attempt must still be
// refused, with the same 403 and the same headers; what must not repeat is the
// re-resolve and the decision row.
func TestPrivateIPMemo_IdenticalRefusalsCostOneDecisionRow(t *testing.T) {
	const host = "svc.priv.internal"
	lookups := 0
	p, buf := newInternalHostsProxy(t,
		types.RunPolicySpec{AllowedDomains: []string{host}},
		countingResolver{m: map[string][]net.IP{host: ips("10.9.8.7")}, lookup: &lookups},
		nil, nil, nil, "127.0.0.1:1")

	// BYTE-IDENTICAL, asserted rather than claimed: attempt 1 is the reference and
	// every memoed attempt after it must match its status, its whole header block
	// and its whole body. A sandbox that can tell the second refusal from the
	// first has learned something about proxy state it was never told, and the
	// operator reading the last of ten retries must see the same sentences as the
	// operator reading the first.
	const attempts = 10
	var wantHeader http.Header
	var wantBody string
	for i := range attempts {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, connectReq(t, host+":443"))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("attempt %d: status = %d, want 403 — a memoed refusal is still a refusal", i+1, rec.Code)
		}
		if got := rec.Header().Get(egressHeaderReason); got != "builtin:private-ip" {
			t.Errorf("attempt %d: %s = %q, want builtin:private-ip", i+1, egressHeaderReason, got)
		}
		if got := rec.Header().Get(egressHeaderRetry); got != egressRetryNever {
			t.Errorf("attempt %d: %s = %q, want %q — the header is what lets a retry loop stop",
				i+1, egressHeaderRetry, got, egressRetryNever)
		}
		if body := rec.Body.String(); !strings.Contains(body, egressInternalHostsRemedy) {
			t.Errorf("attempt %d: 403 body = %q, want the remedy on every attempt, not only the first", i+1, body)
		}
		if i == 0 {
			wantHeader, wantBody = rec.Header().Clone(), rec.Body.String()
			continue
		}
		if !maps.Equal(flattenHeader(rec.Header()), flattenHeader(wantHeader)) {
			t.Errorf("attempt %d headers differ from the first refusal:\n\tgot  %v\n\twant %v",
				i+1, rec.Header(), wantHeader)
		}
		if got := rec.Body.String(); got != wantBody {
			t.Errorf("attempt %d body differs from the first refusal:\n\tgot  %q\n\twant %q", i+1, got, wantBody)
		}
	}

	if lookups != 1 {
		t.Errorf("resolver was asked %d times for %d identical attempts, want 1: the memo exists to stop "+
			"re-resolving a verdict that cannot change mid-run", lookups, attempts)
	}
	decisions := countDecisions(t, buf)
	if len(decisions) != 1 {
		t.Fatalf("%d decisions emitted for %d identical attempts, want 1: %q", len(decisions), attempts, buf.String())
	}
	if decisions[0].Repeat != 0 {
		t.Errorf("the row that OPENED the streak carries repeat=%d — the count belongs on the summary row, "+
			"never on a row already recorded", decisions[0].Repeat)
	}

	// Run end closes the streak with a NEW row carrying the count. The opening
	// row is untouched: the audit chain is append-only.
	p.flushPrivateIPMemo()
	decisions = countDecisions(t, buf)
	if len(decisions) != 2 {
		t.Fatalf("%d decisions after the run-end flush, want 2 (the refusal + one summary): %q",
			len(decisions), buf.String())
	}
	summary := decisions[1]
	if summary.Repeat != attempts-1 {
		t.Errorf("summary repeat = %d, want %d — the nine attempts that got no row of their own",
			summary.Repeat, attempts-1)
	}
	if summary.Decision != egress.Deny || summary.RuleSource != "builtin:private-ip" {
		t.Errorf("summary = %q/%q, want deny/builtin:private-ip", summary.Decision, summary.RuleSource)
	}
	if decisions[0].Repeat != 0 {
		t.Errorf("the opening row was MUTATED to repeat=%d", decisions[0].Repeat)
	}
}

// TestPrivateIPMemo_BoundedAndFlushesTheStreakItEvicts pins the ceiling: the
// keys are host:port strings the sandbox chooses, so the map cannot be allowed
// to grow with them — and an eviction must not swallow the count it was holding.
func TestPrivateIPMemo_BoundedAndFlushesTheStreakItEvicts(t *testing.T) {
	var mo privateIPMemo
	req := func(h string) egress.Request {
		return egress.Request{Host: h, Port: 443, Method: http.MethodConnect, Time: time.Now()}
	}
	// The first entry is the only one with repeats, and the least recently hit,
	// so filling the memo past its ceiling evicts exactly it.
	if evicted := mo.record(req("first.priv.internal"), blockPrivate); evicted != nil {
		t.Fatalf("recording into an empty memo evicted %+v", evicted)
	}
	if !mo.hit("first.priv.internal", 443, time.Now()) {
		t.Fatal("the entry just recorded is not memoed")
	}
	var evictedRow *egress.DecisionLog
	for i := range privateIPMemoMax {
		if row := mo.record(req("h"+strconv.Itoa(i)+".priv.internal"), blockPrivate); row != nil {
			evictedRow = row
		}
	}
	if len(mo.m) > privateIPMemoMax {
		t.Errorf("memo holds %d entries, past its %d ceiling — sandbox-chosen keys need a bound they "+
			"cannot be pushed past", len(mo.m), privateIPMemoMax)
	}
	if evictedRow == nil {
		t.Fatal("filling the memo past its ceiling evicted nothing")
	}
	if evictedRow.Repeat != 1 || evictedRow.Request.Host != "first.priv.internal" {
		t.Errorf("evicted row = host %q repeat %d, want first.priv.internal repeat 1: an eviction that "+
			"drops the count it was holding loses audit", evictedRow.Request.Host, evictedRow.Repeat)
	}
	// A streak nobody repeated produced no suppressed attempt, so it owes no row.
	if row := closeStreak(&privateIPStreak{req: req("quiet.priv.internal")}); row != nil {
		t.Errorf("a streak with zero repeats produced a summary row %+v", row)
	}
}

// TestResolveFailedDenialIsRetryableAndReEmits is the first negative: a name
// that did not resolve is a DNS fault that may clear on the next attempt, so it
// gets neither the retry-never header nor the memo. It also byte-asserts the resolve-failed
// decision string, which must not be reworded.
func TestResolveFailedDenialIsRetryableAndReEmits(t *testing.T) {
	const host = "unresolvable.example"
	p, buf := newInternalHostsProxy(t,
		types.RunPolicySpec{AllowedDomains: []string{host}},
		fakeResolver{err: net.UnknownNetworkError("resolver down")},
		nil, nil, nil, "127.0.0.1:1")

	const attempts = 3
	for i := range attempts {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "http://"+host+"/"))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("attempt %d: status = %d, want 403", i+1, rec.Code)
		}
		if got := rec.Header().Get(egressHeaderRetry); got != "" {
			t.Errorf("attempt %d: %s = %q — a resolver outage must stay retryable, or one bad minute "+
				"kills the run", i+1, egressHeaderRetry, got)
		}
	}
	decisions := countDecisions(t, buf)
	if len(decisions) != attempts {
		t.Fatalf("%d decisions for %d resolve failures, want one each: the memo binds the private-address "+
			"guard and nothing else (%q)", len(decisions), attempts, buf.String())
	}
	for i, d := range decisions {
		if d.RuleSource != "builtin:resolve-failed" {
			t.Errorf("decision %d rule_source = %q, want the literal builtin:resolve-failed", i, d.RuleSource)
		}
	}
}

// TestApprovalPendingRefusalIsRetryableAndReEmits is the second negative: a hold
// is waiting for a HUMAN, so every attempt keeps asking and none of them is told
// the answer will never change.
func TestApprovalPendingRefusalIsRetryableAndReEmits(t *testing.T) {
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/internal/approvals") {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: uuid.New(), State: types.ApprovalPending})
			return
		}
		http.Error(w, "unexpected", http.StatusTeapot)
	}))
	defer cp.Close()
	ap := newApprovalClient(cp.URL, newTokenSource("tok"), uuid.New(), cp.Client())
	p, buf := newTestProxy(t, types.RunPolicySpec{
		AllowedDomains:   []string{"known.test"},
		FirstUseApproval: types.FirstUseDenyWithReview,
	}, "127.0.0.1:1", ap, nil)

	const attempts = 3
	for i := range attempts {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "http://held.test/"))
		if got := rec.Header().Get(egressHeaderRetry); got != "" {
			t.Errorf("attempt %d: %s = %q — an undecided approval is exactly the refusal a retry CAN change",
				i+1, egressHeaderRetry, got)
		}
	}
	decisions := countDecisions(t, buf)
	if len(decisions) != attempts {
		t.Fatalf("%d decisions for %d held attempts, want one each: %q", len(decisions), attempts, buf.String())
	}
	for i, d := range decisions {
		if d.Decision != egress.Pending {
			t.Errorf("decision %d = %q, want pending", i, d.Decision)
		}
	}
}

// TestPrivateIPDenialDetailComposesTheFourSentences is B7 + B2: the 403 a
// HOSTNAME's operator reads is composed of four separately-frozen constants, so
// the owner's canon sitting can reword or drop one without touching the join.
//
// It asserts THROUGH the constants deliberately — the sentences are DRAFT and
// the pin is the composition, not a copy of the text a second time.
func TestPrivateIPDenialDetailComposesTheFourSentences(t *testing.T) {
	pol := CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"priv.example.test"}})
	detail := literalIPDenialDetail("priv.example.test", 443, pol, blockPrivate)
	for _, want := range []string{
		egressPrivateRangeCause,
		egressInternalHostsRemedy,
		siteInternalHostsCIDRHint,
		egressDenialSuffix,
	} {
		if !strings.Contains(detail, want) {
			t.Errorf("the hostname denial detail does not carry %q:\n\t%s", want, detail)
		}
	}
	// B7's inversion, in the words that cost the field report two runs: the
	// parenthetical must no longer tell an operator to enumerate cidrs.
	if strings.Contains(detail, "plus the cidrs it may resolve into") {
		t.Error("the remedy still asks for the cidrs a host may resolve into — that advice is drawn from " +
			"what the OPERATOR's machine resolves, not what the sandbox does, and excludes the address " +
			"the guard actually refuses")
	}
	// A LITERAL private address is a policy problem (allowed_domains /
	// egress_redirects), not a site-config one, so it must not carry site
	// config's lifetime clause.
	if got := literalIPDenialDetail("100.64.5.7", 443, pol, blockPrivate); strings.Contains(got, egressDenialSuffix) {
		t.Errorf("the literal-IP arm carries the site-config lifetime clause:\n\t%s", got)
	}
}

// TestStep0LiteralIPDenialAlsoSaysNeverRetry: the private-address guard refuses
// at TWO spellings — step 0's literal-IP guard, before policy is consulted, and
// the post-resolution re-check the memo sits in front of. Both are the same
// verdict with the same lifetime, so a sandbox must be able to stop retrying
// either one. The remedy for a literal (an exact allowed_domains entry, or an
// egress_redirects "to") is compiled at dispatch just as the internal_hosts lift
// is, so "never" is true here for the same reason.
func TestStep0LiteralIPDenialAlsoSaysNeverRetry(t *testing.T) {
	p, buf := newInternalHostsProxy(t,
		types.RunPolicySpec{AllowedDomains: []string{"anything.example"}},
		publicResolver{}, nil, nil, nil, "127.0.0.1:1")

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, connectReq(t, "10.9.8.7:443"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if d := lastDecision(t, buf); d.RuleSource != "builtin:private-ip" {
		t.Fatalf("rule_source = %q, want builtin:private-ip (step 0)", d.RuleSource)
	}
	if got := rec.Header().Get(egressHeaderRetry); got != egressRetryNever {
		t.Errorf("%s = %q, want %q — step 0 refuses the same verdict as the post-resolution re-check",
			egressHeaderRetry, got, egressRetryNever)
	}
}

// TestNilDecisionLogWithoutAMemoEntryGetsThePlainRefusal is the negative that
// keeps writeEgressDeny's memo lookup from becoming an inference: a DENY with no
// decision log, for a host the private-address guard never refused, must get the
// plain body and NO retry header — even from a caller that declares the verdict
// COULD have come from the memo (memoed=true). If a future deny path starts
// returning a nil log, it inherits nothing from B6.
func TestNilDecisionLogWithoutAMemoEntryGetsThePlainRefusal(t *testing.T) {
	p, _ := newInternalHostsProxy(t,
		types.RunPolicySpec{AllowedDomains: []string{"never-refused.example"}},
		publicResolver{}, nil, nil, nil, "127.0.0.1:1")

	rec := httptest.NewRecorder()
	p.writeEgressDeny(rec, "never-refused.example", 443, nil, true)
	if body := strings.TrimSpace(rec.Body.String()); body != "egress denied by policy" {
		t.Errorf("body = %q, want the plain refusal — a nil log is not evidence of a private-IP verdict", body)
	}
	if got := rec.Header().Get(egressHeaderRetry); got != "" {
		t.Errorf("%s = %q, want unset: nothing memoed this host, so nothing may promise the answer "+
			"will never change", egressHeaderRetry, got)
	}
	if got := rec.Header().Get(egressHeaderDetail); got != "" {
		t.Errorf("%s = %q, want unset", egressHeaderDetail, got)
	}
}

// TestMemoedFlagIsRequiredForTheMemoedRefusal is the pair the negative above was
// missing: the memo's 403 is rebuilt only when BOTH facts hold, so each one is
// pinned with the other held true.
//
// It closes the residual writeEgressDeny's first version stated out loud — a
// future deny path returning a nil log would have inherited B6's private-ip body
// for any host the guard happened to have refused earlier in the run. The flag
// makes that a caller's declaration instead of a consequence of returning nil.
func TestMemoedFlagIsRequiredForTheMemoedRefusal(t *testing.T) {
	const host = "svc.priv.internal"
	p, _ := newInternalHostsProxy(t,
		types.RunPolicySpec{AllowedDomains: []string{host}},
		fakeResolver{m: map[string][]net.IP{host: ips("10.9.8.7")}}, nil, nil, nil, "127.0.0.1:1")

	// Open the memo streak the way the run does: one real refusal.
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, connectReq(t, host+":443"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("seed refusal status = %d, want 403", rec.Code)
	}
	if !p.privateIPMemoed(host, 443) {
		t.Fatal("the refusal did not open a memo streak — this test is asserting nothing")
	}

	// POSITIVE: the caller says the verdict can come from the memo, and it does.
	memoed := httptest.NewRecorder()
	p.writeEgressDeny(memoed, host, 443, nil, true)
	if got := memoed.Header().Get(egressHeaderReason); got != "builtin:private-ip" {
		t.Errorf("%s = %q, want builtin:private-ip", egressHeaderReason, got)
	}
	if got := memoed.Header().Get(egressHeaderRetry); got != egressRetryNever {
		t.Errorf("%s = %q, want %q — a memoed refusal is what the retry header exists for",
			egressHeaderRetry, got, egressRetryNever)
	}
	if body := memoed.Body.String(); !strings.Contains(body, egressInternalHostsRemedy) {
		t.Errorf("body = %q, want the private-IP remedy", body)
	}

	// NEGATIVE: same memoed host, a caller that builds its own denies. It must get
	// the plain refusal — the memo is not a fallback body for every nil log.
	own := httptest.NewRecorder()
	p.writeEgressDeny(own, host, 443, nil, false)
	if body := strings.TrimSpace(own.Body.String()); body != "egress denied by policy" {
		t.Errorf("body = %q, want the plain refusal for a caller that never reads the memo", body)
	}
	for _, h := range []string{egressHeaderRetry, egressHeaderDetail} {
		if got := own.Header().Get(h); got != "" {
			t.Errorf("%s = %q, want unset", h, got)
		}
	}
}
