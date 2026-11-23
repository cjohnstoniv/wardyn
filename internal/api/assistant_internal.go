// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/assistant"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// assistantEventMax is the largest payload the run may post: 256 KiB.
const assistantEventMax = 256 << 10

// mountAssistantInternalRoutes registers the run-token routes of the assistant. Neither is ever relayed: an
// assistant run is placed remote, never on a person's runner.
func (s *Server) mountAssistantInternalRoutes(r chi.Router) {
	r.Get("/internal/assistant/turns", s.handleAssistantTurns)
	r.Post("/internal/assistant/events", s.handleAssistantPostEvent)
}

// assistantRunSession is the session the run's token speaks for: its live session, else its newest, else the same
// 404 an unknown run gets.
func (s *Server) assistantRunSession(w http.ResponseWriter, r *http.Request) (store.AssistantSessionStore, types.AssistantSession, bool) {
	ss, ok := s.assistantStore(w)
	if !ok {
		return nil, types.AssistantSession{}, false
	}
	claims, err := claimsFromContext(r)
	if err != nil {
		writeErrorReason(w, http.StatusUnauthorized, reasonMissingRunClaims, "missing run claims")
		return nil, types.AssistantSession{}, false
	}
	sess, err := ss.LiveAssistantSessionForRun(r.Context(), claims.RunID)
	if errors.Is(err, store.ErrNotFound) {
		sess, err = ss.LatestAssistantSessionForRun(r.Context(), claims.RunID)
	}
	if errors.Is(err, store.ErrNotFound) {
		writeErrorReason(w, http.StatusNotFound, reasonAssistantSessionNotFound, "Session not found.") // PROPOSED
		return nil, types.AssistantSession{}, false
	}
	if err != nil {
		writeServerError(w, r, "read assistant session", err)
		return nil, types.AssistantSession{}, false
	}
	return ss, sess, true
}

// handleAssistantTurns is GET /internal/assistant/turns?after=N: the relay's long-poll, at most 25 seconds. It
// answers the next person turn or reset after N, each with its session id and, on the first turn and on a reset,
// the records and the system prompt; an end turn once the run has no live session. A warm reuse that replaces the
// polled session on this run is followed to the new one, whose reset is numbered after the old session's last
// event, so the relay reads the reset and carries on.
func (s *Server) handleAssistantTurns(w http.ResponseWriter, r *http.Request) {
	ss, sess, ok := s.assistantRunSession(w, r)
	if !ok {
		return
	}
	after, valid := assistantAfter(r)
	if !valid {
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidRequestBody, "after must be a number.") // PROPOSED
		return
	}
	deadline := time.NewTimer(assistantLongPoll)
	defer deadline.Stop()
	tick := time.NewTicker(assistantPollInterval)
	defer tick.Stop()
	for {
		cur, err := ss.GetAssistantSession(r.Context(), sess.ID, sess.Principal)
		if err == nil && !cur.Live() {
			next, nerr := ss.LiveAssistantSessionForRun(r.Context(), *sess.RunID)
			if nerr == nil && next.ID != sess.ID {
				sess = next
				continue
			}
			if nerr != nil && !errors.Is(nerr, store.ErrNotFound) {
				writeServerError(w, r, "read assistant session", nerr)
				return
			}
			writeJSON(w, http.StatusOK, types.AssistantTurn{Kind: types.AssistantTurnEnd, SessionID: cur.ID})
			return
		}
		ev, first, err := ss.NextAssistantTurn(r.Context(), sess.ID, after)
		if err == nil {
			turn, terr := s.assistantTurnOf(r, ss, sess, ev, first)
			if terr != nil {
				writeServerError(w, r, "build assistant turn", terr)
				return
			}
			writeJSON(w, http.StatusOK, turn)
			return
		}
		if !errors.Is(err, store.ErrNotFound) {
			writeServerError(w, r, "read assistant turn", err)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-deadline.C:
			w.WriteHeader(http.StatusNoContent)
			return
		case <-tick.C:
		}
	}
}

// assistantTurnOf is the turn an event stands for. The first turn of a session and a reset carry the system
// prompt, the model, the tool names and the records: the ones stored when the session began, never rebuilt.
func (s *Server) assistantTurnOf(r *http.Request, ss store.AssistantSessionStore, sess types.AssistantSession, ev types.AssistantEvent, first bool) (types.AssistantTurn, error) {
	var p struct {
		Turn    string                   `json:"turn"`
		Text    string                   `json:"text"`
		Records []types.AssistantSection `json:"records"`
	}
	_ = json.Unmarshal(ev.Payload, &p)
	turn := types.AssistantTurn{Seq: ev.Seq, Kind: types.AssistantTurnUser, SessionID: sess.ID, Text: p.Turn}
	if ev.Kind == types.AssistantEventState {
		turn.Kind, turn.Text = types.AssistantTurnReset, ""
	}
	if !first && turn.Kind == types.AssistantTurnUser {
		return turn, nil
	}
	records := p.Records
	if records == nil {
		var err error
		if records, err = s.assistantStoredRecords(r.Context(), ss, sess.ID); err != nil {
			return turn, err
		}
	}
	turn.System, turn.Model, turn.Records, turn.Tools = assistant.SystemPrompt(sess.Purpose), sess.Binding.Model, records, assistantToolNames(sess.Purpose)
	return turn, nil
}

