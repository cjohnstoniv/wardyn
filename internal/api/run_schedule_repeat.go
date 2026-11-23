// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Repeating schedules (#3458): a person's run repeated daily, on weekdays or weekly at one wall-clock time in one
// zone. Each occurrence is booked as a one-time scheduled run (run_schedule.go) a short lead before its time,
// through the create path as the owner, so every start is checked as policy stands then.

const (
	scheduleFireLead  = 2 * time.Minute     // an occurrence is booked this long before it is due
	scheduleOwnerIdle = 30 * 24 * time.Hour // no SSO sign-in for this long pauses the owner's schedules
	runScheduleLimit  = 20                  // schedules per owner
	// scheduleSweepBatch is the most schedules one tick books, one create at a time.
	scheduleSweepBatch = 50
	// scheduleReplayWindow is how long a missed occurrence's booking is replayed under its key: the create
	// keeps an Idempotency-Key's answer this long (handleCreateRunRoute).
	scheduleReplayWindow = 24 * time.Hour
)

// Why a schedule paused (run_schedules.paused_reason). A booking refused 401, 403 or 409 admin_view pauses
// with scheduleRefusedPrefix and the refusal's reason.
const (
	schedulePausedByOwner        = "by_owner"
	schedulePausedByAdmin        = "by_admin"
	schedulePausedOwnerIdle      = "owner_idle"
	schedulePausedIdentity       = "identity_inactive"
	schedulePausedGroupsChanged  = "groups_changed"
	scheduleRefusedPrefix        = "refused:"
	scheduleOutcomeBooked        = "booked"
	scheduleOutcomeMissed        = "missed"
	scheduleOutcomeRefused       = "refused"
	scheduleOutcomeSkippedPrefix = "skipped:"
	scheduleOutcomeCancelled     = "cancelled:"
)

// Why the sweep cancelled a booking it made, or recorded a replayed one as not booked: the kill's reason, the
// fire row's, and after scheduleOutcomeCancelled the slot's outcome. A booking whose owner may no longer act is
// killed session_revoked, as their queued runs are when their credentials are revoked (cancelQueuedRunsOf).
const (
	scheduleCancelGone   = "schedule_gone"
	scheduleCancelPaused = "schedule_paused"
	scheduleCancelUnread = "schedule_unread"
	scheduleRunEnded     = "run_ended" // a replayed booking whose run ended before it started
)

// Every rule a schedule may repeat by.
const (
	scheduleEveryDaily    = "daily"
	scheduleEveryWeekdays = "weekdays"
	scheduleEveryWeekly   = "weekly"
)

// runScheduleStore is the store a repeating schedule needs: its own rows, the owner's latest SSO sign-in
// and its groups, and the blank of one booked run.
type runScheduleStore interface {
	CreateRunSchedule(ctx context.Context, sc store.RunSchedule, limit int) error
	ListRunSchedules(ctx context.Context, owner string) ([]store.RunSchedule, error)
	GetRunSchedule(ctx context.Context, id uuid.UUID) (store.RunSchedule, error)
	PauseRunSchedule(ctx context.Context, id uuid.UUID, reason string, at time.Time) (bool, error)
	ResumeRunSchedule(ctx context.Context, id uuid.UUID, authority json.RawMessage, authorityAt, next time.Time) (bool, error)
	DeleteRunSchedule(ctx context.Context, id uuid.UUID) (bool, error)
	DueRunSchedules(ctx context.Context, before time.Time, limit int) ([]store.RunSchedule, error)
	AdvanceRunSchedule(ctx context.Context, id uuid.UUID, wasNext, next time.Time, runID *uuid.UUID, outcome string) (bool, error)
	ScheduleOwnerSignedInAt(ctx context.Context, principal string) (time.Time, bool, error)
	GetPrincipalGroups(ctx context.Context, principal, portal string) (store.PrincipalGroups, error)
	BlankRunTaskByID(ctx context.Context, runID uuid.UUID) error
}

// runScheduleEraser deletes a person's schedules (eraseRunSchedulesOf).
type runScheduleEraser interface {
	DeleteRunSchedulesOf(ctx context.Context, owner string) (int, error)
}

var (
	_ runScheduleStore  = store.PG{}
	_ runScheduleEraser = store.PG{}
)

// eraseRunSchedulesOf deletes every schedule the person owns. A schedule holds their task text and their identity
// snapshot (email and IdP groups), so the run tasks erasure deletes it, and so does the credentials erasure beside
// the login group snapshot, which a leaver purge runs.
func (s *Server) eraseRunSchedulesOf(ctx context.Context, person string) error {
	se, ok := s.cfg.Store.(runScheduleEraser)
	if !ok {
		return nil
	}
	_, err := se.DeleteRunSchedulesOf(ctx, person)
	return err
}

type scheduleRule struct {
	Every     string       // "daily" | "weekdays" | "weekly"
	Weekday   time.Weekday // weekly only
	Hour, Min int
	Zone      *time.Location
}

// scheduleLocalTime is a schedule's wall-clock time, as the table's own check reads it.
var scheduleLocalTime = regexp.MustCompile(`^([01][0-9]|2[0-3]):([0-5][0-9])$`)

