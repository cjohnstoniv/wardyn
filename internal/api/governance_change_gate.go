// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Four-eyes on governance writes. With WARDYN_GOVERNANCE_SECOND_HUMAN on, a covered write (a
// governance profile or assignment today) is decoded and validated exactly as before and then STORED
// as a pending change (migration 0126) and answered 202; it applies only when a distinct, authorised
// human approves it, in one transaction (governance_changes.go). With the switch off nothing here runs
// and every covered route answers byte for byte as it did.
//
// This file is the gate every covered write goes through, the proposal that stores one, the diff the
// reviewer reads, and the per-target table of who may approve.
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// envGovernanceSecondHuman opts a deployment IN to four-eyes on governance writes: a covered write is
// held for a second human instead of applied. DEFAULT OFF, for the reason the egress switch gives
// (approvals_second_human.go): turning it on unprompted would deadlock every single-administrator
// deployment. Env-only on purpose: a setting held in the database could be switched off by the
// administrator it constrains.
const envGovernanceSecondHuman = "WARDYN_GOVERNANCE_SECOND_HUMAN"

// GovernanceSecondHumanEnabled reports whether the governance four-eyes switch is on. Exported for
// cmd/wardynd's boot checks, so the env name and the truthiness rule keep one definition.
func GovernanceSecondHumanEnabled() bool { return envEnabled(envGovernanceSecondHuman) }

// defaultGovernanceChangeTTL is how long a pending change waits when the deployment sets none.
const defaultGovernanceChangeTTL = 72 * time.Hour

func (s *Server) governanceChangeTTL() time.Duration {
	if s.cfg.GovernanceChangeTTL > 0 {
		return s.cfg.GovernanceChangeTTL
	}
	return defaultGovernanceChangeTTL
}

// The closed set of target kinds (the table is governanceChangeKinds). A later lane adds a kind here
// and a row there, with no DDL: migration 0126 puts no CHECK on target_kind.
const (
	govKindProfile      = types.GovernanceTargetProfile
	govKindAssignment   = types.GovernanceTargetAssignment
	govKindGrant        = types.GovernanceTargetCapabilityGrant
	govKindEnforcement  = types.GovernanceTargetCapabilityEnforcement
	govKindAvailability = types.GovernanceTargetCapabilityAvailability
	govKindUserType     = types.GovernanceTargetUserTypePriority
	govKindRoleMapping  = types.GovernanceTargetRoleMapping
	govKindKeyDomain    = types.GovernanceTargetKeyDomainAssignment
)

// govChangeTarget is the authz.denied target of every refusal a change decision gives.
const govChangeTarget = "governance.change"

// govWriteMode is how a covered write is carried out.
type govWriteMode int

const (
	// govDirect: the switch is off. The write is applied and answered exactly as in 0.8.5.
	govDirect govWriteMode = iota
	// govBypass: the admin token wrote it. Applied directly, plus a governance.change.bypass row.
	govBypass
	// govQueue: a human wrote it. It is held unless an exemption proves it narrowing.
	govQueue
)

// isAdminTokenCaller reports whether the caller is the shared admin token, which carries no per-human
// identity: the break-glass the egress gate also exempts.
func isAdminTokenCaller(r *http.Request) bool {
	actorType, principal := actorFromRequest(r)
	return actorType == types.ActorSystem && principal == adminTokenPrincipal
}

// governanceWriteMode decides how a covered write is carried out, having already written its own 503
// (ok=false) in local mode. The admin token is checked before local mode, so it stays the way past a
// switch local mode cannot enforce.
//
// Local mode refuses outright rather than comparing: it authenticates nobody, so the proposer and the
// approver are both client-supplied and no request there can prove a second human decided
// (requireSecondHuman argues this at length). A refusal that names the incompatibility is the same
// outcome reached honestly, once.
func (s *Server) governanceWriteMode(w http.ResponseWriter, r *http.Request) (govWriteMode, bool) {
	if !envEnabled(envGovernanceSecondHuman) {
		return govDirect, true
	}
	if isAdminTokenCaller(r) {
		return govBypass, true
	}
	if s.cfg.LocalMode {
		s.refuseGovernanceLocalMode(w)
		return govDirect, false
	}
	return govQueue, true
}

