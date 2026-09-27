// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
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