// parseScheduleRule reads a rule from its wire form; false when every, the time or the zone is not one a
// schedule takes. "Local" is refused: it is the server's zone, not one the person chose.
func parseScheduleRule(every, hhmm, zone string) (scheduleRule, bool) {
	switch every {
	case scheduleEveryDaily, scheduleEveryWeekdays, scheduleEveryWeekly:
	default:
		return scheduleRule{}, false
	}
	m := scheduleLocalTime.FindStringSubmatch(hhmm)
	if m == nil || zone == "" || zone == "Local" {
		return scheduleRule{}, false
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return scheduleRule{}, false
	}
	h, _ := strconv.Atoi(m[1])  //nolint:errcheck // two digits, matched above
	mi, _ := strconv.Atoi(m[2]) //nolint:errcheck // two digits, matched above
	return scheduleRule{Every: every, Hour: h, Min: mi, Zone: loc}, true
}

// scheduleRuleOf is a stored schedule's rule, its weekly weekday read back from the row.
func scheduleRuleOf(sc store.RunSchedule) (scheduleRule, error) {
	rule, ok := parseScheduleRule(sc.Every, sc.LocalTime, sc.TimeZone)
	if !ok {
		return scheduleRule{}, fmt.Errorf("schedule %s has a rule this server cannot read (%s %s %s)", sc.ID, sc.Every, sc.LocalTime, sc.TimeZone)
	}
	if rule.Every == scheduleEveryWeekly {
		if sc.Weekday == nil {
			return scheduleRule{}, fmt.Errorf("weekly schedule %s has no weekday", sc.ID)
		}
		rule.Weekday = time.Weekday(*sc.Weekday)
	}
	return rule, nil
}

// nextOccurrence is the first start of r strictly after `after`. A wall time the zone's clocks skip or repeat on
// some day starts at the instant time.Date gives for that day: Go's normalisation applies to every start.
func (r scheduleRule) nextOccurrence(after time.Time) time.Time {
	y, m, d := after.In(r.Zone).Date()
	for i := 0; i <= 8; i++ {
		c := time.Date(y, m, d+i, r.Hour, r.Min, 0, 0, r.Zone)
		if !c.After(after) {
			continue
		}
		switch wd := c.Weekday(); r.Every {
		case scheduleEveryWeekdays:
			if wd == time.Saturday || wd == time.Sunday {
				continue
			}
		case scheduleEveryWeekly:
			if wd != r.Weekday {
				continue
			}
		}
		return c
	}
	return time.Time{} // unreachable for a rule parseScheduleRule accepted: eight days hold every weekday
}

// firstSlot is the schedule's first start after now. For weekly, the weekday is the first slot's own weekday in the
// zone: the next instant at the wall time, as a daily rule finds it, so a weekly schedule made just after its time
// starts the next day and repeats on that weekday.
func (r *scheduleRule) firstSlot(now time.Time) time.Time {
	if r.Every != scheduleEveryWeekly {
		return r.nextOccurrence(now)
	}
	daily := scheduleRule{Every: scheduleEveryDaily, Hour: r.Hour, Min: r.Min, Zone: r.Zone}
	first := daily.nextOccurrence(now)
	r.Weekday = first.In(r.Zone).Weekday()
	return first
}

// narrowScheduleGroups is the stored groups that the owner's latest SSO sign-in still carries, and whether that
// sign-in holds a group the stored list does not (#3458). A gained group is never dropped here: the caller pauses the
// schedule, because a DENY grant on that group must apply. A partial list on either side marks the result partial,
// as store.PG.NarrowPrincipalGroups does. A record with no list (a stored JSON null) removes nothing, gains nothing
// and marks the result partial.
func narrowScheduleGroups(stored []string, storedTruncated bool, rec store.PrincipalGroups) (kept []string, truncated, gained bool) {
	if rec.Groups == nil {
		return stored, true, false
	}
	for _, g := range rec.Groups {
		if !slices.Contains(stored, g) {
			gained = true
			break
		}
	}
	if stored == nil {
		return nil, storedTruncated || rec.Truncated, gained
	}
	kept = make([]string, 0, len(stored))
	for _, g := range stored {
		if slices.Contains(rec.Groups, g) {
			kept = append(kept, g)
		}
	}
	return kept, storedTruncated || rec.Truncated, gained
}

// runScheduleView is a schedule on the wire. It never carries the stored request or authority.
type runScheduleView struct {
	ID       uuid.UUID             `json:"id"`
	Name     string                `json:"name"`
	Every    string                `json:"every"`
	Weekday  *int                  `json:"weekday,omitempty"`
	Time     string                `json:"time"`
	TimeZone string                `json:"time_zone"`
	NextAt   time.Time             `json:"next_at"`
	Paused   *runSchedulePauseView `json:"paused,omitempty"`
	Last     *runScheduleLastView  `json:"last,omitempty"`
	Owner    string                `json:"owner,omitempty"` // the admin view only
}

type runSchedulePauseView struct {
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
}

type runScheduleLastView struct {
	DueAt   time.Time  `json:"due_at"`
	RunID   *uuid.UUID `json:"run_id,omitempty"`
	Outcome string     `json:"outcome"`
}

func runScheduleViewOf(sc store.RunSchedule, withOwner bool) runScheduleView {
	v := runScheduleView{ID: sc.ID, Name: sc.Name, Every: sc.Every, Weekday: sc.Weekday, Time: sc.LocalTime,
		TimeZone: sc.TimeZone, NextAt: sc.NextAt}
	if sc.PausedAt != nil {
		v.Paused = &runSchedulePauseView{At: *sc.PausedAt, Reason: sc.PausedReason}
	}
	if sc.LastDueAt != nil {
		v.Last = &runScheduleLastView{DueAt: *sc.LastDueAt, RunID: sc.LastRunID, Outcome: sc.LastOutcome}
	}
	if withOwner {
		v.Owner = sc.Owner
	}
	return v
}

