// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func mintServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The injector resolves via GET /api/v1/internal/injection/{grantID}:
		// the control plane returns the header name + FORMATTED secret value
		// (format applied server-side; the proxy never sees raw secrets
		// except as the final injectable value).
		if r.Method != http.MethodGet || !strings.Contains(r.URL.Path, "/internal/injection/") {
			http.Error(w, "bad path", http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		gid := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		_ = json.NewEncoder(w).Encode(types.ResolvedInjection{
			Host:   "api.test",
			Header: "Authorization",
			Value:  "Bearer minted-" + gid[:8],
			JTI:    uuid.NewString(),
		})
	}))
}

// staticInj builds an injector with STATIC (never re-resolved) entries — the
// api-key injection shape tests exercise. Dynamic re-resolution is covered
// separately (TestInjectorReResolvesNearExpiry).
func staticInj(entries map[string]injectedHeader) *injector {
	m := make(map[string]*injEntry, len(entries))
	for h, hd := range entries {
		m[h] = &injEntry{header: hd}
	}
	return &injector{byHost: m}
}

func TestBuildInjectorMintsAndFormats(t *testing.T) {
	cp := mintServer(t, "tok")
	defer cp.Close()

	pol := CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"api.test"}})
	gid := uuid.New()
	rules := []InjectionConfig{{
		InjectionRule: egress.InjectionRule{Host: "api.test", Header: "Authorization", Format: "Bearer %s"},
		GrantID:       gid,
	}}
	inj, err := buildInjector(context.Background(), cp.URL, newTokenSource("tok"), pol, rules, cp.Client())
	if err != nil {
		t.Fatalf("buildInjector: %v", err)
	}
	h, ok := inj.byHost["api.test"]
	if !ok {
		t.Fatalf("rule not registered")
	}
	want := "Bearer minted-" + gid.String()[:8]
	if h.header.value != want {
		t.Fatalf("header value = %q, want %q", h.header.value, want)
	}
	if h.header.name != "Authorization" {
		t.Fatalf("header name = %q", h.header.name)
	}
}

// A DYNAMIC injection (non-zero ExpiresAt — the subscription OAuth token) must be
// re-resolved via the control plane once it nears expiry, and NOT re-resolved
// while still fresh. This is what keeps the injected credential from going stale.
func TestInjectorReResolvesNearExpiry(t *testing.T) {
	var calls int32
	nearExp := time.Now().Add(time.Minute).UnixMilli() // inside injectRefreshMargin
	farExp := time.Now().Add(time.Hour).UnixMilli()    // comfortably fresh
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		val, exp := "Bearer token-1", nearExp
		if atomic.AddInt32(&calls, 1) > 1 {
			val, exp = "Bearer token-2", farExp
		}
		_ = json.NewEncoder(w).Encode(types.ResolvedInjection{
			Host: "api.test", Header: "Authorization", Value: val, JTI: uuid.NewString(), ExpiresAt: exp,
		})
	}))
	defer srv.Close()

	pol := CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"api.test"}})
	rules := []InjectionConfig{{
		InjectionRule: egress.InjectionRule{Host: "api.test", Header: "Authorization", Format: "Bearer %s"},
		GrantID:       uuid.New(),
	}}
	inj, err := buildInjector(context.Background(), srv.URL, newTokenSource("tok"), pol, rules, srv.Client())
	if err != nil {
		t.Fatalf("buildInjector: %v", err)
	}

	// Startup mint got token-1 (near expiry); the next resolve must re-resolve.
	h, ok, rerr := inj.resolve("api.test")
	if rerr != nil || !ok {
		t.Fatalf("resolve: ok=%v err=%v", ok, rerr)
	}
	if h.value != "Bearer token-2" {
		t.Fatalf("expected re-resolved token-2, got %q (calls=%d)", h.value, atomic.LoadInt32(&calls))
	}
	// token-2 is far-expiry: a subsequent resolve must NOT hit the control plane.
	before := atomic.LoadInt32(&calls)
	if _, _, err := inj.resolve("api.test"); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&calls); got != before {
		t.Errorf("fresh token must not re-resolve (calls %d -> %d)", before, got)
	}
}

