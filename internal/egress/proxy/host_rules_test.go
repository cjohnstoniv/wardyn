// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const hrHost = "api.example.com"

// hrSets is two layers' sets for hrHost: one allows GET under /public and /repos, the other allows GET
// and denies DELETE everywhere, so a request passes only where both allow.
func hrSets() []types.HostRuleSet {
	return []types.HostRuleSet{
		{Rules: []types.HostRule{{Host: hrHost, Methods: []string{"GET"}, Paths: []string{"/public", "/repos"}, Effect: types.HostRuleAllow}}},
		{Rules: []types.HostRule{
			{Host: hrHost, Methods: []string{"DELETE"}, Effect: types.HostRuleDeny},
			{Host: hrHost, Methods: []string{"GET"}, Effect: types.HostRuleAllow},
		}},
	}
}

type hrOpts struct {
	noCA      bool
	mitmHosts []string
	vm        bool
}

func hrProxy(t *testing.T, o hrOpts, upstream *httptest.Server) (*Proxy, *bytes.Buffer, []byte) {
	t.Helper()
	buf := &bytes.Buffer{}
	sink := &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)}
	opts := Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{hrHost}, HostRules: hrSets()}),
		Sink:            sink,
		Resolver:        publicResolver{},
		Dial:            redirectDial(upstreamAddr(upstream)),
		TLSClientConfig: testInsecureTLSConfig,
		MITMHosts:       o.mitmHosts,
	}
	var certPEM []byte
	if !o.noCA {
		var keyPEM []byte
		certPEM, keyPEM = genTestCA(t)
		ca, err := newCertAuthority(certPEM, keyPEM)
		if err != nil {
			t.Fatalf("newCertAuthority: %v", err)
		}
		opts.CA = ca
	}
	p := newProxy(opts)
	p.vm = o.vm
	return p, buf, certPEM
}

