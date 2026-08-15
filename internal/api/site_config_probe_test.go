// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// ─── classify* pure state-mapping tables ─────────────────────────────────────

// probeHostsLabel is the human-readable host list connection-level failure
// details embed; payload-claim details (reached/intercepted) name the full
// endpoints (proxyProbeEndpointsLabel) instead.
var probeHostsLabel = strings.Join(proxyProbeHosts, ", ")

// builtinSubject is the proxyProbeSubject the handler builds for the default
// multi-target check, chained through an upstream.
var builtinSubject = proxyProbeSubject{
	endpoints: proxyProbeEndpointsLabel,
	hosts:     probeHostsLabel,
	upstream:  "http://proxy.corp:3128",
}

func TestClassifyProxyProbe(t *testing.T) {
	cases := []struct {
		name       string
		res        probeRunResult
		wantState  string
		wantDetail string
		// what the detail must name -- the full endpoints for a payload
		// claim, the hosts for a connection-level failure.
		wantNames string
	}{
		{"reached names endpoints and the payload check",
			probeRunResult{hasExitCode: true, exitCode: 0, elapsed: 250 * time.Millisecond}, "reached", "payloads matched", proxyProbeEndpointsLabel},
		{"blocked with a real curl error, never a generic string",
			probeRunResult{hasExitCode: true, exitCode: 7}, "blocked", "connection refused", probeHostsLabel},
		{"blocked: DNS", probeRunResult{hasExitCode: true, exitCode: 6}, "blocked", "DNS resolution failed", probeHostsLabel},
		{"blocked: TLS", probeRunResult{hasExitCode: true, exitCode: 35}, "blocked", "TLS handshake failed", probeHostsLabel},
		{"blocked: unmapped code still names the real number, not a generic label",
			probeRunResult{hasExitCode: true, exitCode: 99}, "blocked", "curl exit code 99", probeHostsLabel},
		{"incomplete probe (timeout/launch failure) is blocked, not a distinct state",
			probeRunResult{incompleteReason: "did not finish within 50s"}, "blocked", "did not finish within 50s", probeHostsLabel},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyProxyProbe(c.res, builtinSubject)
			if got.State != c.wantState {
				t.Errorf("state = %q, want %q (detail=%q)", got.State, c.wantState, got.Detail)
			}
			if !strings.Contains(got.Detail, c.wantDetail) {
				t.Errorf("detail = %q, want it to contain %q", got.Detail, c.wantDetail)
			}
			if !strings.Contains(got.Detail, c.wantNames) {
				t.Errorf("detail = %q, want it to name what was probed (%s)", got.Detail, c.wantNames)
			}
			if got.Via != "proxy" {
				t.Errorf("via = %q, want proxy (an upstream was configured)", got.Via)
			}
		})
	}
}

func TestClassifyRedirectProbe(t *testing.T) {
	const to, from = "artifactory.corp", "registry.npmjs.org"
	cases := []struct {
		name       string
		res        probeRunResult
		wantState  string
		wantDetail string
	}{
		{"reached: mirror up, public host correctly blocked",
			probeRunResult{hasExitCode: true, exitCode: 0}, "reached", to},
		{"bypass: mirror up AND public host still directly reachable",
			probeRunResult{hasExitCode: true, exitCode: redirectProbeBypassCode}, "bypass", "not enforced"},
		{"blocked: the mirror itself is unreachable, real error surfaced",
			probeRunResult{hasExitCode: true, exitCode: 28}, "blocked", "connection timed out"},
		{"incomplete probe (timeout/launch failure) is blocked",
			probeRunResult{incompleteReason: "boom"}, "blocked", "boom"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyRedirectProbe(c.res, to, from)
			if got.State != c.wantState {
				t.Errorf("state = %q, want %q (detail=%q)", got.State, c.wantState, got.Detail)
			}
			if !strings.Contains(got.Detail, c.wantDetail) {
				t.Errorf("detail = %q, want it to contain %q", got.Detail, c.wantDetail)
			}
		})
	}
}

// bypass must never be reachable via a passed-through curl code: it is only
// ever produced by the script's own explicit `exit 250`, so classify must key
// on the sentinel exactly, and a run that never got an exit code at all must
// never be misread as bypass.
func TestClassifyRedirectProbe_BypassNeverInferred(t *testing.T) {
	got := classifyRedirectProbe(probeRunResult{incompleteReason: "sandbox never started"}, "to.example", "from.example")
	if got.State == "bypass" {
		t.Fatalf("an incomplete probe (no exit code at all) must never be classified bypass; got %+v", got)
	}
}

