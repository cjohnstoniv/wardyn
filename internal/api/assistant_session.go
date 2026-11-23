// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/assistant"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// assistantMessageMax is the longest person message, in characters.
const assistantMessageMax = 8000

// The caps on one edits request, and how much of the person's edits one turn carries: the fields it names, a
// field's name, a value, the draft, and the latest person_edit events since the last message a turn reads.
const (
	assistantEditFieldsMax = 50
	assistantEditFieldMax  = 200
	assistantEditValueMax  = 8000
	assistantDraftMax      = 256 << 10
	assistantEditsWindow   = 10
)

// assistantMessageLimiter spends one token per person message, edit, preview or start: ten at once, one every
// three seconds after.
// ponytail: process-wide, keyed by person; a per-server bucket needs a Server field.
var assistantMessageLimiter = &principalLimiter{rate: 1.0 / 3, burst: 10, max: preflightLimiterMaxPeople}

// assistantRateRefused spends one of person's tokens from the one bucket messages, edits, previews and starts
// share, and answers 429 when none is left. A request with no person spends nothing: it is refused later.
func (s *Server) assistantRateRefused(w http.ResponseWriter, person string) bool {
	if person == "" || assistantMessageLimiter.allow(person, s.cfg.Now()) {
		return false
	}
	w.Header().Set("Retry-After", "3")
	writeErrorReason(w, http.StatusTooManyRequests, reasonAssistantRateLimited, "Too many messages. Wait a few seconds.") // PROPOSED
	return true
}

// assistantStore is the session store, or 501 when the deployment's store keeps none.
func (s *Server) assistantStore(w http.ResponseWriter) (store.AssistantSessionStore, bool) {
	ss, ok := s.cfg.Store.(store.AssistantSessionStore)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonAssistantSessionsUnavailable, "Assistant sessions are not available on this server.") // PROPOSED
	}
	return ss, ok
}

type assistantCreateRequest struct {
	Purpose       types.AssistantPurpose `json:"purpose"`
	Target        types.AssistantTarget  `json:"target"`
	PreviewDigest string                 `json:"preview_digest"`
	WorkspaceID   *string                `json:"workspace_id,omitempty"`
	Repo          *string                `json:"repo,omitempty"`
}

type assistantSessionView struct {
	ID       uuid.UUID         `json:"id"`
	State    string            `json:"state"`
	RunID    *uuid.UUID        `json:"run_id,omitempty"`
	Provider string            `json:"provider"`
	Model    string            `json:"model"`
	Letters  map[string]string `json:"letters"`
}

// handleCreateAssistantSession is POST /api/v1/assistant/sessions. The checks run in order: a person; availability
// ready; the purpose and target pair; the target's kind never sent; the person can read it; the deferral refusal;
// the preview digest against the records and binding built now. The workspace and repository fields answer 400.
func (s *Server) handleCreateAssistantSession(w http.ResponseWriter, r *http.Request) {
	ss, ok := s.assistantStore(w)
	if !ok {
		return
	}
	var req assistantCreateRequest
	if msg := decodeStrictMsg(w, r, &req); msg != "" {
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidRequestBody, msg)
		return
	}
	if req.WorkspaceID != nil || req.Repo != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidRequestBody, "Files from a repository are not supported yet.") // PROPOSED
		return
	}
	if s.assistantRateRefused(w, oidcHumanFromContext(r.Context())) {
		return
	}
	prep, ref := s.assistantPrepare(r, req.Purpose, req.Target)
	if ref != nil {
		ref.write(w, r)
		return
	}
	if req.PreviewDigest != prep.digest {
		writeErrorReason(w, http.StatusConflict, reasonAssistantPreviewStale, "What will be sent changed since the preview. Preview again.") // PROPOSED
		return
	}
	sess, ref := s.assistantStart(r.Context(), ss, prep, req.Purpose)
	if ref != nil {
		ref.write(w, r)
		return
	}
	// The person is the row's actor, so audit sealing and erasure reach them; the role is the one the tool context
	// acts at.
	s.recordAudit(r.Context(), s.auditEvent(sess.RunID, actorTypeFromRequest(r), prep.snap.Principal, assistantSessionStartAction, sess.ID.String(), "success",
		mustJSON(map[string]any{"purpose": req.Purpose, "provider_id": prep.binding.ProviderID, "model": prep.binding.Model,
			"run_id": sess.RunID, "target_kind": req.Target.Kind, "target_key": req.Target.ID, "preview_digest": prep.digest, "role": prep.role})))
	writeJSON(w, http.StatusCreated, assistantSessionView{ID: sess.ID, State: sess.State, RunID: sess.RunID,
		Provider: providerDisplayName(prep.provider), Model: prep.binding.Model, Letters: prep.payload.Letters})
}