// hrConnect sends a CONNECT to hrHost:port and returns the response status and, on 200, the raw conn.
func hrConnect(t *testing.T, proxyURL string, port string) (int, net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(proxyURL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	target := net.JoinHostPort(hrHost, port)
	if _, err := io.WriteString(conn, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode, conn, br
}

// hrInner sends one request inside an opened tunnel, trusting caPEM; it reports the status.
func hrInner(t *testing.T, conn net.Conn, caPEM []byte, method, path string) int {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	tc := tls.Client(conn, &tls.Config{ServerName: hrHost, RootCAs: pool, MinVersion: tls.VersionTLS12})
	if err := tc.Handshake(); err != nil {
		t.Fatalf("a rule host's tunnel was not opened by the proxy (TLS to the proxy's certificate failed): %v", err)
	}
	return hrSend(t, tc, method, path)
}

func hrSend(t *testing.T, c net.Conn, method, path string) int {
	t.Helper()
	if _, err := io.WriteString(c, method+" "+path+" HTTP/1.1\r\nHost: "+hrHost+"\r\nContent-Length: 0\r\n\r\n"); err != nil {
		t.Fatalf("write inner request: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: method})
	if err != nil {
		t.Fatalf("read inner response: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

func hrRows(t *testing.T, buf *bytes.Buffer) []egress.DecisionLog {
	t.Helper()
	var out []egress.DecisionLog
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var d egress.DecisionLog
		if err := json.Unmarshal([]byte(line), &d); err != nil {
			t.Fatalf("decode decision row %q: %v", line, err)
		}
		out = append(out, d)
	}
	return out
}

// TestHostRulesJudgeEachRequestInsideTheTunnel: a rule host's CONNECT is always opened, and each request
// inside passes only where every set naming the host allows it.
func TestHostRulesJudgeEachRequestInsideTheTunnel(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/public/a", http.StatusOK},
		{"GET", "/repos", http.StatusOK},
		{"POST", "/public", http.StatusForbidden},  // the first set names GET only
		{"DELETE", "/repos", http.StatusForbidden}, // the second set denies DELETE
		{"GET", "/admin", http.StatusForbidden},    // the first set matches no rule
		{"GET", "/public/../admin", http.StatusForbidden},
		{"GET", "/public/..%2Fadmin", http.StatusForbidden},
		{"GET", "/%70ublic", http.StatusOK},
	} {
		cu := captureUpstream(t, true, "ok")
		p, buf, caPEM := hrProxy(t, hrOpts{}, cu.srv)
		srv := httptest.NewServer(p)
		status, conn, _ := hrConnect(t, srv.URL, "443")
		if status != http.StatusOK {
			t.Fatalf("CONNECT to a rule host with a CA = %d, want 200", status)
		}
		got := hrInner(t, conn, caPEM, tc.method, tc.path)
		srv.Close()
		if got != tc.want {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, got, tc.want)
		}
		if tc.want == http.StatusForbidden {
			if cu.reached {
				t.Errorf("%s %s reached the upstream", tc.method, tc.path)
			}
			found := false
			for _, d := range hrRows(t, buf) {
				if d.RuleSource == ruleSourceHostRule && d.Decision == egress.Deny && strings.HasPrefix(d.Cause, "host rules: ") {
					found = true
				}
			}
			if !found {
				t.Errorf("%s %s: no policy:host-rule deny row naming the set:\n%s", tc.method, tc.path, buf.String())
			}
		}
	}
}

// TestHostRulesRefuseAPathNotInNormalForm: beside an allow-all rule, a deny on /admin. An upstream that
// resolves /admin//../users by RFC 3986 reads /admin/users, so on a rule host a path with an empty, "."
// or ".." segment, or a ";", is refused rather than judged in a form the host does not read.
func TestHostRulesRefuseAPathNotInNormalForm(t *testing.T) {
	for _, tc := range []struct {
		path  string
		want  int
		cause string
	}{
		{"/admin//../users", http.StatusForbidden, "not in normal form"},
		{"/users/./x", http.StatusForbidden, "not in normal form"},
		{"/admin", http.StatusForbidden, "rule 0 denies"},
		{"/admin;x=1", http.StatusForbidden, "not in normal form"},
		{"/users", http.StatusOK, ""},
	} {
		cu := captureUpstream(t, true, "ok")
		p, buf, caPEM := hrProxy(t, hrOpts{}, cu.srv)
		p.policy.hostRules = []types.HostRuleSet{{Rules: []types.HostRule{
			{Host: hrHost, Paths: []string{"/admin"}, Effect: types.HostRuleDeny},
			{Host: hrHost, Effect: types.HostRuleAllow},
		}}}
		srv := httptest.NewServer(p)
		status, conn, _ := hrConnect(t, srv.URL, "443")
		if status != http.StatusOK {
			t.Fatalf("CONNECT = %d, want 200", status)
		}
		got := hrInner(t, conn, caPEM, http.MethodGet, tc.path)
		srv.Close()
		if got != tc.want || (got == http.StatusForbidden) == cu.reached {
			t.Errorf("GET %s = %d reached=%v, want %d", tc.path, got, cu.reached, tc.want)
		}
		if tc.cause != "" && !slices.ContainsFunc(hrRows(t, buf), func(d egress.DecisionLog) bool {
			return d.RuleSource == ruleSourceHostRule && strings.Contains(d.Cause, tc.cause)
		}) {
			t.Errorf("GET %s: no policy:host-rule row with %q:\n%s", tc.path, tc.cause, buf.String())
		}
	}
}

// servletParamGET sends GET path inside a tunnel to a rule host whose rules are a deny on /admin beside
// allow-all, in front of an upstream that drops ";" path parameters before routing (as a Servlet container
// does, so /admin;x=1 is its /admin). It reports the status, how often the upstream was reached and the rows.
func servletParamGET(t *testing.T, path string) (int, int32, string) {
	t.Helper()
	var reached atomic.Int32
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		segs := strings.Split(r.URL.EscapedPath(), "/")
		for i, s := range segs {
			segs[i], _, _ = strings.Cut(s, ";")
		}
		_, _ = io.WriteString(w, "served "+strings.Join(segs, "/"))
	}))
	t.Cleanup(up.Close)
	p, buf, caPEM := hrProxy(t, hrOpts{}, up)
	p.policy.hostRules = []types.HostRuleSet{{Rules: []types.HostRule{
		{Host: hrHost, Paths: []string{"/admin"}, Effect: types.HostRuleDeny},
		{Host: hrHost, Effect: types.HostRuleAllow},
	}}}
	srv := httptest.NewServer(p)
	defer srv.Close()
	status, conn, _ := hrConnect(t, srv.URL, "443")
	if status != http.StatusOK {
		t.Fatalf("CONNECT = %d, want 200", status)
	}
	return hrInner(t, conn, caPEM, http.MethodGet, path), reached.Load(), buf.String()
}

func wantServletParamRefused(t *testing.T, path string) {
	t.Helper()
	status, reached, rows := servletParamGET(t, path)
	if status != http.StatusForbidden || reached != 0 {
		t.Errorf("GET %s = %d, upstream reached %d time(s), want 403 and never reached", path, status, reached)
	}
	if !strings.Contains(rows, "not in normal form") {
		t.Errorf("GET %s: no row naming the path parameter:\n%s", path, rows)
	}
}

// Each spelling of a path parameter is refused on a rule host, the way the matcher decodes it.
func TestHostRulesRefuseALiteralPathParameter(t *testing.T) { wantServletParamRefused(t, "/admin;x=1") }

func TestHostRulesRefuseAnUppercaseEncodedPathParameter(t *testing.T) {
	wantServletParamRefused(t, "/admin%3Bx=1")
}

func TestHostRulesRefuseALowercaseEncodedPathParameter(t *testing.T) {
	wantServletParamRefused(t, "/admin%3bx=1")
}

func TestHostRulesRefuseAnEncodedPathParameterMidPath(t *testing.T) {
	wantServletParamRefused(t, "/admin%3Bx/users")
}

// A literal parameter before a further segment is refused too, and a plain path still forwards.
func TestHostRulesPathParameterBeforeASegmentAndAPlainPath(t *testing.T) {
	wantServletParamRefused(t, "/admin;x=1/users")
	if status, reached, _ := servletParamGET(t, "/users"); status != http.StatusOK || reached != 1 {
		t.Errorf("GET /users = %d, upstream reached %d time(s), want 200 once", status, reached)
	}
}

// TestHostRulesRefuseAnUninspectedConnect: no CA, a port the proxy does not open for the host, and a VM
// proxy each refuse the CONNECT with policy:host-rule-needs-inspection, never an opaque tunnel.
func TestHostRulesRefuseAnUninspectedConnect(t *testing.T) {
	for name, tc := range map[string]struct {
		o    hrOpts
		port string
	}{
		"no CA":            {hrOpts{noCA: true}, "443"},
		"uninspected port": {hrOpts{mitmHosts: []string{hrHost + ":443"}}, "8443"},
		"VM run":           {hrOpts{vm: true}, "443"},
	} {
		cu := captureUpstream(t, true, "ok")
		p, buf, _ := hrProxy(t, tc.o, cu.srv)
		srv := httptest.NewServer(p)
		status, _, _ := hrConnect(t, srv.URL, tc.port)
		srv.Close()
		if status == http.StatusOK {
			t.Errorf("%s: CONNECT to a rule host = 200, want a refusal", name)
		}
		if !strings.Contains(buf.String(), `"rule_source":"`+ruleSourceHostRuleNeedsInspection+`"`) {
			t.Errorf("%s: no %s row:\n%s", name, ruleSourceHostRuleNeedsInspection, buf.String())
		}
	}
	// The inspected port of the same entry opens.
	cu := captureUpstream(t, true, "ok")
	p, _, caPEM := hrProxy(t, hrOpts{mitmHosts: []string{hrHost + ":443"}}, cu.srv)
	srv := httptest.NewServer(p)
	defer srv.Close()
	status, conn, _ := hrConnect(t, srv.URL, "443")
	if status != http.StatusOK || hrInner(t, conn, caPEM, "GET", "/public") != http.StatusOK {
		t.Errorf("CONNECT on the inspected port = %d, want 200 and an allowed GET", status)
	}
}

// TestHostRulesJudgePlainHTTP: a plain-HTTP request to a rule host is judged by its path, no CA needed.
func TestHostRulesJudgePlainHTTP(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/public", http.StatusOK},
		{"GET", "/admin", http.StatusForbidden},
		{"GET", "/public/%2e%2e/admin", http.StatusForbidden},
		{"GET", "/public//x", http.StatusForbidden}, // an empty segment is not normal form
		{"GET", "/public%00", http.StatusForbidden},
	} {
		cu := captureUpstream(t, false, "ok")
		p, buf, _ := hrProxy(t, hrOpts{noCA: true}, cu.srv)
		srv := httptest.NewServer(p)
		conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(conn, tc.method+" http://"+hrHost+tc.path+" HTTP/1.1\r\nHost: "+hrHost+"\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: tc.method})
		if err != nil {
			t.Fatalf("%s %s: read response: %v", tc.method, tc.path, err)
		}
		_ = resp.Body.Close()
		_ = conn.Close()
		srv.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("plain %s %s = %d, want %d", tc.method, tc.path, resp.StatusCode, tc.want)
		}
		if tc.want == http.StatusForbidden && (cu.reached || !strings.Contains(buf.String(), `"rule_source":"policy:host-rule"`)) {
			t.Errorf("plain %s %s: reached=%v, rows:\n%s", tc.method, tc.path, cu.reached, buf.String())
		}
	}
}