func (s *Server) refuseGovernanceLocalMode(w http.ResponseWriter) {
	writeErrorReason(w, http.StatusServiceUnavailable, reasonGovernanceSecondHumanLocalMode, envGovernanceSecondHuman+
		" cannot be enforced in local mode: local mode authenticates nobody, so both the proposer and the"+
		" approver are client-supplied and no request can prove a second human decided."+
		" Configure SSO to use this switch, or unset it")
}

// recordGovernanceBypass writes the break-glass row beside the target's own audit row: the admin
// token wrote a covered target directly (target is the target's id) or approved a change (target is
// the change id). outcome says whether the write it was spent on happened.
func (s *Server) recordGovernanceBypass(r *http.Request, kind, target, outcome string, extra map[string]any) {
	data := map[string]any{"reason": "admin_token_break_glass", "switch": envGovernanceSecondHuman, "target_kind": kind}
	for k, v := range extra {
		data[k] = v
	}
	actorType, principal := actorFromRequest(r)
	s.recordAudit(r.Context(), s.auditEvent(nil, actorType, principal, "governance.change.bypass", target, outcome, mustJSON(data)))
}

// govApprover is who may decide a change of one kind: the predicate of the tier the write was
// proposed on, never a weaker one, and the refusal reason that tier gives.
type govApprover struct {
	allowed func(s *Server, r *http.Request) bool
	reason  authz.Reason
}

// operatorApprover is the tier of the operatorOnly routes: a super admin only. A security admin who
// tries to decide a change proposed on that tier gets the same admin_surface refusal requireOperator
// gives.
var operatorApprover = govApprover{
	allowed: func(s *Server, r *http.Request) bool { return s.isOperator(r.Context()) },
	reason:  authz.ReasonAdminSurface,
}

// securityApprover is the tier of the securityOps routes: a super admin or a security admin.
var securityApprover = govApprover{
	allowed: func(s *Server, r *http.Request) bool { return s.isSecurityOperator(r.Context()) },
	reason:  authz.ReasonSecurityAdminSurface,
}

// govKind is one row of the table of covered targets.
type govKind struct {
	approver govApprover
	// apply runs inside the decision transaction. It takes the locks it needs, compares the target as
	// it stands with ch.BaseHash (store.ErrGovernanceChangeStale on a mismatch), re-validates against
	// the current state, and writes the target through the Querier forms of the store writes. It never
	// touches the pool, and it never audits: the audit rows follow the commit.
	apply func(s *Server, r *http.Request, q store.Querier, ch types.GovernanceChange) (govApplied, error)
}

// govApplied is what an applied change leaves for the audit rows written after the commit.
type govApplied struct {
	action string
	target string
	data   map[string]any
	// afterCommit, when set, runs once the decision has committed and returns more audit data (a
	// role mapping's revocation of the tokens it demoted runs here, as it runs after the write today).
	afterCommit func() map[string]any
}

// governanceChangeKinds is the single table of covered targets and who may approve each. A role
// mapping is written on the operatorOnly tier, so only a super admin approves it; every other kind is
// written on securityOps.
var governanceChangeKinds = map[string]govKind{
	govKindProfile:      {approver: securityApprover, apply: applyProfileChange},
	govKindAssignment:   {approver: securityApprover, apply: applyAssignmentChange},
	govKindGrant:        {approver: securityApprover, apply: applyGrantChange},
	govKindEnforcement:  {approver: securityApprover, apply: applyEnforcementChange},
	govKindAvailability: {approver: securityApprover, apply: applyAvailabilityChange},
	govKindUserType:     {approver: securityApprover, apply: applyUserTypeChange},
	govKindRoleMapping:  {approver: operatorApprover, apply: applyRoleMappingChange},
	govKindKeyDomain:    {approver: securityApprover, apply: applyKeyDomainAssignmentChange},
}

// canSeeGovernanceKind reports whether the caller may list or read changes of kind: the approver
// predicate, so a kind that only a super admin may write is never shown to a security admin. A kind
// this binary does not know (a row from a newer one) is shown to nobody: it cannot be decided here.
func (s *Server) canSeeGovernanceKind(r *http.Request, kind string) bool {
	k, ok := governanceChangeKinds[kind]
	return ok && k.approver.allowed(s, r)
}

// govProposal is a covered write to hold.
type govProposal struct {
	kind, op, key string
	// payload is the validated request, replayed on approval.
	payload any
	// before and after are the redacted views the reviewer reads; changed lists the field paths that
	// differ. A nil before is a create, a nil after a delete.
	before, after any
	changed       []string
	// baseState is the target as the proposal saw it; its hash is what an approval compares.
	baseState any
}

