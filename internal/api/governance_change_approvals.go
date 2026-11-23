// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Approvals from more than one person on a held governance change.
//
// With WARDYN_GOVERNANCE_MIN_APPROVALS set, or on a change that was held needing several people, a held
// change applies only when it has effective(ch) distinct people who are still eligible on their latest
// sign-in record. An approval that does not finish the job is recorded and the change stays pending; the
// last approval re-checks every earlier one. A person can take their approval back. With the minimum unset,
// and for the kinds that always have one decider, none of this runs: every route reads, writes, audits and
// answers as it did.
//
// This file holds the definitions, the decision, the re-check, the take-back route and the read model;
// governance_change_pool.go holds who can approve.
package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Why an approval stopped counting (the dropped value of an approval row and of the read model).
const (
	droppedDeactivated     = "deactivated"
	droppedTokenRevoked    = "token_revoked"
	droppedSessionRevoked  = "session_revoked"
	droppedRoleRemoved     = "role_removed"
	droppedGrantRemoved    = "grant_removed"
	droppedCannotConfirmed = "cant_confirm"
)

// The reach of a change's remaining approvals.
const (
	reachReachable      = "ok"
	reachCantReach      = "cant_reach"
	reachCantConfirm    = "cant_confirm"
	cantReachByCount    = "count"
	cantReachBySecurity = "security"
)

// What the caller of a change can do, in the read model's viewer.
const (
	viewerCanApprove   = "can_approve"
	viewerApproved     = "approved"
	viewerProposer     = "proposer"
	viewerCannotBeLast = "cannot_be_last"
	viewerNotEligible  = "not_eligible"
)

// singleDecider reports whether a kind always has one decider, whatever the minimum: the two request kinds
// and the run node record, whose record names exactly the proposer and one approver.
func singleDecider(kind string) bool {
	switch kind {
	case types.GovernanceTargetElevation, types.GovernanceTargetImageRequest, govKindRunNodeRecord:
		return true
	}
	return false
}

// multi reports whether the approval rules apply to ch: its kind has more than one decider and the minimum
// is set now, or the change was held needing more than one approval or a security admin. It reads only the
// change row and the configured minimum. When it is false, a decision is today's, unchanged.
func (s *Server) multi(ch types.GovernanceChange) bool {
	return !singleDecider(ch.TargetKind) && (s.GovernanceMinApprovals() > 0 || ch.RequiredApprovals > 1 || ch.RequireSecurity)
}

// effective is how many people ch needs now: the count it was held with, or the minimum if that is higher.
func (s *Server) effective(ch types.GovernanceChange) int {
	return max(ch.RequiredApprovals, s.GovernanceMinApprovals(), 1)
}

// requiredApprovals is how many approvals a held change of kind needs and whether one must be a security
// admin: one and no for a kind with a single decider or with no minimum, else the larger of the minimum and
// the kind's own count (its row of the counts table, else the default row), with that row's security flag.
// It reads nothing about any proposal, so the composer can ask before one exists; targetKey is kept for a
// kind whose count depends on its target.
func (s *Server) requiredApprovals(ctx context.Context, kind, targetKey string) (required int, requireSecurity bool, err error) {
	minimum := s.GovernanceMinApprovals()
	if singleDecider(kind) || minimum <= 0 {
		return 1, false, nil
	}
	if kind == govKindApprovalCounts {
		return s.approvalCountsEditRequired(ctx)
	}
	required = minimum
	if st, ok := s.cfg.Store.(store.GovernanceApprovalStore); ok {
		n, sec, found, rerr := st.GovernanceApprovalCountFor(ctx, kind)
		if rerr != nil {
			return 0, false, rerr
		}
		if found {
			required, requireSecurity = max(minimum, n), sec
		}
	}
	return required, requireSecurity, nil
}

// sameHuman reports whether two principals are one person: the same non-empty principal, or the same
// non-empty email once case-folded. It is the one distinct-human rule, between any two principals: approval
// acceptance, the pool's merging, and the exclusion of the proposer and of every counted approver all use it.
// sameGovernanceHuman is this rule against the proposer.
func sameHuman(aPrincipal, aEmail, bPrincipal, bEmail string) bool {
	if aPrincipal != "" && aPrincipal == bPrincipal {
		return true
	}
	return aEmail != "" && bEmail != "" && strings.EqualFold(aEmail, bEmail)
}

// approvalCaller is who is approving, as one decision carries it.
type approvalCaller struct {
	principal, email string
	// reason is the optional text the approver typed, and scope the unit scope they decide in (for the audit
	// row and the approval row).
	reason, scope string
}

// approvalRun is one decision's account of an approval under the rules: who approved, and what the
// transaction recorded, for the audit row and the answer after it commits.
type approvalRun struct {
	caller         approvalCaller
	have, required int
	tier           string
	// drops are the earlier approvals this decision found no longer counting, for the audit row.
	drops []approvalDrop
}

