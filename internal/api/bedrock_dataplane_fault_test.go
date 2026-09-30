// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// faultHintStore is dispatchTestStore with the hint readable back through
// GetRun (the real PG store's shape) and a no-op TouchRun for the ingest path.
type faultHintStore struct{ *dispatchTestStore }

func (s faultHintStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	r, err := s.dispatchTestStore.GetRun(ctx, id)
	r.FailureHint = s.FailureHint()
	return r, err
}
func (faultHintStore) TouchRun(context.Context, uuid.UUID) error { return nil }

// exitRunner ends the agent with a chosen exit code.
type exitRunner struct {
	*fakeRunner
	code int
}

func (r *exitRunner) Wait(context.Context, string) (int, error) { return r.code, nil }

type faultRun struct {
	t     *testing.T
	srv   *Server
	st    faultHintStore
	id    uuid.UUID
	token string
}

func newFaultRun(t *testing.T, exitCode int) *faultRun {
	t.Helper()
	h := newHarness(t)
	run := newFinalizeRun()
	st := faultHintStore{&dispatchTestStore{run: run, state: types.RunRunning}}
	baseCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cfg := baseTestConfig(h, st)
	cfg.Runner = &exitRunner{fakeRunner: &fakeRunner{}, code: exitCode}
	cfg.Broker = &raceBroker{}
	cfg.BaseCtx = baseCtx
	return &faultRun{t: t, srv: New(cfg), st: st, id: run.ID, token: h.mintRunToken(t, run.ID)}
}

// post sends the decision row the proxy writes for one relayed Bedrock call.
func (f *faultRun) post(fault string) {
	f.t.Helper()
	body, _ := json.Marshal(egress.DecisionLog{
		Request:       egress.Request{Host: "bedrock-runtime.us-east-1.amazonaws.com", Port: 443, Method: http.MethodPost, Path: "/model/m/converse"},
		Decision:      egress.Allow,
		RuleSource:    "scan:mitm",
		UpstreamFault: fault,
	})
	if w := do(f.t, f.srv, http.MethodPost, "/api/v1/internal/decisions", f.token, string(body)); w.Code >= 300 {
		f.t.Fatalf("post decision: %d %s", w.Code, w.Body.String())
	}
}

// end lets the agent exit and waits for the watcher's terminal transition.
func (f *faultRun) end(want types.RunState) {
	f.t.Helper()
	f.srv.startCompletionWatcher(f.id, "ref", "exec")
	deadline := time.Now().Add(10 * time.Second)
	for f.st.State() != want && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := f.st.State(); got != want {
		f.t.Fatalf("state = %s, want %s", got, want)
	}
}

// hint is what a reader of the run is served (every run route projects).
func (f *faultRun) hint() string {
	r, _ := f.st.GetRun(context.Background(), f.id)
	runs := []types.AgentRun{r}
	projectStatusDetail(runs)
	return runs[0].FailureHint
}

func TestBedrockDataPlaneFault_DenyNamesThePolicy(t *testing.T) {
	f := newFaultRun(t, 1)
	f.post("AccessDeniedException")
	f.end(types.RunFailed)
	if h := f.hint(); !strings.Contains(h, "AccessDeniedException") || !strings.Contains(h, "service control policy") {
		t.Fatalf("hint = %q, want the policy deny named", h)
	}
}

func TestBedrockDataPlaneFault_ThrottleThenOKLeavesNoHint(t *testing.T) {
	f := newFaultRun(t, 0)
	f.post("ThrottlingException")
	f.post("ThrottlingException")
	f.post("recovered")
	if got := f.st.FailureHint(); got != "" {
		t.Fatalf("stored hint after recovery = %q, want cleared", got)
	}
	f.post("") // an ordinary call afterwards
	f.end(types.RunCompleted)
	if h := f.hint(); h != "" {
		t.Fatalf("hint = %q on a run that succeeded", h)
	}
}

func TestBedrockDataPlaneFault_ThrottleExhaustedNamesThrottling(t *testing.T) {
	f := newFaultRun(t, 1)
	for i := 0; i < 3; i++ {
		f.post("ThrottlingException")
	}
	f.end(types.RunFailed)
	if h := f.hint(); !strings.Contains(h, "throttling") {
		t.Fatalf("hint = %q, want throttling named", h)
	}
}

// The row can land after the watcher's FAILED CAS but before the revoke that
// follows it kills the run token (the agent exits the instant it reads the
// 403; the sidecar posts asynchronously). A FAILED run with no reason of its
// own still gets this one — driven directly, since through HTTP the window is
// a race. After the revoke the post is refused outright (401), which is the
// residual this cannot close.
func TestBedrockDataPlaneFault_LateRowStillExplainsTheFailure(t *testing.T) {
	f := newFaultRun(t, 1)
	f.end(types.RunFailed)
	f.srv.noteBedrockDataPlaneFault(context.Background(), f.id, "AccessDeniedException")
	if h := f.hint(); !strings.Contains(h, "AccessDeniedException") {
		t.Fatalf("hint = %q, want the late deny recorded", h)
	}
}