// mountRunScheduleRoutes mounts the repeating schedule routes on the authenticated group. Each handler scopes to
// the caller's own schedules; an operator (isSecurityOperator) may also list, pause and delete another person's.
func (s *Server) mountRunScheduleRoutes(r chi.Router) {
	r.Get("/run-schedules", s.handleListRunSchedules)
	r.Post("/run-schedules", s.handleCreateRunSchedule)
	r.Post("/run-schedules/{id}/pause", s.handlePauseRunSchedule)
	r.Post("/run-schedules/{id}/resume", s.handleResumeRunSchedule)
	r.Delete("/run-schedules/{id}", s.handleDeleteRunSchedule)
}

// runScheduleStoreOr is the schedule store, or a 503 scheduled_runs_unavailable when the store keeps none.
func (s *Server) runScheduleStoreOr(w http.ResponseWriter) (runScheduleStore, bool) {
	st, ok := s.cfg.Store.(runScheduleStore)
	if !ok {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonScheduledRunsUnavailable, scheduledRunsUnavailableMsg)
	}
	return st, ok
}

// runScheduleOf reads the {id} schedule; a missing one is the 404 every caller who may not see it also gets.
func runScheduleOf(w http.ResponseWriter, r *http.Request, st runScheduleStore) (store.RunSchedule, bool) {
	id, ok := parseIDParam(w, r, "id", "schedule")
	if !ok {
		return store.RunSchedule{}, false
	}
	sc, err := st.GetRunSchedule(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErrorReason(w, http.StatusNotFound, reasonRunScheduleNotFound, runScheduleNotFoundMsg)
		return store.RunSchedule{}, false
	}
	if err != nil {
		writeServerError(w, r, "read run schedule", err)
		return store.RunSchedule{}, false
	}
	return sc, true
}

// handleListRunSchedules is GET /run-schedules: the caller's own, or with ?view=admin every schedule for an
// operator. A non-operator's view=admin fails closed to their own, as the runs list does.
func (s *Server) handleListRunSchedules(w http.ResponseWriter, r *http.Request) {
	st, ok := s.runScheduleStoreOr(w)
	if !ok {
		return
	}
	ctx := r.Context()
	admin := r.URL.Query().Get("view") == "admin" && s.isSecurityOperator(ctx)
	owner := oidcHumanFromContext(ctx)
	out := []runScheduleView{}
	if !admin && owner == "" {
		writeJSON(w, http.StatusOK, out) // a caller with no SSO identity keeps no schedule
		return
	}
	if admin {
		owner = ""
	}
	rows, err := st.ListRunSchedules(ctx, owner)
	if err != nil {
		writeServerError(w, r, "list run schedules", err)
		return
	}
	for _, sc := range rows {
		out = append(out, runScheduleViewOf(sc, admin))
	}
	writeJSON(w, http.StatusOK, out)
}

// runScheduleBody is POST /run-schedules' body. It has no weekday: a weekly schedule repeats on its first
// slot's weekday (scheduleRule.firstSlot).
type runScheduleBody struct {
	Name     string          `json:"name"`
	Every    string          `json:"every"`
	Time     string          `json:"time"`
	TimeZone string          `json:"time_zone"`
	Request  json.RawMessage `json:"request"`
}

// decodeRunScheduleBody reads the body strictly with the create's body cap. A bad rule, a request that is not
// an object, or one carrying start_at or start_time_zone is a 400 run_schedule_invalid. A name longer than a
// run title may be is a 400 run_field_too_long, as the title's is.
func decodeRunScheduleBody(w http.ResponseWriter, r *http.Request) (runScheduleBody, scheduleRule, bool) {
	var in runScheduleBody
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody))
	dec.DisallowUnknownFields()
	var fields map[string]json.RawMessage
	rule, ok := scheduleRule{}, false
	if dec.Decode(&in) == nil && json.Unmarshal(in.Request, &fields) == nil && fields != nil {
		_, hasStart := fields["start_at"]
		_, hasZone := fields["start_time_zone"]
		if !hasStart && !hasZone {
			rule, ok = parseScheduleRule(in.Every, in.Time, in.TimeZone)
		}
	}
	if !ok {
		writeErrorReason(w, http.StatusBadRequest, reasonRunScheduleInvalid, runScheduleInvalidMsg)
		return in, rule, false
	}
	if n := utf8.RuneCountInString(in.Name); n > maxRunTitleLen {
		writeErrorReason(w, http.StatusBadRequest, reasonRunFieldTooLong, fmt.Sprintf(runFieldTooLongRefusal, "name", n, maxRunTitleLen))
		return in, rule, false
	}
	return in, rule, true
}

// scheduleOwnerAuthority is the caller's authority when they may keep a schedule: a person signed in through
// SSO on a session. A service identity, a portal or device launch, break-glass, a namespace binding, a workload,
// local mode, the admin token and every API token (a wdn_ token of any role, cli_access, a cli_admin step-up) are
// refused 422 run_schedule_owner_unsupported: queueOwnerLive names no token, so a schedule must never outlive
// one.
func scheduleOwnerAuthority(w http.ResponseWriter, r *http.Request) (queueAuthority, bool) {
	a := captureQueueAuthority(r.Context())
	if a.Sub == "" || a.Service != "" || a.Delegate != nil || a.Device != nil || a.BreakGlass != "" || a.Binding != nil ||
		a.WorkloadName != "" || a.portalIssuerLaunch() || apiTokenIDFromContext(r.Context()) != uuid.Nil {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonRunScheduleOwnerUnsupported, runScheduleOwnerUnsupportedMsg)
		return queueAuthority{}, false
	}
	return a, true
}