// approvalDrop is an earlier approval that stopped counting, to be marked when the decision commits.
type approvalDrop struct {
	row     store.GovernanceChangeApproval
	why     string
	at      *time.Time
	replace bool
}

// approvalVerdict is the result of re-checking one earlier approval: whether it still counts and, if so, its
// tier recomputed on the person's latest record (never above the tier it was given at); if not, why.
type approvalVerdict struct {
	counts bool
	why    string
	at     *time.Time
	tier   string
}

func dropVerdict(why string, at *time.Time) approvalVerdict {
	return approvalVerdict{why: why, at: at}
}

// recheckApproval re-checks one earlier approval, failing closed. It stops at the first reason it no longer
// counts: the person is deactivated; the personal API token it was given with is revoked (never merely
// expired, and never a CLI token, which a refresh or a sign-out revokes without saying anything about the
// person); a revocation of the person's sessions or identity took effect after it; or the person's latest
// sign-in record no longer passes the kind's approver predicate. The role is derived from the current record
// and mappings, never stored. Nothing is persisted here.
func (s *Server) recheckApproval(ctx context.Context, q store.Querier, in *recordInputs, kind govKind, row store.GovernanceChangeApproval) (approvalVerdict, error) {
	rec, err := store.GetSignInRecordQ(ctx, q, row.Approver)
	found := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return approvalVerdict{}, err
	}
	if v, dropped, err := s.droppedByIdentity(ctx, q, row, rec, found); err != nil || dropped {
		return v, err
	}
	if !found {
		return dropVerdict(droppedCannotConfirmed, nil), nil
	}
	v := s.judgeRecord(in, nil, kind.approver, rec)
	switch v.outcome {
	case recordPasses:
		return approvalVerdict{counts: true, tier: lowerTier(v.tier, row.Tier)}, nil
	case recordUnconfirmed, recordNone:
		return dropVerdict(droppedCannotConfirmed, signedInAfter(rec, row)), nil
	case recordRoleGone:
		return dropVerdict(droppedRoleRemoved, signedInAfter(rec, row)), nil
	}
	why := droppedRoleRemoved
	if row.Tier == tierUnitAdmin {
		why = droppedGrantRemoved
	}
	return dropVerdict(why, signedInAfter(rec, row)), nil
}

// signedInAfter is the record's sign-in time when it is later than the approval, else nil: display only.
func signedInAfter(rec store.SignInRecord, row store.GovernanceChangeApproval) *time.Time {
	if rec.SignedInAt.After(row.ApprovedAt) {
		at := rec.SignedInAt
		return &at
	}
	return nil
}

// droppedByIdentity is the first three reasons an approval stops counting, the ones that do not read the
// role: the person is deactivated, its token is revoked, its sessions or identity are revoked. dropped is
// true when one holds, with the verdict.
func (s *Server) droppedByIdentity(ctx context.Context, q store.Querier, row store.GovernanceChangeApproval, rec store.SignInRecord, found bool) (approvalVerdict, bool, error) {
	at := rec.DeactivatedAt
	if !found {
		var err error
		if at, err = store.DirectoryDeactivatedAtQ(ctx, q, row.Approver); err != nil {
			return approvalVerdict{}, false, err
		}
	}
	if at != nil {
		return dropVerdict(droppedDeactivated, at), true, nil
	}
	if row.APITokenID != uuid.Nil && row.APITokenKind == types.APITokenKindAPI {
		revokedAt, exists, err := store.APITokenRevokedAtQ(ctx, q, row.APITokenID)
		if err != nil {
			return approvalVerdict{}, false, err
		}
		if !exists || revokedAt != nil {
			return dropVerdict(droppedTokenRevoked, revokedAt), true, nil
		}
	}
	if s.cfg.SessionRevocations == nil {
		return approvalVerdict{}, false, nil
	}
	status, err := s.sessionStatusAt(ctx, q, row.Approver, row.ApproverEmail, oidc.Issue{At: row.ApprovedAt, Seq: row.RevSeq})
	if err != nil {
		return approvalVerdict{}, false, s.sessionUnreadable()
	}
	switch status {
	case oidc.SessionRevoked:
		return dropVerdict(droppedSessionRevoked, nil), true, nil
	case oidc.SessionDeactivated:
		return dropVerdict(droppedDeactivated, nil), true, nil
	}
	return approvalVerdict{}, false, nil
}

// sessionUnreadable is the refusal of a decision or a read that could not read the session revocations: an
// unreadable status never drops an approval.
func (s *Server) sessionUnreadable() *govRefusal {
	return &govRefusal{why: "session revocation unreadable", write: func(w http.ResponseWriter, r *http.Request) {
		s.metrics.authStoreErrorInc()
		writeErrorReason(w, http.StatusServiceUnavailable, reasonTokenLookupUnavailable, "api token lookup failed")
	}}
}

