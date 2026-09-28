// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package approval implements the ApprovalRequest FSM service.
//
// States: PENDING -> APPROVED | DENIED | EXPIRED | CANCELLED
//
// Transitions are single-direction and fail-closed: deciding an
// already-decided approval returns ErrAlreadyDecided. Every state change
// emits an audit event, actor_type=human for decisions or system for
// expirations/CancelForRun.
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

// ErrAlreadyDecided re-exports the store's sentinel (defined in internal/types)
// so errors.Is matches whichever layer raised it.
var ErrAlreadyDecided = types.ErrApprovalAlreadyDecided

// Store is the narrow persistence interface the approval FSM needs.
type Store interface {
	CreateApproval(ctx context.Context, a types.ApprovalRequest) (types.ApprovalRequest, error)
	GetApproval(ctx context.Context, id uuid.UUID) (types.ApprovalRequest, error)
	// ListApprovals returns approvals filtered by state (empty = all).
	ListApprovals(ctx context.Context, stateFilter types.ApprovalState) ([]types.ApprovalRequest, error)
	// DecideApproval transitions PENDING to decision.State; returns
	// ErrAlreadyDecided if not PENDING.
	DecideApproval(ctx context.Context, id uuid.UUID, decision types.ApprovalDecision) (types.ApprovalRequest, error)
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
	// SECURITY: a fresh approval has no decision, so it has no scope either —
	// this zeroing, not the caller's decoding, is what guarantees a compromised
	// sidecar cannot pre-seed the scope.
	req.DecisionScope = ""
	req.DecisionExpiresAt = nil

	created, err := st.CreateApproval(ctx, req)
	if err != nil {
		// The list-then-create dedup has a race window: a concurrent raise can
		// slip in between list and insert. The partial unique index (migrations
		// 0022/0002) rejects the loser as ErrDuplicatePendingApproval; tolerate
		// it as a dedup signal and re-read the winner's row.
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

// Decide transitions an approval to decision.State (APPROVED or DENIED;
// ExpireStale bypasses Decide for EXPIRED, since a sweep is not a decision).
// decidedByType is human (OIDC session or LocalMode operator) or system (bare
// admin-token caller); the audit event records that exact type so an
// admin-token decision is never mislabelled as a human approval (attribution
// honesty invariant).
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
	// Surface the requested-scope host at the top level so a SIEM consumer
	// never has to parse nested JSON; best-effort, omitted when the kind's
	// scope carries no "host".
	if host := requestedScopeHost(result.RequestedScope); host != "" {
		data["host"] = host
	}
	// SECURITY: decision_scope is the decision's blast radius, emitted raw
	// (never Normalize()d) and unconditionally, so a permanent grant is
	// readable from the audit stream alone — "" means no scope recorded, never
	// inferred as `run`.
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
	// Audit log is the system of record: log-loud on a failed write instead of
	// swallowing it. Doesn't shadow the return value — the decision itself
	// already succeeded and is durable.
	if err := st.Record(ctx, ev); err != nil {
		audit.LogWriteFailure(ctx, ev, err)
	}

	return result, nil
}

// requestedScopeHost best-effort-extracts "host" from RequestedScope JSON;
// present on egress_domain and some api_key credential scopes, absent
// (returns "") on a tool_call scope or malformed/empty JSON.
func requestedScopeHost(scope json.RawMessage) string {
	var s struct {
		Host string `json:"host"`
	}
	if err := json.Unmarshal(scope, &s); err != nil {
		return ""
	}
	return s.Host
}

// ExpireStale transitions to EXPIRED every PENDING approval requested before
// the cutoff, or whose own ExpiresAt has passed, emitting one audit event per
// expiration. Returns the count expired. The cutoff is the deployment's
// ceiling and binds every row regardless of its run's wait.
func ExpireStale(ctx context.Context, st Store, olderThan time.Duration) (int, error) {
	n, _, err := ExpireStaleByKind(ctx, st, olderThan)
	return n, err
}

// ExpireStaleByKind is ExpireStale plus a tally of what it moved, keyed by
// TallyKey rather than the bare Kind: a credential_reauth row is either an
// Azure DevOps sign-in/consent or an AWS SSO re-auth, and keying on the bare
// Kind would fold both into one bucket, silently miscounting a metric whose
// HELP promises the AWS SSO population alone.
//
// Tallied HERE, where the state actually changes, not at a later resolve that
// happens to meet a terminal row — that would count retries, not outcomes.
// Kept as a separate function so every existing caller's signature is unchanged.
func ExpireStaleByKind(ctx context.Context, st Store, olderThan time.Duration) (int, map[string]int, error) {
	now := time.Now().UTC()
	cutoff := now.Add(-olderThan)

	pending, err := st.ListApprovals(ctx, types.ApprovalPending)
	if err != nil {
		return 0, nil, fmt.Errorf("approval: list for expiry: %w", err)
	}

	expired := 0
	byKind := map[string]int{}
	// Collected, not returned on the first failure: ListApprovals is ordered
	// and re-listed the same way every tick, so returning early would strand
	// every row sorted after the first failure. Expire what can be, report the
	// rest — same shape FSStore.Sweep uses.
	var failures []error
	for _, ap := range pending {
		runBound := ap.ExpiresAt != nil && !ap.ExpiresAt.After(now)
		if ap.RequestedAt.After(cutoff) && !runBound {
			continue
		}
		// Scope left at zero value (not ScopeRun): an expiry is a sweep nobody
		// decided, not a decision.
		if _, err := st.DecideApproval(ctx, ap.ID, types.ApprovalDecision{
			State: types.ApprovalExpired, DecidedBy: "system", Reason: "stale",
		}); err != nil {
			if errors.Is(err, ErrAlreadyDecided) {
				// Race with a concurrent Decide, not a failure to report.
				continue
			}
			failures = append(failures, fmt.Errorf("approval: expire %s: %w", ap.ID, err))
			continue
		}
		expired++
		byKind[TallyKey(ap)]++

		auditData, _ := json.Marshal(map[string]any{
			"approval_id": ap.ID,
			"cutoff":      cutoff,
			"expires_at":  ap.ExpiresAt,
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
		// Log-loud instead of swallowing — a dropped approval.expire audit must
		// be visible.
		if err := st.Record(ctx, ev); err != nil {
			audit.LogWriteFailure(ctx, ev, err)
		}
	}
	return expired, byKind, errors.Join(failures...)
}

// ExpireOne transitions a single PENDING approval straight to EXPIRED,
// regardless of age — the same terminal state ExpireStaleByKind's sweep would
// eventually give it, but raised eagerly by the client giving up on it
// (wardyn-toolgate at its own -deadline). Without this, a row the gate already
// treats as denied stays PENDING and approvable for up to one sweep interval
// (#811).
//
// Idempotent: a row already decided (human or concurrent sweep) is left
// untouched, ErrAlreadyDecided swallowed as the race it is.
//
// Attributed to the run's agent, not the system: the sandbox can call this at
// any time, so nothing ties it to a deadline.
func ExpireOne(ctx context.Context, st Store, id uuid.UUID, actor, reason string) error {
	result, err := st.DecideApproval(ctx, id, types.ApprovalDecision{
		State: types.ApprovalExpired, DecidedBy: "system", Reason: reason,
	})
	if err != nil {
		if errors.Is(err, ErrAlreadyDecided) {
			return nil
		}
		return fmt.Errorf("approval: expire %s: %w", id, err)
	}
	auditData, _ := json.Marshal(map[string]any{"approval_id": id, "reason": reason})
	ev := types.AuditEvent{
		ID:        uuid.New(),
		Time:      time.Now().UTC(),
		RunID:     &result.RunID,
		ActorType: types.ActorAgent,
		Actor:     actor,
		Action:    "approval.expire",
		Target:    id.String(),
		Outcome:   "success",
		Data:      json.RawMessage(auditData),
	}
	if err := st.Record(ctx, ev); err != nil {
		audit.LogWriteFailure(ctx, ev, err)
	}
	return nil
}

// Tally keys for the rows one kind carries with two meanings (#151). Every
// other row is tallied under its bare kind. Documented on
// docs/AUDIT-ACTIONS.md's approval.cancel row.
const (
	// TallyToolCallADO is an Azure DevOps capability escalation.
	TallyToolCallADO = "tool_call:azure_devops"
	// TallyReauthAWSSSO is a lapsed captured AWS SSO session's re-auth.
	TallyReauthAWSSSO = "credential_reauth:aws_sso"
	// TallyReauthADOSignIn is an Azure DevOps sign-in request.
	TallyReauthADOSignIn = "credential_reauth:azure_devops_signin"
	// TallyReauthADOConsent is an Azure DevOps consent request.
	TallyReauthADOConsent = "credential_reauth:azure_devops_consent"
)

// TallyKey is the key CancelForRun counts ap under, read off the stored row
// alone; restates internal/api's own lane-detection logic, and
// TestApprovalTallyKeyAgreesWithTheLanes pins that they agree.
func TallyKey(ap types.ApprovalRequest) string {
	var sc struct {
		Lane             string    `json:"lane"`
		Mechanism        string    `json:"mechanism"`
		CredentialSource string    `json:"credential_source"`
		GrantID          uuid.UUID `json:"grant_id"`
	}
	if json.Unmarshal(ap.RequestedScope, &sc) != nil {
		return string(ap.Kind)
	}
	switch ap.Kind {
	case types.ApprovalToolCall:
		if ap.GrantID != nil && *ap.GrantID != uuid.Nil && sc.Lane == "azure_devops" && sc.GrantID == *ap.GrantID {
			return TallyToolCallADO
		}
	case types.ApprovalCredentialReauth:
		switch {
		case sc.Lane == "azure_devops" && sc.Mechanism == "entra_signin":
			return TallyReauthADOSignIn
		case sc.Lane == "azure_devops" && sc.Mechanism == "entra_consent":
			return TallyReauthADOConsent
		case sc.Lane == "" && sc.CredentialSource != "":
			return TallyReauthAWSSSO
		}
	}
	return string(ap.Kind)
}

// CancelForRun transitions every still-PENDING approval for runID to
// CANCELLED, returning how many moved per TallyKey (counted where the CAS
// lands, so a row a human decided first is excluded). reason is the run's
// ending transition, recorded on each row and on the one audit event emitted.
//
// Mirrors ExpireStale's shape: list PENDING, move each row with the same CAS
// primitive (DecideApproval's WHERE state='PENDING' IS the FSM guard) so a
// concurrent human decision wins and ErrAlreadyDecided is treated as a race,
// not an error. DecidedBy is "system": nobody decided this, the run ended.
//
// ONE audit row for the whole batch (count + by_kind), not one per approval:
// these all belong to a single run transition an operator reads as one fact,
// keyed to the run so it lands in that run's evidence rail. Nothing pending
// means nothing emitted, keeping a re-kill idempotent in the audit log too.
//
// Emitted on the failure path too, with however many rows DID move plus the
// error, so a partial cancel never leaves CANCELLED rows with no trail.
func CancelForRun(ctx context.Context, st Store, runID uuid.UUID, reason string) (map[string]int, error) {
	pending, err := st.ListApprovals(ctx, types.ApprovalPending)
	if err != nil {
		return nil, fmt.Errorf("approval: list for cancel: %w", err)
	}
	cancelled := 0
	byKind := map[string]int{}
	// recordCancelled writes the one summary row for whatever actually moved;
	// a call that moved nothing writes nothing.
	recordCancelled := func(failure error) {
		if cancelled == 0 {
			return
		}
		data := map[string]any{
			"run_id":  runID,
			"reason":  reason,
			"count":   cancelled,
			"by_kind": byKind,
		}
		outcome := "success"
		if failure != nil {
			// A partial count, and saying so is the point: an operator must be
			// able to tell a bug from an interrupted cascade.
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
			Action:    "approval.cancel",
			Target:    runID.String(),
			Outcome:   outcome,
			Data:      json.RawMessage(auditData),
		}
		// Log-loud, same rule as the expiry sweep.
		if rerr := st.Record(ctx, ev); rerr != nil {
			audit.LogWriteFailure(ctx, ev, rerr)
		}
	}
	for _, ap := range pending {
		if ap.RunID != runID {
			continue
		}
		// Scope left at zero value, as an expiry does: a cancellation is not a
		// decision and authorizes nothing.
		if _, derr := st.DecideApproval(ctx, ap.ID, types.ApprovalDecision{
			State: types.ApprovalCancelled, DecidedBy: "system", Reason: reason,
		}); derr != nil {
			if errors.Is(derr, ErrAlreadyDecided) {
				// A human decided it between the list and the CAS — stands.
				continue
			}
			cerr := fmt.Errorf("approval: cancel %s: %w", ap.ID, derr)
			recordCancelled(cerr)
			return byKind, cerr
		}
		cancelled++
		byKind[TallyKey(ap)]++
	}
	recordCancelled(nil)
	return byKind, nil
}

// scopeHash produces a stable content hash over (runID, kind, scope) for
// deduplication. The scope JSON is re-encoded to normalise key ordering.
func scopeHash(runID uuid.UUID, kind types.ApprovalKind, scope json.RawMessage) string {
	var raw any
	if err := json.Unmarshal(scope, &raw); err != nil {
		raw = string(scope) // not valid JSON: hash the raw bytes
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