// withScheduleStart is a stored request with start_at and start_time_zone set: the body one booking posts.
// Keys marshal in order, so the same schedule and slot always give the same bytes.
func withScheduleStart(request json.RawMessage, startAt, zone string) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(request, &fields); err != nil || fields == nil {
		return nil, fmt.Errorf("stored schedule request is not an object: %v", err)
	}
	fields["start_at"] = mustJSON(startAt)
	fields["start_time_zone"] = mustJSON(zone)
	return json.Marshal(fields)
}

// schedulePreflight runs the launch's refusals, dry and with no row, on request as a one-time scheduled run
// starting a representative near time: the seven-day cap is met by each slot at its own booking. The context
// choice slot is installed as the create installs it, so a unit the request pins is the one judged. A resume
// asks the scheduled-runs door first, with its own sentence. It writes its own refusal and reports false.
func (s *Server) schedulePreflight(w http.ResponseWriter, r *http.Request, request json.RawMessage, zone string, resume bool) bool {
	start := s.cfg.Now().Add(scheduleFireLead + time.Minute).UTC().Format(time.RFC3339)
	body, err := withScheduleStart(request, start, zone)
	if err != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonRunScheduleInvalid, runScheduleInvalidMsg)
		return false
	}
	r2 := r.Clone(r.Context())
	r2.Body, r2.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
	r2 = withContextChoiceSlot(r2)
	req, ceiling, reqCC, _, ok := s.decodeAndValidateCreateRun(w, r2)
	if !ok {
		return false
	}
	if resume {
		if ref := s.scheduleResumeDoorRefusal(r2.Context(), ceiling); ref != nil {
			ref.write(s, w, r2)
			return false
		}
	}
	_, ok = s.foldRunRequest(w, r2, foldPreflight, &req, ceiling, reqCC)
	return ok
}

// scheduleResumeDoorRefusal is scheduleDoorRefusal with the resume's own sentence, so the 403 body and the
// authz.denied row it records both say why a schedule cannot resume, not "Start the run now".
func (s *Server) scheduleResumeDoorRefusal(ctx context.Context, c governanceCeiling) *runRefusal {
	if !c.Limits.DenyScheduledRuns || s.runUngoverned(ctx) {
		return nil
	}
	d := authz.Deny(authz.ReasonGovernanceUnit, "runs.schedule", runScheduleResumeDeniedMsg).WithPolicy(s.ceilingPolicy(ctx, c))
	if c.Unit != nil {
		d = d.OnKnob(s.runLockOf(ctx, c.Unit, "deny_scheduled_runs"))
	}
	return runDoorDenied(reasonScheduledRunsDenied, d)
}

// handleCreateRunSchedule is POST /run-schedules.
func (s *Server) handleCreateRunSchedule(w http.ResponseWriter, r *http.Request) {
	if !envEnabled(envScheduledRuns) {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonScheduledRunsOff, scheduledRunsOffMsg)
		return
	}
	st, ok := s.runScheduleStoreOr(w)
	if !ok {
		return
	}
	if s.refuseAdminViewLaunch(w, r) {
		return
	}
	in, rule, ok := decodeRunScheduleBody(w, r)
	if !ok {
		return
	}
	a, ok := scheduleOwnerAuthority(w, r)
	if !ok {
		return
	}
	now := s.cfg.Now()
	first := rule.firstSlot(now)
	if !s.schedulePreflight(w, r, in.Request, in.TimeZone, false) {
		return
	}
	sc := store.RunSchedule{ID: uuid.New(), Owner: a.Sub, Name: in.Name, Every: rule.Every,
		LocalTime: fmt.Sprintf("%02d:%02d", rule.Hour, rule.Min), TimeZone: in.TimeZone, Request: in.Request,
		Authority: mustJSON(a), AuthorityAt: now, NextAt: first}
	if rule.Every == scheduleEveryWeekly {
		wd := int(rule.Weekday)
		sc.Weekday = &wd
	}
	switch err := st.CreateRunSchedule(r.Context(), sc, runScheduleLimit); {
	case errors.Is(err, store.ErrRunScheduleLimit):
		writeErrorReason(w, http.StatusConflict, reasonRunScheduleLimit, runScheduleLimitMsg)
		return
	case err != nil:
		writeServerError(w, r, "create run schedule", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r), "run.schedule.create",
		sc.ID.String(), "success", mustJSON(map[string]any{"every": sc.Every, "time": sc.LocalTime, "time_zone": sc.TimeZone, "next_at": sc.NextAt})))
	writeJSON(w, http.StatusCreated, runScheduleViewOf(sc, false))
}

