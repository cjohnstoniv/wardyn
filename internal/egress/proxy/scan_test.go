// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/contentscan"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const scanTestSecret = "sk-ant-test-DEADBEEF-0123456789" // >= secretmask.MinLen

// TestLLMInspectionConfigRoundTrip proves an llm_inspection policy block survives
// the WARDYN_PROXY_CONFIG_JSON path (LoadConfigBytes) and yields a working engine —
// i.e. the new RunPolicySpec field threads to the proxy with no extra plumbing.
func TestLLMInspectionConfigRoundTrip(t *testing.T) {
	cfg := Config{
		RunID:           uuid.New(),
		ControlPlaneURL: "http://wardynd:8080",
		RunToken:        "tok",
		Policy: types.RunPolicySpec{
			AllowedDomains: []string{anthropicHost},
			LLMInspection: &types.LLMInspectionSpec{
				Mode:                  "alert",
				DetectSecrets:         true,
				WorkspaceSecretValues: []string{"supersecretvalue123"},
			},
		},
	}
	b, _ := json.Marshal(cfg)
	c, err := LoadConfigBytes(b)
	if err != nil {
		t.Fatalf("LoadConfigBytes: %v", err)
	}
	if c.Policy.LLMInspection == nil || c.Policy.LLMInspection.Mode != "alert" {
		t.Fatalf("llm_inspection did not round-trip: %+v", c.Policy.LLMInspection)
	}
	eng, err := contentscan.NewEngine(*c.Policy.LLMInspection, [][]byte{[]byte("supersecretvalue123")})
	if err != nil || eng == nil {
		t.Fatalf("engine from round-tripped spec: eng=%v err=%v", eng, err)
	}
}

func scanEngine(t *testing.T, mode string, secrets ...string) *contentscan.Engine {
	t.Helper()
	corpus := make([][]byte, 0, len(secrets))
	for _, s := range secrets {
		corpus = append(corpus, []byte(s))
	}
	eng, err := contentscan.NewEngine(types.LLMInspectionSpec{Mode: mode, DetectSecrets: true}, corpus)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	if eng == nil {
		t.Fatal("expected a non-nil engine")
	}
	return eng
}

// scanEngineFailClosed builds the engine a STRICT operator actually buys:
// mode=block PLUS on_scanner_error=block, i.e. Engine.BlocksOnError() == true.
//
// F090: every other scan test in this package builds a fail-OPEN engine
// (scanEngine leaves on_scanner_error at its default) and asserts the ALLOW
// outcome, so the two fail-CLOSED refusals in llm_routes.go — the
// uninspected-channel refusal and the oversize refusal — were executed by ZERO
// tests, while THREAT-MODEL.md §5.1a sells that mode as what the strict
// operator is buying. The BlocksOnError assertion below is part of the pin: if
// engine construction ever stops honouring on_scanner_error, these tests fail
// on the fixture rather than silently degrading into fail-open coverage.
func scanEngineFailClosed(t *testing.T, secrets ...string) *contentscan.Engine {
	t.Helper()
	corpus := make([][]byte, 0, len(secrets))
	for _, s := range secrets {
		corpus = append(corpus, []byte(s))
	}
	eng, err := contentscan.NewEngine(types.LLMInspectionSpec{
		Mode: "block", DetectSecrets: true, OnScannerError: "block",
	}, corpus)
	if err != nil {
		t.Fatalf("NewEngine(fail-closed): %v", err)
	}
	if eng == nil {
		t.Fatal("expected a non-nil engine")
	}
	if !eng.BlocksOnError() {
		t.Fatal("fixture is not fail-closed: on_scanner_error=block must make BlocksOnError() true")
	}
	return eng
}

// errBody is a request body whose read fails — the only way to reach
// scanBufferedBody's unconditional read-error deny.
type errBody struct{}

func (errBody) Read([]byte) (int, error) { return 0, errors.New("simulated body read failure") }
func (errBody) Close() error             { return nil }

func anthropicMessagesBody(content string) string {
	return `{"model":"claude","max_tokens":16,"messages":[{"role":"user","content":"` + content + `"}]}`
}

// anthropicInjector mirrors the startup-minted Anthropic credential used by the
// other LLM-route tests.
func anthropicInjector() *injector {
	return staticInj(map[string]injectedHeader{
		anthropicHost: {name: "X-Api-Key", value: "BROKERED-KEY"},
	})
}