// assistantSessionRow is the row a start writes before anything is dispatched.
func assistantSessionRow(prep *assistantPrep, purpose types.AssistantPurpose, runID *uuid.UUID) types.AssistantSession {
	return types.AssistantSession{
		ID: uuid.New(), RunID: runID, Principal: prep.snap.Principal, Email: prep.snap.Email, Role: prep.snap.Role, UserType: prep.snap.UserType,
		Groups: prep.snap.Groups, GroupsTruncated: prep.snap.GroupsTruncated, Purpose: purpose, TargetKind: prep.target.Kind, TargetKey: prep.target.key(),
		Binding: prep.binding, State: types.AssistantSessionStarting, PersonLetters: prep.payload.Letters, PreviewDigest: prep.digest,
	}
}

// assistantStart makes the session. With no live session for the person and purpose it launches a run. With a live
// one on an equal binding (provider id, UID and model) whose run is still live it replaces the session in one
// transaction, keeps the run and queues a reset. Otherwise it ends the old session, stops its run and launches a
// fresh run sealed to the new binding.
func (s *Server) assistantStart(ctx context.Context, ss store.AssistantSessionStore, prep *assistantPrep, purpose types.AssistantPurpose) (types.AssistantSession, *assistantRefusal) {
	old, err := ss.LiveAssistantSession(ctx, prep.snap.Principal, purpose)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return s.assistantStartFresh(ctx, ss, prep, purpose)
	case err != nil:
		return types.AssistantSession{}, assistantRefusalFor(http.StatusInternalServerError, "", "Could not read your sessions.") // PROPOSED
	}
	if old.Binding == prep.binding && old.RunID != nil {
		live, _, err := s.assistantRunState(ctx, *old.RunID)
		if err != nil {
			return types.AssistantSession{}, assistantRunUnreadable()
		}
		if live {
			return s.assistantReuseRun(ctx, ss, prep, purpose, old)
		}
	}
	s.endAssistantSession(ctx, ss, old, types.AssistantEndReplaced, true)
	return s.assistantStartFresh(ctx, ss, prep, purpose)
}

// assistantRunState reads the session's run: whether it is still live, and the run itself. A store error is
// returned, never read as an ended run.
func (s *Server) assistantRunState(ctx context.Context, run uuid.UUID) (live bool, r types.AgentRun, err error) {
	r, err = s.cfg.Store.GetRun(ctx, run)
	if errors.Is(err, store.ErrNotFound) {
		return false, r, nil
	}
	if err != nil {
		return false, r, err
	}
	return !r.State.IsTerminal(), r, nil
}

// assistantRunUnreadable is the 503 of a run the store could not read: the session is left as it is.
func assistantRunUnreadable() *assistantRefusal {
	return assistantRefusalFor(http.StatusServiceUnavailable, "", "Could not read the assistant's run. Try again.") // PROPOSED
}

func (s *Server) assistantReuseRun(ctx context.Context, ss store.AssistantSessionStore, prep *assistantPrep, purpose types.AssistantPurpose, old types.AssistantSession) (types.AssistantSession, *assistantRefusal) {
	row := assistantSessionRow(prep, purpose, old.RunID)
	reset, _ := json.Marshal(assistantRecordsEvent(prep, "reset", row.ID))
	sess, err := ss.ReplaceAssistantSession(ctx, old.ID, row, reset)
	if err != nil {
		return types.AssistantSession{}, assistantRefusalFor(http.StatusConflict, reasonAssistantSessionNotFound, "Another session started first. Try again.") // PROPOSED
	}
	s.assistantEndAudit(context.WithoutCancel(ctx), ss, old, types.AssistantEndReplaced)
	return sess, nil
}

