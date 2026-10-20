// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// "Needs you" (#2963): the run's agent asks a person to take over its desktop or terminal
// (`wardyn needs-you <reason>`, raised as a human_takeover approval through the proxy). A person
// takes over, drives what the agent already has while the agent waits, and hands back in one click.
// The request is never approved or denied: its three verbs are here, behind the same entry rule as
// an attach ticket (admitRunEntry), because taking over IS interactive entry.
//
// Nothing here lets the run reach more. No credential is stored, brokered or handed to the agent;
// the person types into the desktop or terminal the run already has, and what they type is never
// recorded as input: the boot seed's cast is output only (wardyn-rec runs asciinema without
// --stdin), so is the console terminal's session cast (attach.go), the desktop relay records
// nothing (uigateway.go), and every row below carries a closed set of keys. What the terminal shows,
// echoed characters included, is output, and a recorded run keeps it: recording.pause marks the
// gap, but no recorder acts on recording_gap yet.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/approval"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/livebus"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// approvalTakeover is the optional ApprovalService surface behind take over and hand back.
type approvalTakeover interface {
	TakeOver(ctx context.Context, id uuid.UUID, holder string) (types.ApprovalRequest, string, error)
	HandBack(ctx context.Context, id uuid.UUID, at time.Time) (types.ApprovalRequest, error)
}

// takeoverNotDecidableBody answers approve/deny on a human_takeover row, for every tier.
const takeoverNotDecidableBody = "a takeover request is answered by taking over or declining, not by approve or deny"

// recordingGapReason is the reason the pause and resume rows give for the gap.
const recordingGapReason = "takeover"

// takeoverScopeOf checks a raised human_takeover scope and returns its canonical form: exactly one
// key, a non-empty reason within the cap the proxy clamps to (plus its truncation marker).
func takeoverScopeOf(raw json.RawMessage) (json.RawMessage, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var sc types.TakeoverScope
	if dec.Decode(&sc) != nil {
		return nil, false
	}
	sc.Reason = strings.TrimSpace(sc.Reason)
	if sc.Reason == "" || len(sc.Reason) > types.MaxTakeoverReason+len("… (truncated)") {
		return nil, false
	}
	out, err := json.Marshal(sc)
	return out, err == nil
}

// takeoverReason is a human_takeover row's reason ("" when the scope does not parse).
func takeoverReason(ap types.ApprovalRequest) string {
	var sc types.TakeoverScope
	_ = json.Unmarshal(ap.RequestedScope, &sc)
	return sc.Reason
}

// auditTakeoverRequest writes takeover.request: the agent asked for a person.
func (s *Server) auditTakeoverRequest(ctx context.Context, agent string, ap types.ApprovalRequest) {
	s.recordAudit(ctx, s.auditEvent(&ap.RunID, types.ActorAgent, agent, "takeover.request", ap.ID.String(), "success",
		mustJSON(map[string]any{"approval_id": ap.ID, "reason": takeoverReason(ap)})))
}

