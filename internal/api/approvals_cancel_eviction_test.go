// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/approval"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The eviction seam, end to end.
//
// The pieces are pinned elsewhere: the runner package proves a k8s pod evicted
// out from under a run reads terminal (internal/runner/k8s/terminal_pod_test.go,
// lifecycle_test.go), and this package proves a terminal transition cancels an
// EGRESS_DOMAIN approval (approvals_cancel_terminal_test.go) and that
// cancelRunApprovals counts a credential_reauth row when it is called
// (credential_reauth_metrics_test.go). This drives a runner-reported eviction
// through the completion watcher into finalizeRunTail and asks what happens to a
// held credential_reauth row — the row a person is being asked to sign in for,
// on a screen that would otherwise keep asking after the sandbox it would serve
// is gone.

// evictedExitCode is what Wait reports at THIS package's boundary when a pod is
// evicted: internal/runner/k8s/exec.go's terminalExecStatus sees PodFailed with
// no terminated ephemeral-container status, so the main container's exit is
// unknown (`ExitCode == nil`) and Wait answers notFoundExitCode (the constant
// beside terminalExecStatus in exec.go) with a NIL error — an authoritative, non-zero completion, never a
// probe error. The control plane's only job here is to read non-zero as FAILED
// rather than invent a COMPLETED, and then run the terminal tail.
//
// It is spelled out rather than imported because notFoundExitCode is unexported
// in a `//go:build k8s` package; the assertion below re-states the one property
// that matters if that constant ever moves.
const evictedExitCode = -1

// evictedRunner is the sandbox driver reporting that verdict. Everything else
// is fakeRunner's default, because eviction changes nothing about teardown: the
// pod is already gone and StopSandbox is a no-op the tail still calls.
type evictedRunner struct{ *fakeRunner }

func (r *evictedRunner) Wait(context.Context, string) (int, error) { return evictedExitCode, nil }

// evictionApprovalStore is a minimal approval.Store: a map, the REAL
// `WHERE state='PENDING'` CAS (anything else is ErrAlreadyDecided, exactly as
// store.PG.DecideApproval answers), and a recorder.
//
// It exists because this test has to see the approval.cancelled AUDIT row, and
// that row is written inside approval.CancelForRun over the STORE's recorder.
// The package's own fakeApprovals mirrors the FSM faithfully but is the service,
// not the store, so it emits nothing — which is why every existing terminal
// test can only assert that the cascade was CALLED. Running the shipping FSM
// over a real-enough store is what turns "the wiring fired" into "the row the
// operator reads actually landed".
type evictionApprovalStore struct {
	mu   sync.Mutex
	rows map[uuid.UUID]types.ApprovalRequest
	rec  *syncAudit
}

func (s *evictionApprovalStore) CreateApproval(_ context.Context, a types.ApprovalRequest) (types.ApprovalRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[a.ID] = a
	return a, nil
}

func (s *evictionApprovalStore) GetApproval(_ context.Context, id uuid.UUID) (types.ApprovalRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ap, ok := s.rows[id]
	if !ok {
		return types.ApprovalRequest{}, errStoreNotFound
	}
	return ap, nil
}

func (s *evictionApprovalStore) ListApprovals(_ context.Context, state types.ApprovalState) ([]types.ApprovalRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]types.ApprovalRequest, 0, len(s.rows))
	for _, ap := range s.rows {
		if state == "" || ap.State == state {
			out = append(out, ap)
		}
	}
	return out, nil
}

func (s *evictionApprovalStore) DecideApproval(_ context.Context, id uuid.UUID, d types.ApprovalDecision) (types.ApprovalRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ap, ok := s.rows[id]
	if !ok {
		return types.ApprovalRequest{}, errStoreNotFound
	}
	// THE CAS, not an overwrite: the cascade races a human decision already in
	// flight, and CancelForRun treats ErrAlreadyDecided as a race rather than an
	// error. A fake that let the cancel win unconditionally would hide that.
	if ap.State != types.ApprovalPending {
		return types.ApprovalRequest{}, approval.ErrAlreadyDecided
	}
	ap.State, ap.DecidedBy, ap.Reason = d.State, d.DecidedBy, d.Reason
	ap.DecisionScope, ap.DecisionExpiresAt = d.Scope, d.ExpiresAt
	s.rows[id] = ap
	return ap, nil
}

func (s *evictionApprovalStore) Record(ctx context.Context, ev types.AuditEvent) error {
	return s.rec.Record(ctx, ev)
}

var _ approval.Store = (*evictionApprovalStore)(nil)

