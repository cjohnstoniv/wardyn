// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// newRuleTestProxy is newTestProxy with the VM flag and the port rule set.
func newRuleTestProxy(t *testing.T, spec types.RunPolicySpec, upstreamAddr string, vm bool, rule string, ap *approvalClient) (*Proxy, *syncBuf) {
	t.Helper()
	buf := &syncBuf{}
	p := newProxy(Options{
		RunID:    uuid.New(),
		Policy:   CompilePolicy(spec),
		Approval: ap,
		Sink:     &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)},
		Resolver: publicResolver{},
		Dial:     redirectDial(upstreamAddr),
	})
	p.vm, p.portRule = vm, effectivePortRule(vm, rule)
	return p, buf
}

// syncBuf is the decision sink's buffer for tests whose rows are written off
// the test goroutine (the relay reports asynchronously).
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (s *syncBuf) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b.Reset()
}

func (s *syncBuf) Len() int { return len(s.String()) }

// TestPortRule pins #3193: a bare host is open on every port today and stays
// so with the switch off; on (and on every VM run) it reaches 80/443 only,
// and a host:port entry opens exactly that port.
func TestPortRule(t *testing.T) {
	spec := types.RunPolicySpec{
		AllowedDomains:   []string{"example.test", "*.wild.test", "db.example.test:5432", "denied.test"},
		DeniedDomains:    []string{"blocked.test:22"},
		FirstUseApproval: types.FirstUseAlwaysDeny,
	}
	for _, c := range []struct {
		name      string
		vm        bool
		rule      string
		host      string
		port      int
		want      egress.Decision
		source    string
		dryMarked bool
	}{
		{"off: bare host :22 allowed (today)", false, "", "example.test", 22, egress.Allow, "policy:allowed", false},
		{"explicit off is off", false, PortRuleOff, "example.test", 22, egress.Allow, "policy:allowed", false},
		{"on: bare host :22 refused", false, PortRuleOn, "example.test", 22, egress.Deny, ruleSourcePort, false},
		{"on: bare host :443 allowed", false, PortRuleOn, "example.test", 443, egress.Allow, "policy:allowed", false},
		{"on: bare host :80 allowed", false, PortRuleOn, "example.test", 80, egress.Allow, "policy:allowed", false},
		{"on: wildcard :8443 refused", false, PortRuleOn, "a.wild.test", 8443, egress.Deny, ruleSourcePort, false},
		{"on: host:port grant opens its port", false, PortRuleOn, "db.example.test", 5432, egress.Allow, "policy:allowed", false},
		{"on: a grant opens only its port", false, PortRuleOn, "db.example.test", 5433, egress.Deny, ruleSourcePort, false},
		{"on: deny still wins and keeps its row", false, PortRuleOn, "blocked.test", 22, egress.Deny, "policy:denied", false},
		{"dry run: refused port allowed and marked", false, PortRuleDryRun, "example.test", 22, egress.Allow, "policy:allowed", true},
		{"dry run: web port not marked", false, PortRuleDryRun, "example.test", 443, egress.Allow, "policy:allowed", false},
		{"dry run: granted port not marked", false, PortRuleDryRun, "db.example.test", 5432, egress.Allow, "policy:allowed", false},
		{"vm: on whatever port_rule says", true, "", "example.test", 22, egress.Deny, ruleSourcePort, false},
		{"vm: on even with port_rule off", true, PortRuleOff, "example.test", 22, egress.Deny, ruleSourcePort, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, _ := newRuleTestProxy(t, spec, "127.0.0.1:1", c.vm, c.rule, nil)
			dec, _, log := p.evaluate(context.Background(), c.host, c.port, http.MethodConnect, "")
			if dec != c.want || log == nil || log.RuleSource != c.source {
				t.Fatalf("decision %q %+v, want %q %q", dec, log, c.want, c.source)
			}
			if got := log.PortRule == PortRuleDryRun; got != c.dryMarked {
				t.Fatalf("port_rule %q, want marked=%v", log.PortRule, c.dryMarked)
			}
		})
	}
}

func TestPortRuleAllowAllReachesWebPortsOnly(t *testing.T) {
	p, _ := newRuleTestProxy(t, types.RunPolicySpec{AllowAllEgress: true}, "127.0.0.1:1", false, PortRuleOn, nil)
	if dec, _, log := p.evaluate(context.Background(), "any.test", 22, http.MethodConnect, ""); dec != egress.Deny || log.RuleSource != ruleSourcePort {
		t.Fatalf("allow-all :22 = %q %q, want policy:port", dec, log.RuleSource)
	}
	if dec, _, _ := p.evaluate(context.Background(), "any.test", 443, http.MethodConnect, ""); dec != egress.Allow {
		t.Fatalf("allow-all :443 = %q", dec)
	}
}

