// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// PATCH /runs/{id}: the user changes a run's end and its wait (long-holds
// design rev 4, §2.3, RL-4). Extending the end within the run's captured max is
// always allowed — extending is the lease. Shortening it, setting No end and
// changing the wait need the run's captured user_changes_limits gate. An
// over-ask is capped at the limit and the response says so. The limit is the
// earlier of the captured governance lease (now plus max_end_ahead_sec) and the
// run's lifetime_cap_at, the pool's and the deployment's maximum measured from
// launch; the response names which one bound it. A super admin skips governance
// only (O10-A, #1319) and is still bounded by the captured cap, the way a super
// admin's own run captures no profile limits; a security admin is bounded like
// any user. A later end or No end also re-checks the owner's authority
// (extendRefusal), whoever asks.
//
// A run lost to a reboot or a control-plane outage is still extendable, even
// past its end (F1, long-holds design rev 4 §2.3): extending is how it
// becomes revivable again. So is a run whose OWN lease ended (LostEnded), but
// only while its files are kept (#1061, design rev 4 "Extend + Revive"): it
// stays ended and stopped until someone revives it, and its grace stays
// counted from when it ended, so extending never keeps its files longer.

// runEndWaitRequest is the PATCH body. An absent field is left alone; an
// explicit "ends_at": null is No end.
type runEndWaitRequest struct {
	EndsAt        *time.Time `json:"ends_at"`
	WaitBudgetSec *int       `json:"wait_budget_sec"`
}

// runEndWaitResponse is the run's end and wait after the change.
type runEndWaitResponse struct {
	ID            uuid.UUID  `json:"id"`
	EndsAt        *time.Time `json:"ends_at"`
	WaitBudgetSec int        `json:"wait_budget_sec"`
	// Capped names the fields cut back to the limit.
	Capped []string `json:"capped"`
	// LatestEnd is the furthest the end may be set now; absent is no limit.
	LatestEnd *time.Time `json:"latest_end,omitempty"`
	// LatestEndSource names the limit LatestEnd comes from (pool, governance or
	// deployment), so the console can say whose limit it is.
	LatestEndSource types.LimitSource `json:"latest_end_source,omitempty"`
	// MaxWaitSec is the longest wait allowed; absent is no limit.
	MaxWaitSec int `json:"max_wait_sec,omitempty"`
	// EndsCapLoosened is true when the end was capped by the limit this run
	// captured at launch while the launch profile's own limit has since been
	// loosened (#1322). Display only: the captured limit still binds, and no
	// number from the profile is sent, since profiles are readable only at the
	// security-ops tier.
	EndsCapLoosened bool `json:"ends_cap_loosened,omitempty"`
}

// endWaitPlan is a PATCH decided against the run as read: what to write, or
// why not. A refusal carries the audit row it is recorded under, and its wire
// reason (#656 slice 3).
type endWaitPlan struct {
	resp                    runEndWaitResponse
	endChanged, waitChanged bool
	status                  int
	refusal                 string
	refusalReason           string
	refusedAction           string
}

func (p *endWaitPlan) refuse(status int, action, reason, msg string) {
	p.status, p.refusedAction, p.refusalReason, p.refusal = status, action, reason, msg
}

// gateRefusal is the refusal for a change only the user_changes_limits gate
// allows. reasonRunLimitsGateDenied is shared by all 4 fields this can name —
// the SAME cause (the gate, not the deployment's run-limits shape) regardless
// of which field triggered it, which is exactly what the message names.
func gateRefusal(what string) string {
	return what + " needs your admin to let you change your run's limits; you can still extend it within the limit"
}