// TestRedirectProbeScript_HTTPErrorIsNotReached is the W13-S1-2 regression:
// runs the ACTUAL redirectProbeScript text (not just classifyRedirectProbe's
// exit-code table) through a real shell+curl against a local server that
// answers the To probe with a plain HTTP 403 -- exactly what wardyn-proxy's
// own policy deny looks like, and what any mirror's auth failure looks like
// too. Without -f, curl treats a well-formed 403 response as a SUCCESSFUL
// connection (exit 0) and never looks at the status line, so the script fell
// through to its "From correctly failed" exit-0 branch once the From dial
// (an unroutable loopback port, refused instantly) also failed -- "reached"
// for a request that was actually denied. -f must turn that HTTP error into a
// curl failure (exit 22) that propagates out of the script unchanged.
func TestRedirectProbeScript_HTTPErrorIsNotReached(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not on PATH")
	}
	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden) // wardyn-proxy's own deny shape: a plain HTTP error
	}))
	defer denied.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", redirectProbeScript)
	cmd.Env = append(cmd.Environ(),
		"WARDYN_PROBE_TO_URL="+denied.URL,
		// port 1 on loopback: refused instantly, never a real dial -- keeps this
		// test network-independent and fast regardless of which branch wins.
		"WARDYN_PROBE_FROM_URL=http://127.0.0.1:1",
	)
	_ = cmd.Run()
	exitCode := cmd.ProcessState.ExitCode()
	if exitCode != 22 {
		t.Fatalf("redirectProbeScript exit code = %d, want 22 (curl -f's HTTP-error code): "+
			"an HTTP 403 from To must classify as blocked, never as reached", exitCode)
	}
}

func TestFindEgressRedirect(t *testing.T) {
	sc := types.SiteConfig{EgressRedirects: []types.EgressRedirect{
		{From: "registry.npmjs.org", To: "artifactory.corp/npm", Ecosystem: "npm"},
	}}
	if _, ok := findEgressRedirect(sc, "REGISTRY.NPMJS.ORG"); !ok {
		t.Error("expected a case-insensitive match on the stored From")
	}
	if _, ok := findEgressRedirect(sc, "not-configured.example.com"); ok {
		t.Error("an unconfigured from must not resolve to any row (this IS the SSRF guard)")
	}
}

// ─── fakes: a runner + a store that actually drive dispatchRun to completion ─
//
// Every other test fake in this package is a bespoke, minimal per-file type
// (fakeRunner in interactive_test.go, fkGrantStore/raceStore/bootReconcileStore
// elsewhere) rather than one shared do-everything fake. These two follow the
// same convention, implementing exactly what runSiteConfigProbe's dispatch
// path touches for a minimal (no Bedrock/Secrets/subscription) policy.

// probeFakeRunner is a runner.Runner whose Wait is scriptable: either returns
// a configured exit code promptly, or blocks until its ctx is done (to drive
// the timeout/reclaim path without a real 50s wait).
type probeFakeRunner struct {
	mu        sync.Mutex
	exitCode  int
	block     bool
	createErr error
	stopCalls int
	killCalls int
}

func (r *probeFakeRunner) Name() string { return "probe-fake" }
func (r *probeFakeRunner) Capabilities(context.Context) (runner.Capabilities, error) {
	return runner.Capabilities{Driver: "probe-fake", ConfinementClasses: []types.ConfinementClass{types.CC1}}, nil
}
func (r *probeFakeRunner) CreateSandbox(_ context.Context, spec runner.SandboxSpec) (runner.Sandbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.createErr != nil {
		return runner.Sandbox{}, r.createErr
	}
	return runner.Sandbox{Ref: "probe-" + spec.RunID.String(), Driver: "probe-fake", EnforcedClass: spec.ConfinementClass}, nil
}
func (r *probeFakeRunner) Exec(context.Context, string, []string) (string, error) {
	return "exec-1", nil
}
func (r *probeFakeRunner) Wait(ctx context.Context, _ string) (int, error) {
	r.mu.Lock()
	block, code := r.block, r.exitCode
	r.mu.Unlock()
	if block {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	return code, nil
}
func (r *probeFakeRunner) Attach(context.Context, string, runner.AttachOptions) (runner.Session, error) {
	return nil, context.Canceled
}
func (r *probeFakeRunner) ExecStream(context.Context, string, runner.ExecSpec) (*runner.ExecSession, error) {
	return nil, runner.ErrExecStreamUnsupported
}
func (r *probeFakeRunner) Status(context.Context, string) (runner.Status, error) {
	return runner.Status{State: types.RunRunning}, nil
}
func (r *probeFakeRunner) AgentStatus(context.Context, string, string) (runner.Status, error) {
	return runner.Status{State: types.RunRunning}, nil
}
func (r *probeFakeRunner) StopSandbox(context.Context, string) error {
	r.mu.Lock()
	r.stopCalls++
	r.mu.Unlock()
	return nil
}
func (r *probeFakeRunner) KillSandbox(context.Context, string) error {
	r.mu.Lock()
	r.killCalls++
	r.mu.Unlock()
	return nil
}
func (r *probeFakeRunner) stops() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopCalls
}