// assistantStoredRecords reads the records the session began with: its first state event, which the server
// wrote before the run could post anything (on a reuse, the reset that carries them).
func (s *Server) assistantStoredRecords(ctx context.Context, ss store.AssistantSessionStore, id uuid.UUID) ([]types.AssistantSection, error) {
	evs, err := ss.FindAssistantEvents(ctx, id, store.AssistantEventFilter{Kinds: []string{types.AssistantEventState}, Limit: 1})
	if err != nil || len(evs) == 0 {
		return nil, err
	}
	var p struct {
		State   string                   `json:"state"`
		Records []types.AssistantSection `json:"records"`
	}
	if json.Unmarshal(evs[0].Payload, &p) != nil || p.State != "records" {
		return nil, nil
	}
	return p.Records, nil
}

// assistantRunEvent is the body of POST /internal/assistant/events. Everything but the kind and the session
// id is the payload, kept as the run sent it.
type assistantRunEvent struct {
	SessionID uuid.UUID       `json:"session_id"`
	Kind      string          `json:"kind"`
	Payload   json.RawMessage `json:"payload"`
}

// handleAssistantPostEvent is POST /internal/assistant/events: the run's own kinds only, each with the live
// session's id. A server-only kind is 400; an id that is not the live session's, such as a delayed call of a
// replaced session, is 409 and writes nothing.
func (s *Server) handleAssistantPostEvent(w http.ResponseWriter, r *http.Request) {
	ss, sess, ok := s.assistantRunSession(w, r)
	if !ok {
		return
	}
	var ev assistantRunEvent
	body, ok := readCappedBody(w, r, assistantEventMax, "event")
	if !ok {
		return
	}
	if err := json.Unmarshal(body, &ev); err != nil || !types.AssistantRunEventKind(ev.Kind) || !assistantRunPayloadOK(ev) {
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidRequestBody, "the event is not one the run may post")
		return
	}
	if !sess.Live() || ev.SessionID != sess.ID {
		writeErrorReason(w, http.StatusConflict, reasonAssistantSessionNotFound, "That session is not the live one.") // PROPOSED
		return
	}
	payload := ev.Payload
	if len(payload) == 0 {
		payload = []byte("{}")
	}
	stored, err := ss.AppendAssistantEvent(r.Context(), sess.ID, ev.Kind, payload)
	if errors.Is(err, store.ErrAssistantSessionEnded) {
		writeErrorReason(w, http.StatusConflict, reasonAssistantSessionNotFound, "That session is not the live one.") // PROPOSED
		return
	}
	if err != nil {
		writeServerError(w, r, "append assistant event", err)
		return
	}
	s.assistantAfterRunEvent(r, ss, sess, stored)
	writeJSON(w, http.StatusAccepted, map[string]any{"seq": stored.Seq})
}

// assistantRunPayloadOK refuses a payload that is not a JSON object, a state payload that names a turn (the run
// cannot write a reset of its own), and any payload with records: only the server writes the records a session
// began with, and the audit row's prompt digest is taken from them.
func assistantRunPayloadOK(ev assistantRunEvent) bool {
	if len(ev.Payload) == 0 {
		return true
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(ev.Payload, &obj) != nil {
		return false
	}
	_, named := obj["turn"]
	_, records := obj["records"]
	return !records && !(ev.Kind == types.AssistantEventState && named)
}

// assistantAfterRunEvent does what a posted event owes: a ready state moves the session on; a turn_end writes
// the suggestion event and its audit row; an idle or provider_error notice ends the session.
func (s *Server) assistantAfterRunEvent(r *http.Request, ss store.AssistantSessionStore, sess types.AssistantSession, ev types.AssistantEvent) {
	ctx := r.Context()
	var p struct {
		State  string `json:"state"`
		Notice string `json:"notice"`
	}
	_ = json.Unmarshal(ev.Payload, &p)
	switch {
	case ev.Kind == types.AssistantEventState && p.State == "ready":
		_ = ss.TouchAssistantSession(ctx, sess.ID, true)
	case ev.Kind == types.AssistantEventTurnEnd:
		_ = ss.TouchAssistantSession(ctx, sess.ID, false)
		s.assistantSuggestion(r, ss, sess, ev)
	case ev.Kind == types.AssistantEventNotice && p.Notice == "idle":
		s.endAssistantSession(ctx, ss, sess, types.AssistantEndIdle, true)
	case ev.Kind == types.AssistantEventNotice && p.Notice == "provider_error":
		s.endAssistantSession(ctx, ss, sess, types.AssistantEndProviderError, true)
	}
}