// handlePauseRunSchedule is POST /run-schedules/{id}/pause, for the owner or an operator. A start already booked
// stays booked: it is cancelled on the Runs board.
func (s *Server) handlePauseRunSchedule(w http.ResponseWriter, r *http.Request) {
	st, ok := s.runScheduleStoreOr(w)
	if !ok {
		return
	}
	sc, ok := runScheduleOf(w, r, st)
	if !ok {
		return
	}
	ctx := r.Context()
	caller := oidcHumanFromContext(ctx)
	if sc.Owner != caller && !s.isSecurityOperator(ctx) {
		writeErrorReason(w, http.StatusNotFound, reasonRunScheduleNotFound, runScheduleNotFoundMsg)
		return
	}
	reason := schedulePausedByOwner
	if sc.Owner != caller {
		reason = schedulePausedByAdmin
	}
	now := s.cfg.Now()
	paused, err := st.PauseRunSchedule(ctx, sc.ID, reason, now)
	if err != nil {
		writeServerError(w, r, "pause run schedule", err)
		return
	}
	if paused {
		sc.PausedAt, sc.PausedReason = &now, reason
		s.recordAudit(ctx, s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r), "run.schedule.pause",
			sc.ID.String(), "success", mustJSON(map[string]any{"reason": reason})))
	}
	writeJSON(w, http.StatusOK, runScheduleViewOf(sc, sc.Owner != caller))
}

// handleResumeRunSchedule is POST /run-schedules/{id}/resume, for the owner alone. It runs the create's checks
// again as the caller and stores the caller's authority afresh: this is how a schedule paused for a revocation or
// a changed group recovers.
func (s *Server) handleResumeRunSchedule(w http.ResponseWriter, r *http.Request) {
	if !envEnabled(envScheduledRuns) {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonScheduledRunsOff, runScheduleResumeOffMsg)
		return
	}
	st, ok := s.runScheduleStoreOr(w)
	if !ok {
		return
	}
	if s.refuseAdminViewLaunch(w, r) {
		return
	}
	a, ok := scheduleOwnerAuthority(w, r)
	if !ok {
		return
	}
	sc, ok := runScheduleOf(w, r, st)
	if !ok {
		return
	}
	if sc.Owner != a.Sub {
		writeErrorReason(w, http.StatusNotFound, reasonRunScheduleNotFound, runScheduleNotFoundMsg)
		return
	}
	if sc.PausedAt == nil {
		writeJSON(w, http.StatusOK, runScheduleViewOf(sc, false))
		return
	}
	rule, err := scheduleRuleOf(sc)
	if err != nil {
		writeServerError(w, r, "read run schedule rule", err)
		return
	}
	if !s.schedulePreflight(w, r, sc.Request, sc.TimeZone, true) {
		return
	}
	now := s.cfg.Now()
	next := rule.nextOccurrence(now)
	ctx := r.Context()
	resumed, err := st.ResumeRunSchedule(ctx, sc.ID, mustJSON(a), now, next)
	if err != nil {
		writeServerError(w, r, "resume run schedule", err)
		return
	}
	if resumed {
		sc.PausedAt, sc.PausedReason, sc.NextAt = nil, "", next
		s.recordAudit(ctx, s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r), "run.schedule.resume",
			sc.ID.String(), "success", mustJSON(map[string]any{"next_at": next})))
	}
	writeJSON(w, http.StatusOK, runScheduleViewOf(sc, false))
}

// handleDeleteRunSchedule is DELETE /run-schedules/{id}, for the owner or an operator. A start already booked
// stays booked.
func (s *Server) handleDeleteRunSchedule(w http.ResponseWriter, r *http.Request) {
	st, ok := s.runScheduleStoreOr(w)
	if !ok {
		return
	}
	sc, ok := runScheduleOf(w, r, st)
	if !ok {
		return
	}
	ctx := r.Context()
	if sc.Owner != oidcHumanFromContext(ctx) && !s.isSecurityOperator(ctx) {
		writeErrorReason(w, http.StatusNotFound, reasonRunScheduleNotFound, runScheduleNotFoundMsg)
		return
	}
	deleted, err := st.DeleteRunSchedule(ctx, sc.ID)
	if err != nil {
		writeServerError(w, r, "delete run schedule", err)
		return
	}
	if deleted {
		s.recordAudit(ctx, s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r), "run.schedule.delete",
			sc.ID.String(), "success", mustJSON(map[string]any{"owner": sc.Owner})))
	}
	w.WriteHeader(http.StatusNoContent)
}

// cancelBookedRun kills the one run a booking just made, while it still waits, as the system, with reason. A run
// that left QUEUED since keeps its own end: the kill's claim is QUEUED to KILLED only (cancelQueuedRunsOf).
func (s *Server) cancelBookedRun(ctx context.Context, runID uuid.UUID, reason string) error {
	run, err := s.cfg.Store.GetRun(ctx, runID)
	if err != nil || run.State != types.RunQueued {
		return err
	}
	_, _, err = s.killRunCascade(ctx, run, types.ActorSystem, "wardynd", map[string]any{"reason": reason})
	return err
}

// RunScheduleSweep books every repeating schedule due within the booking lead, on the sweeper leader every 30
// seconds. Off (WARDYN_SCHEDULED_RUNS unset) it starts nothing and pauses nothing. It returns an error only when
// the due read fails.
func (s *Server) RunScheduleSweep(ctx context.Context) error {
	if !envEnabled(envScheduledRuns) {
		return nil
	}
	st, ok := s.cfg.Store.(runScheduleStore)
	if !ok {
		return nil
	}
	return s.newScheduleSweep(st).run(ctx)
}