func TestLLMScanAlertForwardsBodyAndAudits(t *testing.T) {
	cu := captureUpstream(t, true, "llm-ok")

	p, buf := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", upstreamAddr(cu.srv),
		anthropicInjector(), testInsecureTLSConfig)
	p.scanner = scanEngine(t, "alert", scanTestSecret)

	body := anthropicMessagesBody("please use key " + scanTestSecret + " now")
	rec := httptest.NewRecorder()
	req := mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "llm-ok" {
		t.Fatalf("alert must forward: status=%d body=%q", rec.Code, rec.Body.String())
	}
	if cu.body != body {
		t.Fatalf("alert must forward the body UNCHANGED:\n got %q\nwant %q", cu.body, body)
	}
	d := lastDecision(t, buf)
	if d.Decision != egress.Allow || d.RuleSource != ruleSourceLLM {
		t.Fatalf("decision = %+v, want brokered:llm allow", d)
	}
	if d.Scan == nil || d.Scan.Action != "alert" || !d.Scan.Scanned {
		t.Fatalf("scan summary = %+v, want scanned alert", d.Scan)
	}
	if len(d.Scan.Findings) == 0 || d.Scan.Findings[0].Sample != "<secret-hidden>" {
		t.Fatalf("expected a masked finding, got %+v", d.Scan.Findings)
	}
	// The decision log (audit channel) must never carry the raw secret.
	if strings.Contains(buf.String(), scanTestSecret) {
		t.Fatal("decision log leaked the raw secret")
	}
}