// assistantLaunchRefusal is the answer to a run that could not be launched: max_concurrent_runs is the
// person's run limit; a placement denial, a stale group snapshot and a host-capacity refusal each answer as
// the scan launch answers them; anything else is a server error.
func (s *Server) assistantLaunchRefusal(err error) *assistantRefusal {
	if assistantRunLimited(err) {
		return assistantRefusalFor(http.StatusConflict, reasonAssistantAtRunLimit, "You're at your run limit. End a run to start the assistant.") // PROPOSED: the settings card's words
	}
	ref := assistantRefusalFor(http.StatusInternalServerError, "", "Could not start the assistant's run.") // PROPOSED
	ref.custom = func(w http.ResponseWriter, r *http.Request) bool { return s.writeScanLaunchRefusal(w, r, err) }
	return ref
}

func (s *Server) assistantStartFresh(ctx context.Context, ss store.AssistantSessionStore, prep *assistantPrep, purpose types.AssistantPurpose) (types.AssistantSession, *assistantRefusal) {
	actor := prep.snap.Principal
	l, err := s.assistantNewRun(ctx, actor, prep.binding)
	if err != nil {
		return types.AssistantSession{}, s.assistantLaunchRefusal(err)
	}
	runID := l.run.ID
	sess, err := ss.CreateAssistantSession(ctx, assistantSessionRow(prep, purpose, &runID))
	if err != nil {
		s.failAndRevoke(context.WithoutCancel(ctx), runID, types.RunPending, "the assistant session could not start")
		if errors.Is(err, store.ErrAssistantSessionLive) {
			return types.AssistantSession{}, assistantRefusalFor(http.StatusConflict, reasonAssistantSessionNotFound, "Another session started first. Try again.") // PROPOSED
		}
		return types.AssistantSession{}, assistantRefusalFor(http.StatusInternalServerError, "", "Could not start the session.") // PROPOSED
	}
	if body, merr := json.Marshal(assistantRecordsEvent(prep, "", sess.ID)); merr == nil {
		_, err = ss.AppendAssistantEvent(ctx, sess.ID, types.AssistantEventState, body)
	}
	if err != nil {
		s.endAssistantSession(ctx, ss, sess, types.AssistantEndProviderError, false)
		s.failAndRevoke(context.WithoutCancel(ctx), runID, types.RunPending, "the assistant session could not start")
		return types.AssistantSession{}, assistantRefusalFor(http.StatusInternalServerError, "", "Could not start the session.") // PROPOSED
	}
	s.assistantDispatch(ctx, l)
	return sess, nil
}

// assistantOwned reads the session of the {id} path for the signed-in person, else 404: another person's session
// and a missing one answer alike. A session whose run ended is ended lazily here, with reason run_ended.
func (s *Server) assistantOwned(w http.ResponseWriter, r *http.Request) (store.AssistantSessionStore, types.AssistantSession, bool) {
	ss, ok := s.assistantStore(w)
	if !ok {
		return nil, types.AssistantSession{}, false
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	person := oidcHumanFromContext(r.Context())
	if err != nil || person == "" {
		writeErrorReason(w, http.StatusNotFound, reasonAssistantSessionNotFound, "Session not found.") // PROPOSED
		return nil, types.AssistantSession{}, false
	}
	sess, err := ss.GetAssistantSession(r.Context(), id, person)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErrorReason(w, http.StatusNotFound, reasonAssistantSessionNotFound, "Session not found.") // PROPOSED
		} else {
			writeServerError(w, r, "read assistant session", err)
		}
		return nil, types.AssistantSession{}, false
	}
	if sess.Live() && sess.RunID != nil {
		live, run, err := s.assistantRunState(r.Context(), *sess.RunID)
		if err != nil {
			assistantRunUnreadable().write(w, r)
			return nil, types.AssistantSession{}, false
		}
		if !live {
			s.endAssistantSession(r.Context(), ss, sess, assistantRunEndReason(run, s.cfg.Now()), false)
			sess, _ = ss.GetAssistantSession(r.Context(), id, person)
		}
	}
	return ss, sess, true
}