// loadTakeover is the shared front of the three verbs: the approval, then the run's entry rule,
// then the kind. The entry rule runs before the kind so a caller who may not enter the run learns
// nothing about the row; one who may is told plainly that the row is not a takeover request.
func (s *Server) loadTakeover(w http.ResponseWriter, r *http.Request) (types.ApprovalRequest, types.AgentRun, bool) {
	authorizedAt := s.cfg.Now().UTC()
	id, ok := parseIDParam(w, r, "id", "approval")
	if !ok {
		return types.ApprovalRequest{}, types.AgentRun{}, false
	}
	ap, err := s.cfg.Approvals.Get(r.Context(), id)
	if notFoundIf(w, err, "approval", reasonApprovalNotFound) {
		return ap, types.AgentRun{}, false
	}
	if err != nil {
		writeServerError(w, r, "get approval", err)
		return ap, types.AgentRun{}, false
	}
	// Someone who may not enter the run gets the 404 a missing request gets, not the run's own
	// (no oracle on the approval id), audited as a foreign entry. admitRunEntry writes the rest.
	if run, err := s.cfg.Store.GetRun(r.Context(), ap.RunID); err == nil &&
		!mayEnterRun(run, principalFromRequest(r), s.isOperator(r.Context())) &&
		!s.isOperator(r.Context()) && !s.sponsorsRun(r, run) {
		s.refuse(w, r, authz.Deny(authz.ReasonAttachTicketForeignRun, run.ID.String(), "approval not found").
			OnRun(run.ID).AsIf(authz.Reason(reasonApprovalNotFound)))
		return ap, types.AgentRun{}, false
	}
	run, asSponsor, ok := s.admitRunEntry(w, r, ap.RunID, authorizedAt)
	if !ok {
		return ap, run, false
	}
	if ap.Kind != types.ApprovalHumanTakeover {
		writeErrorReason(w, http.StatusConflict, reasonNotATakeoverRequest, "this approval is not a takeover request")
		return ap, run, false
	}
	// The run profile's deny_interactive door, as on the attach WebSocket: a super admin's entry is
	// exempt where the admin door is, a sponsor's never.
	if (!s.isOperator(r.Context()) || asSponsor || !s.adminDoorExempt(run)) && s.refuseInteractiveAttach(w, r, run) {
		return ap, run, false
	}
	return ap, run, true
}

// takeoverResponse is a take over / hand back answer: the row, and where the person drives.
type takeoverResponse struct {
	types.ApprovalRequest
	// Surface is "desktop" when the run's policy declares a novnc UI app, else "terminal".
	Surface string `json:"surface"`
}

// takeoverSurface is where a takeover of run puts the person: its declared desktop, else its terminal.
func (s *Server) takeoverSurface(ctx context.Context, run types.AgentRun) string {
	apps, err := s.effectiveUIApps(ctx, run.ID)
	if err == nil && s.uiGatewayEnabled() {
		for _, app := range apps {
			if app.Name == "novnc" {
				return "desktop"
			}
		}
	}
	return "terminal"
}

// takeoverRecorded reports whether run is being recorded, so a takeover's gap is marked.
func (s *Server) takeoverRecorded(run types.AgentRun) bool {
	return s.cfg.RecordingStore != nil && !runIsUnrecordable(run)
}

// recordingGap writes recording.pause or recording.resume (the gap's start or end, with its reason,
// in the tamper-evident log) and tells every replica's recorder over the live bus. Nothing consumes
// the notice yet, so the screen output of the gap is still recorded.
func (s *Server) recordingGap(ctx context.Context, ap types.ApprovalRequest, paused bool, data map[string]any) {
	action := "recording.resume"
	if paused {
		action = "recording.pause"
	}
	data["approval_id"], data["reason"] = ap.ID, recordingGapReason
	s.recordAudit(ctx, s.auditEvent(&ap.RunID, types.ActorSystem, "wardyn", action, ap.RunID.String(), "success", mustJSON(data)))
	s.publishNotice(livebus.KindRecordingGap, ap.RunID, recordingGapNotice{ApprovalID: ap.ID, Paused: paused, Reason: recordingGapReason})
}

// recordingGapNotice is the live-bus body of a recording pause or resume.
type recordingGapNotice struct {
	ApprovalID uuid.UUID `json:"approval_id"`
	Paused     bool      `json:"paused"`
	Reason     string    `json:"reason"`
}

// writeTakeoverError answers a failed take over or hand back.
func writeTakeoverError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrAlreadyDecided), errors.Is(err, approval.ErrAlreadyDecided):
		writeErrorReason(w, http.StatusConflict, reasonApprovalAlreadyDecided, "this takeover request is no longer open")
	case errors.Is(err, store.ErrNotFound):
		writeErrorReason(w, http.StatusNotFound, reasonApprovalNotFound, "approval not found")
	default:
		writeServerError(w, r, "takeover", err)
	}
}