// probeStore is BOTH the store.Store (dispatch's persistence seam) and the
// audit.Recorder (Config.Audit) a probe run needs, backed by the SAME
// in-memory event slice -- exactly like production, where the innermost audit
// writer and the store that answers QueryAuditEvents are the same Postgres
// table. Without sharing them, probeFailureDetail's read-back of its own
// run.complete event would never see what s.recordAudit just wrote.
type probeStore struct {
	store.Store
	mu     sync.Mutex
	runs   map[uuid.UUID]types.AgentRun
	events []types.AuditEvent
	grants []types.CredentialGrant
	cfg    types.SiteConfig
}

func newProbeStore(cfg types.SiteConfig) *probeStore {
	return &probeStore{runs: map[uuid.UUID]types.AgentRun{}, cfg: cfg}
}

func (s *probeStore) CreateRun(_ context.Context, r types.AgentRun) (types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs[r.ID] = r
	return r, nil
}
func (s *probeStore) GetRun(_ context.Context, id uuid.UUID) (types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	if !ok {
		return types.AgentRun{}, store.ErrNotFound
	}
	return r, nil
}
func (s *probeStore) UpdateRunStateIf(_ context.Context, id uuid.UUID, from, to types.RunState) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	if !ok || r.State != from {
		return false, nil
	}
	r.State = to
	s.runs[id] = r
	return true, nil
}
func (s *probeStore) SetSandboxRef(_ context.Context, id uuid.UUID, ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.runs[id]
	r.SandboxRef = ref
	s.runs[id] = r
	return nil
}
func (s *probeStore) SetRunAgentExecID(_ context.Context, id uuid.UUID, execID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.runs[id]
	r.AgentExecID = execID
	s.runs[id] = r
	return nil
}

// CreateGrant records the eligibility rows an integration probe persists so
// its credential can be resolved proxy-side (probeInjections) — the same write
// a real run's persistRunGrants makes.
func (s *probeStore) CreateGrant(_ context.Context, g types.CredentialGrant) (types.CredentialGrant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grants = append(s.grants, g)
	return g, nil
}
func (s *probeStore) grantSpecs() []types.CredentialGrant {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.grants)
}
func (s *probeStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg, nil
}
func (s *probeStore) QueryAuditEvents(_ context.Context, runID uuid.UUID, limit int) ([]types.AuditEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []types.AuditEvent
	for _, ev := range s.events {
		if ev.RunID != nil && *ev.RunID == runID {
			out = append(out, ev)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
func (s *probeStore) Record(_ context.Context, ev types.AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
	return nil
}
func (s *probeStore) actionEvents(action string) []types.AuditEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []types.AuditEvent
	for _, ev := range s.events {
		if ev.Action == action {
			out = append(out, ev)
		}
	}
	return out
}
func (s *probeStore) runCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.runs)
}
func (s *probeStore) soleRunState() types.RunState {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.runs {
		return r.State
	}
	return ""
}

// newProbeHarness builds a Server whose Store AND Audit are the same
// probeStore (see its doc comment) and whose Runner is fr (nil is a valid,
// meaningful choice -- it drives the no_runner path exactly like a headless
// control plane).
func newProbeHarness(t *testing.T, siteCfg types.SiteConfig, fr runner.Runner) (*Server, *probeStore) {
	t.Helper()
	h := newHarness(t)
	ps := newProbeStore(siteCfg)
	cfg := baseTestConfig(h, ps)
	cfg.Audit = ps
	cfg.Runner = fr
	return New(cfg), ps
}

func redirectSiteConfig() types.SiteConfig {
	return types.SiteConfig{EgressRedirects: []types.EgressRedirect{
		{From: "registry.npmjs.org", To: "artifactory.corp/npm", Ecosystem: "npm"},
	}}
}

func decodeProbeResponse(t *testing.T, body string) siteConfigProbeResponse {
	t.Helper()
	var got siteConfigProbeResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode response: %v (body %q)", err, body)
	}
	return got
}

// ─── test-proxy ───────────────────────────────────────────────────────────────

