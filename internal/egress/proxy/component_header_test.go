// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A component's header delivery, end to end through the compiled injector
// (buildInjector, never a hand-built one): the rule dispatch authors for the
// component's bare host, and the "host:443" interception entry beside it. The
// header rides the host's standard TLS port and nothing else.

const (
	componentHost   = "svc.example"
	componentHeader = "X-Component-Token"
	componentValue  = "component-secret-value"
)

// componentControlPlane resolves every injection grant to the component's
// header and records which grants were asked for.
type componentControlPlane struct {
	*httptest.Server
	mu       sync.Mutex
	resolved []string
}

func newComponentControlPlane(t *testing.T) *componentControlPlane {
	t.Helper()
	cp := &componentControlPlane{}
	cp.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/internal/injection/") {
			http.NotFound(w, r)
			return
		}
		cp.mu.Lock()
		cp.resolved = append(cp.resolved, r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
		cp.mu.Unlock()
		_ = json.NewEncoder(w).Encode(types.ResolvedInjection{
			Host: componentHost, Header: componentHeader, Value: componentValue, JTI: uuid.NewString(),
			ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
		})
	}))
	t.Cleanup(cp.Close)
	return cp
}

func componentRule(host string) InjectionConfig {
	return InjectionConfig{GrantID: uuid.New(), InjectionRule: egress.InjectionRule{
		Host: host, Header: componentHeader, SecretName: "person-secret", Format: "%s", RequireTLS: true}}
}

// componentProxy is a proxy for a run whose component delivers a header to
// componentHost, under a policy that allows `allowed`. Port 443 dials a TLS
// origin that records what it receives, and every other port an echo server.
func componentProxy(t *testing.T, allowed ...string) (p *Proxy, origin *capturedUpstream, caPEM []byte, decisions *bytes.Buffer) {
	t.Helper()
	cp := newComponentControlPlane(t)
	pol := CompilePolicy(types.RunPolicySpec{AllowedDomains: allowed})
	inj, err := buildInjector(context.Background(), cp.URL, newTokenSource("tok"), pol, []InjectionConfig{componentRule(componentHost)}, cp.Client())
	if err != nil {
		t.Fatalf("buildInjector over the component's rule: %v", err)
	}
	certPEM, keyPEM := genTestCA(t)
	ca, err := newCertAuthority(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	origin = captureUpstream(t, true, "origin-ok")
	echo := startEcho(t)
	decisions = &bytes.Buffer{}
	p = newProxy(Options{
		RunID:           uuid.New(),
		Policy:          pol,
		Injector:        inj,
		Sink:            &decisionSink{out: decisions, ch: make(chan egress.DecisionLog, 64)},
		CA:              ca,
		MITMHosts:       []string{componentHost + ":443"}, // what dispatch authors
		Resolver:        publicResolver{},
		TLSClientConfig: testInsecureTLSConfig,
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			target := echo
			if _, port, _ := net.SplitHostPort(addr); port == "443" {
				target = upstreamAddr(origin.srv)
			}
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, target)
		},
	})
	return p, origin, certPEM, decisions
}

// CONNECT host:443: the proxy terminates the tunnel and sets the component's
// header, replacing whatever the sandbox sent under that name.
func TestConnect_ComponentHeaderIsInjectedOn443(t *testing.T) {
	p, origin, caPEM, _ := componentProxy(t, componentHost, componentHost+":8443")
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	conn := connectAndHandshakeMITM(t, proxySrv.URL, componentHost, caPEM)
	defer conn.Close()
	req, _ := http.NewRequest(http.MethodGet, "https://"+componentHost+"/v1/things", nil)
	req.Header.Set(componentHeader, "what-the-sandbox-guessed")
	if err := req.Write(conn); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		t.Fatalf("read the intercepted response: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "origin-ok" {
		t.Fatalf("request through the intercepted tunnel = %d %q, want the origin's answer", resp.StatusCode, body)
	}
	if got := origin.header.Values(componentHeader); len(got) != 1 || got[0] != componentValue {
		t.Fatalf("the origin saw %s = %q, want exactly the component's secret", componentHeader, got)
	}
	if origin.path != "/v1/things" {
		t.Errorf("the origin saw path %q", origin.path)
	}
}