// scheduleSweep is one tick. create, live and cancel are the server's own (handleCreateRunRoute,
// queueOwnerLive, cancelBookedRun); a test replaces them.
type scheduleSweep struct {
	s      *Server
	st     runScheduleStore
	now    time.Time
	create http.HandlerFunc
	live   func(context.Context, queueAuthority, time.Time) (bool, error)
	cancel func(context.Context, uuid.UUID, string) error
}

func (s *Server) newScheduleSweep(st runScheduleStore) *scheduleSweep {
	return &scheduleSweep{s: s, st: st, now: s.cfg.Now(), create: s.handleCreateRunRoute, live: s.queueOwnerLive, cancel: s.cancelBookedRun}
}

// scheduleTick is what one schedule's turn in a tick came to, for the tick's log line.
type scheduleTick int

const (
	scheduleTickNone scheduleTick = iota
	scheduleTickBooked
	scheduleTickMissed
)

func (sw *scheduleSweep) run(ctx context.Context) error {
	due, err := sw.st.DueRunSchedules(ctx, sw.now.Add(scheduleFireLead), scheduleSweepBatch)
	if err != nil {
		return err
	}
	var missed, booked int
	for _, sc := range due {
		switch sw.one(ctx, sc) {
		case scheduleTickBooked:
			booked++
		case scheduleTickMissed:
			missed++
		}
	}
	if missed > 0 {
		slog.WarnContext(ctx, "wardynd: scheduled starts missed", slog.Int("missed", missed), slog.Int("booked", booked))
	}
	return nil
}

// one is a due schedule's turn: the owner's standing, then their groups and email from the latest sign-in, a
// missed slot or a fresh booking, then the booking's outcome.
func (sw *scheduleSweep) one(ctx context.Context, sc store.RunSchedule) scheduleTick {
	rule, err := scheduleRuleOf(sc)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: run schedule skipped", slog.String("schedule", sc.ID.String()), slog.Any("err", err))
		return scheduleTickNone
	}
	var a queueAuthority
	if err := json.Unmarshal(sc.Authority, &a); err != nil {
		slog.WarnContext(ctx, "wardynd: run schedule authority unreadable", slog.String("schedule", sc.ID.String()), slog.Any("err", err))
		return scheduleTickNone
	}
	if !sw.standing(ctx, sc, a) {
		return scheduleTickNone
	}
	rec, err := sw.st.GetPrincipalGroups(ctx, a.Sub, "")
	switch {
	case errors.Is(err, store.ErrNotFound):
		sw.pause(ctx, sc, schedulePausedOwnerIdle)
		return scheduleTickNone
	case err != nil:
		return scheduleTickNone // never narrow against a record that was not read; the next tick tries again
	}
	kept, truncated, gained := narrowScheduleGroups(a.Groups, a.GroupsTruncated, rec)
	if gained {
		sw.pause(ctx, sc, schedulePausedGroupsChanged)
		return scheduleTickNone
	}
	a.Groups, a.GroupsTruncated = kept, truncated
	if rec.Email != "" {
		a.Email = rec.Email // the booked run's start derives role and user type from it, so it is the live one
	}

	if sc.NextAt.Before(sw.now) {
		return sw.missed(ctx, sc, rule, a)
	}
	cur, err := sw.st.GetRunSchedule(ctx, sc.ID)
	if err != nil || cur.PausedAt != nil || !cur.NextAt.Equal(sc.NextAt) {
		return scheduleTickNone // deleted, paused or moved since the due read, or unread: book nothing
	}
	code, hdr, body := sw.book(ctx, sc, a)
	return sw.outcome(ctx, sc, rule, a, code, hdr, body)
}

// standing pauses a schedule whose owner may no longer act (identity_inactive) or has not signed in through SSO
// for scheduleOwnerIdle (owner_idle), and reports whether it may book. A store error books nothing and pauses
// nothing: the next tick asks again.
func (sw *scheduleSweep) standing(ctx context.Context, sc store.RunSchedule, a queueAuthority) bool {
	live, err := sw.live(ctx, a, sc.AuthorityAt)
	if err != nil {
		return false
	}
	if !live {
		sw.pause(ctx, sc, schedulePausedIdentity)
		return false
	}
	at, found, err := sw.st.ScheduleOwnerSignedInAt(ctx, a.Sub)
	if err != nil {
		return false
	}
	if !found || at.Before(sw.now.Add(-scheduleOwnerIdle)) {
		sw.pause(ctx, sc, schedulePausedOwnerIdle)
		return false
	}
	return true
}

// missed handles a slot whose time passed unbooked. Within the create's key window its booking is replayed once
// under the same key, so a run booked before a lost advance is recorded, never skipped. An in-flight key or a
// server fault leaves it for the next tick; anything else records the slot missed.
func (sw *scheduleSweep) missed(ctx context.Context, sc store.RunSchedule, rule scheduleRule, a queueAuthority) scheduleTick {
	if sw.now.Sub(sc.NextAt) < scheduleReplayWindow {
		cur, err := sw.st.GetRunSchedule(ctx, sc.ID)
		if err != nil || cur.PausedAt != nil || !cur.NextAt.Equal(sc.NextAt) {
			return scheduleTickNone
		}
		code, hdr, body := sw.book(ctx, sc, a)
		switch {
		case code == http.StatusCreated:
			return sw.outcome(ctx, sc, rule, a, code, hdr, body)
		case retryableBooking(code, body):
			return scheduleTickNone
		}
	}
	won, err := sw.st.AdvanceRunSchedule(ctx, sc.ID, sc.NextAt, rule.nextOccurrence(sw.now), nil, scheduleOutcomeMissed)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: run schedule advance failed", slog.String("schedule", sc.ID.String()), slog.Any("err", err))
	}
	if !won {
		return scheduleTickNone // another sweep recorded this slot, and wrote its row
	}
	sw.fire(ctx, sc, nil, "failure", map[string]any{"reason": scheduleOutcomeMissed, "due_at": sc.NextAt})
	return scheduleTickMissed
}

