// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/test/awsssofake"
)

const faultBedrockHost = "bedrock-runtime.us-east-1.amazonaws.com"

type bedrockExchange struct {
	status  int
	errType string
	body    string
}

// driveBedrock sends one Converse call per fault value through the proxy's
// MITM'd request path (the bearer lane's) at an awsssofake Bedrock stub, and
// returns what the sandbox saw plus every decision row the proxy wrote.
func driveBedrock(t *testing.T, faults ...string) ([]bedrockExchange, []egress.DecisionLog) {
	t.Helper()
	_, h := awsssofake.NewHandler()
	origin := httptest.NewServer(h)
	t.Cleanup(origin.Close)

	p, buf := newLocalRouteProxy(t, "http://cp.invalid", "RUNTOK", upstreamAddr(origin), &injector{}, nil)
	p.mitmHosts = map[string]bool{faultBedrockHost: true}
	p.mitmPlaintext = map[string]bool{plaintextKey(faultBedrockHost, 443): true}

	var seen []bedrockExchange
	for _, f := range faults {
		req := httptest.NewRequest(http.MethodPost,
			"https://"+faultBedrockHost+"/model/us.anthropic.claude-haiku-4-5/converse", strings.NewReader(`{"messages":[]}`))
		if f != "" {
			req.Header.Set("X-Fake-Bedrock-Fault", f)
		}
		rec := httptest.NewRecorder()
		p.serveMITMRequest(rec, req, faultBedrockHost, 443)
		seen = append(seen, bedrockExchange{rec.Code, rec.Header().Get("x-amzn-ErrorType"), rec.Body.String()})
	}

	var rows []egress.DecisionLog
	sc := bufio.NewScanner(strings.NewReader(buf.String()))
	for sc.Scan() {
		var dl egress.DecisionLog
		if err := json.Unmarshal(sc.Bytes(), &dl); err != nil {
			t.Fatalf("decision line %q: %v", sc.Text(), err)
		}
		rows = append(rows, dl)
	}
	if len(rows) != len(faults) {
		t.Fatalf("%d decision rows for %d calls: %s", len(rows), len(faults), buf.String())
	}
	return seen, rows
}

// The refusal reaches the sandbox EXACTLY as AWS sent it — status, error class
// header and body — so the agent's SDK retries a throttle and stops on a deny
// the way it would talking to AWS directly. The proxy only ever observes.
func TestBedrockDataPlaneFault_ResponsePassesThroughUnchanged(t *testing.T) {
	seen, rows := driveBedrock(t, "deny", "throttle:1", "")
	want := []struct {
		status  int
		errType string
		body    string
	}{
		{403, "AccessDeniedException:", "explicit deny in a service control policy"},
		{429, "ThrottlingException:", "Too many requests"},
		{200, "", "wardyn-bedrock-stub"},
	}
	for i, w := range want {
		got := seen[i]
		if got.status != w.status || !strings.HasPrefix(got.errType, w.errType) || !strings.Contains(got.body, w.body) {
			t.Errorf("call %d: sandbox saw %d %q %s, want %d %q …%s…", i, got.status, got.errType, got.body, w.status, w.errType, w.body)
		}
		if rows[i].Decision != egress.Allow || rows[i].RuleSource != ruleSourceLLMMITM {
			t.Errorf("call %d: decision %s/%s, want allow/%s — the proxy allowed it; AWS refused it", i, rows[i].Decision, rows[i].RuleSource, ruleSourceLLMMITM)
		}
	}
}

// The decision row names what AWS said: the class on each refusal, "recovered"
// once on the first clean call after one, and nothing on an ordinary call.
func TestBedrockDataPlaneFault_DecisionRowNamesTheAWSClass(t *testing.T) {
	for _, tc := range []struct {
		name   string
		faults []string
		want   []string
	}{
		{"deny", []string{"deny"}, []string{"AccessDeniedException"}},
		{"throttle then ok", []string{"throttle:2", "throttle:2", "throttle:2", ""},
			[]string{"ThrottlingException", "ThrottlingException", "recovered", ""}},
		{"throttle exhausted", []string{"throttle:9", "throttle:9", "throttle:9"},
			[]string{"ThrottlingException", "ThrottlingException", "ThrottlingException"}},
		{"clean", []string{"", ""}, []string{"", ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, rows := driveBedrock(t, tc.faults...)
			for i, w := range tc.want {
				if rows[i].UpstreamFault != w {
					t.Errorf("call %d: upstream_fault = %q, want %q", i, rows[i].UpstreamFault, w)
				}
			}
		})
	}
}

// Only the class the header names, on the status AWS documents for it: a 403
// that is an ExpiredTokenException, or a 429 that is ModelNotReadyException,
// is some other failure and must not be reported as a policy deny or throttle.
func TestBedrockDataPlaneFault_OtherClassesAreNotReported(t *testing.T) {
	p, _ := newLocalRouteProxy(t, "http://cp.invalid", "RUNTOK", "127.0.0.1:1", &injector{}, nil)
	for _, tc := range []struct {
		status  int
		errType string
		path    string
	}{
		{403, "ExpiredTokenException", "/model/m/converse"},
		{429, "ModelNotReadyException", "/model/m/converse"},
		{400, "ThrottlingException", "/model/m/converse"},
		{403, "AccessDeniedException", "/federation/credentials"},
	} {
		resp := &http.Response{StatusCode: tc.status, Header: http.Header{"X-Amzn-Errortype": {tc.errType}}}
		if got := p.bedrockUpstreamFault(faultBedrockHost, tc.path, resp); got != "" {
			t.Errorf("%d %s %s: upstream_fault = %q, want none", tc.status, tc.errType, tc.path, got)
		}
	}
}

// The plain forward lane (the http:// WARDYN_BEDROCK_BASE_URL test hatch, a
// cluster-local endpoint isBedrockHost does not recognise) reports the same
// class: the /model/ path, not the hostname, is what marks a data-plane call.
func TestBedrockDataPlaneFault_PlainLaneReportsTheClass(t *testing.T) {
	_, h := awsssofake.NewHandler()
	origin := httptest.NewServer(h)
	defer origin.Close()
	buf := &bytes.Buffer{}
	p := newProxy(Options{
		RunID:    uuid.New(),
		Policy:   CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{plainMITMHost}}),
		Sink:     &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)},
		Resolver: publicResolver{},
		Dial:     redirectDial(upstreamAddr(origin)),
	})
	req := mustAbsReq(t, http.MethodPost, "http://"+plainMITMHost+":8090/model/m/converse", "{}")
	req.Header.Set("X-Fake-Bedrock-Fault", "deny")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want the 403 relayed: %s", rec.Code, rec.Body.String())
	}
	var dl egress.DecisionLog
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &dl); err != nil {
		t.Fatalf("decision row %q: %v", buf.String(), err)
	}
	if dl.Decision != egress.Allow || dl.UpstreamFault != "AccessDeniedException" {
		t.Errorf("decision = %s upstream_fault = %q, want allow/AccessDeniedException", dl.Decision, dl.UpstreamFault)
	}
}
