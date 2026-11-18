// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/livebus"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Support sessions, the grant's API half: a super admin asks to help inside a person's running run, the run's
// owner accepts with a door list and a lifetime, and either of them, or a security operator, ends it. No door
// opens from here: supportMayEnter (support_entry.go) is what the entry points ask. The record is
// run_support_grants (internal/store/store_support_grants.go); every time in it is the database's.

// The reasons a grant ends, as support.end rows and the status route say them. The store and the sweep write
// the others (expired, run_ended, lapsed, support_off).
const (
	supportEndRevoked         = "revoked"           // the owner, by the end route
	supportEndByHelper        = "ended_by_helper"   // the helper, by the end route
	supportEndBySecurity      = "ended_by_security" // a security operator, by the end route
	supportEndHelperSignedOut = "helper_signed_out" // supportMayEnter's standing check
	supportEndOwnerRevoked    = "owner_revoked"     // supportMayEnter's standing check
)

const (
	supportDisabledMsg = "Support sessions are turned off on this deployment."
	supportMaxReason   = 500 // characters, as the table's CHECK
)

// supportMinutes are the lifetimes an owner may pick when accepting.
var supportMinutes = []int{15, 30, 60}

// supportState is the replica's part of Server for the audit window: the open accepted grant of each run it
// knows of, so a row written for that run while a helper has it open can name the grant (stampSupportWindow).
// Embedded, so its fields read as Server's own.
type supportState struct {
	supportMu   sync.Mutex
	supportOpen map[uuid.UUID]uuid.UUID // run id -> grant id
}

func (s *Server) setSupportWindow(run, grant uuid.UUID) {
	s.supportMu.Lock()
	defer s.supportMu.Unlock()
	if s.supportOpen == nil {
		s.supportOpen = map[uuid.UUID]uuid.UUID{}
	}
	s.supportOpen[run] = grant
}

func (s *Server) clearSupportWindow(run uuid.UUID) {
	s.supportMu.Lock()
	delete(s.supportOpen, run)
	s.supportMu.Unlock()
}

// replaceSupportWindow makes refs the whole map: the sweep's refresh from the store.
func (s *Server) replaceSupportWindow(refs []store.SupportGrantRef) {
	m := make(map[uuid.UUID]uuid.UUID, len(refs))
	for _, ref := range refs {
		m[ref.RunID] = ref.ID
	}
	s.supportMu.Lock()
	s.supportOpen = m
	s.supportMu.Unlock()
}

// stampSupportWindow adds support_grant to data when this replica knows an open grant on the run. A row that
// already names a grant (an entry row) keeps its own. The field may trail an expiry or a lost notice by one
// sweep tick; support.accept and support.end mark the window exactly.
func (s *Server) stampSupportWindow(run *uuid.UUID, data json.RawMessage) json.RawMessage {
	if run == nil {
		return data
	}
	s.supportMu.Lock()
	grant, open := s.supportOpen[*run]
	s.supportMu.Unlock()
	if !open {
		return data
	}
	m := map[string]json.RawMessage{}
	if len(data) > 0 && string(data) != "null" {
		if err := json.Unmarshal(data, &m); err != nil {
			m = map[string]json.RawMessage{"data": data} // not an object: kept whole, so the row still carries the stamp
		}
	}
	if _, has := m["support_grant"]; has {
		return data
	}
	m["support_grant"], _ = json.Marshal(grant)
	out, _ := json.Marshal(m) // a map of RawMessage cannot fail to marshal
	return out
}

// onSupportGrantNotice re-reads the run's grant after another replica accepted or ended one.
func (s *Server) onSupportGrantNotice(m livebus.Message) {
	st, ok := s.cfg.Store.(store.SupportGrantStore)
	if !ok || m.Origin == s.replicaName() {
		return
	}
	s.goBackground(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(s.cfg.BaseCtx), liveBusPublishTimeout)
		defer cancel()
		open, _, err := st.SupportGrantStatus(ctx, m.Run)
		if err != nil {
			slog.Warn("wardynd: a support grant notice could not be read; the next sweep tick refreshes it", slog.String("run_id", m.Run.String()), slog.Any("err", err))
			return
		}
		if open != nil && open.AcceptedAt != nil {
			s.setSupportWindow(m.Run, open.ID)
			return
		}
		s.clearSupportWindow(m.Run)
	})
}