// TestPortRuleRaisesNoApproval: a first-use approval is host-wide and never
// opens a port, so the rule refuses before one is raised.
func TestPortRuleRaisesNoApproval(t *testing.T) {
	var raised atomic.Int32
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raised.Add(1)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: uuid.New(), State: types.ApprovalPending})
	}))
	defer cp.Close()
	ap := newApprovalClient(cp.URL, newTokenSource("tok"), uuid.New(), cp.Client())
	p, _ := newRuleTestProxy(t, types.RunPolicySpec{FirstUseApproval: types.FirstUseDenyWithReview}, "127.0.0.1:1", true, "", ap)
	dec, _, log := p.evaluate(context.Background(), "unknown.test", 22, http.MethodConnect, "")
	if dec != egress.Deny || log.RuleSource != ruleSourcePort || raised.Load() != 0 {
		t.Fatalf("decision %q %q, %d approvals raised; want policy:port and none", dec, log.RuleSource, raised.Load())
	}
}

// TestPortRuleRefusalOverTheWire: the sandbox sees the reason, the detail and
// whose policy decided, like any other policy refusal.
func TestPortRuleRefusalOverTheWire(t *testing.T) {
	p, buf := newRuleTestProxy(t, types.RunPolicySpec{AllowedDomains: []string{"example.test"}}, "127.0.0.1:1", false, PortRuleOn, nil)
	srv := httptest.NewServer(p)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodConnect, srv.URL, nil)
	req.Host = "example.test:22"
	resp, err := srv.Client().Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || resp.Header.Get(egressHeaderReason) != ruleSourcePort ||
		!strings.Contains(resp.Header.Get(egressHeaderDetail), "ports 80 and 443") {
		t.Fatalf("status %d headers %v", resp.StatusCode, resp.Header)
	}
	if rs := rows(t, buf); len(rs) != 1 || rs[0].RuleSource != ruleSourcePort || rs[0].Request.Port != 22 {
		t.Fatalf("rows %+v", rs)
	}
	if !policyDecidedReason(ruleSourcePort) {
		t.Fatal("policy:port must carry the policy attribution headers")
	}
}

func TestConfigPortRuleValidated(t *testing.T) {
	base := `{"run_id":"5f1d0c2e-0000-4000-8000-0000000005a1","control_plane_url":"http://127.0.0.1:9","run_token":"t","policy":{"allowed_domains":["a.test"]}`
	for rule, ok := range map[string]bool{"": true, "off": true, "dry_run": true, "on": true, "On": false, "strict": false} {
		_, err := LoadConfigBytes([]byte(base + `,"port_rule":"` + rule + `"}`))
		if (err == nil) != ok {
			t.Errorf("port_rule %q: err %v, want ok=%v", rule, err, ok)
		}
	}
	if _, err := LoadConfigBytes([]byte(base + `,"vm":true}`)); err != nil {
		t.Fatalf("vm: %v", err)
	}
}

// TestNewServerCarriesPortRule: the config's vm flag and port rule reach the
// proxy NewServer builds (a VM run's rule is on whatever port_rule says).
func TestNewServerCarriesPortRule(t *testing.T) {
	for _, c := range []struct {
		vm   bool
		rule string
		want string
	}{{false, "", PortRuleOff}, {false, PortRuleDryRun, PortRuleDryRun}, {true, PortRuleOff, PortRuleOn}} {
		cfg := &Config{RunID: uuid.New(), ControlPlaneURL: "http://127.0.0.1:9", RunToken: "tok",
			Policy: types.RunPolicySpec{AllowedDomains: []string{"a.test"}}, VM: c.vm, PortRule: c.rule}
		if err := cfg.applyDefaultsAndValidate(); err != nil {
			t.Fatal(err)
		}
		srv, err := NewServer(context.Background(), cfg, http.DefaultClient, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if srv.proxy.vm != c.vm || srv.proxy.portRule != c.want {
			t.Errorf("vm=%v rule=%q: proxy has vm=%v rule %q, want %q", c.vm, c.rule, srv.proxy.vm, srv.proxy.portRule, c.want)
		}
		_ = srv.Shutdown(context.Background())
	}
}