func (s *Server) handleSetRunEndAndWait(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	// Owner or SUPER admin: moving a foreign run's end keeps a sandbox and its
	// credentials alive, a write outside the security tier's inspect-or-stop.
	run, ok := s.getRunAuthorizedBy(w, r, id, s.ownsRunOrSuperAdmin)
	if !ok {
		return
	}
	var req runEndWaitRequest
	present, msg := decodeStrictKeys(w, r, &req)
	switch {
	case present == nil && msg == "":
		return
	case msg != "":
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidRequestBody, msg)
		return
	case !present["ends_at"] && !present["wait_budget_sec"]:
		writeErrorReason(w, http.StatusBadRequest, reasonRunEndWaitNeitherField, "set ends_at, wait_budget_sec or both")
		return
	case present["wait_budget_sec"] && req.WaitBudgetSec == nil:
		writeErrorReason(w, http.StatusBadRequest, reasonRunWaitBudgetNotANumber, "wait_budget_sec must be a number of seconds")
		return
	case s.endWaitRunRefusal(w, run):
		return
	}
	leaser, ok := s.cfg.Store.(store.RunLeaser)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonRunEndWaitStoreUnavailable, "this store cannot change a run's end")
		return
	}

	exempt := s.runUngoverned(r.Context())
	p := planRunEndWait(run, req, present, exempt, s.cfg.Now(), s.cfg.ApprovalExpiryAfter)
	actorType, actor := actorFromRequest(r)
	if p.status == http.StatusForbidden {
		s.recordAudit(r.Context(), s.auditEvent(&run.ID, actorType, actor, p.refusedAction, run.ID.String(),
			"denied", mustJSON(map[string]any{"reason": p.refusalReason})))
	}
	if p.refusal != "" {
		writeErrorReason(w, p.status, p.refusalReason, p.refusal)
		return
	}
	// Only when the captured profile limit is what cut the end: a pool or
	// deployment lifetime cap binds a new run the same way.
	if !exempt && slices.Contains(p.resp.Capped, "ends_at") && p.resp.LatestEndSource == types.LimitSourceGovernance {
		p.resp.EndsCapLoosened = s.endsCapLoosened(r.Context(), run)
	}
	if p.endChanged && (p.resp.EndsAt == nil || (run.EndsAt != nil && p.resp.EndsAt.After(*run.EndsAt))) {
		if ref := s.extendRefusal(r, run); ref != nil {
			s.recordAudit(r.Context(), s.auditEvent(&run.ID, actorType, actor, "run.end.set", run.ID.String(),
				"denied", mustJSON(map[string]any{"subject": run.CreatedBy, "reason": ref.reason})))
			writeErrorReason(w, ref.status, ref.reason, ref.msg)
			return
		}
	}
	if p.endChanged || p.waitChanged {
		// Under the run's lock, which the lease sweep holds from its re-read of a
		// kept run to its stop (leaseRun): a moved end lands before that read or
		// after that stop, never between.
		lctx, unlock, err := s.lockRunOp(r.Context(), run.ID)
		if err != nil {
			writeLockRefused(w, r, err)
			return
		}
		applied, err := leaser.SetRunEndAndWait(lctx, run.ID, run.RunLimits, run.EndsAt, run.WaitBudgetSec,
			p.resp.EndsAt, p.resp.WaitBudgetSec, s.endedKept(run, s.cfg.Now()))
		unlock()
		if err != nil {
			writeServerError(w, r, "set run end and wait", err)
			return
		}
		if !applied {
			writeErrorReason(w, http.StatusConflict, reasonRunEndWaitChanged, "run changed while this was being decided; reload it and try again")
			return
		}
	}
	if p.endChanged {
		if run.Placement == types.PlacementLocal {
			s.renewLeaseAfterEndChange(r.Context(), run.ID)
		}
		data := endSetAudit(run, p.resp, exempt)
		s.recordAudit(r.Context(), s.auditEvent(&run.ID, actorType, actor, "run.end.set", run.ID.String(),
			"success", mustJSON(data)))
	}
	if p.waitChanged {
		s.recordAudit(r.Context(), s.auditEvent(&run.ID, actorType, actor, "run.wait_budget.set", run.ID.String(),
			"success", mustJSON(map[string]any{
				"from": run.WaitBudgetSec, "to": p.resp.WaitBudgetSec, "max": p.resp.MaxWaitSec,
				"capped": slices.Contains(p.resp.Capped, "wait_budget_sec"), "limits_exempt": exempt,
			})))
	}
	writeJSON(w, http.StatusOK, p.resp)
}