// callerTier checks the approver against their latest sign-in record and returns the tier their approval is
// given at: the lower of the tier the record recomputes and the tier the authenticated request carries. A
// person with no record, one the people directory counts as inactive, one whose role sign-in would now refuse,
// one whose partial record cannot establish it, or one the predicate no longer admits is refused as an approver
// is, whatever their older session says.
func (s *Server) callerTier(ctx context.Context, r *http.Request, q store.Querier, in *recordInputs, kind govKind, principal string) (string, error) {
	rec, err := store.GetSignInRecordQ(ctx, q, principal)
	if errors.Is(err, store.ErrNotFound) {
		return "", s.denyApprover(kind)
	}
	if err != nil {
		return "", err
	}
	if rec.Inactive() {
		return "", s.denyApprover(kind)
	}
	v := s.judgeRecord(in, nil, kind.approver, rec)
	if v.outcome != recordPasses {
		return "", s.denyApprover(kind)
	}
	return lowerTier(v.tier, s.tierOf(r.Context())), nil
}

// decideMulti is an approval's part of the decision transaction when the approval rules apply, after the
// approver's own re-check and the distinct-human check. The change row is locked.
//
// An approval that is not the last is recorded: the held write is replayed under a savepoint that is rolled
// back, so a stale or invalid change is found at every approval, and then the approval row is written and
// store.ErrGovernanceApprovalRecorded keeps the change pending. The last approval re-checks every earlier
// one, drops those that no longer count and, if enough remain, applies.
func (s *Server) decideMulti(ctx context.Context, r *http.Request, q store.Querier, kind govKind, ch types.GovernanceChange,
	ar *approvalRun, applied *govApplied) error {
	c := ar.caller
	if c.principal == "" {
		return s.denyApprover(kind) // never ask a predicate about an empty principal, which reads as the admin token
	}
	in, err := s.loadRecordInputs(ctx, q)
	if err != nil {
		return err
	}
	if ar.tier, err = s.callerTier(ctx, r, q, in, kind, c.principal); err != nil {
		return err
	}
	rows, err := store.ListGovernanceChangeApprovalsQ(ctx, q, ch.ID)
	if err != nil {
		return err
	}
	counting, drops, err := s.settleOwnApproval(ctx, q, in, kind, rows, c)
	if err != nil {
		return err
	}
	need := s.effective(ch)
	var kept []keptApproval
	if len(counting)+1 >= need {
		if kept, drops, err = s.recheckEarlier(ctx, q, in, kind, counting, drops); err != nil {
			return err
		}
	} else {
		kept = storedApprovals(counting)
	}
	ar.have, ar.required, ar.drops = len(kept)+1, need, drops
	if len(kept)+1 < need {
		return s.recordApproval(ctx, r, q, kind, ch, ar, drops)
	}
	return s.finishApproval(ctx, r, q, kind, ch, ar, kept, drops, applied)
}

// keptApproval is an earlier approval that still counts, with its tier as last judged.
type keptApproval struct {
	row  store.GovernanceChangeApproval
	tier string
}

func storedApprovals(rows []store.GovernanceChangeApproval) []keptApproval {
	out := make([]keptApproval, len(rows))
	for i, row := range rows {
		out[i] = keptApproval{row: row, tier: row.Tier}
	}
	return out
}

// settleOwnApproval separates the approvals that count from the caller's own. An approval of the same human
// (by principal, or by the email stored on the row) that still counts is a 409; one that would drop is
// replaced by this approval, so a person the page shows as able to approve never meets that 409. A row
// already marked dropped never refuses: it is replaced when the approval is written.
func (s *Server) settleOwnApproval(ctx context.Context, q store.Querier, in *recordInputs, kind govKind,
	rows []store.GovernanceChangeApproval, c approvalCaller) ([]store.GovernanceChangeApproval, []approvalDrop, error) {
	var counting []store.GovernanceChangeApproval
	var drops []approvalDrop
	// own is whether any of the same human's rows that still count is the caller's own, as the viewer judges it:
	// an earlier row held only by a shared email must not hide the caller's own approval from the refusal.
	var blocked, own bool
	for _, row := range rows {
		if !row.Counts() {
			continue
		}
		if !sameHuman(c.principal, c.email, row.Approver, row.ApproverEmail) {
			counting = append(counting, row)
			continue
		}
		v, err := s.recheckApproval(ctx, q, in, kind, row)
		if err != nil {
			return nil, nil, err
		}
		if v.counts {
			blocked, own = true, own || s.holdsApproval(c.principal, c.email, row)
			continue
		}
		drops = append(drops, approvalDrop{row: row, why: v.why, at: v.at, replace: true})
	}
	if blocked {
		return nil, nil, s.alreadyApproved(own)
	}
	return counting, drops, nil
}