// book posts the slot's one-time scheduled run through the create path as the schedule's owner, with the
// schedule's stored revocation issue on the context so the booked run judges revocations from it, under the key
// run-schedule:<id>:<due unix>.
func (sw *scheduleSweep) book(ctx context.Context, sc store.RunSchedule, a queueAuthority) (int, http.Header, []byte) {
	body, err := withScheduleStart(sc.Request, sc.NextAt.Format(time.RFC3339), sc.TimeZone)
	if err != nil {
		return http.StatusBadRequest, http.Header{}, nil
	}
	ctx2 := bindAuthority(ctx, a, contextChoice{})
	if a.Epoch != nil {
		ctx2 = oidc.WithAuthorityEpoch(ctx2, *a.Epoch)
	}
	if a.Seq != 0 {
		ctx2 = oidc.WithRevocationSeq(ctx2, a.Sub, a.Seq)
	}
	r, err := http.NewRequestWithContext(ctx2, http.MethodPost, "/api/v1/runs", bytes.NewReader(body))
	if err != nil {
		return http.StatusInternalServerError, http.Header{}, nil
	}
	r.Header.Set(idempotencyKeyHeader, fmt.Sprintf("run-schedule:%s:%d", sc.ID, sc.NextAt.Unix()))
	r.Header.Set("Content-Type", "application/json")
	rec := &scheduleRecorder{header: http.Header{}}
	sw.create(rec, r)
	if rec.code == 0 {
		rec.code = http.StatusOK
	}
	return rec.code, rec.header, rec.body.Bytes()
}

// retryableBooking is an answer the same key may meet again: another create under it still running, or a
// server fault.
func retryableBooking(code int, body []byte) bool {
	return code >= 500 || (code == http.StatusConflict && bookingReason(body) == reasonIdempotencyKeyInFlight)
}

func bookingReason(body []byte) string {
	var b struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(body, &b) //nolint:errcheck // a body with no reason has none
	return b.Reason
}

// outcome records what a booking came to. A fresh 201 re-reads the schedule (a Pause, Delete or erasure in the
// tick, or a failed read, cancels the run it made) and re-checks the owner; a replayed 201 was booked before any
// such change and keeps its run, unless that run ended before it started. A refusal by the owner's standing pauses the schedule; a busy key or a fault leaves it for the
// next tick; any other refusal skips this slot.
func (sw *scheduleSweep) outcome(ctx context.Context, sc store.RunSchedule, rule scheduleRule, a queueAuthority, code int, hdr http.Header, body []byte) scheduleTick {
	reason := bookingReason(body)
	switch {
	case code == http.StatusCreated:
		return sw.booked(ctx, sc, rule, a, hdr, body)
	case code == http.StatusUnauthorized || code == http.StatusForbidden ||
		(code == http.StatusConflict && reason == string(authz.ReasonAdminView)):
		if reason == "" {
			reason = strconv.Itoa(code)
		}
		sw.pause(ctx, sc, scheduleRefusedPrefix+reason)
		if sw.advance(ctx, sc, rule, nil, scheduleOutcomeRefused) {
			sw.fire(ctx, sc, nil, "failure", map[string]any{"reason": reason, "due_at": sc.NextAt})
		}
		return scheduleTickNone
	case retryableBooking(code, body):
		return scheduleTickNone
	default:
		if reason == "" {
			reason = strconv.Itoa(code)
		}
		if sw.advance(ctx, sc, rule, nil, scheduleOutcomeSkippedPrefix+reason) {
			sw.fire(ctx, sc, nil, "failure", map[string]any{"reason": reason, "due_at": sc.NextAt})
		}
		return scheduleTickNone
	}
}

