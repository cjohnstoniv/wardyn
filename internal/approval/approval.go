// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package approval implements the ApprovalRequest FSM service.
//
// States:   PENDING -> APPROVED | DENIED | EXPIRED | CANCELLED
//
// Transitions are single-direction and fail-closed: any attempt to decide
// an already-decided approval returns ErrAlreadyDecided. Every state
// change emits an audit event with actor_type=human (for decisions) or
// actor_type=system (for expirations and for the terminal-run cancellation
// cascade, CancelForRun).
//
// Pure business logic: storage is injected via the Store interface so this
// package can be tested with an in-memory fake.
package approval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ErrAlreadyDecided is re-exported here for callers that import only this
// package. It IS the store's sentinel — one value, defined in internal/types —
// so errors.Is matches whichever layer raised it.
var ErrAlreadyDecided = types.ErrApprovalAlreadyDecided

// Store is the narrow persistence interface the approval FSM needs.
// The real implementation is internal/store; tests supply a fake.
type Store interface {
	// CreateApproval persists a new PENDING approval and returns it.
	CreateApproval(ctx context.Context, a types.ApprovalRequest) (types.ApprovalRequest, error)
	// GetApproval fetches an approval by id.
	GetApproval(ctx context.Context, id uuid.UUID) (types.ApprovalRequest, error)
	// ListApprovals returns approvals filtered by state (empty = all).
	ListApprovals(ctx context.Context, stateFilter types.ApprovalState) ([]types.ApprovalRequest, error)
	// DecideApproval transitions state from PENDING to decision.State; returns
	// ErrAlreadyDecided if the approval is not PENDING.
	DecideApproval(ctx context.Context, id uuid.UUID, decision types.ApprovalDecision) (types.ApprovalRequest, error)
	// Record appends an audit event (approval.decide, approval.expire).
	Record(ctx context.Context, ev types.AuditEvent) error
}

// RequestApproval creates a new PENDING approval, or returns the existing
// PENDING approval when one already exists for the same run+kind+scope hash
// (deduplication guard).
func RequestApproval(ctx context.Context, st Store, req types.ApprovalRequest) (types.ApprovalRequest, error) {
	// Dedup: return any existing PENDING approval for the same run+kind+scope.
	hash := scopeHash(req.RunID, req.Kind, req.RequestedScope)
	if existing, found, err := findPendingDup(ctx, st, req, hash); err != nil || found {
		return existing, err
	}

	// Normalise fields before persisting.
	if req.ID == uuid.Nil {
		req.ID = uuid.New()
	}
	req.State = types.ApprovalPending
	req.RequestedAt = time.Now().UTC()
	req.DecidedAt = nil
	req.DecidedBy = ""
	req.MintedJTI = ""
	req.Reason = ""
	// A freshly-raised approval has no decision, so it has no scope either —
	// this is the chokepoint the "a compromised sidecar cannot pre-seed the
	// scope" claim actually rests on (handleInternalRequestApproval decodes
	// only {Kind, RequestedScope} into a fresh struct, but this zeroing is
	// what makes that safe by CONSTRUCTION rather than by accident of which
	// Store backs this call).
	req.DecisionScope = ""
	req.DecisionExpiresAt = nil

	created, err := st.CreateApproval(ctx, req)
	if err != nil {
		// The list-then-create dedup above has a race window: a concurrent raise of
		// the same run+kind+scope can slip in between our list and our insert. The
		// partial unique index (migration 0022 for non-credential kinds, 0002 for
		// credential) rejects the loser, surfacing as store.ErrDuplicatePending.
		// Tolerate it as a dedup signal — re-read and return the winner's row —
		// exactly as if the pre-insert scan had seen it.
		if errors.Is(err, types.ErrDuplicatePendingApproval) {
			if existing, found, rerr := findPendingDup(ctx, st, req, hash); rerr != nil || found {
				return existing, rerr
			}
		}
		return types.ApprovalRequest{}, fmt.Errorf("approval: create: %w", err)
	}
	return created, nil
}