// evictionApprovals is the SHIPPING approval service over that store — the same
// composition cmd/wardynd's approvalService uses (adapters.go), so the cascade
// this drive exercises is the one that runs in production.
type evictionApprovals struct{ st *evictionApprovalStore }

func (a evictionApprovals) Request(ctx context.Context, req types.ApprovalRequest) (types.ApprovalRequest, error) {
	return approval.RequestApproval(ctx, a.st, req)
}

func (a evictionApprovals) Decide(ctx context.Context, id uuid.UUID, by types.ActorType, d types.ApprovalDecision) (types.ApprovalRequest, error) {
	return approval.Decide(ctx, a.st, id, by, d)
}

func (a evictionApprovals) Get(ctx context.Context, id uuid.UUID) (types.ApprovalRequest, error) {
	return a.st.GetApproval(ctx, id)
}

func (a evictionApprovals) List(ctx context.Context, state types.ApprovalState) ([]types.ApprovalRequest, error) {
	return a.st.ListApprovals(ctx, state)
}

func (a evictionApprovals) CancelForRun(ctx context.Context, runID uuid.UUID, reason string) (int, error) {
	return approval.CancelForRun(ctx, a.st, runID, reason)
}

func (a evictionApprovals) ExpireOne(ctx context.Context, id uuid.UUID, actor, reason string) error {
	return approval.ExpireOne(ctx, a.st, id, actor, reason)
}

func (a evictionApprovals) CountForRun(ctx context.Context, runID uuid.UUID) (int, error) {
	rows, err := a.st.ListApprovals(ctx, "")
	if err != nil {
		return 0, err
	}
	n := 0
	for _, ap := range rows {
		if ap.RunID == runID {
			n++
		}
	}
	return n, nil
}

var _ ApprovalService = evictionApprovals{}