func TestLLMScanBlockRefusesAndSkipsUpstream(t *testing.T) {
	cases := []struct {
		name    string
		prefix  string
		reqPath string
		inj     *injector
		reqBody string
	}{
		{
			name:    "anthropic",
			prefix:  llmAnthropicPrefix,
			reqPath: "v1/messages",
			inj:     anthropicInjector(),
			reqBody: anthropicMessagesBody("leak " + scanTestSecret),
		},
		{
			name:    "openai",
			prefix:  llmOpenAIPrefix,
			reqPath: "v1/chat/completions",
			inj:     openaiInjector(),
			reqBody: `{"model":"gpt-4","messages":[{"role":"user","content":"leak ` + scanTestSecret + `"}]}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cu := captureUpstream(t, true, "should-not-happen")

			p, buf := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", upstreamAddr(cu.srv),
				tc.inj, testInsecureTLSConfig)
			p.scanner = scanEngine(t, "block", scanTestSecret)

			rec := httptest.NewRecorder()
			req := mustLocalReq(t, http.MethodPost, tc.prefix+tc.reqPath, strings.NewReader(tc.reqBody))
			req.Header.Set("Content-Type", "application/json")
			p.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("block status = %d, want 403", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "llm_content_blocked") {
				t.Fatalf("block body = %q, want llm_content_blocked", rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), scanTestSecret) {
				t.Fatal("block response leaked the raw secret (must be content-free)")
			}
			if cu.reached {
				t.Fatal("blocked request must NOT reach the upstream")
			}
			d := lastDecision(t, buf)
			if d.Decision != egress.Deny || d.RuleSource != ruleSourceLLMBlocked {
				t.Fatalf("decision = %+v, want scan:blocked deny", d)
			}
			if d.Scan == nil || d.Scan.Action != "block" {
				t.Fatalf("scan summary = %+v, want block", d.Scan)
			}
		})
	}
}

func TestLLMScanCleanTurnIsQuiet(t *testing.T) {
	cu := captureUpstream(t, true, "llm-ok")

	p, buf := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", upstreamAddr(cu.srv),
		anthropicInjector(), testInsecureTLSConfig)
	p.scanner = scanEngine(t, "alert", scanTestSecret)

	rec := httptest.NewRecorder()
	req := mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"v1/messages",
		strings.NewReader(anthropicMessagesBody("an ordinary clean prompt")))
	req.Header.Set("Content-Type", "application/json")
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "llm-ok" {
		t.Fatalf("clean turn must forward: status=%d body=%q", rec.Code, rec.Body.String())
	}
	d := lastDecision(t, buf)
	if d.Decision != egress.Allow || d.RuleSource != ruleSourceLLM {
		t.Fatalf("decision = %+v, want brokered:llm allow", d)
	}
	if d.Scan != nil {
		t.Fatalf("a clean scan must stay quiet (no scan summary), got %+v", d.Scan)
	}
}

func TestLLMScanCountTokensIsScanned(t *testing.T) {
	cu := captureUpstream(t, true, "")
	p, buf := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", upstreamAddr(cu.srv),
		anthropicInjector(), testInsecureTLSConfig)
	p.scanner = scanEngine(t, "block", scanTestSecret)

	rec := httptest.NewRecorder()
	req := mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"v1/messages/count_tokens",
		strings.NewReader(anthropicMessagesBody("leak "+scanTestSecret)))
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("count_tokens carrying a secret must be scanned+blocked, got %d", rec.Code)
	}
	if cu.reached {
		t.Fatal("blocked count_tokens must not reach the upstream")
	}
	if d := lastDecision(t, buf); d.RuleSource != ruleSourceLLMBlocked {
		t.Fatalf("decision = %+v, want scan:blocked", d)
	}
}

func TestLLMScanBatchesMarkedUninspected(t *testing.T) {
	cu := captureUpstream(t, true, "ok")
	p, buf := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", upstreamAddr(cu.srv),
		anthropicInjector(), testInsecureTLSConfig)
	p.scanner = scanEngine(t, "alert", scanTestSecret)

	rec := httptest.NewRecorder()
	req := mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"v1/messages/batches",
		strings.NewReader(`{"requests":[]}`))
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !cu.reached {
		t.Fatalf("batches in alert mode must forward, status=%d reached=%v", rec.Code, cu.reached)
	}
	d := lastDecision(t, buf)
	if d.Scan == nil || d.Scan.Action != "skipped" || d.Scan.SkipReason != "uninspected_channel" {
		t.Fatalf("batches must be honestly marked uninspected, got %+v", d.Scan)
	}
}

func TestLLMScanToolUseInputThroughProxy(t *testing.T) {
	cu := captureUpstream(t, true, "ok")
	p, buf := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", upstreamAddr(cu.srv),
		anthropicInjector(), testInsecureTLSConfig)
	p.scanner = scanEngine(t, "alert", scanTestSecret)

	body := `{"model":"c","max_tokens":1,"messages":[{"role":"assistant","content":` +
		`[{"type":"tool_use","name":"db","input":{"pw":"` + scanTestSecret + `"}}]}]}`
	rec := httptest.NewRecorder()
	req := mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"v1/messages", strings.NewReader(body))
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || cu.body != body {
		t.Fatalf("alert must forward tool_use body unchanged: status=%d", rec.Code)
	}
	d := lastDecision(t, buf)
	if d.Scan == nil || len(d.Scan.Findings) == 0 {
		t.Fatalf("secret in tool_use.input should be found, got %+v", d.Scan)
	}
	if !strings.Contains(d.Scan.Findings[0].FieldPath, ".input.pw") {
		t.Fatalf("unexpected field path %q", d.Scan.Findings[0].FieldPath)
	}
}

func TestLLMScanOversizeBodyForwardedIntact(t *testing.T) {
	saved := maxLLMScanBody
	maxLLMScanBody = 64 // force the oversize path without allocating tens of MiB
	defer func() { maxLLMScanBody = saved }()

	cu := captureUpstream(t, true, "ok")
	p, buf := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", upstreamAddr(cu.srv),
		anthropicInjector(), testInsecureTLSConfig)
	p.scanner = scanEngine(t, "alert", scanTestSecret) // alert => fail-open on oversize

	body := anthropicMessagesBody("padding " + strings.Repeat("x", 300) + " " + scanTestSecret)
	if len(body) <= maxLLMScanBody {
		t.Fatal("test body must exceed the scan cap")
	}
	rec := httptest.NewRecorder()
	req := mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"v1/messages", strings.NewReader(body))
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("oversize body in alert mode must forward, got %d", rec.Code)
	}
	if cu.body != body {
		t.Fatalf("oversize body must be forwarded INTACT (never truncated): got %d want %d bytes", len(cu.body), len(body))
	}
	d := lastDecision(t, buf)
	if d.Scan == nil || d.Scan.SkipReason != "body_oversize" || d.Scan.Scanned {
		t.Fatalf("want body_oversize skip, got %+v", d.Scan)
	}
}

func openaiInjector() *injector {
	return staticInj(map[string]injectedHeader{
		openaiHost: {name: "Authorization", value: "Bearer BROKERED-OPENAI"},
	})
}

func forwardScanEngine(t *testing.T, mode string) *contentscan.Engine {
	t.Helper()
	eng, err := contentscan.NewEngine(types.LLMInspectionSpec{
		Mode: mode, DetectSecrets: true, InspectForwardEgress: true,
	}, [][]byte{[]byte(scanTestSecret)})
	if err != nil || eng == nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return eng
}

func TestForwardEgressScanBlocksGenericConnector(t *testing.T) {
	cu := captureUpstream(t, false, "")
	p, buf := newTestProxy(t, types.RunPolicySpec{AllowedDomains: []string{"connector.test"}},
		upstreamAddr(cu.srv), nil, nil)
	p.scanner = forwardScanEngine(t, "block")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://connector.test/api",
		strings.NewReader(`{"payload":"leak `+scanTestSecret+`"}`))
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("generic connector POST with a secret must block, got %d", rec.Code)
	}
	if cu.reached {
		t.Fatal("blocked forward request must not reach the upstream")
	}
	if d := lastDecision(t, buf); d.RuleSource != ruleSourceLLMBlocked {
		t.Fatalf("decision = %+v, want scan:blocked", d)
	}
}

func TestForwardEgressScanAlertForwardsBody(t *testing.T) {
	cu := captureUpstream(t, false, "ok")
	p, buf := newTestProxy(t, types.RunPolicySpec{AllowedDomains: []string{"connector.test"}},
		upstreamAddr(cu.srv), nil, nil)
	p.scanner = forwardScanEngine(t, "alert")

	body := `{"payload":"leak ` + scanTestSecret + `"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://connector.test/api", strings.NewReader(body))
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("alert forward must pass, got %d", rec.Code)
	}
	if cu.body != body {
		t.Fatalf("forward body not preserved: got %q want %q", cu.body, body)
	}
	d := lastDecision(t, buf)
	if d.Scan == nil || d.Scan.Channel != "generic" || len(d.Scan.Findings) == 0 {
		t.Fatalf("expected a generic scan summary with findings, got %+v", d.Scan)
	}
}

func TestForwardEgressNotScannedWhenDisabled(t *testing.T) {
	cu := captureUpstream(t, false, "ok")
	p, _ := newTestProxy(t, types.RunPolicySpec{AllowedDomains: []string{"connector.test"}},
		upstreamAddr(cu.srv), nil, nil)
	// LLM-only scanner (InspectForwardEgress defaults false): the forward path is
	// untouched even with a secret in the body and even in block mode.
	p.scanner = scanEngine(t, "block", scanTestSecret)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://connector.test/api",
		strings.NewReader(`{"payload":"leak `+scanTestSecret+`"}`))
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !cu.reached {
		t.Fatalf("forward path must be unscanned when inspect_forward_egress is off: status=%d reached=%v", rec.Code, cu.reached)
	}
}

func TestLLMScanBlindOnOpaqueConnect(t *testing.T) {
	p, buf := newTestProxy(t, types.RunPolicySpec{AllowedDomains: []string{anthropicHost}}, startEcho(t), nil, nil)
	p.scanner = scanEngine(t, "alert", scanTestSecret)
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	// doConnect opens a CONNECT tunnel to the model host and waits for the 200.
	// The allow + blind decisions are emitted BEFORE the 200, so once the 200 is
	// read the buffer is settled (the tunnel goroutine never writes decisions).
	doConnect := func() {
		conn, status := connectThrough(t, proxySrv.URL, anthropicHost+":443")
		defer conn.Close()
		if !strings.Contains(status, "200") {
			t.Fatalf("CONNECT response = %q, want 200", status)
		}
	}

	doConnect()
	// The blind coverage signal is emitted before the trailing CONNECT allow
	// (E3: the allow is now emitted only AFTER a successful tunnel dial), so
	// locate it by rule_source rather than assuming it is the last decision.
	d := findDecision(t, buf, ruleSourceLLMBlind)
	if d.Scan == nil || d.Scan.Action != "blind" || d.Scan.Scanned || d.Scan.Coverage != coverageOpaque {
		t.Fatalf("blind scan summary = %+v", d.Scan)
	}

	// Blind is emitted at most once per host: a second CONNECT adds no new blind.
	before := strings.Count(buf.String(), ruleSourceLLMBlind)
	doConnect()
	if after := strings.Count(buf.String(), ruleSourceLLMBlind); after != before {
		t.Fatalf("blind must be once-per-host: before=%d after=%d", before, after)
	}
}

// TestLLMScanFailClosedRefusesUninspectedChannel pins the fail-CLOSED half of
// the uninspected-channel arm (llm_routes.go, inspectLLM's scanOpaque case):
// under mode=block + on_scanner_error=block a prompt-bearing subpath the
// parser cannot inspect is REFUSED, not forwarded. Its fail-open sibling is
// TestLLMScanBatchesMarkedUninspected above, which asserts the opposite
// outcome for the same request under a fail-open engine — the two together are
// the whole contract.
func TestLLMScanFailClosedRefusesUninspectedChannel(t *testing.T) {
	cu := captureUpstream(t, true, "ok")
	p, buf := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", upstreamAddr(cu.srv),
		anthropicInjector(), testInsecureTLSConfig)
	p.scanner = scanEngineFailClosed(t, scanTestSecret)

	rec := httptest.NewRecorder()
	req := mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"v1/messages/batches",
		strings.NewReader(`{"requests":[]}`))
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("fail-closed must REFUSE an uninspectable prompt-bearing subpath, got %d", rec.Code)
	}
	if cu.reached {
		t.Fatal("a refused request must not reach the upstream")
	}
	d := lastDecision(t, buf)
	if d.Decision != egress.Deny || d.RuleSource != ruleSourceLLMBlocked {
		t.Fatalf("decision = %+v, want deny/%s", d, ruleSourceLLMBlocked)
	}
	if d.Scan == nil || d.Scan.Action != "block" || d.Scan.SkipReason != "uninspected_channel" {
		t.Fatalf("scan summary = %+v, want block/uninspected_channel", d.Scan)
	}
	if strings.Contains(rec.Body.String(), scanTestSecret) {
		t.Fatal("the 403 body must never carry scanned content")
	}
}