// sweepSupportGrants is the support grant pass of the run watcher tick. It ends lapsed, expired and run-over
// grants (and every open one when the switch is off), writes the one support.end of each end no route audited,
// and replaces this replica's open-grant map from the store.
func (s *Server) sweepSupportGrants(ctx context.Context) error {
	st, ok := s.cfg.Store.(store.SupportGrantStore)
	if !ok {
		return nil
	}
	ended, err := st.SweepSupportGrants(ctx, !envEnabled(envSupportSessions))
	for _, g := range ended {
		// The store marked g audited when it claimed it, so a row lost here is not written again. Accepted
		// means written or held by the audit spool, which retries it (recordAuditDurably).
		if !s.recordAuditDurably(ctx, s.supportEndEvent(types.ActorSystem, "system", g)) {
			slog.ErrorContext(ctx, "wardynd: a support.end row was not accepted by the audit sink or its spool", "support_grant", g.ID, "run_id", g.RunID)
		}
	}
	refs, lerr := st.LiveSupportGrants(ctx)
	if lerr == nil {
		s.replaceSupportWindow(refs)
	}
	if err != nil {
		return err
	}
	return lerr
}

// supportEndEvent is the support.end row of g: its actor, its stored reason and when it ended.
func (s *Server) supportEndEvent(actorType types.ActorType, actor string, g store.SupportGrant) types.AuditEvent {
	data := map[string]any{"support_grant": g.ID, "helper": g.Helper, "reason": g.EndReason}
	if g.EndedAt != nil {
		data["ended_at"] = g.EndedAt.UTC()
	}
	return s.auditEvent(&g.RunID, actorType, actor, "support.end", g.ID.String(), "success", mustJSON(data))
}