// findPendingDup returns the existing PENDING approval matching req's
// run+kind+scope hash, if any. Shared by the pre-insert dedup scan and the
// post-unique-violation re-read.
func findPendingDup(ctx context.Context, st Store, req types.ApprovalRequest, hash string) (types.ApprovalRequest, bool, error) {
	pending, err := st.ListApprovals(ctx, types.ApprovalPending)
	if err != nil {
		return types.ApprovalRequest{}, false, fmt.Errorf("approval: list pending for dedup: %w", err)
	}
	for _, existing := range pending {
		if existing.RunID == req.RunID && existing.Kind == req.Kind &&
			scopeHash(existing.RunID, existing.Kind, existing.RequestedScope) == hash {
			return existing, true, nil
		}
	}
	return types.ApprovalRequest{}, false, nil
}

// Decide transitions an existing approval request to decision.State (which
// the caller sets — APPROVED or DENIED; ExpireStale below bypasses Decide
// entirely for EXPIRED, since a sweep is not a decision). decision.DecidedBy
// is the principal that decided and decidedByType is its actor type (human
// for an OIDC session or a LocalMode operator; system for a bare admin-token
// caller). The audit event records that exact type so an admin-token decision
// is not mislabelled as a human approval (invariant 4/6 attribution honesty).
func Decide(ctx context.Context, st Store, id uuid.UUID, decidedByType types.ActorType, decision types.ApprovalDecision) (types.ApprovalRequest, error) {
	result, err := st.DecideApproval(ctx, id, decision)
	if err != nil {
		// Return the bare sentinel so callers only need to import this package.
		if errors.Is(err, ErrAlreadyDecided) {
			return types.ApprovalRequest{}, ErrAlreadyDecided
		}
		return types.ApprovalRequest{}, fmt.Errorf("approval: decide: %w", err)
	}

	data := map[string]any{
		"approval_id": id,
		"decision":    string(decision.State),
		"reason":      decision.Reason,
	}
	// Self-joining SIEM stream (W20-hold-fsm-1's companion): surface the
	// approval's own requested-scope host at the top level, when it has one, so
	// a consumer of this event never has to parse the nested requested_scope
	// JSON to learn which host a human just approved/denied. Best-effort — a
	// kind whose scope carries no "host" (credential approvals commonly do,
	// tool_call approvals may not) simply omits the key rather than adding an
	// empty one.
	if host := requestedScopeHost(result.RequestedScope); host != "" {
		data["host"] = host
	}
	// The decision's BLAST RADIUS, emitted raw (never Normalize()d): "" means
	// "no scope recorded", and asserting `run` for a decision nobody scoped is
	// exactly what the empty-string column exists to avoid. Present
	// unconditionally, like "reason" above, so a SIEM consumer can key on it —
	// a permanent grant must be readable from the audit stream ALONE, which is
	// what the demo harness's publish checklist verifies.
	data["decision_scope"] = string(decision.Scope)
	if decision.ExpiresAt != nil {
		data["decision_expires_at"] = decision.ExpiresAt.UTC()
	}
	auditData, _ := json.Marshal(data)
	ev := types.AuditEvent{
		ID:        uuid.New(),
		Time:      time.Now().UTC(),
		RunID:     &result.RunID,
		ActorType: decidedByType,
		Actor:     decision.DecidedBy,
		Action:    "approval.decide",
		Target:    id.String(),
		Outcome:   "success",
		Data:      json.RawMessage(auditData),
	}
	// FIX #5: the audit log is the system of record — do NOT silently swallow a
	// failed decide audit. Log loudly (matching Server.recordAudit's intent) so a
	// dropped approval.decide event is visible. The write does not shadow the
	// primary return value (the decision itself already succeeded and is durable).
	if err := st.Record(ctx, ev); err != nil {
		audit.LogWriteFailure(ctx, ev, err)
	}

	return result, nil
}

// requestedScopeHost best-effort-extracts the "host" field from an approval's
// RequestedScope JSON — present on an egress_domain scope (egressScope,
// internal/egress/proxy/approvals.go) and on the api_key credential scopes
// planArtifactRedirect/authorBedrockBearerInjection author, absent on a
// tool_call scope or malformed/empty JSON, in which case it returns "".
func requestedScopeHost(scope json.RawMessage) string {
	var s struct {
		Host string `json:"host"`
	}
	if err := json.Unmarshal(scope, &s); err != nil {
		return ""
	}
	return s.Host
}