func TestBuildInjectorRefusesNonExactHost(t *testing.T) {
	cp := mintServer(t, "tok")
	defer cp.Close()

	// Wildcard allow only — injection must refuse (would leak secret to any
	// subdomain and widen egress trust).
	pol := CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"*.api.test"}})
	rules := []InjectionConfig{{
		InjectionRule: egress.InjectionRule{Host: "a.api.test", Header: "Authorization", Format: "Bearer %s"},
		GrantID:       uuid.New(),
	}}
	_, err := buildInjector(context.Background(), cp.URL, newTokenSource("tok"), pol, rules, cp.Client())
	if err == nil {
		t.Fatalf("expected refusal for wildcard-only host")
	}
	if !strings.Contains(err.Error(), "exact allowlist") {
		t.Fatalf("error = %v, want exact-allowlist refusal", err)
	}
}

func TestBuildInjectorFailsClosedOnMintError(t *testing.T) {
	// Server returns 409 (approval pending) -> startup must fail.
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"approval_id": uuid.NewString()})
	}))
	defer cp.Close()

	pol := CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"api.test"}})
	rules := []InjectionConfig{{
		InjectionRule: egress.InjectionRule{Host: "api.test"},
		GrantID:       uuid.New(),
	}}
	_, err := buildInjector(context.Background(), cp.URL, newTokenSource("tok"), pol, rules, cp.Client())
	if err == nil {
		t.Fatalf("expected fail-closed on 409 mint")
	}
}

func TestBuildInjectorRequiresGrantID(t *testing.T) {
	pol := CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"api.test"}})
	rules := []InjectionConfig{{InjectionRule: egress.InjectionRule{Host: "api.test"}}}
	if _, err := buildInjector(context.Background(), "http://unused", newTokenSource("tok"), pol, rules, nil); err == nil {
		t.Fatalf("expected error for missing grant_id")
	}
}

func TestLoadConfigDefaultsAndValidation(t *testing.T) {
	dir := t.TempDir()
	good := Config{
		RunID:           uuid.New(),
		ControlPlaneURL: "http://wardynd:8080",
		RunToken:        "tok",
		Policy:          types.RunPolicySpec{AllowedDomains: []string{"api.test"}},
	}
	path := filepath.Join(dir, "good.json")
	b, _ := json.Marshal(good)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if c.Listen != defaultListen {
		t.Errorf("listen default = %q, want %q", c.Listen, defaultListen)
	}
	if c.DecisionBufferSize != defaultBufferSize {
		t.Errorf("buffer default = %d", c.DecisionBufferSize)
	}

	// Missing required fields -> error.
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{"control_plane_url":"http://x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(bad); err == nil {
		t.Fatalf("expected validation error for missing run_id/token")
	}

	if _, err := LoadConfig(filepath.Join(dir, "nope.json")); err == nil {
		t.Fatalf("expected error for missing file")
	}

	// Missing control_plane_url / run_token, and malformed JSON, are each
	// rejected by the same required-field / parse checks above.
	if _, err := LoadConfigBytes([]byte(`{"run_id":"` + uuid.New().String() + `","run_token":"t"}`)); err == nil || !strings.Contains(err.Error(), "control_plane_url is required") {
		t.Fatalf("expected control_plane_url required error, got %v", err)
	}
	if _, err := LoadConfigBytes([]byte(`{"run_id":"` + uuid.New().String() + `","control_plane_url":"http://x"}`)); err == nil || !strings.Contains(err.Error(), "run_token is required") {
		t.Fatalf("expected run_token required error, got %v", err)
	}
	if _, err := LoadConfigBytes([]byte(`{not json`)); err == nil || !strings.Contains(err.Error(), "parse config") {
		t.Fatalf("expected parse config error, got %v", err)
	}

	// Explicit Listen/DecisionBufferSize override the defaults.
	explicit := `{"run_id":"` + uuid.New().String() + `","control_plane_url":"http://wardynd:8080","run_token":"t","listen":"127.0.0.1:9999","decision_buffer_size":7}`
	ec, err := LoadConfigBytes([]byte(explicit))
	if err != nil {
		t.Fatalf("LoadConfigBytes explicit: %v", err)
	}
	if ec.Listen != "127.0.0.1:9999" || ec.DecisionBufferSize != 7 {
		t.Errorf("explicit overrides not kept: listen=%q buffer=%d", ec.Listen, ec.DecisionBufferSize)
	}
}

