// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// F7 PROBE (lane F7-redirect-probe-sni-literal-ip) — NOT part of the tree.
//
// Intended destination: internal/api/site_config_probe_f7_probe_test.go
// (package api — it reaches the unexported redirectProbeTo, probeTargetURL,
// redirectPort, validateSiteConfig and redirectProbeScript directly).
//
// Run (no Postgres needed — every test here is pure or loopback-only):
//
//   cp local/review-0.7/deep/F7-redirect-probe-sni-literal-ip/site_config_probe_f7_probe_test.go internal/api/
//   nice -n 10 GOMAXPROCS=8 go test -p 4 ./internal/api -run 'TestF7_' -count=1 -v
//   rm internal/api/site_config_probe_f7_probe_test.go
//
// INVARIANT UNDER TEST (see ../F7-redirect-probe-sni-literal-ip.md §0):
// probe 1 of the redirect probe dials ONLY the stored To (host, port AND
// scheme), and its verdict about "the mirror" is never derived from a dial
// that actually landed on the public From host; probe 2's verdict "From is
// correctly blocked" is never emitted when the public host answered.
//
// EXPECTED STATE AT fa910735 (this is a finding-seeding probe, not a green
// gate): the subtests tagged wantRedToday FAIL on the unmodified tree — each
// one is a numbered hypothesis in the trace doc (H-1, H-2, H-4). Everything
// else must pass; a new failure elsewhere is a regression.

package api

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/hostrules"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// f7ConnectTo is one parsed curl --connect-to HOST1:PORT1:HOST2:PORT2 rule.
// IPv6 literals must be bracketed in curl's syntax; an unbracketed IPv6
// literal splits into the wrong number of fields and is reported as an error.
type f7ConnectTo struct {
	h1, h2 string
	p1, p2 int
}