// ExpireStale transitions all PENDING approvals that were requested before
// the cutoff (time.Now().UTC().Add(-olderThan)) to EXPIRED and emits one
// audit event per expiration. Returns the number of approvals expired.
func ExpireStale(ctx context.Context, st Store, olderThan time.Duration) (int, error) {
	n, _, err := ExpireStaleByKind(ctx, st, olderThan)
	return n, err
}

// ExpireStaleByKind is ExpireStale plus a per-KIND tally of what it moved.
//
// It exists because a metric that counts a state transition has to be
// incremented WHERE THE STATE CHANGES: bumping the credential re-auth counter
// at a later resolve that happens to meet a terminal row would count retries
// rather than outcomes — with the measured ~30 s SDK cadence one aged-out
// request would score dozens of "expired" — and would never fire at all for a
// run whose sidecar had already given up. A separate function rather than a
// changed signature: every existing caller asks the question it always asked.
func ExpireStaleByKind(ctx context.Context, st Store, olderThan time.Duration) (int, map[types.ApprovalKind]int, error) {
	cutoff := time.Now().UTC().Add(-olderThan)

	pending, err := st.ListApprovals(ctx, types.ApprovalPending)
	if err != nil {
		return 0, nil, fmt.Errorf("approval: list for expiry: %w", err)
	}

	expired := 0
	byKind := map[types.ApprovalKind]int{}
	// Collected, not returned on the first failure: ListApprovals is ordered
	// (ORDER BY requested_at DESC in the store) and the sweeper re-lists in the
	// same order every tick, so returning on the first permanently failing
	// PENDING row would abort the sweep at the same position every time —
	// stranding every approval sorted after it, fleet-wide, with no expiry ever
	// reaching them. The sweep instead expires what it can and reports every row
	// it could not, the shape FSStore.Sweep already uses.
	var failures []error
	for _, ap := range pending {
		if ap.RequestedAt.After(cutoff) {
			continue
		}
		// Scope is left at its zero value ("", not ScopeRun): an expiry is a
		// sweep nobody decided, not a decision — see types.ApprovalDecision.
		if _, err := st.DecideApproval(ctx, ap.ID, types.ApprovalDecision{
			State: types.ApprovalExpired, DecidedBy: "system", Reason: "stale",
		}); err != nil {
			if errors.Is(err, ErrAlreadyDecided) {
				// Race with a concurrent Decide — not an error, and deliberately
				// not collected either: on a busy deployment every sweep would
				// otherwise report a failure it did not have.
				continue
			}
			failures = append(failures, fmt.Errorf("approval: expire %s: %w", ap.ID, err))
			continue
		}
		expired++
		byKind[ap.Kind]++

		auditData, _ := json.Marshal(map[string]any{
			"approval_id": ap.ID,
			"cutoff":      cutoff,
		})
		ev := types.AuditEvent{
			ID:        uuid.New(),
			Time:      time.Now().UTC(),
			RunID:     &ap.RunID,
			ActorType: types.ActorSystem,
			Actor:     "wardyn/approval-sweeper",
			Action:    "approval.expire",
			Target:    ap.ID.String(),
			Outcome:   "success",
			Data:      json.RawMessage(auditData),
		}
		// FIX #5: log-loud instead of swallowing — a dropped approval.expire audit
		// must be visible, not silently lost.
		if err := st.Record(ctx, ev); err != nil {
			audit.LogWriteFailure(ctx, ev, err)
		}
	}
	return expired, byKind, errors.Join(failures...)
}