// requireTLSInj is staticInj plus the rule's transport declaration — the shape
// buildInjector produces from an api_key grant scope carrying require_tls.
func requireTLSInj(host string, hdr injectedHeader) *injector {
	inj := staticInj(map[string]injectedHeader{host: hdr})
	inj.byHost[host].requireTLS = true
	return inj
}

// TestRequireTLSRefusesCleartextRequest (F110 residual): with require_tls on the
// rule, a plain-HTTP request to that host is DENIED — not merely forwarded
// uncredentialed.
//
// The difference is the whole point of the flag. injectableTransport's own rules
// withhold the credential silently, which an operator cannot tell apart from a
// wrong key: the agent sees a 401 from the vendor. An operator who declares the
// credential TLS-only is refusing the REQUEST, so the refusal has to be Wardyn's,
// with the reason on the response and a deny row in the trail.
func TestRequireTLSRefusesCleartextRequest(t *testing.T) {
	var hits int
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer upstream.Close()

	p, buf := newTestProxy(t, types.RunPolicySpec{AllowedDomains: []string{"allowed.test"}},
		upstreamAddr(upstream), nil, requireTLSInj("allowed.test", injectedHeader{name: "X-Api-Key", value: "SECRET-KEY"}))

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "http://allowed.test/path"))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %q)", rec.Code, rec.Body.String())
	}
	if hits != 0 {
		t.Fatalf("upstream was hit %d times for a refused request", hits)
	}
	if got := rec.Header().Get(egressHeaderReason); got != ruleSourceRequireTLS {
		t.Errorf("%s = %q, want %q", egressHeaderReason, got, ruleSourceRequireTLS)
	}
	if got := rec.Header().Get(egressHeaderStatus); got != egressRefusalDenied {
		t.Errorf("%s = %q, want %q", egressHeaderStatus, got, egressRefusalDenied)
	}
	if got := rec.Header().Get(egressHeaderHost); got != "allowed.test" {
		t.Errorf("%s = %q, want the host", egressHeaderHost, got)
	}
	// The body has to say which host and why — writeEgressDeny's fixed "egress
	// denied by policy" is what this arm exists to avoid.
	if !strings.Contains(rec.Body.String(), "allowed.test") ||
		!strings.Contains(rec.Body.String(), "require_tls") {
		t.Errorf("body = %q, want it to name the host and the rule", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "SECRET-KEY") {
		t.Fatal("the injected credential leaked into the refusal body")
	}
	// ONE row, and it is the deny: the allow the evaluator granted must not also
	// be emitted, or the trail claims egress that never happened.
	d := lastDecision(t, buf)
	if d.RuleSource != ruleSourceRequireTLS || d.Decision != egress.Deny {
		t.Fatalf("decision = %+v, want a %s deny", d, ruleSourceRequireTLS)
	}
	if n := strings.Count(buf.String(), "\n"); n != 1 {
		t.Fatalf("decision log = %q, want exactly one row", buf.String())
	}
}

// TestRequireTLSUnsetKeepsTodaysCleartextInjection is the other half: the flag
// is opt-in, so a rule without it is judged by injectableTransport exactly as
// before — port 80 cleartext is injected and forwarded, byte-for-byte today.
func TestRequireTLSUnsetKeepsTodaysCleartextInjection(t *testing.T) {
	var gotKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Api-Key")
	}))
	defer upstream.Close()

	p, buf := newTestProxy(t, types.RunPolicySpec{AllowedDomains: []string{"allowed.test"}},
		upstreamAddr(upstream), nil, staticInj(map[string]injectedHeader{
			"allowed.test": {name: "X-Api-Key", value: "SECRET-KEY"},
		}))

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "http://allowed.test/path"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if gotKey != "SECRET-KEY" {
		t.Errorf("upstream X-Api-Key = %q, want the injected credential", gotKey)
	}
	if strings.Contains(buf.String(), ruleSourceRequireTLS) {
		t.Errorf("decision log = %q, want no %s row for a rule that never set it", buf.String(), ruleSourceRequireTLS)
	}
}