func TestHandleTestSiteConfigProxy_Reached(t *testing.T) {
	fr := &probeFakeRunner{exitCode: 0}
	srv, ps := newProbeHarness(t, types.SiteConfig{UpstreamProxyURL: "http://proxy.corp:3128"}, fr)

	w := do(t, srv, http.MethodPost, "/api/v1/site-config/test-proxy", adminToken, "{}")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	got := decodeProbeResponse(t, w.Body.String())
	if got.State != "reached" {
		t.Errorf("state = %q, want reached (detail=%q)", got.State, got.Detail)
	}
	if !strings.Contains(got.Detail, proxyProbeEndpointsLabel) || !strings.Contains(got.Detail, "payloads matched") {
		t.Errorf("detail = %q, want it to name %s and claim the payload check", got.Detail, proxyProbeEndpointsLabel)
	}
	if got.Via != "proxy" || got.Custom || got.Intercepted {
		t.Errorf("qualifiers = via:%q custom:%v intercepted:%v, want a plain via-proxy reached", got.Via, got.Custom, got.Intercepted)
	}

	events := ps.actionEvents("site_config.test_proxy")
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 site_config.test_proxy audit event, got %d", len(events))
	}
	if events[0].Outcome != "success" {
		t.Errorf("audit outcome = %q, want success", events[0].Outcome)
	}
	var data map[string]any
	_ = json.Unmarshal(events[0].Data, &data)
	if data["target_host"] != probeHostsLabel {
		t.Errorf("audit target_host = %v, want %s", data["target_host"], probeHostsLabel)
	}
	if data["state"] != "reached" {
		t.Errorf("audit state = %v, want reached", data["state"])
	}
}

func TestHandleTestSiteConfigProxy_BlockedWithRealError(t *testing.T) {
	fr := &probeFakeRunner{exitCode: 7} // curl: connection refused
	srv, _ := newProbeHarness(t, types.SiteConfig{UpstreamProxyURL: "http://proxy.corp:3128"}, fr)

	w := do(t, srv, http.MethodPost, "/api/v1/site-config/test-proxy", adminToken, "{}")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (blocked is still a definite answer); body=%s", w.Code, w.Body.String())
	}
	got := decodeProbeResponse(t, w.Body.String())
	if got.State != "blocked" {
		t.Errorf("state = %q, want blocked", got.State)
	}
	if !strings.Contains(got.Detail, "connection refused") {
		t.Errorf("detail = %q, want the REAL curl error (connection refused), never a generic string", got.Detail)
	}
}

// TestHandleTestSiteConfigProxy_UnresolvableUpstreamReportsDirect is the
// W13-S1-4 / W12-W12-C-2 regression: an https:// upstream_proxy_url (a row
// written before validateSiteConfig's http-only gate existed — the store
// fixture below bypasses that gate on purpose, to model exactly that) cannot
// be used by resolveRunUpstreamProxy at real dispatch (the sidecar's
// plaintext-CONNECT hop cannot carry https), so a run goes DIRECT. The probe
// used to read UpstreamProxyURL straight off stored config for its `upstream`
// display, so it reported via=proxy / "chained to https://..." for a chain no
// run ever actually traverses. It must report via=direct and say why.
func TestHandleTestSiteConfigProxy_UnresolvableUpstreamReportsDirect(t *testing.T) {
	fr := &probeFakeRunner{exitCode: 0}
	srv, _ := newProbeHarness(t, types.SiteConfig{UpstreamProxyURL: "https://proxy.corp:8443"}, fr)

	w := do(t, srv, http.MethodPost, "/api/v1/site-config/test-proxy", adminToken, "{}")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	got := decodeProbeResponse(t, w.Body.String())
	if got.Via != "direct" {
		t.Fatalf("via = %q, want direct: an https:// upstream never resolves at dispatch, so a run never actually chains through it", got.Via)
	}
	if strings.Contains(got.Detail, "chained to") {
		t.Errorf("detail = %q, must never claim a chain dispatch would silently drop", got.Detail)
	}
	if !strings.Contains(got.Detail, "NOT used") || !strings.Contains(got.Detail, "https") {
		t.Errorf("detail = %q, want it to say the configured proxy was NOT used and why (https not supported)", got.Detail)
	}
}