func f7ParseConnectTo(s string) (f7ConnectTo, error) {
	// Tokenise on ':' but keep bracketed IPv6 literals whole.
	var parts []string
	var cur strings.Builder
	depth := 0
	for _, r := range s {
		switch {
		case r == '[':
			depth++
		case r == ']':
			depth--
		case r == ':' && depth == 0:
			parts = append(parts, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteRune(r)
	}
	parts = append(parts, cur.String())
	if len(parts) != 4 {
		return f7ConnectTo{}, fmt.Errorf("--connect-to %q: want HOST1:PORT1:HOST2:PORT2 (4 fields, IPv6 bracketed), got %d fields", s, len(parts))
	}
	p1, err := strconv.Atoi(parts[1])
	if err != nil {
		return f7ConnectTo{}, fmt.Errorf("--connect-to %q: PORT1 %q not numeric", s, parts[1])
	}
	p2, err := strconv.Atoi(parts[3])
	if err != nil {
		return f7ConnectTo{}, fmt.Errorf("--connect-to %q: PORT2 %q not numeric", s, parts[3])
	}
	return f7ConnectTo{h1: strings.Trim(parts[0], "[]"), p1: p1, h2: strings.Trim(parts[2], "[]"), p2: p2}, nil
}

// f7SimulateProbe1 models what the sandbox's curl actually puts on the wire
// toward wardyn-proxy for probe 1 (redirectProbeScript lines 1-5): the host
// and port the proxy is asked to CONNECT to, and the application protocol curl
// then speaks inside. Two curl facts are modelled (both outside this repo —
// see the trace doc, marked (unverified) there):
//
//  1. --connect-to fires only when HOST1:PORT1 equals the requested URL's
//     host:port (case-insensitive host; port defaulted from the scheme).
//  2. A fired --connect-to swap decides the CONNECTION, whatever the URL's
//     scheme: curl asks the proxy to CONNECT to HOST2:PORT2 and then speaks
//     the URL's OWN application protocol inside that tunnel.
//
// Fact 2 was MEASURED, not assumed, on curl 8.5.0 against a recording stand-in
// proxy (the trace doc marks it (unverified) and models it the other way — as
// "a plain http:// URL is never tunnelled, so the swap is inert on the proxy
// leg"). Observed, with http_proxy/https_proxy both pointed at the stand-in:
//
//	To=https://10.40.1.5   From=http://mirror.example.com
//	  -> OUTER "CONNECT 10.40.1.5:443", INNER cleartext "GET / HTTP/1.1"
//	To=http://10.40.2.11:8080  From=https://pypi.org
//	  -> OUTER "CONNECT 10.40.2.11:8080", INNER a TLS ClientHello (0x16)
//
// So the swap DOES reach the proxy leg for an http:// URL — the host/port half
// of the trace doc's H-1 does not reproduce — while the SCHEME half of both
// H-1 and H-4 does: curl speaks the From URL's protocol against a port the
// mirror serves with the other one. TestF7_HTTPFrom_LiteralIPTo_EndToEndThroughScript
// is the end-to-end witness for the host/port half.
func f7SimulateProbe1(toURL, connectTo string) (dialHost string, dialPort int, wireScheme string, err error) {
	u, err := url.Parse(toURL)
	if err != nil {
		return "", 0, "", err
	}
	reqHost := u.Hostname()
	reqPort := 443
	if u.Scheme == "http" {
		reqPort = 80
	}
	if ps := u.Port(); ps != "" {
		if reqPort, err = strconv.Atoi(ps); err != nil {
			return "", 0, "", fmt.Errorf("request url %q: bad port", toURL)
		}
	}
	if connectTo == "" {
		return reqHost, reqPort, u.Scheme, nil
	}
	ct, err := f7ParseConnectTo(connectTo)
	if err != nil {
		return "", 0, "", err
	}
	if strings.EqualFold(ct.h1, reqHost) && ct.p1 == reqPort {
		// Fact 2: the swap decides the connection; the URL's own scheme is
		// what curl then speaks inside it.
		return ct.h2, ct.p2, u.Scheme, nil
	}
	return reqHost, reqPort, u.Scheme, nil
}

// f7ToScheme is the application protocol the STORED To actually speaks.
func f7ToScheme(to string) string {
	if strings.HasPrefix(strings.ToLower(to), "http://") {
		return "http"
	}
	return "https"
}

// TestF7_RedirectProbeTo_Probe1DialsOnlyTheStoredTo is the table the lane
// asked for. Inputs are fed through the REAL hostrules.HostOf (the handler's
// own extraction, in handleTestSiteConfigRedirect) — not the test-local
// hostOfForTest copy in site_config_noproxy_test.go, which skips the
// ValidApprovedHost gate and therefore cannot see what the handler sees.
func TestF7_RedirectProbeTo_Probe1DialsOnlyTheStoredTo(t *testing.T) {
	cases := []struct {
		name         string
		red          types.EgressRedirect
		wantRedToday string // non-empty: expected to FAIL at fa910735, naming the hypothesis
	}{
		{
			name: "hostname To, https From (baseline: no swap)",
			red:  types.EgressRedirect{From: "https://pypi.org/simple/", To: "https://mirror.corp.internal/simple"},
		},
		{
			name: "literal-IP To, bare From (the private-endpoint shape)",
			red:  types.EgressRedirect{From: "pypi.org", To: "https://100.64.5.7"},
		},
		{
			name: "literal-IP To with explicit port",
			red:  types.EgressRedirect{From: "https://pypi.org/simple/", To: "https://10.40.2.11:8443"},
		},
		{
			name: "literal-IP To as bare host:port/path (validSiteURLOrHost third shape)",
			red:  types.EgressRedirect{From: "ghcr.io", To: "10.40.2.11:8443/ghcr-remote"},
		},
		{
			name: "From with a non-443 port: connect-to must match on that port",
			red:  types.EgressRedirect{From: "https://registry.corp.example:8443/", To: "https://10.40.2.11"},
		},
		{
			name: "bare From with a port",
			red:  types.EgressRedirect{From: "registry.corp.example:8443", To: "https://10.40.2.11"},
		},
		{
			name: "From with uppercase spelling (curl matches HOST1 case-insensitively — modelled)",
			red:  types.EgressRedirect{From: "https://PyPI.org/simple/", To: "https://10.40.2.11"},
		},
		{
			name: "From with no host: no swap, probe 1 must still land on To",
			red:  types.EgressRedirect{From: "", To: "https://100.64.5.7"},
		},
		{
			name: "http:// From with literal-IP To — the swap reaches the stored To and curl speaks To's TLS, not From's cleartext",
			red:  types.EgressRedirect{From: "http://mirror.example.com", To: "https://10.40.1.5"},
		},
		{
			name: "plain-http To (http://IP:8080) — the swap speaks To's cleartext, not From's implied https",
			red:  types.EgressRedirect{From: "pypi.org", To: "http://10.40.2.11:8080"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			toHost := hostrules.HostOf(c.red.To)
			fromHost := hostrules.HostOf(c.red.From)
			if toHost == "" {
				t.Fatalf("hostrules.HostOf(%q) = \"\" — this row could never be probed (validateSiteConfig would refuse it); fix the case", c.red.To)
			}
			toURL, connectTo := redirectProbeTo(c.red, toHost, fromHost)
			dialHost, dialPort, wireScheme, err := f7SimulateProbe1(toURL, connectTo)
			if err != nil {
				t.Fatalf("probe 1 could not be modelled: %v (toURL=%q connectTo=%q)", err, toURL, connectTo)
			}
			var problems []string
			if !strings.EqualFold(dialHost, toHost) {
				problems = append(problems, fmt.Sprintf("probe 1 dials host %q, want the stored To host %q — the verdict would be about a host that is not the mirror", dialHost, toHost))
			}
			if wantPort := redirectPort(c.red.To); dialPort != wantPort {
				problems = append(problems, fmt.Sprintf("probe 1 dials port %d, want To's port %d", dialPort, wantPort))
			}
			if want := f7ToScheme(c.red.To); wireScheme != want {
				problems = append(problems, fmt.Sprintf("probe 1 speaks %s on the wire, but To is %s — a protocol the mirror does not serve", wireScheme, want))
			}
			// When the swap is in play, it must name the From authority curl will
			// actually request, or it silently never fires.
			if connectTo != "" {
				ct, perr := f7ParseConnectTo(connectTo)
				if perr != nil {
					problems = append(problems, perr.Error())
				} else if u, uerr := url.Parse(toURL); uerr == nil && !strings.EqualFold(ct.h1, u.Hostname()) {
					problems = append(problems, fmt.Sprintf("connect-to HOST1 %q != requested host %q: the swap never fires", ct.h1, u.Hostname()))
				}
			}
			if len(problems) == 0 {
				if c.wantRedToday != "" {
					t.Errorf("%s: this case was expected RED at fa910735 and is now green — update the trace doc (toURL=%q connectTo=%q)", c.wantRedToday, toURL, connectTo)
				}
				return
			}
			msg := strings.Join(problems, "; ") + fmt.Sprintf(" [toURL=%q connectTo=%q]", toURL, connectTo)
			if c.wantRedToday != "" {
				t.Errorf("%s (expected red at fa910735): %s", c.wantRedToday, msg)
				return
			}
			t.Error(msg)
		})
	}
}

// TestF7_IPv6LiteralTo_IsRefusedAtWriteOrBracketed pins the IPv6 half of the
// literal-IP question. Today hostrules.HostOf cannot represent an IPv6
// literal at all (the '[' and ':' fail hostrules.ValidApprovedHost),
// so validateSiteConfig refuses every IPv6 To and redirectProbeTo is never
// reached with one. If that gate is ever loosened, the swap MUST bracket HOST2
// (curl's --connect-to syntax) and the run's allowlist entry must be a
// classifyDomain-parsable IPv6 — this test then fails on the first shape that
// is not.
func TestF7_IPv6LiteralTo_IsRefusedAtWriteOrBracketed(t *testing.T) {
	for _, to := range []string{"https://[fd00::1]:8443/", "[fd00::1]:8443", "fd00::1", "https://[fd00::1]"} {
		t.Run(to, func(t *testing.T) {
			cfg := types.SiteConfig{EgressRedirects: []types.EgressRedirect{{From: "ghcr.io", To: to}}}
			err := validateSiteConfig(cfg)
			toHost := hostrules.HostOf(to)
			if err != nil {
				if toHost != "" {
					t.Errorf("validateSiteConfig refused %q but HostOf still yields %q — the two gates disagree", to, toHost)
				}
				return // refused at write time: the probe path is unreachable, as documented
			}
			// Validation now ACCEPTS an IPv6 To. Everything downstream must cope.
			if net.ParseIP(toHost) == nil {
				t.Fatalf("validation accepts %q but HostOf gives %q, which is not an IP: redirectProbeTo would treat it as a HOSTNAME and never swap", to, toHost)
			}
			toURL, connectTo := redirectProbeTo(types.EgressRedirect{From: "ghcr.io", To: to}, toHost, "ghcr.io")
			if _, perr := f7ParseConnectTo(connectTo); perr != nil {
				t.Fatalf("IPv6 To accepted but the swap is not curl-parsable: %v (toURL=%q)", perr, toURL)
			}
			if !strings.Contains(connectTo, "["+toHost+"]") {
				t.Fatalf("connect-to %q must bracket the IPv6 HOST2 %q", connectTo, toHost)
			}
		})
	}
}

// TestF7_RedirectProbeScript_PublicHostHTTPErrorIsBypassNotReached runs the
// ACTUAL redirectProbeScript against two loopback servers: To answers 200,
// and the PUBLIC From host ANSWERS — with an HTTP 403. Probe 2 dialled the
// public host and got a well-formed reply, which is the definition of "the
// confinement class does not structurally block From". The script must exit
// the bypass sentinel (250), never 0 ("From is correctly blocked when dialed
// directly (redirect enforced)", classifyRedirectProbe's exit-0 arm in
// site_config_probe_classify.go).
//
// Expected RED at fa910735 (H-2): probe 2's `-f` turns the 403 into a curl
// failure, the `&& exit 250` is skipped, and the script exits 0.
func TestF7_RedirectProbeScript_PublicHostHTTPErrorIsBypassNotReached(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not on PATH")
	}
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer mirror.Close()
	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden) // the public host ANSWERED; it is reachable
	}))
	defer public.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", redirectProbeScript)
	cmd.Env = append(cmd.Environ(),
		"WARDYN_PROBE_TO_URL="+mirror.URL,
		"WARDYN_PROBE_TO_CONNECT=",
		"WARDYN_PROBE_FROM_URL="+public.URL,
		// Never let the developer shell's proxy settings route loopback.
		"http_proxy=", "https_proxy=", "HTTP_PROXY=", "HTTPS_PROXY=",
	)
	_ = cmd.Run()
	if got := cmd.ProcessState.ExitCode(); got != redirectProbeBypassCode {
		t.Fatalf("H-2 (expected red at fa910735): redirectProbeScript exit = %d, want %d (bypass): the public From host was dialled directly and answered (403), "+
			"so the confinement class does NOT block it — yet the probe reports the redirect as enforced", got, redirectProbeBypassCode)
	}
}