// CONNECT host:8443: the entry names :443, so this tunnel is not terminated.
// The proxy never sees the request inside it and can set no header; whether
// the tunnel opens at all is the egress entry's question, and it is refused
// when the component's hosts do not list the port.
func TestConnect_ComponentHeaderHostOn8443CarriesNoHeader(t *testing.T) {
	t.Run("the egress entry lists the port: an opaque tunnel", func(t *testing.T) {
		p, origin, _, _ := componentProxy(t, componentHost+":443", componentHost+":8443")
		proxySrv := httptest.NewServer(p)
		defer proxySrv.Close()

		conn, status := connectThrough(t, proxySrv.URL, componentHost+":8443")
		defer conn.Close()
		if !strings.Contains(status, "200") {
			t.Fatalf("CONNECT %s:8443 = %q, want the tunnel the egress entry allows", componentHost, status)
		}
		// A terminated tunnel would answer with a TLS handshake; bytes echoed
		// back verbatim are a tunnel the proxy is not reading.
		probe := "GET / HTTP/1.1\r\nHost: " + componentHost + "\r\n\r\n"
		if _, err := io.WriteString(conn, probe); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, len(probe))
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, err := io.ReadFull(conn, got); err != nil || string(got) != probe {
			t.Fatalf("the :8443 tunnel returned %q (%v), want the bytes sent, untouched", got, err)
		}
		if strings.Contains(string(got), componentValue) || origin.reached {
			t.Error("the component's header reached the :8443 connection")
		}
	})
	t.Run("the egress entry does not list the port: refused", func(t *testing.T) {
		p, origin, _, decisions := componentProxy(t, componentHost+":443")
		proxySrv := httptest.NewServer(p)
		defer proxySrv.Close()

		conn, status := connectThrough(t, proxySrv.URL, componentHost+":8443")
		defer conn.Close()
		if !strings.Contains(status, "403") {
			t.Fatalf("CONNECT %s:8443 = %q, want 403: the component's hosts list only :443", componentHost, status)
		}
		if origin.reached || strings.Contains(status, componentValue) || strings.Contains(decisions.String(), componentValue) {
			t.Error("a refused tunnel reached the origin or disclosed the secret")
		}
	})
}

// Plain HTTP to the header host: the rule requires TLS, so the request is
// refused outright — on port 80 and on a cleartext request aimed at :443 —
// and the secret goes nowhere.
func TestConnect_ComponentHeaderPlainHTTPIsRefusedByRequireTLS(t *testing.T) {
	for _, url := range []string{"http://" + componentHost + "/v1/things", "http://" + componentHost + ":443/v1/things"} {
		t.Run(url, func(t *testing.T) {
			p, origin, _, decisions := componentProxy(t, componentHost)
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, url))
			if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "requires TLS") {
				t.Fatalf("plain request = %d %q, want 403 naming the TLS requirement", rec.Code, rec.Body.String())
			}
			if origin.reached {
				t.Error("a refused plain request reached the origin")
			}
			if !strings.Contains(decisions.String(), ruleSourceRequireTLS) {
				t.Errorf("no %s decision was recorded: %s", ruleSourceRequireTLS, decisions.String())
			}
			if strings.Contains(rec.Body.String(), componentValue) || strings.Contains(decisions.String(), componentValue) {
				t.Error("the refusal discloses the secret")
			}
		})
	}
}

// One host, one rule. A second rule for a host — however it is spelled — is
// refused before its credential is resolved: the first rule's credential
// would otherwise be replaced by the second's on the first's traffic.
func TestInjector_SecondRuleForOneHostIsRefused(t *testing.T) {
	for name, second := range map[string]string{
		"the same host":       componentHost,
		"another case":        "SVC.Example",
		"a trailing dot":      componentHost + ".",
		"padded with a space": " " + componentHost,
	} {
		t.Run(name, func(t *testing.T) {
			cp := newComponentControlPlane(t)
			pol := CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{componentHost, "other.example"}})
			first, other, dup := componentRule(componentHost), componentRule("other.example"), componentRule(second)
			_, err := buildInjector(context.Background(), cp.URL, newTokenSource("tok"), pol, []InjectionConfig{first, other, dup}, cp.Client())
			if err == nil || !strings.Contains(err.Error(), "more than one rule") {
				t.Fatalf("buildInjector = %v, want a refusal of the second rule for %s", err, componentHost)
			}
			cp.mu.Lock()
			defer cp.mu.Unlock()
			for _, id := range cp.resolved {
				if id == dup.GrantID.String() {
					t.Error("the refused rule's credential was resolved")
				}
			}
		})
	}
	// Two rules for two hosts are what they always were.
	cp := newComponentControlPlane(t)
	pol := CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{componentHost, "other.example"}})
	inj, err := buildInjector(context.Background(), cp.URL, newTokenSource("tok"), pol,
		[]InjectionConfig{componentRule(componentHost), componentRule("other.example")}, cp.Client())
	if err != nil || len(inj.byHost) != 2 {
		t.Fatalf("buildInjector over two hosts = %v (%d rules), want both", err, len(inj.byHost))
	}
}

// The same refusal is the proxy's boot: a config with two rules for one host
// never becomes a server, so the run fails before its sandbox has any egress.
func TestInjector_SecondRuleForOneHostRefusesProxyBoot(t *testing.T) {
	cp := newComponentControlPlane(t)
	boot := func(rules ...InjectionConfig) error {
		// Through the loader, as every substrate hands the sidecar its config.
		raw, err := json.Marshal(&Config{
			RunID: uuid.New(), ControlPlaneURL: cp.URL, RunToken: "tok", Listen: "127.0.0.1:0",
			Policy:    types.RunPolicySpec{AllowedDomains: []string{componentHost}},
			Injection: rules,
		})
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfigBytes(raw)
		if err != nil {
			t.Fatalf("LoadConfigBytes: %v", err)
		}
		srv, err := NewServer(context.Background(), cfg, cp.Client(), &bytes.Buffer{})
		if err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx)
		}
		return err
	}
	if err := boot(componentRule(componentHost), componentRule(componentHost)); err == nil || !strings.Contains(err.Error(), "more than one rule") {
		t.Fatalf("NewServer over two rules for one host = %v, want a refusal", err)
	}
	if err := boot(componentRule(componentHost)); err != nil {
		t.Fatalf("NewServer over one rule = %v, want it to boot", err)
	}
}