// assistantRunEndReason is why a session whose run is terminal ended: max_lifetime when the run lived out its
// two hours, else run_ended.
func assistantRunEndReason(run types.AgentRun, now time.Time) string {
	if !run.CreatedAt.IsZero() && now.Sub(run.CreatedAt) >= assistantRunLifetimeSec*time.Second {
		return types.AssistantEndMaxLifetime
	}
	return types.AssistantEndRunEnded
}

// assistantRoleRefused re-derives the session person's role as a tool call does. When sign-in would now refuse
// them, the session ends as role_denied and the call answers 403.
func (s *Server) assistantRoleRefused(w http.ResponseWriter, r *http.Request, ss store.AssistantSessionStore, sess types.AssistantSession) bool {
	_, release, err := s.assistantToolCtx(r, assistantSnapshotOfSession(sess))
	if err == nil {
		release()
		return false
	}
	if errors.Is(err, errAssistantRoleDenied) {
		s.endAssistantSession(r.Context(), ss, sess, types.AssistantEndRoleDenied, true)
	}
	s.assistantToolCtxRefusal(err).write(w, r)
	return true
}

// assistantStillAvailable re-checks the session's own binding, never generic availability: a ready answer on
// another provider or model authorises nothing. In order: the purpose is on; the binding holds; the target's kind
// is not never-sent; the provider does not defer inspection. The run-limit count is not re-checked, since the
// session's own run already holds its slot. On a failure it ends the session as turned_off and returns the refusal.
// ctx is the context whose ceiling and credentials are asked: the person's own request, or a tool context.
func (s *Server) assistantStillAvailable(ctx context.Context, ss store.AssistantSessionStore, sess types.AssistantSession) *assistantRefusal {
	ref := s.assistantAvailableNow(ctx, sess)
	if ref != nil {
		s.endAssistantSession(ctx, ss, sess, types.AssistantEndTurnedOff, true)
	}
	return ref
}

func (s *Server) assistantAvailableNow(ctx context.Context, sess types.AssistantSession) *assistantRefusal {
	fail := func(status int, reason, msg string) *assistantRefusal {
		return assistantRefusalFor(status, reason, msg)
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return fail(http.StatusInternalServerError, "", "Could not read the settings.") // PROPOSED
	}
	lim, err := s.assistantLimitsFor(ctx, sc, sess.Principal)
	if err != nil {
		return fail(http.StatusInternalServerError, "", "Could not read your limits.") // PROPOSED
	}
	providers := siteProviders(sc)
	switch reason := assistantBindingHolds(sc.Assistant, sess.Purpose, sess.Binding, providers, inspectionDeferredFor, lim.deny, lim.allowed, lim.connected); reason {
	case "":
	case reasonAssistantOff:
		return fail(http.StatusConflict, reasonAssistantOff, "The assistant is off for your organisation.") // PROPOSED: the board's words
	default:
		return fail(http.StatusConflict, reasonAssistantSessionProviderChanged, "The assistant's provider or model changed. Start again to see what will be sent.") // PROPOSED text
	}
	kind, err := s.assistantSessionGovKind(ctx, sess)
	if err != nil {
		return fail(http.StatusInternalServerError, "", "Could not read the target.") // PROPOSED
	}
	if ref := assistantNeverSent(sc.Assistant, kind); ref != nil {
		return ref
	}
	p, _ := modelProviderByID(sc.ModelProviders, sess.Binding.ProviderID)
	return refuseAssistantDeferral(p)
}

// assistantSessionGovKind is the governance kind of the session's target, read now: a change's own kind is on its row.
func (s *Server) assistantSessionGovKind(ctx context.Context, sess types.AssistantSession) (string, error) {
	switch sess.TargetKind {
	case types.AssistantTargetUnit:
		return govKindUnit, nil
	case types.AssistantTargetChange:
		id, err := uuid.Parse(sess.TargetKey)
		if err != nil {
			return "", nil
		}
		ch, err := s.cfg.Store.GetGovernanceChange(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			return "", nil
		}
		return ch.TargetKind, err
	}
	return "", nil
}