// TestF7_HTTPFrom_LiteralIPTo_EndToEndThroughScript is H-1's behavioural
// half, using the real script + curl with a loopback stand-in for
// wardyn-proxy. The stand-in records what it was asked for: for an http://
// From, curl sends `GET http://<From>/ HTTP/1.1` to the proxy (absolute-form,
// no CONNECT), so the proxy is asked for the PUBLIC host, never the To
// address named in --connect-to. Expected RED at fa910735.
func TestF7_HTTPFrom_LiteralIPTo_EndToEndThroughScript(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not on PATH")
	}
	var seen []string
	fakeProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.Host+" "+r.RequestURI)
		w.WriteHeader(http.StatusForbidden) // whatever it asked for, refuse — we only care WHAT it asked for
	}))
	defer fakeProxy.Close()

	red := types.EgressRedirect{From: "http://mirror.example.com", To: "https://10.40.1.5"}
	toHost, fromHost := hostrules.HostOf(red.To), hostrules.HostOf(red.From)
	toURL, connectTo := redirectProbeTo(red, toHost, fromHost)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", redirectProbeScript)
	cmd.Env = append(cmd.Environ(),
		"WARDYN_PROBE_TO_URL="+toURL,
		"WARDYN_PROBE_TO_CONNECT="+connectTo,
		"WARDYN_PROBE_FROM_URL="+probeTargetURL(red.From),
		"http_proxy="+fakeProxy.URL, "https_proxy="+fakeProxy.URL,
		"HTTP_PROXY="+fakeProxy.URL, "HTTPS_PROXY="+fakeProxy.URL,
		"no_proxy=", "NO_PROXY=",
	)
	_ = cmd.Run()
	if len(seen) == 0 {
		t.Fatalf("curl never reached the stand-in proxy (toURL=%q connectTo=%q)", toURL, connectTo)
	}
	first := seen[0]
	if !strings.Contains(first, toHost) {
		t.Fatalf("H-1 (expected red at fa910735): the proxy was asked for %q — the PUBLIC From host — not the stored To %q; "+
			"the resulting 403 classifies as 'could not reach the mirror' (toURL=%q connectTo=%q)", first, toHost, toURL, connectTo)
	}
}