// TestHostRulesImposeNothingOnOtherHosts: a host no set names keeps today's lanes.
func TestHostRulesImposeNothingOnOtherHosts(t *testing.T) {
	p := CompilePolicy(types.RunPolicySpec{HostRules: hrSets()})
	if p.namesRuleHost("other.example.com") || !p.namesRuleHost(hrHost) {
		t.Error("namesRuleHost should name exactly the rule hosts")
	}
}

// TestConfigHostRulesLoadOnEveryRouteButAVM: rules on a VM run are refused at load. A rule on a host a
// broker or a via_org route serves is judged on each request (host_rules_broker_test.go,
// host_rules_via_org_test.go), so it loads.
func TestConfigHostRulesLoadOnEveryRouteButAVM(t *testing.T) {
	rule := func(h string) []types.HostRuleSet {
		return []types.HostRuleSet{{Rules: []types.HostRule{{Host: h, Effect: types.HostRuleAllow}}}}
	}
	gitGrant := uuid.New()
	for name, tc := range map[string]struct {
		c    Config
		want string
	}{
		"vm":   {Config{Policy: types.RunPolicySpec{HostRules: rule(hrHost)}, VM: true}, "VM run"},
		"hold": {Config{Policy: types.RunPolicySpec{HostRules: []types.HostRuleSet{{Rules: []types.HostRule{{Host: hrHost, Effect: types.HostRuleHold}}}}}}, "later release"},
	} {
		err := tc.c.validateHostRules()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one naming %q", name, err, tc.want)
		}
	}
	for name, c := range map[string]Config{
		"model route": {Policy: types.RunPolicySpec{HostRules: rule("api.anthropic.com")}},
		"git broker":  {Policy: types.RunPolicySpec{HostRules: rule("github.com")}, GitGrants: map[string]uuid.UUID{"o/r": uuid.New()}},
		"pat broker":  {Policy: types.RunPolicySpec{HostRules: rule("dev.azure.com")}, PATGrants: map[string]PATGrant{"dev.azure.com": {}}},
		"mcp broker":  {Policy: types.RunPolicySpec{HostRules: rule("mcp.example.com")}, MCPServers: []MCPServerConfig{{URL: "https://mcp.example.com/mcp"}}},
		"local model": {Policy: types.RunPolicySpec{HostRules: rule("models.internal")}, LocalModels: map[string]types.LocalModelSettings{"models.internal:11434": {}}},
		"via_org":     {Policy: types.RunPolicySpec{HostRules: rule("corp.example.com")}, ViaOrgRoutes: []ViaOrgRoute{{Host: "corp.example.com"}}},
		"via_org git": {Policy: types.RunPolicySpec{HostRules: rule("github.com")}, GitGrants: map[string]uuid.UUID{"o/r": gitGrant}, ViaOrgRoutes: []ViaOrgRoute{{GrantID: gitGrant, Host: "github.com"}}},
	} {
		if err := c.validateHostRules(); err != nil {
			t.Errorf("%s: a rule on a host judged per request was refused: %v", name, err)
		}
	}
}