// assistantLiveOr409 answers 409 session_not_found for a session that already ended.
func assistantLiveOr409(w http.ResponseWriter, sess types.AssistantSession) bool {
	if sess.Live() {
		return true
	}
	writeAssistantEnded(w)
	return false
}

func writeAssistantEnded(w http.ResponseWriter) {
	writeErrorReason(w, http.StatusConflict, reasonAssistantSessionNotFound, "This session has ended.") // PROPOSED
}

// writeAssistantAppendError answers an append that failed: a session that ended since it was read is 409,
// anything else a server error.
func writeAssistantAppendError(w http.ResponseWriter, r *http.Request, what string, err error) {
	if errors.Is(err, store.ErrAssistantSessionEnded) {
		writeAssistantEnded(w)
		return
	}
	writeServerError(w, r, what, err)
}

type assistantMessageRequest struct {
	Text string `json:"text"`
}

// assistantSessionUsable runs what every message and edit runs first, on a session the person owns: it must be
// live, the person's role must still allow it, and its own binding must still hold.
func (s *Server) assistantSessionUsable(w http.ResponseWriter, r *http.Request) (store.AssistantSessionStore, types.AssistantSession, bool) {
	ss, sess, ok := s.assistantOwned(w, r)
	if !ok || !assistantLiveOr409(w, sess) || s.assistantRoleRefused(w, r, ss, sess) {
		return nil, types.AssistantSession{}, false
	}
	if ref := s.assistantStillAvailable(r.Context(), ss, sess); ref != nil {
		ref.write(w, r)
		return nil, types.AssistantSession{}, false
	}
	return ss, sess, true
}

// handleAssistantMessage is POST .../{id}/messages: the person's turn. Still available first, at most 8000
// characters, rate limited per person, then stored as user_message with the person's own edits since their last turn.
func (s *Server) handleAssistantMessage(w http.ResponseWriter, r *http.Request) {
	ss, sess, ok := s.assistantSessionUsable(w, r)
	if !ok {
		return
	}
	var req assistantMessageRequest
	if msg := decodeStrictMsg(w, r, &req); msg != "" {
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidRequestBody, msg)
		return
	}
	if strings.TrimSpace(req.Text) == "" || utf8.RuneCountInString(req.Text) > assistantMessageMax {
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidRequestBody, "A message is 1 to 8000 characters.") // PROPOSED
		return
	}
	if s.assistantRateRefused(w, sess.Principal) {
		return
	}
	edits, err := s.assistantPendingEdits(r.Context(), ss, sess.ID)
	if err != nil {
		writeServerError(w, r, "read assistant edits", err)
		return
	}
	payload, _ := json.Marshal(map[string]any{"text": req.Text, "turn": assistant.NextTurn(req.Text, edits)})
	ev, err := ss.AppendAssistantEvent(r.Context(), sess.ID, types.AssistantEventUserMessage, payload)
	if err != nil {
		writeAssistantAppendError(w, r, "append assistant message", err)
		return
	}
	_ = ss.TouchAssistantSession(r.Context(), sess.ID, false)
	writeJSON(w, http.StatusAccepted, map[string]any{"seq": ev.Seq})
}

type assistantEditsRequest struct {
	Draft   json.RawMessage  `json:"draft"`
	Changed []assistant.Edit `json:"changed"`
}

// assistantEditsValid reports whether an edits request is within its caps: at most 50 fields, each name at most
// 200 characters and each value at most 8000, and a draft of at most 256 KiB.
func assistantEditsValid(req assistantEditsRequest) bool {
	if len(req.Changed) > assistantEditFieldsMax || len(req.Draft) > assistantDraftMax {
		return false
	}
	for _, c := range req.Changed {
		if utf8.RuneCountInString(c.Field) > assistantEditFieldMax || utf8.RuneCountInString(c.Value) > assistantEditValueMax {
			return false
		}
	}
	return true
}