// recheckEarlier re-checks every approval that counts, adding the ones that no longer count to drops.
func (s *Server) recheckEarlier(ctx context.Context, q store.Querier, in *recordInputs, kind govKind,
	counting []store.GovernanceChangeApproval, drops []approvalDrop) ([]keptApproval, []approvalDrop, error) {
	var kept []keptApproval
	for _, row := range counting {
		v, err := s.recheckApproval(ctx, q, in, kind, row)
		if err != nil {
			return nil, nil, err
		}
		if v.counts {
			kept = append(kept, keptApproval{row: row, tier: v.tier})
			continue
		}
		drops = append(drops, approvalDrop{row: row, why: v.why, at: v.at})
	}
	return kept, drops, nil
}

// alreadyApproved is the refusal of an approval from a person whose earlier one still counts. own says the
// approval is the caller's own; one held only by the same email, which sign-in does not vouch for, is
// someone else's to take back, so the caller is not offered that.
func (s *Server) alreadyApproved(own bool) *govRefusal {
	msg := "Someone with your email address has already approved this change; it needs approval from someone else"
	if own {
		msg = "You have already approved this change; it needs approval from someone else, or you can take your approval back"
	}
	return &govRefusal{why: "approver already approved", write: func(w http.ResponseWriter, r *http.Request) {
		writeErrorReason(w, http.StatusConflict, reasonGovernanceChangeAlreadyApproved, msg)
	}}
}

// holdsApproval reports whether row is the caller's own approval: the same principal, or the same email only
// where sign-in vouches for every caller's email. Whoever else shares the email is the same human for the
// distinct-human rule, but the approval is not theirs to see as their own or to take back.
func (s *Server) holdsApproval(principal, email string, row store.GovernanceChangeApproval) bool {
	if principal != "" && row.Approver == principal {
		return true
	}
	return s.emailTrusted() && sameHuman(principal, email, row.Approver, row.ApproverEmail)
}

