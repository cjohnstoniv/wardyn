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

// assistantMessageLimiter spends one token per person message: ten at once, one every three seconds after.
// ponytail: process-wide, keyed by person; a per-server bucket needs a Server field.
var assistantMessageLimiter = &principalLimiter{rate: 1.0 / 3, burst: 10, max: preflightLimiterMaxPeople}

// assistantStore is the session store, or 501 when the deployment's store keeps none.
func (s *Server) assistantStore(w http.ResponseWriter) (store.AssistantSessionStore, bool) {
	ss, ok := s.cfg.Store.(store.AssistantSessionStore)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonAssistantSessionNotFound, "This store does not keep assistant sessions.") // PROPOSED
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
		writeErrorReason(w, http.StatusBadRequest, reasonAssistantPurposeTargetMismatch, msg)
		return
	}
	if req.WorkspaceID != nil || req.Repo != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonAssistantPurposeTargetMismatch, "Files from a repository are not supported yet.") // PROPOSED
		return
	}
	prep, ref := s.assistantPrepare(r, req.Purpose, req.Target)
	if ref != nil {
		ref.write(w)
		return
	}
	if req.PreviewDigest != prep.digest {
		writeErrorReason(w, http.StatusConflict, reasonAssistantPreviewStale, "What will be sent changed since the preview. Preview again.") // PROPOSED
		return
	}
	sess, ref := s.assistantStart(r.Context(), ss, prep, req.Purpose)
	if ref != nil {
		ref.write(w)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(sess.RunID, actorTypeFromRequest(r), prep.snap.Principal, assistantSessionStartAction, sess.ID.String(), "success",
		mustJSON(map[string]any{"person": prep.snap.Principal, "purpose": req.Purpose, "provider_id": prep.binding.ProviderID, "model": prep.binding.Model,
			"run_id": sess.RunID, "target_kind": req.Target.Kind, "target_key": req.Target.ID, "preview_digest": prep.digest, "role": prep.snap.Role})))
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
	if old.Binding == prep.binding && old.RunID != nil && s.assistantRunLive(ctx, *old.RunID) {
		return s.assistantReuseRun(ctx, ss, prep, purpose, old)
	}
	s.endAssistantSession(ctx, ss, old, types.AssistantEndReplaced, true)
	return s.assistantStartFresh(ctx, ss, prep, purpose)
}