// CancelForRun transitions every still-PENDING approval belonging to runID to
// CANCELLED and returns how many it moved. reason is the transition that ended
// the run ("run_killed", "run_completed", "run_failed", "run_stopped"), recorded
// on each row and on the ONE audit event this emits.
//
// It is ExpireStale's shape deliberately: list PENDING, then move each row with
// the SAME CAS primitive a human decision uses (DecideApproval is
// WHERE state='PENDING', which IS the FSM guard), so a human deciding
// concurrently with the kill cascade wins and is never overwritten —
// ErrAlreadyDecided is a race, not an error. DecidedBy is "system" for the same
// reason the sweeper's is: nobody decided this, the run ended.
//
// ONE audit row for the batch, carrying the count, rather than one per approval:
// unlike an expiry sweep (whose rows are independent events spread over days),
// these all belong to a single run transition an operator reads as one fact, and
// the row is keyed to the run so it lands in that run's evidence rail. A run
// with nothing pending emits nothing at all, which is what makes a re-kill
// idempotent in the audit log as well as in the store.
//
// The row is emitted on the failure path too, carrying however many rows DID
// move plus the error: returning the CAS failure before reaching the audit
// block would leave a cancel that moved two approvals and then hit a store
// error with two rows durably CANCELLED and nothing in the trail — exactly the
// unexplained emptying of the operator's queue this row exists to explain, and
// the reason ExpireStale records inside its own loop rather than after it.
func CancelForRun(ctx context.Context, st Store, runID uuid.UUID, reason string) (int, error) {
	pending, err := st.ListApprovals(ctx, types.ApprovalPending)
	if err != nil {
		return 0, fmt.Errorf("approval: list for cancel: %w", err)
	}
	cancelled := 0
	// recordCancelled writes the ONE summary row for whatever this call actually
	// moved. failure is nil on the clean path and the CAS error on the partial
	// one; either way a call that moved NOTHING writes nothing, because there is
	// no state change to explain.
	recordCancelled := func(failure error) {
		if cancelled == 0 {
			return
		}
		data := map[string]any{
			"run_id": runID,
			"reason": reason,
			"count":  cancelled,
		}
		outcome := "success"
		if failure != nil {
			// The count is now a PARTIAL one, and saying so is the point: an
			// operator reading "3 cancelled" against a queue that still holds two
			// would have no way to tell a bug from an interrupted cascade.
			outcome = "failure"
			data["error"] = failure.Error()
		}
		auditData, _ := json.Marshal(data)
		ev := types.AuditEvent{
			ID:        uuid.New(),
			Time:      time.Now().UTC(),
			RunID:     &runID,
			ActorType: types.ActorSystem,
			Actor:     "wardyn/approval-cancel",
			Action:    "approval.cancelled",
			Target:    runID.String(),
			Outcome:   outcome,
			Data:      json.RawMessage(auditData),
		}
		// Log-loud instead of swallowing, same rule as the expiry sweep: a dropped
		// approval.cancelled row would leave the queue's emptying unexplained.
		if rerr := st.Record(ctx, ev); rerr != nil {
			audit.LogWriteFailure(ctx, ev, rerr)
		}
	}
	for _, ap := range pending {
		if ap.RunID != runID {
			continue
		}
		// Scope is left at its zero value ("", not ScopeRun), exactly as an
		// expiry does: a cancellation is not a decision, so it authorizes
		// nothing for the rest of the run — see types.ApprovalDecision.
		if _, derr := st.DecideApproval(ctx, ap.ID, types.ApprovalDecision{
			State: types.ApprovalCancelled, DecidedBy: "system", Reason: reason,
		}); derr != nil {
			if errors.Is(derr, ErrAlreadyDecided) {
				// A human decided it between the list and the CAS — their
				// decision stands.
				continue
			}
			cerr := fmt.Errorf("approval: cancel %s: %w", ap.ID, derr)
			recordCancelled(cerr)
			return cancelled, cerr
		}
		cancelled++
	}
	recordCancelled(nil)
	return cancelled, nil
}

// scopeHash produces a stable content hash over (runID, kind, scope) for
// deduplication. The scope JSON is re-encoded to normalise key ordering.
func scopeHash(runID uuid.UUID, kind types.ApprovalKind, scope json.RawMessage) string {
	// Normalise scope JSON: unmarshal/remarshal to sort keys.
	var raw any
	if err := json.Unmarshal(scope, &raw); err != nil {
		// If scope is not valid JSON, use the raw bytes directly.
		raw = string(scope)
	}
	norm, _ := json.Marshal(raw)

	h := sha256.New()
	h.Write([]byte(runID.String()))
	h.Write([]byte("|"))
	h.Write([]byte(kind))
	h.Write([]byte("|"))
	h.Write(norm)
	return hex.EncodeToString(h.Sum(nil))
}