// …but never over a reason the failure already has, and a throttle that the
// agent survived (exit 0) is not shown as a failure.
func TestBedrockDataPlaneFault_NeverOverwritesOrPaintsASuccess(t *testing.T) {
	f := newFaultRun(t, 1)
	f.end(types.RunFailed)
	ctx := context.Background()
	_ = f.st.SetRunFailureHint(ctx, f.id, "mount failed")
	f.srv.noteBedrockDataPlaneFault(ctx, f.id, "AccessDeniedException")
	f.srv.noteBedrockDataPlaneFault(ctx, f.id, "recovered")
	if h := f.hint(); h != "mount failed" {
		t.Fatalf("hint = %q, want the dispatch reason kept", h)
	}

	g := newFaultRun(t, 0)
	g.post("ThrottlingException")
	g.end(types.RunCompleted)
	if h := g.hint(); h != "" {
		t.Fatalf("hint = %q served on a COMPLETED run", h)
	}
}

// The class rides the egress.allow audit row itself, not only the hint.
func TestBedrockDataPlaneFault_AuditRowCarriesTheClass(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	body, _ := json.Marshal(egress.DecisionLog{
		Request:  egress.Request{Host: "bedrock-runtime.us-east-1.amazonaws.com", Port: 443, Method: http.MethodPost},
		Decision: egress.Allow, RuleSource: "scan:mitm", UpstreamFault: "ThrottlingException",
	})
	if w := do(t, h.srv, http.MethodPost, "/api/v1/internal/decisions", h.mintRunToken(t, runID), string(body)); w.Code >= 300 {
		t.Fatalf("post decision: %d", w.Code)
	}
	var data map[string]any
	_ = json.Unmarshal(lastAuditEvent(t, h.audit.events, "egress.allow").Data, &data)
	if data["upstream_fault"] != "ThrottlingException" {
		t.Fatalf("egress.allow data = %v, want upstream_fault", data)
	}
}

// modelAccessCast is a recording whose agent printed Claude Code's model-access
// error, as seen on the per-user AWS SSO Bedrock lane (#1280), with the
// terminal's escape sequences around it, and then something else.
func modelAccessCast(t *testing.T) string {
	t.Helper()
	lines := []string{`{"version":2,"width":80,"height":24}`}
	for i, out := range []string{
		"\x1b[?25l\x1b[2K\x1b[1G Working on the task\r\n",
		"\x1b[31m\u23bf  There's an issue with the selected model (us.anthropic.claude-sonnet-4-5-20250929-v1:0). " +
			"It may not exist or you may not have access to it. Run /model to pick a different model.\x1b[39m\r\n",
		"\x1b[?25h\r\n",
	} {
		ev, _ := json.Marshal([]any{float64(i) + 0.5, "o", out})
		lines = append(lines, string(ev))
	}
	return strings.Join(lines, "\n") + "\n"
}

// TestModelAccessFailureHint_QuotesTheAgentsOwnLine (#1280): a run that exits
// non-zero with no reason of its own, whose recording's last lines name a
// model-access problem, is served that line as its failure hint, quoted as the
// agent's output and pointing at the recording; the proxy never saw the
// model's answer on that lane, so the hint claims no cause of its own. A run
// that succeeded, a recording naming no such problem, and a failure that
// already has its reason are left as they were.
func TestModelAccessFailureHint_QuotesTheAgentsOwnLine(t *testing.T) {
	const quoted = `"There's an issue with the selected model (us.anthropic.claude-sonnet-4-5-20250929-v1:0). ` +
		`It may not exist or you may not have access to it. Run /model to pick a different model."`
	for _, tc := range []struct {
		name    string
		exit    int
		cast    func(*testing.T) string
		arrange func(*faultRun)
		want    func(string) bool
	}{
		{"failed, model access named", 1, modelAccessCast, func(*faultRun) {},
			func(h string) bool { return strings.Contains(h, quoted) && strings.Contains(h, "recording") }},
		{"failed, nothing named", 1, func(*testing.T) string {
			return `{"version":2,"width":80,"height":24}` + "\n" + `[0.5, "o", "error: tests failed\r\n"]` + "\n"
		}, func(*faultRun) {}, func(h string) bool { return h == "" }},
		{"failed, no recording", 1, nil, func(*faultRun) {}, func(h string) bool { return h == "" }},
		{"succeeded", 0, modelAccessCast, func(*faultRun) {}, func(h string) bool { return h == "" }},
		{"failed with its own reason", 1, modelAccessCast, func(f *faultRun) { f.post("AccessDeniedException") },
			func(h string) bool {
				return strings.Contains(h, "AccessDeniedException") && !strings.Contains(h, "selected model")
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFaultRun(t, tc.exit)
			rs, err := recording.NewFSStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			f.srv.cfg.RecordingStore = rs
			if tc.cast != nil {
				if err := rs.SaveCast(context.Background(), f.id.String(), strings.NewReader(tc.cast(t))); err != nil {
					t.Fatal(err)
				}
			}
			tc.arrange(f)
			f.end(map[bool]types.RunState{true: types.RunCompleted, false: types.RunFailed}[tc.exit == 0])
			if h := f.hint(); !tc.want(h) {
				t.Fatalf("hint = %q", h)
			}
		})
	}
}