func (sw *scheduleSweep) booked(ctx context.Context, sc store.RunSchedule, rule scheduleRule, a queueAuthority, hdr http.Header, body []byte) scheduleTick {
	var made struct {
		ID uuid.UUID `json:"id"`
	}
	_ = json.Unmarshal(body, &made) //nolint:errcheck // a 201 carries the run; a nil id is handled below
	runID := made.ID
	replayed := hdr.Get(idempotentReplayedHeader) == "true"
	if runID == uuid.Nil {
		slog.WarnContext(ctx, "wardynd: run schedule booking answered no run id", slog.String("schedule", sc.ID.String()))
		return scheduleTickNone
	}
	if !replayed {
		cur, err := sw.st.GetRunSchedule(ctx, sc.ID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return sw.dropBooked(ctx, sc, rule, runID, scheduleCancelGone)
		case err != nil:
			slog.WarnContext(ctx, "wardynd: run schedule unread after its booking", slog.String("schedule", sc.ID.String()), slog.Any("err", err))
			return sw.dropBooked(ctx, sc, rule, runID, scheduleCancelUnread)
		case cur.PausedAt != nil:
			return sw.dropBooked(ctx, sc, rule, runID, scheduleCancelPaused)
		}
	} else if run, err := sw.s.cfg.Store.GetRun(ctx, runID); err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.WarnContext(ctx, "wardynd: run schedule booking unread", slog.String("run_id", runID.String()), slog.Any("err", err))
		return scheduleTickNone // the next tick replays it again
	} else if err != nil || (run.State.IsTerminal() && run.AdmittedAt == nil) {
		if sw.advance(ctx, sc, rule, &runID, scheduleOutcomeCancelled+scheduleRunEnded) {
			sw.fire(ctx, sc, &runID, "failure", map[string]any{"reason": scheduleRunEnded, "run_id": runID, "due_at": sc.NextAt, "replayed": true})
		}
		return scheduleTickNone
	}
	if live, err := sw.live(ctx, a, sc.AuthorityAt); err != nil || !live {
		if err := sw.cancel(ctx, runID, queueCancelSessionRevoked); err != nil {
			slog.WarnContext(ctx, "wardynd: run schedule booking not cancelled", slog.String("run_id", runID.String()), slog.Any("err", err))
		}
		sw.pause(ctx, sc, schedulePausedIdentity)
		if sw.advance(ctx, sc, rule, &runID, scheduleOutcomeCancelled+schedulePausedIdentity) {
			sw.fire(ctx, sc, &runID, "failure", map[string]any{"reason": schedulePausedIdentity, "run_id": runID, "due_at": sc.NextAt})
		}
		return scheduleTickNone
	}
	if !sw.advance(ctx, sc, rule, &runID, scheduleOutcomeBooked) {
		return scheduleTickNone // another sweep recorded this slot's booking, and wrote its row
	}
	data := map[string]any{"run_id": runID, "due_at": sc.NextAt}
	if replayed {
		data["replayed"] = true
	}
	sw.fire(ctx, sc, &runID, "success", data)
	return scheduleTickBooked
}

// dropBooked cancels a fresh booking whose schedule is gone, paused or unread, with reason, and moves the
// schedule past the slot with that outcome, so a later replay of the slot's key never records the killed run
// booked. A gone schedule (the owner's Delete or an erasure) also blanks that one run's text while it still
// waits. Either call's error leaves the other to run.
func (sw *scheduleSweep) dropBooked(ctx context.Context, sc store.RunSchedule, rule scheduleRule, runID uuid.UUID, reason string) scheduleTick {
	if reason == scheduleCancelGone {
		if run, err := sw.s.cfg.Store.GetRun(ctx, runID); err != nil || run.State == types.RunQueued {
			if err := sw.st.BlankRunTaskByID(ctx, runID); err != nil {
				slog.WarnContext(ctx, "wardynd: run schedule booking not blanked", slog.String("run_id", runID.String()), slog.Any("err", err))
			}
		}
	}
	if err := sw.cancel(ctx, runID, reason); err != nil {
		slog.WarnContext(ctx, "wardynd: run schedule booking not cancelled", slog.String("schedule", sc.ID.String()),
			slog.String("run_id", runID.String()), slog.Any("err", err))
	}
	if sw.advance(ctx, sc, rule, &runID, scheduleOutcomeCancelled+reason) {
		sw.fire(ctx, sc, &runID, "failure", map[string]any{"reason": reason, "run_id": runID, "due_at": sc.NextAt})
	}
	return scheduleTickNone
}

// advance moves the schedule past this slot to the next after it, and reports whether this sweep's
// compare-and-set did: only then does it write the slot's run.schedule.fire row, so two sweeps at once write one.
func (sw *scheduleSweep) advance(ctx context.Context, sc store.RunSchedule, rule scheduleRule, runID *uuid.UUID, outcome string) bool {
	won, err := sw.st.AdvanceRunSchedule(ctx, sc.ID, sc.NextAt, rule.nextOccurrence(sc.NextAt), runID, outcome)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: run schedule advance failed", slog.String("schedule", sc.ID.String()), slog.Any("err", err))
	}
	return won && err == nil
}

// pause pauses a schedule as the system, with the reason; the row is written only when it was active.
func (sw *scheduleSweep) pause(ctx context.Context, sc store.RunSchedule, reason string) {
	paused, err := sw.st.PauseRunSchedule(ctx, sc.ID, reason, sw.now)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: run schedule pause failed", slog.String("schedule", sc.ID.String()), slog.Any("err", err))
		return
	}
	if paused {
		sw.s.recordAudit(ctx, sw.s.auditEvent(nil, types.ActorSystem, "wardynd", "run.schedule.pause", sc.ID.String(), "success",
			mustJSON(map[string]any{"reason": reason})))
	}
}

// fire writes one slot's run.schedule.fire row, on the booked run's trail when there is one.
func (sw *scheduleSweep) fire(ctx context.Context, sc store.RunSchedule, runID *uuid.UUID, outcome string, data map[string]any) {
	sw.s.recordAudit(ctx, sw.s.auditEvent(runID, types.ActorSystem, "wardynd", "run.schedule.fire", sc.ID.String(), outcome, mustJSON(data)))
}

// scheduleRecorder is the answer a booking's create wrote.
type scheduleRecorder struct {
	code   int
	header http.Header
	body   bytes.Buffer
}

func (r *scheduleRecorder) Header() http.Header { return r.header }

func (r *scheduleRecorder) WriteHeader(code int) {
	if r.code == 0 {
		r.code = code
	}
}

func (r *scheduleRecorder) Write(b []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	return r.body.Write(b)
}