// TestRequireTLSAllowsTLSTransport: the flag refuses a TRANSPORT, not a host. An
// https request on the same lane is the transport the operator asked for, so it
// is injected and forwarded like any other.
func TestRequireTLSAllowsTLSTransport(t *testing.T) {
	var gotKey string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Api-Key")
	}))
	defer upstream.Close()

	buf := &bytes.Buffer{}
	p := newProxy(Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"allowed.test"}}),
		Injector:        requireTLSInj("allowed.test", injectedHeader{name: "X-Api-Key", value: "SECRET-KEY"}),
		Sink:            &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)},
		Resolver:        publicResolver{},
		Dial:            redirectDial(upstreamAddr(upstream)),
		TLSClientConfig: testInsecureTLSConfig,
	})

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "https://allowed.test/path"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if gotKey != "SECRET-KEY" {
		t.Errorf("upstream X-Api-Key = %q, want the injected credential over TLS", gotKey)
	}
	if strings.Contains(buf.String(), ruleSourceRequireTLS) {
		t.Errorf("decision log = %q, want no refusal for the transport require_tls asks for", buf.String())
	}
}

// TestRequireTLSRefusalPreemptsInspection pins the ORDER of the two things the
// plain lane does to a request it is about to refuse.
//
// The arm sits ahead of the content-inspection block, not beside the injection
// it guards, because a refusal ends the request: scanning first spends the scan
// budget on bytes nothing forwards, and the honest-coverage marker
// (emitLLMBlindOnce) posts a row saying a body went UNINSPECTED to an upstream
// that never received it. Here the scanner is in BLOCK mode over a body that
// carries the secret it blocks on, so if inspection ran first the 403 would be
// inspectLLM's, with a scan:blocked row — a different refusal for a different
// reason, told to the operator in place of the one that actually applies.
func TestRequireTLSRefusalPreemptsInspection(t *testing.T) {
	cu := captureUpstream(t, true, "never-forwarded")
	p, buf := newPlainLaneProxy(t, upstreamAddr(cu.srv),
		requireTLSInj(anthropicHost, injectedHeader{name: "X-Api-Key", value: "BROKERED-KEY"}))
	p.scanner = scanEngine(t, "block", scanTestSecret)

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustAbsReq(t, http.MethodPost, "http://"+anthropicHost+"/v1/messages",
		anthropicMessagesBody("please use key "+scanTestSecret+" now")))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if got := rec.Header().Get(egressHeaderReason); got != ruleSourceRequireTLS {
		t.Fatalf("%s = %q, want %q — the transport refusal is the one that applies",
			egressHeaderReason, got, ruleSourceRequireTLS)
	}
	d := lastDecision(t, buf)
	if d.RuleSource != ruleSourceRequireTLS || d.Decision != egress.Deny {
		t.Fatalf("decision = %+v, want the %s deny", d, ruleSourceRequireTLS)
	}
	if d.Scan != nil {
		t.Errorf("scan summary = %+v, want none: the body of a refused request is never read", d.Scan)
	}
	if n := strings.Count(strings.TrimSpace(buf.String()), "\n"); n != 0 {
		t.Errorf("decision log = %q, want exactly one row (no scan:blocked, no llm.scan.blind)", buf.String())
	}
}

// No hold at boot. buildInjector runs under the proxy's 30s startupCtx, seconds
// after dispatch refreshed the credential synchronously — a dead credential
// THERE is a race measured in seconds, not a person who needs to sign in, and a
// hold would fight the startup canary. A 423 at boot must fail closed exactly
// as any other status does.
func TestBuildInjectorFailsClosedOnAReauth423(t *testing.T) {
	prevPoll := holdPollInterval
	holdPollInterval = 5 * time.Millisecond
	defer func() { holdPollInterval = prevPoll }()
	t.Setenv(envCredentialReauthTimeout, "1800s")

	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusLocked)
		_, _ = w.Write([]byte(`{"state":"reauth_pending","approval_id":"` + uuid.NewString() + `"}`))
	}))
	defer cp.Close()

	pol := CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"portal.sso.eu-west-2.amazonaws.com"}})
	rules := []InjectionConfig{{
		InjectionRule: egress.InjectionRule{Host: "portal.sso.eu-west-2.amazonaws.com", Header: "x-amz-sso_bearer_token"},
		GrantID:       uuid.New(),
	}}

	start := time.Now()
	inj, err := buildInjector(context.Background(), cp.URL, newTokenSource("tok"), pol, rules, cp.Client())
	if err == nil {
		t.Fatal("buildInjector succeeded on a 423; want a fail-closed startup error")
	}
	if inj != nil {
		t.Error("buildInjector returned an injector alongside its error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("boot took %v — buildInjector HELD instead of failing closed", elapsed)
	}
}