// TestHandleTestSiteConfigProxy_UnresolvableSecretRefReportsDirect is the
// W12-W12-C-2 regression, the sibling case to the https:// one above: a
// configured upstream_proxy_secret_ref that names no ACTUALLY stored secret
// (no secret store wired at all here -- the simplest way to make it
// unresolvable) must not report via=proxy / state=reached either. The probe
// used to build its `upstream` display from the stored secret_ref alone,
// never asking whether it resolves to anything a real run could use.
func TestHandleTestSiteConfigProxy_UnresolvableSecretRefReportsDirect(t *testing.T) {
	fr := &probeFakeRunner{exitCode: 0}
	srv, _ := newProbeHarness(t, types.SiteConfig{UpstreamProxySecretRef: "corp-proxy-url"}, fr)

	w := do(t, srv, http.MethodPost, "/api/v1/site-config/test-proxy", adminToken, "{}")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	got := decodeProbeResponse(t, w.Body.String())
	if got.Via != "direct" {
		t.Fatalf("via = %q, want direct: an unresolvable secret ref never produces a proxy a real run could use", got.Via)
	}
	if strings.Contains(got.Detail, "chained to") {
		t.Errorf("detail = %q, must never claim a chain dispatch would silently drop", got.Detail)
	}
	if !strings.Contains(got.Detail, "NOT used") {
		t.Errorf("detail = %q, want it to say the configured proxy was NOT used", got.Detail)
	}
}

func TestHandleTestSiteConfigProxy_NoUpstreamConfiguredStillProbes(t *testing.T) {
	// An unconfigured proxy is the COMMON case, not an error. The operator's
	// real question -- can a sandbox reach the internet from this host? -- is
	// worth answering either way, and refusing to run made the button useless
	// on exactly the hosts where nothing is wrong yet.
	fr := &probeFakeRunner{exitCode: 0}
	srv, ps := newProbeHarness(t, types.SiteConfig{}, fr)
	w := do(t, srv, http.MethodPost, "/api/v1/site-config/test-proxy", adminToken, "{}")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	got := decodeProbeResponse(t, w.Body.String())
	if got.State != "reached" {
		t.Errorf("state = %q, want reached", got.State)
	}
	// It must NOT claim a proxy it never chained through — the direct wording
	// says so outright (T.TEST_OK_DIRECT), and the via qualifier is the
	// machine-readable form of the same fact.
	if !strings.Contains(got.Detail, "No proxy is configured and none was needed") {
		t.Errorf("detail = %q, want the direct-path wording", got.Detail)
	}
	if got.Via != "direct" {
		t.Errorf("via = %q, want direct", got.Via)
	}
	if ps.runCount() != 1 {
		t.Errorf("runCount = %d, want 1 -- the probe must actually launch", ps.runCount())
	}
}