// TestEvictedSandbox_CancelsAHeldCredentialReauthAndShutsTheInternalDoor is the
// combined coverage: a run whose sandbox the platform EVICTED, with a sign-in
// request open for it, reaches the terminal tail and leaves nothing live behind.
//
// Four facts, in the order the operator meets them:
//  1. the run is FAILED — an evicted pod is not a success, and Wait's non-zero
//     answer is what says so;
//  2. the credential_reauth row is CANCELLED with reason=run_failed, so the
//     console stops asking a person to sign in for a sandbox that is gone and
//     the 24h sweeper never gets to record it as "nobody answered";
//  3. an approval.cancelled audit row landed — the REAL one, from the shipping
//     FSM — so the queue's emptying is explained;
//  4. wardyn_credential_reauth_total{outcome="cancelled"} moved by exactly one.
//
// Then the fifth, which is the security half: the proxy sidecar is still
// holding that request open and will poll for it. Its next internal read must
// be REFUSED (403), because a run token outliving its run is precisely the
// window refuseTerminalRun exists to close — and a hold that kept polling a
// dead run would burn its whole budget against it.
func TestEvictedSandbox_CancelsAHeldCredentialReauthAndShutsTheInternalDoor(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	st := &dispatchTestStore{
		run:   types.AgentRun{ID: runID, CreatedBy: "alice@example.com", SandboxRef: "sbx-evicted"},
		state: types.RunRunning,
	}
	audit := &syncAudit{}
	approvals := evictionApprovals{st: &evictionApprovalStore{
		rows: map[uuid.UUID]types.ApprovalRequest{}, rec: audit,
	}}

	// The held sign-in request: raised while the run was RUNNING, still PENDING
	// when the node evicted the pod.
	apID := uuid.New()
	if _, err := approvals.Request(context.Background(), types.ApprovalRequest{
		ID: apID, RunID: runID, Kind: types.ApprovalCredentialReauth,
		RequestedScope: json.RawMessage(`{"provider":"aws_sso","owner":"alice@example.com"}`),
		RequestedAt:    time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed the held re-auth request: %v", err)
	}

	cfg := baseTestConfig(h, st)
	cfg.Runner = &evictedRunner{fakeRunner: &fakeRunner{}}
	cfg.Broker = h.broker
	cfg.Approvals = approvals
	cfg.Audit = audit
	srv := New(cfg)
	token := h.mintRunToken(t, runID)

	// The sidecar can read its own request while the run is alive — the control
	// for the 403 below, so that refusal cannot be a route that never worked.
	if w := do(t, srv, http.MethodGet, "/api/v1/internal/approvals/"+apID.String(), token, ""); w.Code != http.StatusOK {
		t.Fatalf("internal approval read on a LIVE run: code = %d, want 200. body=%s", w.Code, w.Body.String())
	}

	before := metricValue(t, srv, `wardyn_credential_reauth_total{outcome="cancelled"}`)

	// THE EVICTION. The completion watcher is the run's only watcher; Wait
	// answers the evicted verdict and the tail runs from there.
	srv.startCompletionWatcher(runID, "sbx-evicted", "exec-evicted")

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ap, err := approvals.Get(context.Background(), apID); err == nil && ap.State != types.ApprovalPending {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 1 — the run is FAILED, not COMPLETED.
	if got := st.State(); got != types.RunFailed {
		t.Fatalf("an evicted sandbox left the run %q, want FAILED — a pod the kubelet evicted "+
			"never reported an agent exit, and claiming success for it is the one thing "+
			"notFoundExitCode's non-zero value exists to prevent", got)
	}
	if evictedExitCode == 0 {
		t.Fatal("the evicted exit code is 0; the watcher would map it to COMPLETED")
	}

	// 2 — the held sign-in request is CANCELLED, and names the transition.
	ap, err := approvals.Get(context.Background(), apID)
	if err != nil {
		t.Fatalf("read the re-auth request back: %v", err)
	}
	if ap.State != types.ApprovalCancelled {
		t.Fatalf("the held credential_reauth row is %q after an eviction, want CANCELLED — the console "+
			"otherwise keeps asking alice to sign in for a sandbox that no longer exists, until the "+
			"24h sweeper records it as \"nobody answered\"", ap.State)
	}
	if ap.DecidedBy != "system" || ap.Reason != "run_failed" {
		t.Errorf("decided_by/reason = %q/%q, want system/run_failed (the transition that actually won)",
			ap.DecidedBy, ap.Reason)
	}

	// 3 — the REAL approval.cancelled row, once, carrying this run and count 1.
	rows := cancelledRows(audit, runID)
	if len(rows) != 1 {
		t.Fatalf("approval.cancelled rows = %d, want exactly 1 — one run transition is one fact in the "+
			"trail, and an unexplained emptying of the queue is what this row exists to prevent", len(rows))
	}
	var data map[string]any
	if err := json.Unmarshal(rows[0].Data, &data); err != nil {
		t.Fatalf("approval.cancelled data is not JSON: %v", err)
	}
	if data["reason"] != "run_failed" {
		t.Errorf("approval.cancelled reason = %v, want run_failed", data["reason"])
	}
	if n, ok := data["count"].(float64); !ok || int(n) != 1 {
		t.Errorf("approval.cancelled count = %v, want 1", data["count"])
	}

	// 4 — the metric moved by exactly one.
	if after := metricValue(t, srv, `wardyn_credential_reauth_total{outcome="cancelled"}`); after == before {
		t.Errorf("wardyn_credential_reauth_total{outcome=\"cancelled\"} stayed at %s across an eviction "+
			"that cancelled a held request", before)
	} else if before == "0" && after != "1" {
		t.Errorf("cancelled counter = %s after ONE cancelled row, want 1", after)
	}

	// 5 — and the sidecar's door is shut. The hold is still parked on this
	// approval id and will poll it; the read must come back with one of the
	// statuses holdForReauth classifies as TERMINAL (401/403/410,
	// internal/egress/proxy/credhold.go), so the hold ends within one poll
	// instead of running its whole budget against a dead run.
	//
	// WHICH of the three answers is not this test's to fix, and 401 is what wins
	// here: finalizeRunTail runs revokeRunCascade BEFORE cancelRunApprovals, so
	// by the time the sidecar comes back its run token no longer verifies and
	// internalAuth refuses it before refuseTerminalRun ever reads the run. The
	// 403 "run is terminal" is the answer on the narrower path that gate exists
	// for — a run whose revocation write FAILED — and
	// TestInternalAuth_TerminalRunIsRefusedAtEveryDoor owns it, on this very
	// route. Asserting the SET rather than one code is what keeps this pin true
	// whichever of the two closes the door first.
	w := do(t, srv, http.MethodGet, "/api/v1/internal/approvals/"+apID.String(), token, "")
	switch w.Code {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusGone:
	default:
		t.Fatalf("the sidecar's read after the eviction: code = %d, want one of 401/403/410 — a run token "+
			"that outlives its run is the window the revoke cascade and refuseTerminalRun close between "+
			"them, and anything else leaves the hold polling a dead run for its whole budget. body=%s",
			w.Code, w.Body.String())
	}
}