// endWaitRunRefusal answers a PATCH on a run whose state no longer lets its end
// or wait move, and reports whether it did: a finished run; a run at or past
// its lifetime cap, which the lease sweep is ending; a run kept by its end whose
// files are gone. A run waiting in a pool's queue has no end yet: it is set when the run starts.
func (s *Server) endWaitRunRefusal(w http.ResponseWriter, run types.AgentRun) bool {
	switch {
	case run.State == types.RunQueued:
		writeErrorReason(w, http.StatusConflict, reasonRunQueued, "this run is waiting for a slot in its pool; its end is set when it starts")
	case isTerminalRunState(run.State):
		writeErrorReason(w, http.StatusConflict, reasonRunEndWaitAlreadyFinished, "run has already finished (state="+string(run.State)+")")
	case run.LifetimeCapAt != nil && !s.cfg.Now().Before(*run.LifetimeCapAt):
		writeErrorReason(w, http.StatusConflict, reasonRunEndWaitMaxLifetime, "run has reached its maximum lifetime and is being ended; its end and wait cannot be changed")
	case run.LostReason == types.LostEnded && !s.endedFilesKept(run, s.cfg.Now()):
		writeErrorReason(w, http.StatusConflict, reasonRunEndWaitFilesGone, "run has ended and its files are no longer kept; its end cannot be moved")
	default:
		return false
	}
	return true
}

// endSetAudit is the run.end.set success row's data.
func endSetAudit(run types.AgentRun, resp runEndWaitResponse, exempt bool) map[string]any {
	capped := slices.Contains(resp.Capped, "ends_at")
	data := map[string]any{
		"from": run.EndsAt, "to": resp.EndsAt, "max": run.RunLimits.MaxEndAheadSec,
		"capped": capped, "limits_exempt": exempt,
	}
	if resp.EndsCapLoosened {
		data["ends_cap_loosened"] = true
	}
	if capped && resp.LatestEndSource != "" {
		data["source"] = resp.LatestEndSource
	}
	return data
}

// endsCapLoosened reports whether the profile this run launched under now
// allows a later end than the limit the run captured (#1322); 0 is no limit.
// It reads the launch profile, never the owner's current assignment. A read
// that fails or a profile that is gone says false: the PATCH answers as it
// would have without this, never a 500 and never a looser check.
func (s *Server) endsCapLoosened(ctx context.Context, run types.AgentRun) bool {
	captured := run.RunLimits.MaxEndAheadSec
	if run.GovernanceProfileID == nil || captured <= 0 {
		return false
	}
	p, err := s.resolveProfileByID(ctx, *run.GovernanceProfileID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.WarnContext(ctx, "wardynd: reading a run's launch profile for its end cap failed",
				slog.String("run_id", run.ID.String()), slog.Any("err", err))
		}
		return false
	}
	live := p.Limits.RunLimits.MaxEndAheadSec
	return live == 0 || live > captured
}

// planRunEndWait decides a PATCH against the bounds the run captured at
// create, never the profile's current ones. exempt (a super admin) skips the
// governance limits only: the run's lifetime cap still binds (O10-A).
func planRunEndWait(run types.AgentRun, req runEndWaitRequest, present map[string]bool, exempt bool,
	now time.Time, deploymentWait time.Duration) endWaitPlan {
	l := run.RunLimits
	if exempt {
		l = types.RunLimits{AllowNoEnd: true, UserChangesLimits: true}
	}
	p := endWaitPlan{resp: runEndWaitResponse{
		ID: run.ID, EndsAt: run.EndsAt, WaitBudgetSec: run.WaitBudgetSec, Capped: []string{},
	}}
	if present["ends_at"] {
		p.planEnd(run.EndsAt, req.EndsAt, l, runCap{at: run.LifetimeCapAt, source: run.LifetimeCapSource}, now)
	}
	if p.refusal == "" && present["wait_budget_sec"] {
		p.planWait(run.WaitBudgetSec, *req.WaitBudgetSec, l, deploymentWait)
	}
	return p
}

// runCap is a run's absolute lifetime cap (agent_runs.lifetime_cap_at) and which
// source bound it; at is nil for no cap.
type runCap struct {
	at     *time.Time
	source types.LimitSource
}

// endBound is the furthest a finite end may sit past the moment it is set.
type endBound struct {
	ahead   time.Duration
	source  types.LimitSource // the limit that binds; empty when only the run-limit ceiling does
	bounded bool
}

// latestEndAhead is the one rule for how far a finite end may be set, shared by
// create and PATCH so the same ask is cut to the same bound at both: the earlier
// of the governance lease (leaseSec past now; 0 none) and the pool's and
// deployment's cap (capAhead; nil none). A tie names governance over the
// deployment, the way runnerpool.Effective does. With neither, only the
// run-limit ceiling bounds it.
func latestEndAhead(leaseSec int, capAhead *time.Duration, capSource types.LimitSource) endBound {
	b := endBound{ahead: maxRunLimitSec * time.Second}
	if capAhead != nil {
		b = endBound{ahead: *capAhead, source: capSource, bounded: true}
	}
	if lease := time.Duration(leaseSec) * time.Second; leaseSec > 0 &&
		(!b.bounded || lease < b.ahead || (lease == b.ahead && capSource == types.LimitSourceDeployment)) {
		b = endBound{ahead: lease, source: types.LimitSourceGovernance, bounded: true}
	}
	return b
}