func TestHandleTestSiteConfigProxy_CustomURL(t *testing.T) {
	// The escape for a host with no public internet: point the probe at
	// something it CAN reach. Without this a hard gate would trap an
	// internal-only deployment in setup forever.
	fr := &probeFakeRunner{exitCode: 0}
	srv, ps := newProbeHarness(t, types.SiteConfig{}, fr)
	w := do(t, srv, http.MethodPost, "/api/v1/site-config/test-proxy", adminToken,
		`{"url":"https://intranet.corp.internal/health"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	got := decodeProbeResponse(t, w.Body.String())
	if got.State != "reached" {
		t.Fatalf("state = %q, want reached", got.State)
	}
	// It must NOT borrow the default targets' credibility: nothing verified the
	// body, so the detail claims only that the request COMPLETED (never
	// "payloads matched", never "Reached"), and the custom qualifier is set so
	// the UI renders its own caveat (T.CUSTOM_CAVEAT) and the weaker chip.
	if !strings.Contains(got.Detail, "completed") || strings.Contains(got.Detail, "payloads matched") || strings.Contains(got.Detail, "Reached") {
		t.Errorf("detail = %q, want the weaker request-completed claim only", got.Detail)
	}
	if !strings.Contains(got.Detail, "intranet.corp.internal/health") {
		t.Errorf("detail = %q, want it to name the custom endpoint", got.Detail)
	}
	if !got.Custom {
		t.Error("custom = false, want true — the UI keys its weaker-claim rendering off this")
	}
	if ps.runCount() != 1 {
		t.Errorf("runCount = %d, want 1", ps.runCount())
	}
}

func TestHandleTestSiteConfigProxy_CustomURLRejectsJunk(t *testing.T) {
	// An operator-only endpoint that dials a caller-named target still validates
	// it: same rules as any stored site-config URL. A shell metacharacter or a
	// non-http scheme never reaches the probe sandbox.
	for _, bad := range []string{
		"file:///etc/passwd",
		"https://evil.internal/$(id)",
		"not a url",
		"ftp://mirror.corp.internal",
		"https://host with spaces/x",
	} {
		fr := &probeFakeRunner{exitCode: 0}
		srv, ps := newProbeHarness(t, types.SiteConfig{}, fr)
		w := do(t, srv, http.MethodPost, "/api/v1/site-config/test-proxy", adminToken,
			`{"url":`+mustQuote(bad)+`}`)
		if w.Code != http.StatusBadRequest {
			t.Errorf("url %q: code = %d, want 400", bad, w.Code)
		}
		if ps.runCount() != 0 {
			t.Errorf("url %q: launched %d probe sandboxes, want 0", bad, ps.runCount())
		}
	}
}

func TestClassifyProxyProbe_InterceptedIsBlockedNotReached(t *testing.T) {
	// The corporate-network state an exit-code-only probe scores as SUCCESS: a
	// block page or captive portal replies 200, so the connection "worked"
	// while egress is firmly shut. Now that a green probe UNLOCKS the setup
	// gate, calling this reached would wave an operator through a network that
	// cannot actually reach anything.
	got := classifyProxyProbe(probeRunResult{hasExitCode: true, exitCode: proxyProbeInterceptedCode}, builtinSubject)
	if got.State != "blocked" {
		t.Fatalf("state = %q, want blocked — an intercepted reply is NOT reachability", got.State)
	}
	if !got.Intercepted {
		t.Fatal("intercepted = false, want true — the UI renders this blocked flavor apart (different person to call, not a scarier error)")
	}
	if !strings.Contains(got.Detail, "captive portal") || !strings.Contains(got.Detail, "not actually open") {
		t.Errorf("detail = %q, want it to name interception and say egress is not open", got.Detail)
	}
	// The flag exists so no client string-matches; but the plain blocked case
	// must never set it — same verdict, different flavor, and only the real
	// sentinel exit may produce the flavor.
	if plain := classifyProxyProbe(probeRunResult{hasExitCode: true, exitCode: 7}, builtinSubject); plain.Intercepted {
		t.Error("a connection-level blocked must never read as intercepted")
	}
}

func TestProxyProbeScript_ChecksBodyAndCoversEveryTarget(t *testing.T) {
	// Two invariants the script must keep, both load-bearing:
	//  1. every target's host is in the egress allowlist, or it fails as "DNS
	//     resolution failed" and reads as a real network fault;
	//  2. each target's expected BODY appears in the script — a probe that only
	//     looked at exit codes is the bug this replaced.
	if len(proxyProbeTargets) < 2 {
		t.Fatal("want at least two independent targets: one blocked endpoint must not fail the whole probe")
	}
	for _, tg := range proxyProbeTargets {
		h := workspacescan.HostOf(tg.url)
		if !slices.Contains(proxyProbeHosts, h) {
			t.Errorf("target %s: host %q missing from the probe egress allowlist", tg.url, h)
		}
		if !strings.Contains(proxyProbeScript, tg.want) {
			t.Errorf("target %s: expected body %q never checked by the script", tg.url, tg.want)
		}
	}
	// github.com is exactly the wrong pick — orgs block it, and a false "no
	// internet" now gates setup.
	if strings.Contains(proxyProbeScript, "github") {
		t.Error("probe must not depend on github.com: plenty of orgs block it outright")
	}
}

func TestHandleTestSiteConfigProxy_EmptyBodyIsTheOrdinaryCall(t *testing.T) {
	// The UI POSTs with NO body (fetch sends none when there is nothing to
	// send). decodeStrict read that as io.EOF and 400'd every click with
	// "invalid JSON body: EOF" -- the endpoint takes no fields, so an absent
	// body is the normal shape, not a malformed one.
	fr := &probeFakeRunner{exitCode: 0}
	srv, _ := newProbeHarness(t, types.SiteConfig{UpstreamProxyURL: "http://proxy.corp:3128"}, fr)
	w := do(t, srv, http.MethodPost, "/api/v1/site-config/test-proxy", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 for a body-less POST; body=%s", w.Code, w.Body.String())
	}
	if got := decodeProbeResponse(t, w.Body.String()); got.State != "reached" {
		t.Errorf("state = %q, want reached", got.State)
	}
}

func TestHandleTestSiteConfigProxy_NoRunner(t *testing.T) {
	srv, ps := newProbeHarness(t, types.SiteConfig{UpstreamProxyURL: "http://proxy.corp:3128"}, nil)
	w := do(t, srv, http.MethodPost, "/api/v1/site-config/test-proxy", adminToken, "{}")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (no_runner is an honest state, not a transport failure); body=%s", w.Code, w.Body.String())
	}
	got := decodeProbeResponse(t, w.Body.String())
	if got.State != "no_runner" {
		t.Errorf("state = %q, want no_runner", got.State)
	}
	if got.Detail != "no runner configured, nothing to launch a probe with" {
		t.Errorf("detail = %q", got.Detail)
	}
	if ps.runCount() != 0 {
		t.Error("no_runner must never attempt to launch a sandbox")
	}
}

func TestHandleTestSiteConfigProxy_RejectsUnknownFields(t *testing.T) {
	fr := &probeFakeRunner{}
	srv, _ := newProbeHarness(t, types.SiteConfig{UpstreamProxyURL: "http://proxy.corp:3128"}, fr)
	w := do(t, srv, http.MethodPost, "/api/v1/site-config/test-proxy", adminToken, `{"target":"https://evil.internal"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 for an unknown field (this endpoint accepts no caller-supplied target)", w.Code)
	}
}

// TestHandleTestSiteConfigProxy_NeverLogsCredentialedUpstreamURL is the
// explicit security assertion the task calls out: the upstream proxy URL may
// legitimately resolve from a secret carrying an embedded credential, and it
// must be resolved for the probe (dispatchRun's own resolveRunUpstreamProxy
// does that) but NEVER appear in this endpoint's own audit event or HTTP
// response -- only the fixed, non-secret target host this code chose to
// probe.
func TestHandleTestSiteConfigProxy_NeverLogsCredentialedUpstreamURL(t *testing.T) {
	const credentialedURL = "http://svc-account:hunter2-token@proxy.corp:3128"
	fr := &probeFakeRunner{exitCode: 0}
	srv, ps := newProbeHarness(t, types.SiteConfig{UpstreamProxySecretRef: "corp-proxy-url"}, fr)
	srv.cfg.Secrets = &memSecrets{m: map[string][]byte{"corp-proxy-url": []byte(credentialedURL)}}

	w := do(t, srv, http.MethodPost, "/api/v1/site-config/test-proxy", adminToken, "{}")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "hunter2-token") || strings.Contains(w.Body.String(), "svc-account") {
		t.Fatalf("response body leaks the credentialed upstream URL: %s", w.Body.String())
	}
	for _, ev := range ps.events {
		raw := string(ev.Data)
		if strings.Contains(raw, "hunter2-token") || strings.Contains(raw, "svc-account") {
			t.Fatalf("audit event %q leaks the credentialed upstream URL: %s", ev.Action, raw)
		}
	}
}