// TestLLMScanFailClosedRefusesOversizeBody pins the fail-CLOSED half of the
// oversize arm (scanBufferedBody): a body larger than maxLLMScanBody cannot be
// inspected, so under on_scanner_error=block it is refused rather than
// forwarded intact. TestLLMScanOversizeBodyForwardedIntact is the fail-open
// sibling for the same request.
func TestLLMScanFailClosedRefusesOversizeBody(t *testing.T) {
	saved := maxLLMScanBody
	maxLLMScanBody = 64 // force the oversize path without allocating tens of MiB
	defer func() { maxLLMScanBody = saved }()

	cu := captureUpstream(t, true, "ok")
	p, buf := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", upstreamAddr(cu.srv),
		anthropicInjector(), testInsecureTLSConfig)
	p.scanner = scanEngineFailClosed(t, scanTestSecret)

	body := anthropicMessagesBody("padding " + strings.Repeat("x", 300))
	if len(body) <= maxLLMScanBody {
		t.Fatal("test body must exceed the scan cap")
	}
	rec := httptest.NewRecorder()
	req := mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"v1/messages", strings.NewReader(body))
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("fail-closed must REFUSE an oversize (uninspectable) body, got %d", rec.Code)
	}
	if cu.reached {
		t.Fatal("a refused oversize body must not reach the upstream")
	}
	d := lastDecision(t, buf)
	if d.Decision != egress.Deny || d.RuleSource != ruleSourceLLMBlocked {
		t.Fatalf("decision = %+v, want deny/%s", d, ruleSourceLLMBlocked)
	}
	if d.Scan == nil || d.Scan.Action != "block" || d.Scan.SkipReason != "body_oversize" || d.Scan.Scanned {
		t.Fatalf("scan summary = %+v, want unscanned block/body_oversize", d.Scan)
	}
}

// TestLLMScanBodyReadErrorDenies pins scanBufferedBody's read-error arm, which
// is UNCONDITIONAL (it does not consult BlocksOnError): a body the proxy could
// not read whole was never inspected, so it must be denied rather than
// forwarded — proved here with a fail-OPEN engine so the assertion is about
// the arm itself, not about on_scanner_error.
func TestLLMScanBodyReadErrorDenies(t *testing.T) {
	cu := captureUpstream(t, true, "ok")
	p, buf := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", upstreamAddr(cu.srv),
		anthropicInjector(), testInsecureTLSConfig)
	p.scanner = scanEngine(t, "alert", scanTestSecret) // fail-OPEN on purpose

	rec := httptest.NewRecorder()
	req := mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"v1/messages", strings.NewReader("{}"))
	req.Body = io.NopCloser(errBody{})
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("an unreadable body must be refused 400, got %d", rec.Code)
	}
	if cu.reached {
		t.Fatal("an unreadable body must not reach the upstream")
	}
	if d := lastDecision(t, buf); d.Decision != egress.Deny {
		t.Fatalf("decision = %+v, want deny (never a silent allow for a body we could not read)", d)
	}
}