// supportGrantView is a grant as the routes answer it. Reason is the helper's own words, shown to the owner and
// the helper only; it never rides an audit row.
type supportGrantView struct {
	Grant       uuid.UUID  `json:"grant"`
	Helper      string     `json:"helper"`
	Reason      string     `json:"reason,omitempty"`
	DoorsAsked  []string   `json:"doors_asked"`
	Doors       []string   `json:"doors,omitempty"`
	Minutes     int        `json:"minutes,omitempty"`
	RequestedAt time.Time  `json:"requested_at"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	LastSeenAt  *time.Time `json:"last_seen_at,omitempty"`
	EndedAt     *time.Time `json:"ended_at,omitempty"`
	// State is requested (waiting for the owner), active (accepted, not ended) or ended.
	State     string `json:"state"`
	EndReason string `json:"end_reason,omitempty"`
	// RecordedDoors, on a grant that is not ended, is the doors Wardyn records on this run (terminal, desktop).
	RecordedDoors []string `json:"recorded_doors,omitempty"`
}

func (s *Server) supportView(g store.SupportGrant, run types.AgentRun, person string, owner bool) supportGrantView {
	v := supportGrantView{Grant: g.ID, Helper: g.Helper, DoorsAsked: g.DoorsAsked, Doors: g.Doors, Minutes: g.Minutes,
		RequestedAt: g.RequestedAt, ExpiresAt: g.ExpiresAt, LastSeenAt: g.LastSeenAt, EndedAt: g.EndedAt, EndReason: g.EndReason}
	switch {
	case g.EndedAt != nil:
		v.State = "ended"
	case g.AcceptedAt == nil:
		v.State = "requested"
	default:
		v.State = "active"
	}
	// The helper's own words show to the owner, and to the helper while the grant is open; once it has ended the
	// helper reads its state and end reason, not the text.
	if owner || (person != "" && person == g.Helper && g.EndedAt == nil) {
		v.Reason = g.Reason
	}
	if g.EndedAt == nil {
		v.RecordedDoors = s.supportRecordedDoors(run)
	}
	return v
}

// supportRecordedDoors is what the owner's consent screen is told is recorded: terminal when a session the
// server opens on the run would be recorded (newSessionRecorder's conditions), desktop when its stream is.
func (s *Server) supportRecordedDoors(run types.AgentRun) []string {
	var doors []string
	if s.cfg.RecordingStore != nil && !runIsUnrecordable(run) && run.NotRecorded != types.NotRecordedPolicyOff &&
		run.RecordingLevel != types.RecordingOff {
		doors = append(doors, string(supportDoorTerminal))
	}
	if s.desktopRecorded(run) {
		doors = append(doors, string(supportDoorDesktop))
	}
	return doors
}

// supportDoorsOffered is the doors a request may ask for: all six, but not web_apps or desktop in shared-origin
// (path) mode, where every run's apps share one origin.
func (s *Server) supportDoorsOffered(run types.AgentRun) []string {
	if s.uiRunOrigin(run.ID) != "" {
		return slices.Clone(supportDoorNames)
	}
	return slices.Clone(supportDoorNames[:4])
}

// mountSupportRoutes registers the five support routes on the authenticated group. All are passive run routes:
// they open no interactive surface (run_mode_doors.go), and the helper's doors are the nine entry points
// supportMayEnter names.
func (s *Server) mountSupportRoutes(r chi.Router) {
	r.Post("/runs/{id}/support", s.handleSupportRequest)
	r.Get("/runs/{id}/support", s.handleSupportStatus)
	r.Post("/runs/{id}/support/{grant}/accept", s.handleSupportAccept)
	r.Post("/runs/{id}/support/{grant}/decline", s.handleSupportDecline)
	r.Post("/runs/{id}/support/{grant}/end", s.handleSupportEnd)
}

// supportIneligibleMsg is the sentence for a run supportRunIneligible turned away.
func supportIneligibleMsg(kind string) string {
	switch kind {
	case supportIneligibleLocalMode:
		return "Support sessions need two signed-in people, which local mode does not have."
	case supportIneligibleNotRunning:
		return "This run is not running, so nobody can help inside it."
	case supportIneligibleLost:
		return "This run's machine is lost, so nobody can help inside it."
	case supportIneligibleOperator:
		return "This run has no personal owner, and admins can already open it."
	case supportIneligibleService:
		return "This run belongs to a service identity, which has no person to answer a request."
	case supportIneligibleReserved:
		return "This run is a sign-in or a workspace step, which belongs to its owner alone, so nobody can help inside it."
	}
	return "This run is a background task, which cannot take a helper."
}

// refuseSupportGate answers the two refusals every request and accept meets first: local mode (409
// support_run_ineligible, ahead of the switch, since there is no second person to ask) and the switch (409
// support_disabled). It reports whether it answered.
func (s *Server) refuseSupportGate(w http.ResponseWriter) bool {
	if s.cfg.LocalMode {
		writeErrorReason(w, http.StatusConflict, reasonSupportRunIneligible, supportIneligibleMsg(supportIneligibleLocalMode))
		return true
	}
	if !envEnabled(envSupportSessions) {
		writeErrorReason(w, http.StatusConflict, reasonSupportDisabled, supportDisabledMsg)
		return true
	}
	return false
}

// supportGrantStore is the store's grant record, or the 503 when this store keeps none.
func (s *Server) supportGrantStore(w http.ResponseWriter) (store.SupportGrantStore, bool) {
	st, ok := s.cfg.Store.(store.SupportGrantStore)
	if !ok {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonSupportUnavailable, "this deployment keeps no support sessions")
	}
	return st, ok
}

// refuseNotSupportParty answers a caller who is none of the run's owner, the grant's helper or a security
// operator. An admin, or the owner on a sign-in that may not answer (a personal token alone), learns why with a
// 403 support_not_allowed; anyone else gets the foreign-run 404, so the route is no existence oracle.
func (s *Server) refuseNotSupportParty(w http.ResponseWriter, r *http.Request, run types.AgentRun) {
	ctx := r.Context()
	if s.isOperator(ctx) || (run.CreatedBy != "" && oidcHumanFromContext(ctx) == run.CreatedBy) {
		writeErrorReason(w, http.StatusForbidden, reasonSupportNotAllowed, "only the person who started this run can answer a help request, from the console or a CLI sign-in")
		return
	}
	s.refuse(w, r, authz.Deny(authz.ReasonNotOwner, run.ID.String(), "run not found").OnRun(run.ID).AsIf(authz.Reason(reasonRunNotFound)))
}

// supportOwnerRead is the run's owner when the caller is that person on any sign-in but a portal's delegated
// request, a personal token included: ending a grant only restricts, and reading its state decides nothing.
func supportOwnerRead(ctx context.Context, run types.AgentRun) bool {
	return !delegatedRequest(ctx) && run.CreatedBy != "" && oidcHumanFromContext(ctx) == run.CreatedBy
}

// supportOwner is the run's owner when the caller is that person on a sign-in that may answer, else "".
func supportOwner(ctx context.Context, run types.AgentRun) string {
	if p := supportPerson(ctx); p != "" && p == run.CreatedBy {
		return p
	}
	return ""
}

// validSupportDoors reports whether doors is a non-empty list of distinct names from the six.
func validSupportDoors(doors []string) bool {
	if len(doors) == 0 {
		return false
	}
	seen := map[string]bool{}
	for _, d := range doors {
		if !slices.Contains(supportDoorNames, d) || seen[d] {
			return false
		}
		seen[d] = true
	}
	return true
}

// recordSupport writes a support.* row for the caller of r, on run, naming the grant.
func (s *Server) recordSupport(r *http.Request, run types.AgentRun, action string, g store.SupportGrant, data map[string]any) {
	at, principal := actorFromRequest(r)
	data["support_grant"] = g.ID
	s.recordAudit(r.Context(), s.auditEvent(&run.ID, at, principal, action, g.ID.String(), "success", mustJSON(data)))
}

type supportRequestBody struct {
	Doors  []string `json:"doors"`
	Reason string   `json:"reason"`
}

// handleSupportRequest records a super admin's request to help inside a run:
//
//	POST /api/v1/runs/{id}/support  {doors, reason}
//
// The caller is a super admin signed in as a person, not the run's own owner; the run must be one a helper can
// enter (supportRunIneligible). The owner answers within 15 minutes (accept or decline), or the request lapses.
// A request names no lifetime: the owner picks it.
func (s *Server) handleSupportRequest(w http.ResponseWriter, r *http.Request) {
	authorizedAt := s.cfg.Now().UTC()
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok || s.refuseSupportGate(w) {
		return
	}
	ctx := r.Context()
	if !s.supportCaller(ctx) {
		writeErrorReason(w, http.StatusForbidden, reasonSupportNotAllowed, "only a super admin signed in as a person can ask to help inside a run")
		return
	}
	var body supportRequestBody
	if !decodeStrict(w, r, &body) {
		return
	}
	body.Reason = strings.TrimSpace(body.Reason)
	switch {
	case !validSupportDoors(body.Doors):
		writeErrorReasonField(w, http.StatusBadRequest, reasonSupportRequestBad, "doors must name at least one of "+strings.Join(supportDoorNames, ", ")+", each once", "doors")
		return
	case body.Reason == "" || utf8.RuneCountInString(body.Reason) > supportMaxReason:
		writeErrorReasonField(w, http.StatusBadRequest, reasonSupportRequestBad, "reason must be 1 to 500 characters", "reason")
		return
	}
	run, ok := s.getRunOr404(w, r, id)
	if !ok {
		return
	}
	helper := oidcHumanFromContext(ctx)
	if run.CreatedBy == helper {
		writeErrorReason(w, http.StatusForbidden, reasonSupportNotAllowed, "you started this run, so you do not need to ask")
		return
	}
	if kind := s.supportRunIneligible(run); kind != "" {
		writeErrorReason(w, http.StatusConflict, reasonSupportRunIneligible, supportIneligibleMsg(kind))
		return
	}
	offered := s.supportDoorsOffered(run)
	if slices.ContainsFunc(body.Doors, func(d string) bool { return !slices.Contains(offered, d) }) {
		writeErrorReason(w, http.StatusForbidden, reasonInboundNeedsHostMode, "This deployment serves apps from one shared host, so web apps and the desktop cannot be shared with a helper.")
		return
	}
	st, ok := s.supportGrantStore(w)
	if !ok {
		return
	}
	issue, err := s.ticketIssue(ctx, helper, authorizedAt)
	if err != nil {
		writeServerError(w, r, "support request", err)
		return
	}
	g, err := st.RequestSupportGrant(ctx, store.SupportGrant{RunID: run.ID, Helper: helper, HelperEmail: oidcEmailFromContext(ctx),
		Reason: body.Reason, DoorsAsked: body.Doors, RequestRevSeq: issue.Seq})
	if errors.Is(err, store.ErrSupportGrantOpen) {
		writeErrorReason(w, http.StatusConflict, reasonSupportGrantOpen, "this run already has a help request or session open")
		return
	}
	if err != nil {
		writeServerError(w, r, "support request", err)
		return
	}
	s.recordSupport(r, run, "support.request", g, map[string]any{"doors_asked": g.DoorsAsked})
	writeJSON(w, http.StatusCreated, s.supportView(g, run, helper, false))
}

type supportAcceptBody struct {
	Doors   []string `json:"doors"`
	Minutes int      `json:"minutes"`
}

// handleSupportAccept is the run owner's yes:
//
//	POST /api/v1/runs/{id}/support/{grant}/accept  {doors, minutes}
//
// doors is a non-empty subset of those asked and minutes 15, 30 or 60. Only the run's creator, from a console
// session or a CLI sign-in, may accept; the store checks the owner, the run's state and the 15-minute window
// again in the same statement.
func (s *Server) handleSupportAccept(w http.ResponseWriter, r *http.Request) {
	authorizedAt := s.cfg.Now().UTC()
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	grant, ok := parseIDParam(w, r, "grant", "support grant")
	if !ok || s.refuseSupportGate(w) {
		return
	}
	run, ok := s.getRunOr404(w, r, id)
	if !ok {
		return
	}
	ctx := r.Context()
	owner := supportOwner(ctx, run)
	if owner == "" {
		s.refuseNotSupportParty(w, r, run)
		return
	}
	var body supportAcceptBody
	if !decodeStrict(w, r, &body) {
		return
	}
	if !validSupportDoors(body.Doors) {
		writeErrorReasonField(w, http.StatusBadRequest, reasonSupportRequestBad, "doors must name at least one door the helper asked for, each once", "doors")
		return
	}
	if !slices.Contains(supportMinutes, body.Minutes) {
		writeErrorReasonField(w, http.StatusBadRequest, reasonSupportRequestBad, "minutes must be 15, 30 or 60", "minutes")
		return
	}
	if kind := s.supportRunIneligible(run); kind != "" {
		writeErrorReason(w, http.StatusConflict, reasonSupportRunIneligible, supportIneligibleMsg(kind))
		return
	}
	st, ok := s.supportGrantStore(w)
	if !ok {
		return
	}
	open, _, err := st.SupportGrantStatus(ctx, run.ID)
	if err != nil {
		writeServerError(w, r, "support accept", err)
		return
	}
	if open == nil || open.ID != grant || open.AcceptedAt != nil {
		writeErrorReason(w, http.StatusNotFound, reasonSupportGrantNotFound, supportGrantNotFoundMsg)
		return
	}
	if slices.ContainsFunc(body.Doors, func(d string) bool { return !slices.Contains(open.DoorsAsked, d) }) {
		writeErrorReasonField(w, http.StatusBadRequest, reasonSupportRequestBad, "doors must be among those the helper asked for", "doors")
		return
	}
	issue, err := s.ticketIssue(ctx, owner, authorizedAt)
	if err != nil {
		writeServerError(w, r, "support accept", err)
		return
	}
	g, err := st.AcceptSupportGrant(ctx, run.ID, grant, owner, oidcEmailFromContext(ctx), body.Doors, body.Minutes, issue.Seq)
	if errors.Is(err, store.ErrNotFound) {
		writeErrorReason(w, http.StatusNotFound, reasonSupportGrantNotFound, supportGrantNotFoundMsg)
		return
	}
	if err != nil {
		writeServerError(w, r, "support accept", err)
		return
	}
	s.setSupportWindow(run.ID, g.ID)
	s.publishNotice(livebus.KindSupportGrant, run.ID, nil)
	data := map[string]any{"helper": g.Helper, "doors": g.Doors, "minutes": g.Minutes, "recorded_doors": s.supportRecordedDoors(run)}
	if g.ExpiresAt != nil {
		data["expires_at"] = g.ExpiresAt.UTC()
	}
	s.recordSupport(r, run, "support.accept", g, data)
	writeJSON(w, http.StatusOK, s.supportView(g, run, owner, true))
}

const supportGrantNotFoundMsg = "No such open help request on this run: it may have been answered, ended or lapsed"

// handleSupportDecline is the run owner's no, on a request not yet accepted:
//
//	POST /api/v1/runs/{id}/support/{grant}/decline
func (s *Server) handleSupportDecline(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	grant, ok := parseIDParam(w, r, "grant", "support grant")
	if !ok {
		return
	}
	run, ok := s.getRunOr404(w, r, id)
	if !ok {
		return
	}
	owner := supportOwner(r.Context(), run)
	if owner == "" {
		s.refuseNotSupportParty(w, r, run)
		return
	}
	st, ok := s.supportGrantStore(w)
	if !ok {
		return
	}
	g, err := st.DeclineSupportGrant(r.Context(), run.ID, grant, owner)
	if errors.Is(err, store.ErrNotFound) {
		writeErrorReason(w, http.StatusNotFound, reasonSupportGrantNotFound, supportGrantNotFoundMsg)
		return
	}
	if err != nil {
		writeServerError(w, r, "support decline", err)
		return
	}
	s.recordSupport(r, run, "support.decline", g, map[string]any{"helper": g.Helper})
	writeJSON(w, http.StatusOK, s.supportView(g, run, owner, true))
}

// handleSupportEnd ends an open grant, requested or accepted:
//
//	POST /api/v1/runs/{id}/support/{grant}/end
//
// By the run's owner (reason revoked), the grant's helper (ended_by_helper) or a security operator
// (ended_by_security). This route writes the support.end row itself, so the store marks the end audited and
// the sweep writes none.
func (s *Server) handleSupportEnd(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	grant, ok := parseIDParam(w, r, "grant", "support grant")
	if !ok {
		return
	}
	run, ok := s.getRunOr404(w, r, id)
	if !ok {
		return
	}
	st, ok := s.supportGrantStore(w)
	if !ok {
		return
	}
	ctx := r.Context()
	open, _, err := st.SupportGrantStatus(ctx, run.ID)
	if err != nil {
		writeServerError(w, r, "support end", err)
		return
	}
	person := supportPerson(ctx)
	reason := ""
	switch {
	case supportOwnerRead(ctx, run):
		reason = supportEndRevoked
	case open != nil && open.ID == grant && person != "" && person == open.Helper:
		reason = supportEndByHelper
	case s.isSecurityOperator(ctx):
		reason = supportEndBySecurity
	default:
		s.refuseNotSupportParty(w, r, run)
		return
	}
	if open == nil || open.ID != grant {
		writeErrorReason(w, http.StatusNotFound, reasonSupportGrantNotFound, supportGrantNotFoundMsg)
		return
	}
	g, err := st.EndSupportGrant(ctx, run.ID, grant, reason, true)
	if errors.Is(err, store.ErrNotFound) {
		writeErrorReason(w, http.StatusNotFound, reasonSupportGrantNotFound, supportGrantNotFoundMsg)
		return
	}
	if err != nil {
		writeServerError(w, r, "support end", err)
		return
	}
	s.clearSupportWindow(run.ID)
	s.publishNotice(livebus.KindSupportGrant, run.ID, nil)
	at, principal := actorFromRequest(r)
	s.recordAudit(ctx, s.supportEndEvent(at, principal, g))
	writeJSON(w, http.StatusOK, s.supportView(g, run, person, reason == supportEndRevoked))
}

// handleSupportStatus reports a run's help request or session, and the last one that ended:
//
//	GET /api/v1/runs/{id}/support  ->  {open, ended, doors_offered, recorded_doors}
//
// Readable by the run's owner (a personal token included), the helper of the open grant, a security operator
// (every super admin is one, so any support caller reads doors_offered before a grant exists), and the helper
// of the last ended grant, who sees that grant alone and never its reason text; anyone else gets the foreign-run
// 404. recorded_doors is the doors this run would record, as support.accept computes it.
func (s *Server) handleSupportStatus(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	run, ok := s.getRunOr404(w, r, id)
	if !ok {
		return
	}
	st, ok := s.supportGrantStore(w)
	if !ok {
		return
	}
	open, ended, err := st.SupportGrantStatus(r.Context(), run.ID)
	if err != nil {
		writeServerError(w, r, "support status", err)
		return
	}
	ctx := r.Context()
	person := supportPerson(ctx)
	owner := supportOwnerRead(ctx, run)
	if !owner && !s.isSecurityOperator(ctx) && (open == nil || person == "" || person != open.Helper) {
		if ended != nil && person != "" && person == ended.Helper {
			writeJSON(w, http.StatusOK, map[string]any{"ended": s.supportView(*ended, run, person, false), "doors_offered": s.supportDoorsOffered(run)})
			return
		}
		s.refuse(w, r, authz.Deny(authz.ReasonNotOwner, run.ID.String(), "run not found").OnRun(run.ID).AsIf(authz.Reason(reasonRunNotFound)))
		return
	}
	out := map[string]any{"open": nil, "ended": nil, "doors_offered": s.supportDoorsOffered(run),
		"recorded_doors": append([]string{}, s.supportRecordedDoors(run)...)}
	if open != nil {
		out["open"] = s.supportView(*open, run, person, owner)
	}
	if ended != nil {
		out["ended"] = s.supportView(*ended, run, person, owner)
	}
	writeJSON(w, http.StatusOK, out)
}