func (s *Server) assistantRunLive(ctx context.Context, run uuid.UUID) bool {
	r, err := s.cfg.Store.GetRun(ctx, run)
	return err == nil && !r.State.IsTerminal()
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

func (s *Server) assistantStartFresh(ctx context.Context, ss store.AssistantSessionStore, prep *assistantPrep, purpose types.AssistantPurpose) (types.AssistantSession, *assistantRefusal) {
	actor := prep.snap.Principal
	l, err := s.assistantNewRun(ctx, actor, prep.binding)
	switch {
	case assistantRunLimited(err):
		return types.AssistantSession{}, assistantRefusalFor(http.StatusConflict, reasonAssistantAtRunLimit, "You are at your limit of runs.") // PROPOSED
	case err != nil:
		var denied *placementDeniedError
		if errors.As(err, &denied) {
			return types.AssistantSession{}, assistantRefusalFor(http.StatusConflict, reasonAssistantAtRunLimit, "A run can't be placed for you.") // PROPOSED
		}
		return types.AssistantSession{}, assistantRefusalFor(http.StatusInternalServerError, "", "Could not start the assistant's run.") // PROPOSED
	}
	runID := l.run.ID
	sess, err := ss.CreateAssistantSession(ctx, assistantSessionRow(prep, purpose, &runID))
	if err != nil {
		s.failAndRevoke(context.WithoutCancel(ctx), runID, types.RunPending, "the assistant session could not start")
		return types.AssistantSession{}, assistantRefusalFor(http.StatusConflict, reasonAssistantSessionNotFound, "You already have a session. End it first.") // PROPOSED
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
	if sess.Live() && sess.RunID != nil && !s.assistantRunLive(r.Context(), *sess.RunID) {
		s.endAssistantSession(r.Context(), ss, sess, types.AssistantEndRunEnded, false)
		sess, _ = ss.GetAssistantSession(r.Context(), id, person)
	}
	return ss, sess, true
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
		return fail(http.StatusConflict, reasonAssistantOff, "The assistant is turned off.") // PROPOSED
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
	writeErrorReason(w, http.StatusConflict, reasonAssistantSessionNotFound, "This session has ended.") // PROPOSED
	return false
}

type assistantMessageRequest struct {
	Text string `json:"text"`
}

// handleAssistantMessage is POST .../{id}/messages: the person's turn. Still available first, at most 8000
// characters, rate limited per person, then stored as user_message with the person's own edits since their last turn.
func (s *Server) handleAssistantMessage(w http.ResponseWriter, r *http.Request) {
	ss, sess, ok := s.assistantOwned(w, r)
	if !ok || !assistantLiveOr409(w, sess) {
		return
	}
	if ref := s.assistantStillAvailable(r.Context(), ss, sess); ref != nil {
		ref.write(w)
		return
	}
	var req assistantMessageRequest
	if msg := decodeStrictMsg(w, r, &req); msg != "" {
		writeErrorReason(w, http.StatusBadRequest, reasonAssistantPurposeTargetMismatch, msg)
		return
	}
	if strings.TrimSpace(req.Text) == "" || utf8.RuneCountInString(req.Text) > assistantMessageMax {
		writeErrorReason(w, http.StatusBadRequest, reasonAssistantPurposeTargetMismatch, "A message is 1 to 8000 characters.") // PROPOSED
		return
	}
	if !assistantMessageLimiter.allow(sess.Principal, s.cfg.Now()) {
		w.Header().Set("Retry-After", "3")
		writeErrorReason(w, http.StatusTooManyRequests, reasonAssistantRateLimited, "Too many messages. Slow down.") // PROPOSED
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
		writeServerError(w, r, "append assistant message", err)
		return
	}
	_ = ss.TouchAssistantSession(r.Context(), sess.ID, false)
	writeJSON(w, http.StatusAccepted, map[string]any{"seq": ev.Seq})
}

type assistantEditsRequest struct {
	Draft   json.RawMessage `json:"draft"`
	Changed []struct {
		Field string `json:"field"`
		Value string `json:"value"`
	} `json:"changed"`
}

// handleAssistantEdits is POST .../{id}/edits: the person's own change to the draft. Still available first. The
// draft is stored, a person_edit event records it, and the next message carries a line for each changed field.
func (s *Server) handleAssistantEdits(w http.ResponseWriter, r *http.Request) {
	ss, sess, ok := s.assistantOwned(w, r)
	if !ok || !assistantLiveOr409(w, sess) {
		return
	}
	if ref := s.assistantStillAvailable(r.Context(), ss, sess); ref != nil {
		ref.write(w)
		return
	}
	var req assistantEditsRequest
	if msg := decodeStrictMsg(w, r, &req); msg != "" {
		writeErrorReason(w, http.StatusBadRequest, reasonAssistantPurposeTargetMismatch, msg)
		return
	}
	if err := ss.SetAssistantDraft(r.Context(), sess.ID, req.Draft); err != nil {
		writeServerError(w, r, "store assistant draft", err)
		return
	}
	payload, _ := json.Marshal(map[string]any{"changed": req.Changed})
	ev, err := ss.AppendAssistantEvent(r.Context(), sess.ID, types.AssistantEventPersonEdit, payload)
	if err != nil {
		writeServerError(w, r, "append assistant edit", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"seq": ev.Seq})
}

// assistantPendingEdits are the fields the person changed since their last message, in order.
func (s *Server) assistantPendingEdits(ctx context.Context, ss store.AssistantSessionStore, id uuid.UUID) ([]assistant.Edit, error) {
	evs, err := ss.ListAssistantEvents(ctx, id, 0, assistantEventsScan)
	if err != nil {
		return nil, err
	}
	var edits []assistant.Edit
	for _, ev := range evs {
		switch ev.Kind {
		case types.AssistantEventUserMessage:
			edits = nil
		case types.AssistantEventPersonEdit:
			var p struct {
				Changed []assistant.Edit `json:"changed"`
			}
			_ = json.Unmarshal(ev.Payload, &p)
			edits = append(edits, p.Changed...)
		}
	}
	return edits, nil
}

// assistantEventsScan bounds how many events one read of a session's transcript takes.
const assistantEventsScan = 5000

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