// failWithCast ends f's run FAILED with cast as its recording.
func (f *faultRun) failWithCast(cast string) {
	f.t.Helper()
	rs, err := recording.NewFSStore(f.t.TempDir())
	if err != nil {
		f.t.Fatal(err)
	}
	f.srv.cfg.RecordingStore = rs
	if err := rs.SaveCast(context.Background(), f.id.String(), strings.NewReader(cast)); err != nil {
		f.t.Fatal(err)
	}
	f.end(types.RunFailed)
}

// TestModelAccessFailureHint_StripsControlsAndKeepsItsQuote: every C0 and C1
// control is gone from the quoted line, 8-bit CSI and OSC sequences with
// their parameters, and a double quote inside the line cannot close the
// hint's quote early; a full stop follows the quote.
func TestModelAccessFailureHint_StripsControlsAndKeepsItsQuote(t *testing.T) {
	ev, _ := json.Marshal([]any{0.5, "o", "\a\u009b31m\u009d0;title\u009cYou don't have access to the model \"claude\" " +
		"with the specified model ID.\a\x1b\u0085\r\n"})
	f := newFaultRun(t, 1)
	f.failWithCast(`{"version":2,"width":80,"height":24}` + "\n" + string(ev) + "\n")
	want := `The agent's last output before it exited reported a model-access problem: ` +
		`"You don't have access to the model 'claude' with the specified model ID.". ` +
		`Wardyn did not see the model's answer itself; the run's recording has the full output.`
	if h := f.hint(); h != want {
		t.Fatalf("hint = %q\nwant   %q", h, want)
	}
}

// readFaultStore is faultHintStore with the reads GET /runs (plain and
// filtered) and GET /runs/{id} make: the run listed, for its owner or for
// everyone, and no audit trail.
type readFaultStore struct{ faultHintStore }

func (s readFaultStore) ListRuns(ctx context.Context) ([]types.AgentRun, error) {
	r, err := s.GetRun(ctx, s.run.ID)
	return []types.AgentRun{r}, err
}

func (s readFaultStore) ListRunsPageByCreator(ctx context.Context, by string, _ store.Page) ([]types.AgentRun, error) {
	runs, err := s.ListRuns(ctx)
	return slices.DeleteFunc(runs, func(r types.AgentRun) bool { return r.CreatedBy != by }), err
}

func (s readFaultStore) ListRunsFiltered(ctx context.Context, f store.RunFilter, p store.Page) ([]types.AgentRun, error) {
	if f.Owner == "" {
		return s.ListRuns(ctx)
	}
	return s.ListRunsPageByCreator(ctx, f.Owner, p)
}

func (readFaultStore) CountHiddenRuns(context.Context, store.RunFilter) (int, int, error) {
	return 0, 0, nil
}

func (readFaultStore) QueryAuditEvents(context.Context, uuid.UUID, int) ([]types.AuditEvent, error) {
	return nil, nil
}

// TestModelAccessFailureHint_QuoteOnlyForRecordingReaders: the quoted line is
// recording content, so a run read serves it only to a reader who could open
// the recording (the owner, a super admin). A security admin reads the run but
// not its recording, and is served the hint without the quote.
func TestModelAccessFailureHint_QuoteOnlyForRecordingReaders(t *testing.T) {
	f := newFaultRun(t, 1)
	f.srv.cfg.OIDC = &oidc.Authenticator{}
	f.srv.cfg.Store = readFaultStore{f.st}
	f.srv.router = f.srv.routes()
	f.failWithCast(modelAccessCast(t))
	const quote = "you may not have access to it"
	for _, tc := range []struct {
		name   string
		cookie *http.Cookie
		quoted bool
	}{
		{"owner", ssoSession(t, "t@example.com", "t@example.com", oidc.RoleUser), true},
		{"super admin", ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin), true},
		{"security admin", ssoSession(t, "sub-sec", "sec@corp.example", oidc.RoleSecurityAdmin), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, path := range []string{"/api/v1/runs/" + f.id.String(), "/api/v1/runs", "/api/v1/runs?owner=all"} {
				w := doSSO(t, f.srv, http.MethodGet, path, tc.cookie, "")
				if w.Code != http.StatusOK {
					t.Fatalf("GET %s = %d %s", path, w.Code, w.Body.String())
				}
				body := w.Body.String()
				if got := strings.Contains(body, quote); got != tc.quoted {
					t.Errorf("GET %s quotes the recording: %v, want %v; body=%s", path, got, tc.quoted, body)
				}
				if !tc.quoted && !strings.Contains(body, "The agent's last output before it exited reported a model-access problem. "+
					"Wardyn did not see the model's answer itself; the run's owner can open its recording for the full output.") {
					t.Errorf("GET %s: want the unquoted hint; body=%s", path, body)
				}
			}
		})
	}
}