// proposeGovernanceChange stores p as pending and answers 202 with the pending change. A live change
// already holding the target is a 409 governance_change_pending naming it.
func (s *Server) proposeGovernanceChange(w http.ResponseWriter, r *http.Request, p govProposal) {
	payload, err := json.Marshal(p.payload)
	if err != nil {
		writeServerError(w, r, "encode governance change", err)
		return
	}
	_, proposer := actorFromRequest(r)
	ch := types.GovernanceChange{
		TargetKind: p.kind, Op: p.op, TargetKey: p.key,
		Payload:         payload,
		Diff:            renderGovernanceDiff(p.before, p.after, p.changed),
		BaseHash:        computeETag(p.baseState),
		DeploymentHash:  computeETag(s.cfg.DefaultPolicy),
		ProposedBy:      proposer,
		ProposedByEmail: oidcEmailFromContext(r.Context()),
	}
	saved, expired, err := s.cfg.Store.ProposeGovernanceChange(r.Context(), ch, s.governanceChangeTTL())
	var pending *store.ErrGovernanceChangePending
	switch {
	case errors.As(err, &pending):
		writeErrorReason(w, http.StatusConflict, reasonGovernanceChangePending,
			"a change to this target is already waiting for approval: "+pending.ID.String())
		return
	case err != nil:
		writeServerError(w, r, "hold governance change", err)
		return
	}
	actorType, actor := actorFromRequest(r)
	for _, id := range expired {
		s.recordAudit(r.Context(), s.auditEvent(nil, actorType, actor, "governance.change.expire", id.String(), "success",
			mustJSON(map[string]any{"target_kind": p.kind, "target_key": p.key})))
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorType, actor, "governance.change.propose", saved.ID.String(), "success",
		mustJSON(map[string]any{"target_kind": p.kind, "op": p.op, "target_key": p.key, "expires_at": saved.ExpiresAt})))
	w.Header().Set("Location", "/api/v1/governance/changes/"+saved.ID.String())
	writeJSON(w, http.StatusAccepted, map[string]any{"pending_change": saved})
}

// governanceDiffBody is a change's diff column.
type governanceDiffBody struct {
	Before  json.RawMessage `json:"before,omitempty"`
	After   json.RawMessage `json:"after,omitempty"`
	Changed []string        `json:"changed"`
}

// renderGovernanceDiff is the diff stored with a change: the redacted before and after views and the
// changed field paths, rendered by the server so the console and the CLI never compute their own.
func renderGovernanceDiff(before, after any, changed []string) json.RawMessage {
	body := governanceDiffBody{Changed: changed}
	if body.Changed == nil {
		body.Changed = []string{}
	}
	if before != nil {
		body.Before = mustJSON(before)
	}
	if after != nil {
		body.After = mustJSON(after)
	}
	return mustJSON(body)
}

// changedPaths lists the dotted field paths at which the JSON forms of before and after differ,
// sorted. An array is one leaf: a list that changes is reported at its own path. A nil side is every
// path of the other.
func changedPaths(before, after any) []string {
	var b, a any
	roundTripJSON(before, &b)
	roundTripJSON(after, &a)
	var out []string
	walkChanged("", b, a, &out)
	slices.Sort(out)
	return out
}

func roundTripJSON(in any, out *any) {
	if in == nil {
		return
	}
	if raw, err := json.Marshal(in); err == nil {
		_ = json.Unmarshal(raw, out)
	}
}

func walkChanged(path string, b, a any, out *[]string) {
	bm, bIsMap := b.(map[string]any)
	am, aIsMap := a.(map[string]any)
	if (bIsMap || b == nil) && (aIsMap || a == nil) && (bIsMap || aIsMap) {
		keys := map[string]bool{}
		for k := range bm {
			keys[k] = true
		}
		for k := range am {
			keys[k] = true
		}
		for k := range keys {
			walkChanged(joinPath(path, k), bm[k], am[k], out)
		}
		return
	}
	if !reflect.DeepEqual(b, a) {
		if path == "" {
			path = "(root)"
		}
		*out = append(*out, path)
	}
}

func joinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// maxGovernanceRejectReasonLen bounds a rejection's reason, in characters.
const maxGovernanceRejectReasonLen = 512
