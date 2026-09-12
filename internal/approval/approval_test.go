// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package approval_test

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/approval"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ─── In-memory fake store ────────────────────────────────────────────────────

type fakeStore struct {
	mu      sync.Mutex
	records []types.ApprovalRequest
	audit   []types.AuditEvent
	// decideErrOn fails the Nth DecideApproval call (1-based) with decideErr,
	// which is how a test reaches a PARTLY applied cascade — some rows durably
	// moved, then the store refused.
	decideCalls int
	decideErrOn int
	decideErr   error
}

func (f *fakeStore) CreateApproval(_ context.Context, a types.ApprovalRequest) (types.ApprovalRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, a)
	return a, nil
}

func (f *fakeStore) GetApproval(_ context.Context, id uuid.UUID) (types.ApprovalRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.records {
		if a.ID == id {
			return a, nil
		}
	}
	return types.ApprovalRequest{}, approval.ErrAlreadyDecided // sentinel not ideal, but unused in current tests
}

func (f *fakeStore) ListApprovals(_ context.Context, stateFilter types.ApprovalState) ([]types.ApprovalRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []types.ApprovalRequest
	for _, a := range f.records {
		if stateFilter == "" || a.State == stateFilter {
			out = append(out, a)
		}
	}
	return out, nil
}

func (f *fakeStore) DecideApproval(_ context.Context, id uuid.UUID, decision types.ApprovalDecision) (types.ApprovalRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decideCalls++
	if f.decideErrOn != 0 && f.decideCalls == f.decideErrOn {
		return types.ApprovalRequest{}, f.decideErr
	}
	for i, a := range f.records {
		if a.ID == id {
			if a.State != types.ApprovalPending {
				return types.ApprovalRequest{}, approval.ErrAlreadyDecided
			}
			now := time.Now().UTC()
			f.records[i].State = decision.State
			f.records[i].DecidedAt = &now
			f.records[i].DecidedBy = decision.DecidedBy
			f.records[i].Reason = decision.Reason
			f.records[i].DecisionScope = decision.Scope
			f.records[i].DecisionExpiresAt = decision.ExpiresAt
			return f.records[i], nil
		}
	}
	return types.ApprovalRequest{}, approval.ErrAlreadyDecided
}