// ─── test-redirect ────────────────────────────────────────────────────────────

func TestHandleTestSiteConfigRedirect_Reached(t *testing.T) {
	fr := &probeFakeRunner{exitCode: 0}
	srv, ps := newProbeHarness(t, redirectSiteConfig(), fr)

	body := `{"from":"registry.npmjs.org","to":"artifactory.corp/npm"}`
	w := do(t, srv, http.MethodPost, "/api/v1/site-config/test-redirect", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	got := decodeProbeResponse(t, w.Body.String())
	if got.State != "reached" {
		t.Errorf("state = %q, want reached (detail=%q)", got.State, got.Detail)
	}
	if !strings.Contains(got.Detail, "artifactory.corp") || !strings.Contains(got.Detail, "registry.npmjs.org") {
		t.Errorf("detail = %q, want both hosts named", got.Detail)
	}

	events := ps.actionEvents("site_config.test_redirect")
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 site_config.test_redirect audit event, got %d", len(events))
	}
	var data map[string]any
	_ = json.Unmarshal(events[0].Data, &data)
	if data["to_host"] != "artifactory.corp" || data["from_host"] != "registry.npmjs.org" {
		t.Errorf("audit data = %+v, want to_host/from_host set from the STORED row", data)
	}
}

func TestHandleTestSiteConfigRedirect_Bypass(t *testing.T) {
	fr := &probeFakeRunner{exitCode: redirectProbeBypassCode}
	srv, _ := newProbeHarness(t, redirectSiteConfig(), fr)

	body := `{"from":"registry.npmjs.org","to":"artifactory.corp/npm"}`
	w := do(t, srv, http.MethodPost, "/api/v1/site-config/test-redirect", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	got := decodeProbeResponse(t, w.Body.String())
	if got.State != "bypass" {
		t.Errorf("state = %q, want bypass (mirror reachable AND the public host still directly reachable); detail=%q", got.State, got.Detail)
	}
}

func TestHandleTestSiteConfigRedirect_BlockedWithRealError(t *testing.T) {
	fr := &probeFakeRunner{exitCode: 35} // curl: TLS handshake failed
	srv, _ := newProbeHarness(t, redirectSiteConfig(), fr)

	body := `{"from":"registry.npmjs.org","to":"artifactory.corp/npm"}`
	w := do(t, srv, http.MethodPost, "/api/v1/site-config/test-redirect", adminToken, body)
	got := decodeProbeResponse(t, w.Body.String())
	if got.State != "blocked" {
		t.Errorf("state = %q, want blocked", got.State)
	}
	if !strings.Contains(got.Detail, "TLS handshake failed") {
		t.Errorf("detail = %q, want the real curl error", got.Detail)
	}
}

// TestHandleTestSiteConfigRedirect_UnknownFromIsRefused pins the SSRF guard:
// a `from` that does not name a row in the stored EgressRedirects must be
// refused (404) and must NEVER cause a probe sandbox to launch against
// whatever `to` the caller supplied -- that would be an SSRF gadget built on
// the operator's own throwaway-sandbox credentials.
func TestHandleTestSiteConfigRedirect_UnknownFromIsRefused(t *testing.T) {
	fr := &probeFakeRunner{}
	srv, ps := newProbeHarness(t, redirectSiteConfig(), fr)

	body := `{"from":"not-configured.example.com","to":"169.254.169.254"}`
	w := do(t, srv, http.MethodPost, "/api/v1/site-config/test-redirect", adminToken, body)
	if w.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404; body=%s", w.Code, w.Body.String())
	}
	if ps.runCount() != 0 {
		t.Fatal("SSRF guard violated: an unconfigured from must never launch a probe sandbox")
	}
}