// noEndBlock is why governance does not let a run have No end, or "" when it
// does: the same two answers at create and in a PATCH.
func noEndBlock(l types.RunLimits) (reason, msg string) {
	switch {
	case !l.AllowNoEnd:
		return reasonRunEndNoEndNotAllowed, "No end is not allowed for this run"
	case !l.UserChangesLimits:
		return reasonRunLimitsGateDenied, gateRefusal("Setting No end")
	}
	return "", ""
}

func (p *endWaitPlan) planEnd(cur, asked *time.Time, l types.RunLimits, lifetime runCap, now time.Time) {
	var capAhead *time.Duration
	if lifetime.at != nil {
		d := lifetime.at.Sub(now)
		capAhead = &d
	}
	bound := latestEndAhead(l.MaxEndAheadSec, capAhead, lifetime.source)
	latest := now.Add(bound.ahead).Truncate(time.Second)
	if bound.bounded {
		p.resp.LatestEnd, p.resp.LatestEndSource = &latest, bound.source
	}
	if asked == nil {
		switch reason, msg := noEndBlock(l); {
		case cur == nil:
		case lifetime.at != nil:
			p.refuse(http.StatusForbidden, "run.end.set", reasonRunEndNoEndNotAllowed,
				"No end is not allowed: "+endSourceLabel(lifetime.source, "")+" limits how long a run may live")
		case reason != "":
			p.refuse(http.StatusForbidden, "run.end.set", reason, msg)
		default:
			p.resp.EndsAt, p.endChanged = nil, true
		}
		return
	}
	if !asked.After(now) {
		p.refuse(http.StatusBadRequest, "run.end.set", reasonRunEndMustBeFuture, "ends_at must be in the future; to end the run now, kill it")
		return
	}
	if lifetime.at != nil && !lifetime.at.After(now) {
		p.refuse(http.StatusConflict, "run.end.set", reasonRunEndWaitMaxLifetime,
			"run has reached its maximum lifetime and is being ended; its end and wait cannot be changed")
		return
	}
	end := asked.UTC()
	if end.After(latest) {
		end = latest
		p.resp.Capped = append(p.resp.Capped, "ends_at")
	}
	switch {
	// An extension the cap cuts back to the current end or earlier keeps the
	// end: the cap never turns an extension into a shortening.
	case cur != nil && (end.Equal(*cur) || (asked.After(*cur) && end.Before(*cur))):
		return
	case cur != nil && end.After(*cur), l.UserChangesLimits:
	case cur == nil:
		p.refuse(http.StatusForbidden, "run.end.set", reasonRunLimitsGateDenied, gateRefusal("Setting an end on a run with no end"))
		return
	default:
		p.refuse(http.StatusForbidden, "run.end.set", reasonRunLimitsGateDenied, gateRefusal("Moving this run's end earlier"))
		return
	}
	p.resp.EndsAt, p.endChanged = &end, true
}

func (p *endWaitPlan) planWait(cur, asked int, l types.RunLimits, deploymentWait time.Duration) {
	if asked < 1 {
		p.refuse(http.StatusBadRequest, "run.wait_budget.set", reasonRunWaitBudgetTooSmall, "wait_budget_sec must be at least 1")
		return
	}
	ceiling := waitCeilingSec(l, deploymentWait)
	if ceiling > 0 {
		p.resp.MaxWaitSec = ceiling
	}
	if ceiling <= 0 || ceiling > maxRunLimitSec {
		ceiling = maxRunLimitSec
	}
	if asked > ceiling {
		asked = ceiling
		p.resp.Capped = append(p.resp.Capped, "wait_budget_sec")
	}
	if asked == cur {
		return
	}
	if !l.UserChangesLimits {
		p.refuse(http.StatusForbidden, "run.wait_budget.set", reasonRunLimitsGateDenied, gateRefusal("Changing how long this run waits for a decision"))
		return
	}
	p.resp.WaitBudgetSec, p.waitChanged = asked, true
}