func (f *fakeStore) Record(_ context.Context, ev types.AuditEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.audit = append(f.audit, ev)
	return nil
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func newReq(runID uuid.UUID, kind types.ApprovalKind, scope json.RawMessage) types.ApprovalRequest {
	return types.ApprovalRequest{
		RunID:          runID,
		Kind:           kind,
		RequestedScope: scope,
	}
}

// ─── Tests ───────────────────────────────────────────────────────────────────

func TestRequestApproval_Creates(t *testing.T) {
	ctx := context.Background()
	st := &fakeStore{}
	runID := uuid.New()
	scope := json.RawMessage(`{"host":"api.github.com"}`)

	got, err := approval.RequestApproval(ctx, st, newReq(runID, types.ApprovalEgressDomain, scope))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID == uuid.Nil {
		t.Error("expected non-nil ID")
	}
	if got.State != types.ApprovalPending {
		t.Errorf("want PENDING, got %s", got.State)
	}
	if len(st.records) != 1 {
		t.Errorf("expected 1 record, got %d", len(st.records))
	}
}

func TestRequestApproval_Dedup(t *testing.T) {
	ctx := context.Background()
	st := &fakeStore{}
	runID := uuid.New()
	scope := json.RawMessage(`{"host":"api.github.com"}`)

	first, err := approval.RequestApproval(ctx, st, newReq(runID, types.ApprovalEgressDomain, scope))
	if err != nil {
		t.Fatalf("first request: %v", err)
	}
	// Same run+kind+scope: must return the existing pending approval.
	second, err := approval.RequestApproval(ctx, st, newReq(runID, types.ApprovalEgressDomain, scope))
	if err != nil {
		t.Fatalf("second request: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("dedup failed: got two different IDs %s vs %s", first.ID, second.ID)
	}
	if len(st.records) != 1 {
		t.Errorf("expected exactly 1 record after dedup, got %d", len(st.records))
	}
}

func TestRequestApproval_DifferentScopes_BothCreated(t *testing.T) {
	ctx := context.Background()
	st := &fakeStore{}
	runID := uuid.New()

	scopeA := json.RawMessage(`{"host":"api.github.com"}`)
	scopeB := json.RawMessage(`{"host":"registry.npmjs.org"}`)

	a, err := approval.RequestApproval(ctx, st, newReq(runID, types.ApprovalEgressDomain, scopeA))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	b, err := approval.RequestApproval(ctx, st, newReq(runID, types.ApprovalEgressDomain, scopeB))
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if a.ID == b.ID {
		t.Error("different scopes should produce different approvals")
	}
	if len(st.records) != 2 {
		t.Errorf("expected 2 records, got %d", len(st.records))
	}
}

func TestDecide_Approve(t *testing.T) {
	ctx := context.Background()
	st := &fakeStore{}
	runID := uuid.New()

	ap, _ := approval.RequestApproval(ctx, st, newReq(runID, types.ApprovalCredential, json.RawMessage(`{}`)))
	result, err := approval.Decide(ctx, st, ap.ID, types.ActorHuman, types.ApprovalDecision{
		State: types.ApprovalApproved, DecidedBy: "alice@example.com", Reason: "looks good",
	})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if result.State != types.ApprovalApproved {
		t.Errorf("want APPROVED, got %s", result.State)
	}
	if result.DecidedBy != "alice@example.com" {
		t.Errorf("want decided_by=alice@example.com, got %s", result.DecidedBy)
	}
	// Audit event must have been emitted.
	if len(st.audit) == 0 {
		t.Error("expected at least one audit event")
	}
	if st.audit[0].ActorType != types.ActorHuman {
		t.Errorf("want actor_type=human, got %s", st.audit[0].ActorType)
	}
	if st.audit[0].Action != "approval.decide" {
		t.Errorf("want action=approval.decide, got %s", st.audit[0].Action)
	}
}

// TestDecide_AdminTokenRecordsAsSystem proves the FIX #10 completion: when the
// decider is a bare admin-token caller (ActorSystem, "admin-token"), the
// approval.decide audit event records actor_type=system — NOT human. Auditing a
// tokened decision as a named human would break attribution honesty (invariant
// 4/6): "who approved this credential" must not claim a person when only the
// shared token acted.
func TestDecide_AdminTokenRecordsAsSystem(t *testing.T) {
	ctx := context.Background()
	st := &fakeStore{}
	runID := uuid.New()

	ap, _ := approval.RequestApproval(ctx, st, newReq(runID, types.ApprovalCredential, json.RawMessage(`{}`)))
	if _, err := approval.Decide(ctx, st, ap.ID, types.ActorSystem, types.ApprovalDecision{
		State: types.ApprovalApproved, DecidedBy: "admin-token", Reason: "ok",
	}); err != nil {
		t.Fatalf("decide: %v", err)
	}
	if len(st.audit) == 0 {
		t.Fatal("expected an audit event")
	}
	if st.audit[0].ActorType != types.ActorSystem {
		t.Errorf("want actor_type=system for an admin-token decision, got %s", st.audit[0].ActorType)
	}
	if st.audit[0].Actor != "admin-token" {
		t.Errorf("want actor=admin-token, got %s", st.audit[0].Actor)
	}
}

// TestDecide_AuditDataIncludesRequestedScopeHost is W20-hold-fsm-1's companion
// fix: approval.Decide's audit event now surfaces the approval's own
// requested_scope host at the top level (when it has one), so a SIEM consumer
// can join "who decided this" straight to "which host" without parsing the
// nested requested_scope JSON itself. Fails on base 6d76911, whose audit data
// carries only approval_id/decision/reason.
func TestDecide_AuditDataIncludesRequestedScopeHost(t *testing.T) {
	ctx := context.Background()
	st := &fakeStore{}
	runID := uuid.New()

	scope := json.RawMessage(`{"host":"api.github.com","mode":"deny_with_review"}`)
	ap, _ := approval.RequestApproval(ctx, st, newReq(runID, types.ApprovalEgressDomain, scope))
	if _, err := approval.Decide(ctx, st, ap.ID, types.ActorHuman, types.ApprovalDecision{
		State: types.ApprovalApproved, DecidedBy: "alice@example.com", Reason: "ok",
	}); err != nil {
		t.Fatalf("decide: %v", err)
	}
	if len(st.audit) == 0 {
		t.Fatal("expected an audit event")
	}
	var data struct {
		Host string `json:"host"`
	}
	if err := json.Unmarshal(st.audit[0].Data, &data); err != nil {
		t.Fatalf("decode audit data: %v (%s)", err, st.audit[0].Data)
	}
	if data.Host != "api.github.com" {
		t.Errorf("audit data host = %q, want api.github.com", data.Host)
	}
}

// TestDecide_AuditDataOmitsHostWhenScopeHasNone proves the host surfacing is
// best-effort: a kind whose scope carries no "host" key (e.g. a bare
// credential scope) must not inject a spurious empty field.
func TestDecide_AuditDataOmitsHostWhenScopeHasNone(t *testing.T) {
	ctx := context.Background()
	st := &fakeStore{}
	runID := uuid.New()

	ap, _ := approval.RequestApproval(ctx, st, newReq(runID, types.ApprovalCredential, json.RawMessage(`{}`)))
	if _, err := approval.Decide(ctx, st, ap.ID, types.ActorHuman, types.ApprovalDecision{
		State: types.ApprovalApproved, DecidedBy: "alice", Reason: "ok",
	}); err != nil {
		t.Fatalf("decide: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(st.audit[0].Data, &data); err != nil {
		t.Fatalf("decode audit data: %v", err)
	}
	if _, ok := data["host"]; ok {
		t.Errorf("audit data must not carry an empty host key when the scope has none: %s", st.audit[0].Data)
	}
}

func TestDecide_Deny(t *testing.T) {
	ctx := context.Background()
	st := &fakeStore{}
	runID := uuid.New()

	ap, _ := approval.RequestApproval(ctx, st, newReq(runID, types.ApprovalCredential, json.RawMessage(`{}`)))
	result, err := approval.Decide(ctx, st, ap.ID, types.ActorHuman, types.ApprovalDecision{
		State: types.ApprovalDenied, DecidedBy: "bob@example.com", Reason: "too risky",
	})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if result.State != types.ApprovalDenied {
		t.Errorf("want DENIED, got %s", result.State)
	}
}

func TestDecide_AlreadyDecided(t *testing.T) {
	ctx := context.Background()
	st := &fakeStore{}
	runID := uuid.New()

	ap, _ := approval.RequestApproval(ctx, st, newReq(runID, types.ApprovalCredential, json.RawMessage(`{}`)))
	_, _ = approval.Decide(ctx, st, ap.ID, types.ActorHuman, types.ApprovalDecision{
		State: types.ApprovalApproved, DecidedBy: "alice", Reason: "ok",
	})
	// Second decision on the same approval must fail.
	_, err := approval.Decide(ctx, st, ap.ID, types.ActorHuman, types.ApprovalDecision{
		State: types.ApprovalDenied, DecidedBy: "bob", Reason: "changed my mind",
	})
	if !isAlreadyDecided(err) {
		t.Errorf("expected ErrAlreadyDecided, got %v", err)
	}
}

func TestExpireStale(t *testing.T) {
	ctx := context.Background()
	st := &fakeStore{}
	runID := uuid.New()

	// Create two approvals: one old, one recent.
	old, _ := approval.RequestApproval(ctx, st, newReq(runID, types.ApprovalEgressDomain, json.RawMessage(`{"host":"old.example.com"}`)))
	// Back-date the old approval by manually setting its requested_at.
	for i, r := range st.records {
		if r.ID == old.ID {
			st.records[i].RequestedAt = time.Now().UTC().Add(-10 * time.Hour)
		}
	}

	_, _ = approval.RequestApproval(ctx, st, newReq(runID, types.ApprovalEgressDomain, json.RawMessage(`{"host":"new.example.com"}`)))

	expired, err := approval.ExpireStale(ctx, st, 5*time.Hour)
	if err != nil {
		t.Fatalf("expire stale: %v", err)
	}
	if expired != 1 {
		t.Errorf("expected 1 expired, got %d", expired)
	}

	// Check the audit log.
	var expireEvents int
	for _, ev := range st.audit {
		if ev.Action == "approval.expire" {
			expireEvents++
		}
	}
	if expireEvents != 1 {
		t.Errorf("expected 1 expire audit event, got %d", expireEvents)
	}
}

func TestExpireStale_AlreadyDecidedRace(t *testing.T) {
	// If a concurrent Decide wins, ExpireStale should not error.
	ctx := context.Background()
	st := &fakeStore{}
	runID := uuid.New()

	ap, _ := approval.RequestApproval(ctx, st, newReq(runID, types.ApprovalEgressDomain, json.RawMessage(`{"host":"x.com"}`)))
	// Back-date.
	for i := range st.records {
		st.records[i].RequestedAt = time.Now().UTC().Add(-10 * time.Hour)
	}
	// Approve concurrently.
	_, _ = approval.Decide(ctx, st, ap.ID, types.ActorHuman, types.ApprovalDecision{
		State: types.ApprovalApproved, DecidedBy: "human", Reason: "ok",
	})

	// ExpireStale should skip the already-decided record gracefully.
	expired, err := approval.ExpireStale(ctx, st, 5*time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Zero expired because the approval was already decided.
	if expired != 0 {
		t.Errorf("expected 0 expired, got %d", expired)
	}
}

// ─── CancelForRun (B4: a run's terminal transition ends its open questions) ──

// TestCancelForRun_MovesOnlyThisRunsPending is the whole contract in one drive:
// only PENDING rows move, only this run's, they land on CANCELLED with
// decided_by=system and the transition as the reason, and the batch emits ONE
// approval.cancelled audit row carrying the count.
func TestCancelForRun_MovesOnlyThisRunsPending(t *testing.T) {
	ctx := context.Background()
	st := &fakeStore{}
	killed, other := uuid.New(), uuid.New()

	a1, _ := approval.RequestApproval(ctx, st, newReq(killed, types.ApprovalEgressDomain, json.RawMessage(`{"host":"a.example.com"}`)))
	a2, _ := approval.RequestApproval(ctx, st, newReq(killed, types.ApprovalToolCall, json.RawMessage(`{"tool":"Bash"}`)))
	decided, _ := approval.RequestApproval(ctx, st, newReq(killed, types.ApprovalEgressDomain, json.RawMessage(`{"host":"decided.example.com"}`)))
	foreign, _ := approval.RequestApproval(ctx, st, newReq(other, types.ApprovalEgressDomain, json.RawMessage(`{"host":"b.example.com"}`)))
	if _, err := approval.Decide(ctx, st, decided.ID, types.ActorHuman, types.ApprovalDecision{
		State: types.ApprovalApproved, DecidedBy: "human", Reason: "ok",
	}); err != nil {
		t.Fatalf("seed decide: %v", err)
	}

	n, err := approval.CancelForRun(ctx, st, killed, "run_killed")
	if err != nil {
		t.Fatalf("cancel for run: %v", err)
	}
	if n != 2 {
		t.Fatalf("cancelled = %d, want 2 (the run's two PENDING rows)", n)
	}
	byID := map[uuid.UUID]types.ApprovalRequest{}
	for _, r := range st.records {
		byID[r.ID] = r
	}
	for _, id := range []uuid.UUID{a1.ID, a2.ID} {
		got := byID[id]
		if got.State != types.ApprovalCancelled {
			t.Errorf("approval %s state = %q, want CANCELLED", id, got.State)
		}
		if got.DecidedBy != "system" {
			t.Errorf("approval %s decided_by = %q, want system — nobody decided it", id, got.DecidedBy)
		}
		if got.Reason != "run_killed" {
			t.Errorf("approval %s reason = %q, want run_killed", id, got.Reason)
		}
		if got.DecisionScope != "" {
			t.Errorf("approval %s decision_scope = %q; a cancellation authorizes nothing", id, got.DecisionScope)
		}
	}
	if got := byID[decided.ID].State; got != types.ApprovalApproved {
		t.Errorf("an already-APPROVED row became %q — a decision must never be overwritten", got)
	}
	if got := byID[foreign.ID].State; got != types.ApprovalPending {
		t.Errorf("another run's PENDING row became %q — cancellation is run-scoped", got)
	}

	var evs []types.AuditEvent
	for _, ev := range st.audit {
		if ev.Action == "approval.cancelled" {
			evs = append(evs, ev)
		}
	}
	if len(evs) != 1 {
		t.Fatalf("approval.cancelled rows = %d, want exactly 1 for the batch", len(evs))
	}
	if evs[0].ActorType != types.ActorSystem {
		t.Errorf("actor_type = %q, want system", evs[0].ActorType)
	}
	if evs[0].RunID == nil || *evs[0].RunID != killed {
		t.Errorf("the row must be keyed to the run so it lands in its evidence rail; run_id = %v", evs[0].RunID)
	}
	var data struct {
		RunID  string `json:"run_id"`
		Reason string `json:"reason"`
		Count  int    `json:"count"`
	}
	if err := json.Unmarshal(evs[0].Data, &data); err != nil {
		t.Fatalf("decode audit data: %v", err)
	}
	if data.Count != 2 || data.Reason != "run_killed" || data.RunID != killed.String() {
		t.Errorf("audit data = %+v, want {run_id:%s reason:run_killed count:2}", data, killed)
	}
}

// TestCancelForRun_IdempotentAndSilentWithNothingPending pins the re-kill path:
// a second cancellation finds nothing PENDING, moves nothing and — the part
// that matters for the append-only log — writes NO audit row at all.
func TestCancelForRun_IdempotentAndSilentWithNothingPending(t *testing.T) {
	ctx := context.Background()
	st := &fakeStore{}
	runID := uuid.New()
	_, _ = approval.RequestApproval(ctx, st, newReq(runID, types.ApprovalEgressDomain, json.RawMessage(`{"host":"a.example.com"}`)))

	if n, err := approval.CancelForRun(ctx, st, runID, "run_killed"); err != nil || n != 1 {
		t.Fatalf("first cancel = (%d, %v), want (1, nil)", n, err)
	}
	before := len(st.audit)
	n, err := approval.CancelForRun(ctx, st, runID, "run_killed")
	if err != nil {
		t.Fatalf("second cancel: %v", err)
	}
	if n != 0 {
		t.Errorf("second cancel moved %d rows, want 0 — a re-kill must be a no-op", n)
	}
	if len(st.audit) != before {
		t.Errorf("second cancel wrote %d new audit rows, want 0", len(st.audit)-before)
	}
}

// TestCancelForRun_PartialFailureStillRecordsWhatMoved is the trail's half of the
// cascade. A store error part-way through leaves the approvals it already moved
// durably CANCELLED — the operator's queue empties — so returning the error
// before the audit block meant exactly the unexplained emptying this row exists
// to explain. The row goes out with the PARTIAL count, outcome=failure and the
// error, and the caller still gets the failure.
func TestCancelForRun_PartialFailureStillRecordsWhatMoved(t *testing.T) {
	ctx := context.Background()
	st := &fakeStore{decideErrOn: 2, decideErr: errors.New("store: connection reset")}
	runID := uuid.New()
	for i := range 3 {
		_, _ = approval.RequestApproval(ctx, st, newReq(runID, types.ApprovalEgressDomain,
			json.RawMessage(`{"host":"h`+strconv.Itoa(i)+`.example.com"}`)))
	}

	n, err := approval.CancelForRun(ctx, st, runID, "run_killed")
	if err == nil {
		t.Fatal("a refused DecideApproval must be surfaced, not swallowed")
	}
	if n != 1 {
		t.Fatalf("cancelled = %d, want 1 (the row that moved before the store refused)", n)
	}
	var evs []types.AuditEvent
	for _, ev := range st.audit {
		if ev.Action == "approval.cancelled" {
			evs = append(evs, ev)
		}
	}
	if len(evs) != 1 {
		t.Fatalf("approval.cancelled rows = %d, want 1 — one approval is durably CANCELLED, so the trail must "+
			"say who emptied it", len(evs))
	}
	if evs[0].Outcome != "failure" {
		t.Errorf("outcome = %q, want failure — the count is partial and the row must say so", evs[0].Outcome)
	}
	var data struct {
		Count int    `json:"count"`
		Error string `json:"error"`
	}
	if uerr := json.Unmarshal(evs[0].Data, &data); uerr != nil {
		t.Fatalf("decode audit data: %v", uerr)
	}
	if data.Count != 1 {
		t.Errorf("audit count = %d, want 1 (what actually moved)", data.Count)
	}
	if data.Error == "" {
		t.Error("the failure row carries no error field; the partial count alone reads as a bug rather than an " +
			"interrupted cascade")
	}
}

// TestCancelForRun_AFailureBeforeAnythingMovedRecordsNothing: the other side of
// the same rule. Nothing moved, so there is no state change to explain — a row
// here would claim a cancellation that never happened.
func TestCancelForRun_AFailureBeforeAnythingMovedRecordsNothing(t *testing.T) {
	ctx := context.Background()
	st := &fakeStore{decideErrOn: 1, decideErr: errors.New("store: connection reset")}
	runID := uuid.New()
	_, _ = approval.RequestApproval(ctx, st, newReq(runID, types.ApprovalEgressDomain, json.RawMessage(`{"host":"h.example.com"}`)))

	n, err := approval.CancelForRun(ctx, st, runID, "run_killed")
	if err == nil || n != 0 {
		t.Fatalf("CancelForRun = (%d, %v), want (0, an error)", n, err)
	}
	for _, ev := range st.audit {
		if ev.Action == "approval.cancelled" {
			t.Errorf("a cascade that moved nothing emitted %+v", ev)
		}
	}
}

// TestExpireStale_LeavesCancelledAlone: the stale sweeper reads PENDING only, so
// a cancelled row is out of its reach by construction. Pinned because the
// alternative — a sweeper that re-decides a terminal row — would rewrite the
// reason an operator reads on the queue AND emit an approval.expire row for a
// question the run's own end had already closed.
func TestExpireStale_LeavesCancelledAlone(t *testing.T) {
	ctx := context.Background()
	st := &fakeStore{}
	runID := uuid.New()
	ap, _ := approval.RequestApproval(ctx, st, newReq(runID, types.ApprovalEgressDomain, json.RawMessage(`{"host":"a.example.com"}`)))
	for i := range st.records {
		st.records[i].RequestedAt = time.Now().UTC().Add(-10 * time.Hour)
	}
	if _, err := approval.CancelForRun(ctx, st, runID, "run_completed"); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	expired, err := approval.ExpireStale(ctx, st, 5*time.Hour)
	if err != nil {
		t.Fatalf("expire stale: %v", err)
	}
	if expired != 0 {
		t.Errorf("expired = %d, want 0 — a CANCELLED row is not PENDING", expired)
	}
	for _, r := range st.records {
		if r.ID == ap.ID && (r.State != types.ApprovalCancelled || r.Reason != "run_completed") {
			t.Errorf("the sweeper rewrote a cancelled row: state=%q reason=%q", r.State, r.Reason)
		}
	}
	for _, ev := range st.audit {
		if ev.Action == "approval.expire" {
			t.Error("the sweeper emitted approval.expire over an already-cancelled approval")
		}
	}
}

// ─── Helper ──────────────────────────────────────────────────────────────────

func isAlreadyDecided(err error) bool {
	return err != nil && err == approval.ErrAlreadyDecided
}