func TestHandleTestSiteConfigRedirect_MissingFrom(t *testing.T) {
	fr := &probeFakeRunner{}
	srv, _ := newProbeHarness(t, redirectSiteConfig(), fr)
	w := do(t, srv, http.MethodPost, "/api/v1/site-config/test-redirect", adminToken, `{"to":"artifactory.corp/npm"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 when from is omitted; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleTestSiteConfigRedirect_NoRunner(t *testing.T) {
	srv, ps := newProbeHarness(t, redirectSiteConfig(), nil)
	body := `{"from":"registry.npmjs.org","to":"artifactory.corp/npm"}`
	w := do(t, srv, http.MethodPost, "/api/v1/site-config/test-redirect", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	got := decodeProbeResponse(t, w.Body.String())
	if got.State != "no_runner" {
		t.Errorf("state = %q, want no_runner", got.State)
	}
	if ps.runCount() != 0 {
		t.Error("no_runner must never attempt to launch a sandbox")
	}
}

// ─── timeout / cleanup ────────────────────────────────────────────────────────

// TestSiteConfigProbe_TimeoutReclaimsSandbox pins the hard bound: a probe
// whose task never finishes must not hold a live sandbox forever. It shrinks
// the package's wait timeout so the test does not take the real 50s.
func TestSiteConfigProbe_TimeoutReclaimsSandbox(t *testing.T) {
	orig := siteConfigProbeWaitTimeout
	siteConfigProbeWaitTimeout = 100 * time.Millisecond
	t.Cleanup(func() { siteConfigProbeWaitTimeout = orig })

	fr := &probeFakeRunner{block: true} // Wait never returns on its own
	srv, ps := newProbeHarness(t, types.SiteConfig{UpstreamProxyURL: "http://proxy.corp:3128"}, fr)

	w := do(t, srv, http.MethodPost, "/api/v1/site-config/test-proxy", adminToken, "{}")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 even on a probe timeout (still a definite, if blocked, answer); body=%s", w.Code, w.Body.String())
	}
	got := decodeProbeResponse(t, w.Body.String())
	if got.State != "blocked" {
		t.Errorf("state = %q, want blocked (the probe never finished)", got.State)
	}

	if n := fr.stops(); n != 1 {
		t.Errorf("StopSandbox calls = %d, want exactly 1 (the hung probe's sandbox must be torn down)", n)
	}
	if st := ps.soleRunState(); st != types.RunKilled {
		t.Errorf("run state = %q, want KILLED (a hung probe must not be left RUNNING forever)", st)
	}
}

// ─── operator-only ────────────────────────────────────────────────────────────

// TestHandleTestSiteConfig_OperatorOnly reuses rbac_test.go's SSO-session
// harness (rbacServer/ssoSession/doSSO, same package) to prove both new
// routes sit behind requireOperator: a signed-in human outside the operator
// allowlist must be refused, never reach the handler.
func TestHandleTestSiteConfig_OperatorOnly(t *testing.T) {
	srv := rbacServer(t, rbacOperator)
	viewer := ssoSession(t, "sub-viewer", rbacViewer, oidc.RoleMember)
	for _, path := range []string{
		"/api/v1/site-config/test-proxy",
		"/api/v1/site-config/test-redirect",
	} {
		t.Run(path, func(t *testing.T) {
			w := doSSO(t, srv, http.MethodPost, path, viewer, "{}")
			if w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (viewer must not reach this handler): %s", w.Code, w.Body.String())
			}
		})
	}
}

// mustQuote JSON-quotes a string for inline test bodies.
func mustQuote(v string) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