// handleTakeOverApproval is POST /approvals/{id}/take-over: the caller gets control. On an open
// request it is the first take over (takeover.begin, then recording.pause on a recorded run); on a
// request someone else holds it replaces them (takeover.begin naming who it replaced; the recording
// is already paused).
func (s *Server) handleTakeOverApproval(w http.ResponseWriter, r *http.Request) {
	ap, run, ok := s.loadTakeover(w, r)
	if !ok {
		return
	}
	if run.State != types.RunRunning {
		writeErrorReason(w, http.StatusConflict, reasonAttachNotRunning, "run is not RUNNING; cannot take over (state="+string(run.State)+")")
		return
	}
	tk, can := s.cfg.Approvals.(approvalTakeover)
	if !can {
		writeErrorReason(w, http.StatusNotFound, reasonApprovalNotFound, "approval not found")
		return
	}
	actorType, principal := actorFromRequest(r)
	got, prev, err := tk.TakeOver(r.Context(), ap.ID, principal)
	if err != nil {
		writeTakeoverError(w, r, err)
		return
	}
	surface := s.takeoverSurface(r.Context(), run)
	data := map[string]any{"approval_id": got.ID, "surface": surface}
	if prev != "" {
		data["from"] = prev
	} else {
		data["after_ms"] = s.cfg.Now().Sub(got.RequestedAt).Milliseconds()
	}
	s.recordAudit(r.Context(), s.auditEvent(&got.RunID, actorType, principal, "takeover.begin", got.ID.String(), "success", mustJSON(data)))
	if prev == "" && s.takeoverRecorded(run) {
		s.recordingGap(r.Context(), got, true, map[string]any{})
	}
	writeJSON(w, http.StatusOK, takeoverResponse{ApprovalRequest: got, Surface: surface})
}

// handleHandBackApproval is POST /approvals/{id}/hand-back: the person holding control gives it back
// and the agent's `wardyn needs-you` returns. recording.resume (on a recorded run) then
// takeover.close, the order the gap closes in.
func (s *Server) handleHandBackApproval(w http.ResponseWriter, r *http.Request) {
	ap, run, ok := s.loadTakeover(w, r)
	if !ok {
		return
	}
	actorType, principal := actorFromRequest(r)
	if ap.State == types.ApprovalApproved && ap.HandedBackAt == nil && ap.TakeoverHolder != principal {
		writeErrorReason(w, http.StatusConflict, reasonTakeoverNotHolder, "you do not have control of this run; "+ap.TakeoverHolder+" does")
		return
	}
	tk, can := s.cfg.Approvals.(approvalTakeover)
	if !can {
		writeErrorReason(w, http.StatusNotFound, reasonApprovalNotFound, "approval not found")
		return
	}
	got, err := tk.HandBack(r.Context(), ap.ID, s.cfg.Now().UTC())
	if err != nil {
		writeTakeoverError(w, r, err)
		return
	}
	var held int64
	if got.DecidedAt != nil && got.HandedBackAt != nil {
		held = got.HandedBackAt.Sub(*got.DecidedAt).Milliseconds()
	}
	if s.takeoverRecorded(run) {
		s.recordingGap(r.Context(), got, false, map[string]any{"gap_ms": held})
	}
	s.recordAudit(r.Context(), s.auditEvent(&got.RunID, actorType, principal, "takeover.close", got.ID.String(), "success",
		mustJSON(map[string]any{"approval_id": got.ID, "held_ms": held})))
	writeJSON(w, http.StatusOK, takeoverResponse{ApprovalRequest: got, Surface: s.takeoverSurface(r.Context(), run)})
}

// handleDeclineTakeover is POST /approvals/{id}/decline: the person says no and the agent carries
// on without them. A decision like any other (approval.decide, decision DENIED), made under the
// entry rule rather than decide()'s, since only someone who could have taken over may decline.
func (s *Server) handleDeclineTakeover(w http.ResponseWriter, r *http.Request) {
	ap, _, ok := s.loadTakeover(w, r)
	if !ok {
		return
	}
	actorType, principal := actorFromRequest(r)
	got, err := s.cfg.Approvals.Decide(r.Context(), ap.ID, actorType, types.ApprovalDecision{
		State: types.ApprovalDenied, DecidedBy: principal, Reason: "declined",
	})
	if err != nil {
		writeDecideError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, got)
}