// handleAssistantEdits is POST .../{id}/edits: the person's own change to the draft. Still available first, the
// request's caps, then the person's bucket messages share. The draft is stored, a person_edit event records it,
// and the next message carries a line for each changed field.
func (s *Server) handleAssistantEdits(w http.ResponseWriter, r *http.Request) {
	ss, sess, ok := s.assistantSessionUsable(w, r)
	if !ok {
		return
	}
	var req assistantEditsRequest
	if msg := decodeStrictMsg(w, r, &req); msg != "" {
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidRequestBody, msg)
		return
	}
	if !assistantEditsValid(req) {
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidRequestBody, "An edit names at most 50 fields of 8000 characters each, with a draft of at most 256 KiB.") // PROPOSED
		return
	}
	if s.assistantRateRefused(w, sess.Principal) {
		return
	}
	if err := ss.SetAssistantDraft(r.Context(), sess.ID, req.Draft); err != nil {
		writeServerError(w, r, "store assistant draft", err)
		return
	}
	payload, _ := json.Marshal(map[string]any{"changed": req.Changed})
	ev, err := ss.AppendAssistantEvent(r.Context(), sess.ID, types.AssistantEventPersonEdit, payload)
	if err != nil {
		writeAssistantAppendError(w, r, "append assistant edit", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"seq": ev.Seq})
}

// assistantLastSeq is the seq of the newest event of kind below before (0 for no bound), 0 when there is none.
func assistantLastSeq(ctx context.Context, ss store.AssistantSessionStore, id uuid.UUID, kind string, before int64) (int64, error) {
	evs, err := ss.FindAssistantEvents(ctx, id, store.AssistantEventFilter{Before: before, Kinds: []string{kind}, Limit: 1, Newest: true})
	if err != nil || len(evs) == 0 {
		return 0, err
	}
	return evs[0].Seq, nil
}

// assistantPendingEdits are the fields the person changed since their last message: only the latest
// assistantEditsWindow person_edit events after it are read. A field changed twice keeps its last value, in the
// place it was first changed, and the latest assistantEditFieldsMax fields are kept.
func (s *Server) assistantPendingEdits(ctx context.Context, ss store.AssistantSessionStore, id uuid.UUID) ([]assistant.Edit, error) {
	last, err := assistantLastSeq(ctx, ss, id, types.AssistantEventUserMessage, 0)
	if err != nil {
		return nil, err
	}
	evs, err := ss.FindAssistantEvents(ctx, id, store.AssistantEventFilter{After: last, Kinds: []string{types.AssistantEventPersonEdit},
		Limit: assistantEditsWindow, Newest: true})
	if err != nil {
		return nil, err
	}
	var edits []assistant.Edit
	at := map[string]int{}
	for _, ev := range evs {
		var p struct {
			Changed []assistant.Edit `json:"changed"`
		}
		_ = json.Unmarshal(ev.Payload, &p)
		for _, e := range p.Changed {
			if i, ok := at[e.Field]; ok {
				edits[i].Value = e.Value
				continue
			}
			at[e.Field] = len(edits)
			edits = append(edits, e)
		}
	}
	if len(edits) > assistantEditFieldsMax {
		edits = edits[len(edits)-assistantEditFieldsMax:]
	}
	return edits, nil
}

// handleDeleteAssistantSession is DELETE .../{id}: it ends the session and stops its run unless a newer live
// session uses it.
func (s *Server) handleDeleteAssistantSession(w http.ResponseWriter, r *http.Request) {
	ss, sess, ok := s.assistantOwned(w, r)
	if !ok {
		return
	}
	if sess.Live() {
		s.endAssistantSession(r.Context(), ss, sess, types.AssistantEndPerson, true)
	}
	w.WriteHeader(http.StatusNoContent)
}

// assistantAfter reads the ?after=N cursor, 0 when absent.
func assistantAfter(r *http.Request) (int64, bool) {
	raw := r.URL.Query().Get("after")
	if raw == "" {
		return 0, true
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	return n, err == nil && n >= 0
}

// assistantPollInterval is how often a long-poll or stream rereads the store.
const assistantPollInterval = 500 * time.Millisecond