// applyHeld is today's check and apply of a held change: the deployment default must be what the proposal
// reviewed, then the kind's own apply writes the target on q.
func (s *Server) applyHeld(r *http.Request, q store.Querier, kind govKind, ch types.GovernanceChange) (govApplied, error) {
	if ch.DeploymentHash != computeETag(s.cfg.DefaultPolicy) {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	return kind.apply(s, r, q, ch)
}

// replayApply runs the held write under a savepoint and rolls it back: what a decision would answer for
// stale or invalid, found without writing the target. An apply reads and writes only on q, and never audits,
// so the rollback leaves nothing behind.
func (s *Server) replayApply(ctx context.Context, r *http.Request, q store.Querier, kind govKind, ch types.GovernanceChange) error {
	if _, err := q.Exec(ctx, `SAVEPOINT governance_approval`); err != nil {
		return err
	}
	_, err := s.applyHeld(r, q, kind, ch)
	if _, rerr := q.Exec(ctx, `ROLLBACK TO SAVEPOINT governance_approval`); rerr != nil && err == nil {
		err = rerr
	}
	return err
}

// recordApproval records an approval that is not the last: the replay, then the rows, and the sentinel that
// commits them and leaves the change pending.
func (s *Server) recordApproval(ctx context.Context, r *http.Request, q store.Querier, kind govKind, ch types.GovernanceChange,
	ar *approvalRun, drops []approvalDrop) error {
	if err := s.replayApply(ctx, r, q, kind, ch); err != nil {
		return err
	}
	if err := s.writeApproval(ctx, r, q, ch, ar, drops); err != nil {
		return err
	}
	return store.ErrGovernanceApprovalRecorded
}

// finishApproval is the last approval: enough earlier ones still count, so the rules for the last one apply
// (a security admin among those counted when the change needs one, an active proposer), and then the change
// applies as it always did, with the approval rows and the drops written beside it.
func (s *Server) finishApproval(ctx context.Context, r *http.Request, q store.Querier, kind govKind, ch types.GovernanceChange,
	ar *approvalRun, kept []keptApproval, drops []approvalDrop, applied *govApplied) error {
	if ch.RequireSecurity && !tierIsSecurity(ar.tier) && !slices.ContainsFunc(kept, func(k keptApproval) bool { return tierIsSecurity(k.tier) }) {
		return &govRefusal{why: "no security admin among the approvers", write: func(w http.ResponseWriter, r *http.Request) {
			writeErrorReason(w, http.StatusConflict, reasonGovernanceChangeSecurityApprover,
				"This change needs a security admin among its approvers, and none of those who approved holds that role")
		}}
	}
	if inactive, err := s.proposerInactive(ctx, q, ch); err != nil {
		return err
	} else if inactive {
		return &govRefusal{why: "proposer is not active", write: func(w http.ResponseWriter, r *http.Request) {
			writeErrorReason(w, http.StatusConflict, reasonGovernanceChangeProposerInactive,
				"The person who proposed this change is no longer active, so it cannot be applied; reject it and ask for it again")
		}}
	}
	a, err := s.applyHeld(r, q, kind, ch)
	if err != nil {
		return err
	}
	*applied = a
	if err := s.writeApproval(ctx, r, q, ch, ar, drops); err != nil {
		return err
	}
	counted := make([]string, 0, len(kept)+1)
	for _, k := range kept {
		counted = append(counted, k.row.Approver)
	}
	applied.approvalData = s.finalApprovalData(ar, append(counted, ar.caller.principal), drops)
	return nil
}

// finalApprovalData is the keys the applied change's governance.change.approve row gains under the rules:
// how many approvals it had and needed, who counted, who stopped counting and why, and the approver's reason.
func (s *Server) finalApprovalData(ar *approvalRun, counted []string, drops []approvalDrop) map[string]any {
	data := map[string]any{"have": ar.have, "required": ar.required, "approvals": counted}
	if len(drops) > 0 {
		data["dropped"] = droppedData(drops)
	}
	if ar.caller.reason != "" {
		data["reason"] = ar.caller.reason
	}
	return data
}

// proposerInactive reports whether the people directory counts the proposer as deactivated.
func (s *Server) proposerInactive(ctx context.Context, q store.Querier, ch types.GovernanceChange) (bool, error) {
	at, err := store.DirectoryDeactivatedAtQ(ctx, q, ch.ProposedBy)
	return at != nil, err
}

// writeApproval writes what an approval changes: the drop marks of the earlier approvals that stopped
// counting, then the approver's own row (a new one, or their dropped one replaced). The revocation sequence is
// read before any write, and a failure to read it records nothing. A token caller's row names the token
// re-read on q.
func (s *Server) writeApproval(ctx context.Context, r *http.Request, q store.Querier, ch types.GovernanceChange, ar *approvalRun, drops []approvalDrop) error {
	c := ar.caller
	approvedAt := s.cfg.Now().UTC()
	issue, err := s.mintIssue(r.Context(), c.principal, approvedAt)
	if err != nil {
		return err
	}
	row := store.GovernanceChangeApproval{
		ChangeID: ch.ID, Approver: c.principal, ApproverEmail: c.email, Tier: ar.tier, UnitScope: c.scope,
		Reason: c.reason, RevSeq: issue.Seq,
	}
	tok, err := s.recheckLiveToken(ctx, q)
	if err != nil {
		return err
	}
	if tok != nil {
		row.APITokenID, row.APITokenKind = tok.ID, tok.Kind
		if tok.CLISessionID != nil {
			row.CLISessionID = *tok.CLISessionID
		}
	}
	for _, d := range drops {
		if d.replace && d.row.Approver == c.principal {
			continue // this approval replaces the row in place
		}
		if err := store.DropGovernanceChangeApprovalQ(ctx, q, ch.ID, d.row.Approver, d.why, d.at); err != nil {
			return err
		}
	}
	return store.PutGovernanceChangeApprovalQ(ctx, q, row)
}

// approvalRecordedData is the audit row of an approval that was recorded and did not apply the change.
func approvalRecordedData(out types.GovernanceChange, ar *approvalRun) map[string]any {
	data := map[string]any{
		"target_kind": out.TargetKind, "target_key": out.TargetKey, "proposed_by": out.ProposedBy,
		"have": ar.have, "required": ar.required, "tier": ar.tier,
	}
	if ar.caller.scope != "" {
		data["unit_scope"] = ar.caller.scope
	}
	if len(ar.drops) > 0 {
		data["dropped"] = droppedData(ar.drops)
	}
	if ar.caller.reason != "" {
		data["reason"] = ar.caller.reason
	}
	return data
}

// droppedData is the earlier approvals a decision found no longer counting, each with the principal and why.
func droppedData(drops []approvalDrop) []map[string]string {
	dropped := make([]map[string]string, 0, len(drops))
	for _, d := range drops {
		dropped = append(dropped, map[string]string{"principal": d.row.Approver, "reason": d.why})
	}
	return dropped
}

// approvalRunFor starts a decision under the approval rules: nil (today's path) for the admin token, a reject
// and a change the rules do not cover. An approve reads its optional reason and the unit scope it decides in; a
// reject by someone other than the proposer must give a reason. It writes the refusal and reports false when
// the body is refused.
func (s *Server) approvalRunFor(w http.ResponseWriter, r *http.Request, head types.GovernanceChange, approve, bypass bool,
	principal, email, rejectReason string) (*approvalRun, bool) {
	if bypass || !s.multi(head) {
		return nil, true
	}
	if !approve {
		if rejectReason == "" && !sameGovernanceHuman(head, principal, email) {
			writeErrorReason(w, http.StatusBadRequest, reasonGovernanceChangeReasonRequired,
				"A reason is required to reject a change you did not propose")
			return nil, false
		}
		return nil, true
	}
	reason, ok := s.readRejectReason(w, r)
	if !ok {
		return nil, false
	}
	c := approvalCaller{principal: principal, email: email, reason: reason}
	if units, strict, scoped := unitChangeScope(head); scoped {
		c.scope = s.auditUnitScope(r.Context(), units[0], strict)
	}
	return &approvalRun{caller: c}, true
}

// answerRecordedApproval answers an approval that was recorded and left the change pending: the
// governance.change.approval row, and 200 with the change in the shape GET answers, approvals included.
func (s *Server) answerRecordedApproval(w http.ResponseWriter, r *http.Request, out types.GovernanceChange, ar *approvalRun, actorType types.ActorType) {
	s.recordAudit(r.Context(), s.auditEvent(nil, actorType, ar.caller.principal, "governance.change.approval", out.ID.String(), "success",
		mustJSON(approvalRecordedData(out, ar))))
	s.writeChangeView(w, r, out)
}

// ── the read model ────────────────────────────────────────────────────────────

// governanceChangeView is a held change as GET /governance/changes/{id} answers it: the change, who may
// decide it, and, while it is pending under the approval rules, the approvals block.
type governanceChangeView struct {
	governanceChangeRead
	Approvals *approvalsRead `json:"approvals,omitempty"`
}

// listedChange is a row of GET /governance/changes under the approval rules.
type listedChange struct {
	types.GovernanceChange
	Approvals *approvalsListRead `json:"approvals,omitempty"`
}

// approvalsListRead is a list row's approvals: counts from the stored rows only (no pool and no re-check per
// row), so its viewer is never cannot_be_last.
type approvalsListRead struct {
	Required        int             `json:"required"`
	Have            int             `json:"have"`
	RequireSecurity bool            `json:"require_security"`
	Viewer          *approvalViewer `json:"viewer,omitempty"`
}

// approvalEntry is one approval of the change in the read model.
type approvalEntry struct {
	Approver   string     `json:"approver"`
	Tier       string     `json:"tier"`
	UnitName   string     `json:"unit_name,omitempty"`
	Reason     string     `json:"reason,omitempty"`
	ApprovedAt time.Time  `json:"approved_at"`
	Counts     bool       `json:"counts"`
	Dropped    string     `json:"dropped,omitempty"`
	DroppedAt  *time.Time `json:"dropped_at,omitempty"`
}

// approvalViewer is what the caller can do with the change.
type approvalViewer struct {
	State      string     `json:"state"`
	Last       bool       `json:"last,omitempty"`
	ApprovedAt *time.Time `json:"approved_at,omitempty"`
}

// approvalsRead is the approvals block of one pending change.
type approvalsRead struct {
	Required             int             `json:"required"`
	RequireSecurity      bool            `json:"require_security"`
	Have                 int             `json:"have"`
	Entries              []approvalEntry `json:"entries"`
	Eligible             []poolPerson    `json:"eligible"`
	Uncountable          []poolSource    `json:"uncountable"`
	Reach                string          `json:"reach"`
	CantReachReason      string          `json:"cant_reach_reason,omitempty"`
	StillNeeded          int             `json:"still_needed"`
	RemainingEligible    int             `json:"remaining_eligible"`
	ProposerActive       bool            `json:"proposer_active"`
	RejectReasonRequired bool            `json:"reject_reason_required"`
	Viewer               *approvalViewer `json:"viewer,omitempty"`
}

// changeView is the change in the shape GET answers it. A pending change under the approval rules carries the
// approvals block, built from one rolled-back read transaction; every other change is exactly as it was.
func (s *Server) changeView(r *http.Request, ch types.GovernanceChange) (governanceChangeView, error) {
	v := governanceChangeView{governanceChangeRead: governanceChangeRead{GovernanceChange: s.changeForRead(r, ch), Approvers: s.changeApprovers(r.Context(), ch)}}
	if ch.State != types.GovernanceChangePending || !s.multi(ch) {
		return v, nil
	}
	var err error
	v.Approvals, err = s.approvalsRead(r, ch)
	return v, err
}

// writeChangeView answers 200 with the change in GET's shape.
func (s *Server) writeChangeView(w http.ResponseWriter, r *http.Request, ch types.GovernanceChange) {
	v, err := s.changeView(r, ch)
	var refusal *govRefusal
	switch {
	case errors.As(err, &refusal):
		refusal.write(w, r)
	case err != nil:
		writeServerError(w, r, "read governance change approvals", err)
	default:
		writeJSON(w, http.StatusOK, v)
	}
}

// approvalsRead builds the approvals block: every approval re-checked read-only (so a drop shows on page
// load; nothing is persisted), then who else can approve, then what the caller can do. It runs on one
// rolled-back transaction, with the unit scope read through the request's shared memo.
func (s *Server) approvalsRead(r *http.Request, ch types.GovernanceChange) (*approvalsRead, error) {
	var blk *approvalsRead
	err := s.cfg.Store.DryRunGovernance(r.Context(), func(q store.Querier) error {
		var err error
		blk, err = s.buildApprovals(r, q, ch)
		return err
	})
	return blk, err
}

func (s *Server) buildApprovals(r *http.Request, q store.Querier, ch types.GovernanceChange) (*approvalsRead, error) {
	ctx := r.Context()
	kind := governanceChangeKinds[ch.TargetKind].forChange(ch)
	rows, err := store.ListGovernanceChangeApprovalsQ(ctx, q, ch.ID)
	if err != nil {
		return nil, err
	}
	in, err := s.loadPoolInputs(ctx, q)
	if err != nil {
		return nil, err
	}
	entries, kept, err := s.entriesFor(ctx, q, &in.recordInputs, kind, rows)
	if err != nil {
		return nil, err
	}
	counted := make([]countedPerson, len(kept))
	for i, k := range kept {
		counted[i] = countedPerson{Principal: k.row.Approver, Email: k.row.ApproverEmail}
	}
	pool, err := s.poolForChange(in, ch, counted)
	if err != nil {
		return nil, err
	}
	inactive, err := s.proposerInactive(ctx, q, ch)
	if err != nil {
		return nil, err
	}
	if err := s.nameEntryUnits(ctx, q, entries); err != nil {
		return nil, err
	}
	need := s.effective(ch)
	blk := &approvalsRead{
		Required: need, RequireSecurity: ch.RequireSecurity, Have: len(kept), Entries: entries,
		Eligible: pool.Eligible, Uncountable: pool.Uncountable,
		StillNeeded: max(need-len(kept), 0), RemainingEligible: len(pool.Eligible), ProposerActive: !inactive,
	}
	blk.Reach, blk.CantReachReason = reachOf(blk, kept, pool)
	_, principal := actorFromRequest(r)
	if !isAdminTokenCaller(r) {
		email := oidcEmailFromContext(ctx)
		blk.RejectReasonRequired = !sameGovernanceHuman(ch, principal, email)
		blk.Viewer, err = s.viewerOf(r, q, in, kind, ch, kept, blk)
		if err != nil {
			return nil, err
		}
	}
	return blk, nil
}

// entriesFor re-checks every approval of a change read-only and returns one entry for each (counting ones
// with their recomputed tier, dropped ones with the tier they were given at) and the approvals that still
// count.
func (s *Server) entriesFor(ctx context.Context, q store.Querier, in *recordInputs, kind govKind,
	rows []store.GovernanceChangeApproval) ([]approvalEntry, []keptApproval, error) {
	entries := make([]approvalEntry, 0, len(rows))
	var kept []keptApproval
	for _, row := range rows {
		e := approvalEntry{Approver: row.Approver, Tier: row.Tier, UnitName: row.UnitScope, Reason: row.Reason, ApprovedAt: row.ApprovedAt}
		v := approvalVerdict{counts: row.Counts(), why: row.Dropped, at: row.DroppedAt}
		if row.Counts() {
			var err error
			if v, err = s.recheckApproval(ctx, q, in, kind, row); err != nil {
				return nil, nil, err
			}
		}
		if v.counts {
			e.Counts, e.Tier = true, v.tier
			kept = append(kept, keptApproval{row: row, tier: v.tier})
		} else {
			e.Dropped, e.DroppedAt = v.why, v.at
		}
		entries = append(entries, e)
	}
	return entries, kept, nil
}

// nameEntryUnits turns the unit each approval was given in (a unit id, or a word such as "security") into
// the unit's name where it is one, reading the units once and only when an approval names one.
func (s *Server) nameEntryUnits(ctx context.Context, q store.Querier, entries []approvalEntry) error {
	var names map[uuid.UUID]string
	for i, e := range entries {
		id, err := uuid.Parse(e.UnitName)
		if err != nil {
			entries[i].UnitName = ""
			continue
		}
		if names == nil {
			units, err := store.ListGovernanceUnitsQ(ctx, q)
			if err != nil {
				return err
			}
			names = make(map[uuid.UUID]string, len(units))
			for _, u := range units {
				names[u.ID] = u.Name
			}
		}
		entries[i].UnitName = names[id]
	}
	return nil
}

// reachOf says whether the change can still get the approvals it needs from the people who can approve it:
// cant_reach when there are too few of them, or when a security admin is required and none counts or can, and
// nothing is uncountable; cant_confirm when the same shortfall holds but some people cannot be counted. A
// cant_reach says why: count, or security.
func reachOf(blk *approvalsRead, kept []keptApproval, pool poolResult) (reach, why string) {
	short := blk.RemainingEligible < blk.StillNeeded
	securityUnmet := blk.RequireSecurity && !slices.ContainsFunc(kept, func(k keptApproval) bool { return tierIsSecurity(k.tier) }) &&
		!slices.ContainsFunc(pool.Eligible, func(p poolPerson) bool { return tierIsSecurity(p.Tier) })
	switch {
	case !short && !securityUnmet:
		return reachReachable, ""
	case len(pool.Uncountable) > 0:
		return reachCantConfirm, ""
	case short:
		return reachCantReach, cantReachByCount
	}
	return reachCantReach, cantReachBySecurity
}

// viewerOf is what the caller can do with the change, in the order the page reads it: the proposer, someone
// whose approval already counts, someone who is not eligible (by the request, or by their latest sign-in
// record), someone who could not be the last approver, else able to approve.
func (s *Server) viewerOf(r *http.Request, q store.Querier, in *poolInputs, kind govKind, ch types.GovernanceChange,
	kept []keptApproval, blk *approvalsRead) (*approvalViewer, error) {
	ctx := r.Context()
	_, principal := actorFromRequest(r)
	email := oidcEmailFromContext(ctx)
	if sameGovernanceHuman(ch, principal, email) {
		return &approvalViewer{State: viewerProposer}, nil
	}
	if i := slices.IndexFunc(kept, func(k keptApproval) bool { return s.holdsApproval(principal, email, k.row) }); i >= 0 {
		at := kept[i].row.ApprovedAt
		return &approvalViewer{State: viewerApproved, ApprovedAt: &at}, nil
	}
	if slices.ContainsFunc(kept, func(k keptApproval) bool { return sameHuman(principal, email, k.row.Approver, k.row.ApproverEmail) }) {
		return &approvalViewer{State: viewerNotEligible}, nil // the same human as an approver, by an email nothing vouches for
	}
	last := blk.Have+1 >= blk.Required
	if !kind.approver.allowed(s, r) {
		return &approvalViewer{State: viewerNotEligible}, nil
	}
	tier, err := s.viewerTier(r, q, &in.recordInputs, kind, principal)
	if err != nil {
		return nil, err
	}
	switch {
	case tier == "":
		return &approvalViewer{State: viewerNotEligible}, nil
	case last && blk.RequireSecurity && !tierIsSecurity(tier) && !slices.ContainsFunc(kept, func(k keptApproval) bool { return tierIsSecurity(k.tier) }):
		return &approvalViewer{State: viewerCannotBeLast, Last: last}, nil
	}
	return &approvalViewer{State: viewerCanApprove, Last: last}, nil
}

// viewerTier is the tier the caller's approval would be given at, "" when their latest sign-in record
// would refuse them. It is callerTier read-only, through the request's shared memo.
func (s *Server) viewerTier(r *http.Request, q store.Querier, in *recordInputs, kind govKind, principal string) (string, error) {
	tier, err := s.callerTier(r.Context(), r, q, in, kind, principal)
	var refusal *govRefusal
	if errors.As(err, &refusal) {
		return "", nil
	}
	return tier, err
}

// ── the list ──────────────────────────────────────────────────────────────────

// listWithApprovals adds the approvals counts to the pending rows of a list that are under the approval
// rules. With none, the list is returned as it is, and nothing is read. The counts come from one grouped
// read of the stored approvals of the page's changes: no pool and no re-check, so a row's viewer is never
// cannot_be_last (the page works that out).
func (s *Server) listWithApprovals(r *http.Request, list []types.GovernanceChange) (any, error) {
	var ids []uuid.UUID
	for _, ch := range list {
		if ch.State == types.GovernanceChangePending && s.multi(ch) {
			ids = append(ids, ch.ID)
		}
	}
	if len(ids) == 0 {
		return list, nil
	}
	var rows []store.GovernanceChangeApproval
	if st, ok := s.cfg.Store.(store.GovernanceApprovalStore); ok {
		var err error
		if rows, err = st.ListGovernanceChangeApprovals(r.Context(), ids, true); err != nil {
			return nil, err
		}
	}
	_, principal := actorFromRequest(r)
	email := oidcEmailFromContext(r.Context())
	out := make([]listedChange, len(list))
	for i, ch := range list {
		out[i].GovernanceChange = ch
		if !slices.Contains(ids, ch.ID) {
			continue
		}
		out[i].Approvals = s.listRead(r, ch, rows, principal, email)
	}
	return out, nil
}

// listRead is one pending row's approvals counts and the caller's state.
func (s *Server) listRead(r *http.Request, ch types.GovernanceChange, rows []store.GovernanceChangeApproval, principal, email string) *approvalsListRead {
	blk := &approvalsListRead{Required: s.effective(ch), RequireSecurity: ch.RequireSecurity}
	var own *store.GovernanceChangeApproval
	alias := false
	for i, row := range rows {
		if row.ChangeID != ch.ID {
			continue
		}
		blk.Have++
		if own == nil && s.holdsApproval(principal, email, row) {
			own = &rows[i]
		} else if sameHuman(principal, email, row.Approver, row.ApproverEmail) {
			alias = true
		}
	}
	if isAdminTokenCaller(r) {
		return blk
	}
	switch {
	case sameGovernanceHuman(ch, principal, email):
		blk.Viewer = &approvalViewer{State: viewerProposer}
	case own != nil:
		at := own.ApprovedAt
		blk.Viewer = &approvalViewer{State: viewerApproved, ApprovedAt: &at}
	case alias:
		blk.Viewer = &approvalViewer{State: viewerNotEligible}
	case !governanceChangeKinds[ch.TargetKind].forChange(ch).approver.allowed(s, r):
		blk.Viewer = &approvalViewer{State: viewerNotEligible}
	default:
		blk.Viewer = &approvalViewer{State: viewerCanApprove}
	}
	return blk
}
