// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
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
// over-ask is capped at the limit and the response says so. Only a super admin
// is exempt, bounded by the deployment alone, the way a super admin's own run
// captures no profile limits; a security admin is bounded like any user.

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
	// MaxWaitSec is the longest wait allowed; absent is no limit.
	MaxWaitSec int `json:"max_wait_sec,omitempty"`
}

// endWaitPlan is a PATCH decided against the run as read: what to write, or
// why not. A refusal carries the audit row it is recorded under.
type endWaitPlan struct {
	resp                    runEndWaitResponse
	endChanged, waitChanged bool
	status                  int
	refusal                 string
	refusedAction           string
}

func (p *endWaitPlan) refuse(status int, action, msg string) {
	p.status, p.refusedAction, p.refusal = status, action, msg
}

// gateRefusal is the refusal for a change only the user_changes_limits gate
// allows.
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
		writeError(w, http.StatusBadRequest, msg)
		return
	case !present["ends_at"] && !present["wait_budget_sec"]:
		writeError(w, http.StatusBadRequest, "set ends_at, wait_budget_sec or both")
		return
	case present["wait_budget_sec"] && req.WaitBudgetSec == nil:
		writeError(w, http.StatusBadRequest, "wait_budget_sec must be a number of seconds")
		return
	case isTerminalRunState(run.State):
		writeError(w, http.StatusConflict, "run has already finished (state="+string(run.State)+")")
		return
	case run.LostAt != nil:
		writeError(w, http.StatusConflict, "run has ended and is kept; its end cannot be moved")
		return
	}
	leaser, ok := s.cfg.Store.(store.RunLeaser)
	if !ok {
		writeError(w, http.StatusNotImplemented, "this store cannot change a run's end")
		return
	}

	exempt := s.isOperator(r.Context())
	p := planRunEndWait(run, req, present, exempt, s.cfg.Now(), s.cfg.ApprovalExpiryAfter)
	actorType, actor := actorFromRequest(r)
	if p.status == http.StatusForbidden {
		s.recordAudit(r.Context(), s.auditEvent(&run.ID, actorType, actor, p.refusedAction, run.ID.String(),
			"denied", mustJSON(map[string]any{"reason": p.refusal})))
	}
	if p.refusal != "" {
		writeError(w, p.status, p.refusal)
		return
	}
	if p.endChanged || p.waitChanged {
		applied, err := leaser.SetRunEndAndWait(r.Context(), run.ID, run.RunLimits, run.EndsAt, run.WaitBudgetSec,
			p.resp.EndsAt, p.resp.WaitBudgetSec)
		if err != nil {
			writeServerError(w, r, "set run end and wait", err)
			return
		}
		if !applied {
			writeError(w, http.StatusConflict, "run changed while this was being decided; reload it and try again")
			return
		}
	}
	if p.endChanged {
		s.recordAudit(r.Context(), s.auditEvent(&run.ID, actorType, actor, "run.end.set", run.ID.String(),
			"success", mustJSON(map[string]any{
				"from": run.EndsAt, "to": p.resp.EndsAt, "max": run.RunLimits.MaxEndAheadSec,
				"capped": slices.Contains(p.resp.Capped, "ends_at"), "limits_exempt": exempt,
			})))
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

// planRunEndWait decides a PATCH against the bounds the run captured at
// create, never the profile's current ones. exempt (a super admin) swaps them
// for the deployment's alone.
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
		p.planEnd(run.EndsAt, req.EndsAt, l, now)
	}
	if p.refusal == "" && present["wait_budget_sec"] {
		p.planWait(run.WaitBudgetSec, *req.WaitBudgetSec, l, deploymentWait)
	}
	return p
}

func (p *endWaitPlan) planEnd(cur, asked *time.Time, l types.RunLimits, now time.Time) {
	latest := now.Add(maxRunLimitSec * time.Second).Truncate(time.Second)
	if l.MaxEndAheadSec > 0 {
		latest = now.Add(time.Duration(l.MaxEndAheadSec) * time.Second).Truncate(time.Second)
		p.resp.LatestEnd = &latest
	}
	if asked == nil {
		switch {
		case cur == nil:
		case !l.AllowNoEnd:
			p.refuse(http.StatusForbidden, "run.end.set", "No end is not allowed for this run")
		case !l.UserChangesLimits:
			p.refuse(http.StatusForbidden, "run.end.set", gateRefusal("Setting No end"))
		default:
			p.resp.EndsAt, p.endChanged = nil, true
		}
		return
	}
	if !asked.After(now) {
		p.refuse(http.StatusBadRequest, "run.end.set", "ends_at must be in the future; to end the run now, kill it")
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
		p.refuse(http.StatusForbidden, "run.end.set", gateRefusal("Setting an end on a run with no end"))
		return
	default:
		p.refuse(http.StatusForbidden, "run.end.set", gateRefusal("Moving this run's end earlier"))
		return
	}
	p.resp.EndsAt, p.endChanged = &end, true
}

func (p *endWaitPlan) planWait(cur, asked int, l types.RunLimits, deploymentWait time.Duration) {
	if asked < 1 {
		p.refuse(http.StatusBadRequest, "run.wait_budget.set", "wait_budget_sec must be at least 1")
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
		p.refuse(http.StatusForbidden, "run.wait_budget.set", gateRefusal("Changing how long this run waits for a decision"))
		return
	}
	p.resp.WaitBudgetSec, p.waitChanged = asked, true
}
