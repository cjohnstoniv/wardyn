// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package approval implements the ApprovalRequest FSM service.
//
// States:   PENDING -> APPROVED | DENIED | EXPIRED
//
// Transitions are single-direction and fail-closed: any attempt to decide
// an already-decided approval returns ErrAlreadyDecided. Every state
// change emits an audit event with actor_type=human (for decisions) or
// actor_type=system (for expirations).
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
	"log"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ErrAlreadyDecided is re-exported here for callers that import only this
// package. The underlying store returns the same sentinel.
var ErrAlreadyDecided = errors.New("approval: already decided")

// Store is the narrow persistence interface the approval FSM needs.
// The real implementation is internal/store; tests supply a fake.
type Store interface {
	// CreateApproval persists a new PENDING approval and returns it.
	CreateApproval(ctx context.Context, a types.ApprovalRequest) (types.ApprovalRequest, error)
	// GetApproval fetches an approval by id.
	GetApproval(ctx context.Context, id uuid.UUID) (types.ApprovalRequest, error)
	// ListApprovals returns approvals filtered by state (empty = all).
	ListApprovals(ctx context.Context, stateFilter types.ApprovalState) ([]types.ApprovalRequest, error)
	// DecideApproval transitions state from PENDING; returns ErrAlreadyDecided
	// if the approval is not PENDING.
	DecideApproval(ctx context.Context, id uuid.UUID, state types.ApprovalState, decidedBy, reason string) (types.ApprovalRequest, error)
	// Record appends an audit event (approval.decide, approval.expire).
	Record(ctx context.Context, ev types.AuditEvent) error
}

// RequestApproval creates a new PENDING approval, or returns the existing
// PENDING approval when one already exists for the same run+kind+scope hash
// (deduplication guard).
func RequestApproval(ctx context.Context, st Store, req types.ApprovalRequest) (types.ApprovalRequest, error) {
	// Dedup: find any existing PENDING approval for the same run+kind+scope.
	hash := scopeHash(req.RunID, req.Kind, req.RequestedScope)

	pending, err := st.ListApprovals(ctx, types.ApprovalPending)
	if err != nil {
		return types.ApprovalRequest{}, fmt.Errorf("approval: list pending for dedup: %w", err)
	}
	for _, existing := range pending {
		if existing.RunID == req.RunID && existing.Kind == req.Kind {
			if scopeHash(existing.RunID, existing.Kind, existing.RequestedScope) == hash {
				return existing, nil
			}
		}
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

	created, err := st.CreateApproval(ctx, req)
	if err != nil {
		return types.ApprovalRequest{}, fmt.Errorf("approval: create: %w", err)
	}
	return created, nil
}

// Decide approves or denies an existing approval request. decidedBy is the
// principal that decided and decidedByType is its actor type (human for an OIDC
// session or a LocalMode operator; system for a bare admin-token caller). The
// audit event records that exact type so an admin-token decision is not
// mislabelled as a human approval (invariant 4/6 attribution honesty).
func Decide(ctx context.Context, st Store, id uuid.UUID, approve bool, decidedByType types.ActorType, decidedBy, reason string) (types.ApprovalRequest, error) {
	newState := types.ApprovalDenied
	if approve {
		newState = types.ApprovalApproved
	}

	result, err := st.DecideApproval(ctx, id, newState, decidedBy, reason)
	if err != nil {
		// Translate store sentinel so callers only need to import this package.
		if isAlreadyDecided(err) {
			return types.ApprovalRequest{}, ErrAlreadyDecided
		}
		return types.ApprovalRequest{}, fmt.Errorf("approval: decide: %w", err)
	}

	outcome := "success"
	action := "approval.decide"
	auditData, _ := json.Marshal(map[string]any{
		"approval_id": id,
		"decision":    string(newState),
		"reason":      reason,
	})
	ev := types.AuditEvent{
		ID:        uuid.New(),
		Time:      time.Now().UTC(),
		RunID:     &result.RunID,
		ActorType: decidedByType,
		Actor:     decidedBy,
		Action:    action,
		Target:    id.String(),
		Outcome:   outcome,
		Data:      json.RawMessage(auditData),
	}
	// FIX #5: the audit log is the system of record — do NOT silently swallow a
	// failed decide audit. Log loudly (matching Server.recordAudit's intent) so a
	// dropped approval.decide event is visible. The write does not shadow the
	// primary return value (the decision itself already succeeded and is durable).
	if err := st.Record(ctx, ev); err != nil {
		log.Printf("wardyn: AUDIT WRITE FAILED action=%s target=%s outcome=%s: %v", ev.Action, ev.Target, ev.Outcome, err)
	}

	return result, nil
}

// ExpireStale transitions all PENDING approvals that were requested before
// the cutoff (time.Now().UTC().Add(-olderThan)) to EXPIRED and emits one
// audit event per expiration. Returns the number of approvals expired.
func ExpireStale(ctx context.Context, st Store, olderThan time.Duration) (int, error) {
	cutoff := time.Now().UTC().Add(-olderThan)

	pending, err := st.ListApprovals(ctx, types.ApprovalPending)
	if err != nil {
		return 0, fmt.Errorf("approval: list for expiry: %w", err)
	}

	expired := 0
	for _, ap := range pending {
		if ap.RequestedAt.After(cutoff) {
			continue
		}
		if _, err := st.DecideApproval(ctx, ap.ID, types.ApprovalExpired, "system", "stale"); err != nil {
			if isAlreadyDecided(err) {
				// Race with a concurrent Decide — not an error.
				continue
			}
			return expired, fmt.Errorf("approval: expire %s: %w", ap.ID, err)
		}
		expired++

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
			log.Printf("wardyn: AUDIT WRITE FAILED action=%s target=%s outcome=%s: %v", ev.Action, ev.Target, ev.Outcome, err)
		}
	}
	return expired, nil
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

// isAlreadyDecided detects the ErrAlreadyDecided sentinel from the store layer
// without importing the store package (avoids an import cycle since the store
// also uses approval types). We match by message string as the two error
// values are defined in separate packages.
func isAlreadyDecided(err error) bool {
	return err != nil && (errors.Is(err, ErrAlreadyDecided) ||
		err.Error() == "store: approval already decided")
}
