// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	// assistantLongPoll is how long a long-poll waits for an event.
	assistantLongPoll = 25 * time.Second
	// assistantStreamHold bounds one stream; the browser reconnects with its last seq.
	assistantStreamHold = 5 * time.Minute
	// assistantStreamBeat spaces the keepalive comment that holds a proxy's idle timeout.
	assistantStreamBeat = 15 * time.Second
	// assistantEventsPage is the most events one read returns.
	assistantEventsPage = 200
)

// assistantEventView is an event as the browser reads it, as stored. The console renders every payload as
// plain text.
type assistantEventView struct {
	Seq       int64           `json:"seq"`
	Kind      string          `json:"kind"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"created_at"`
}

func assistantEventViews(evs []types.AssistantEvent) []assistantEventView {
	out := make([]assistantEventView, len(evs))
	for i, e := range evs {
		out[i] = assistantEventView{Seq: e.Seq, Kind: e.Kind, Payload: e.Payload, CreatedAt: e.CreatedAt}
	}
	return out
}

// handleAssistantEvents is GET .../{id}/events?after=N for the session's owner. It answers a server-sent
// event stream when the request accepts text/event-stream, and otherwise a long-poll of at most 25 seconds.
// Both read the store, so a replica other than the one that took the event serves it within one poll interval.
func (s *Server) handleAssistantEvents(w http.ResponseWriter, r *http.Request) {
	ss, sess, ok := s.assistantOwned(w, r)
	if !ok {
		return
	}
	after, valid := assistantAfter(r)
	if !valid {
		writeErrorReason(w, http.StatusBadRequest, reasonAssistantSessionNotFound, "after must be a number.") // PROPOSED
		return
	}
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		s.assistantStream(w, r, ss, sess, after)
		return
	}
	deadline := time.NewTimer(assistantLongPoll)
	defer deadline.Stop()
	tick := time.NewTicker(assistantPollInterval)
	defer tick.Stop()
	for {
		evs, err := ss.ListAssistantEvents(r.Context(), sess.ID, after, assistantEventsPage)
		if err != nil {
			writeServerError(w, r, "read assistant events", err)
			return
		}
		cur, cerr := ss.GetAssistantSession(r.Context(), sess.ID, sess.Principal)
		if len(evs) > 0 || cerr != nil || !cur.Live() {
			writeJSON(w, http.StatusOK, map[string]any{"events": assistantEventViews(evs), "state": cur.State, "letters": cur.PersonLetters})
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-deadline.C:
			writeJSON(w, http.StatusOK, map[string]any{"events": []assistantEventView{}, "state": cur.State, "letters": cur.PersonLetters})
			return
		case <-tick.C:
		}
	}
}

// assistantStream writes events as they are stored until the session ends, the hold passes or the client goes.
func (s *Server) assistantStream(w http.ResponseWriter, r *http.Request, ss store.AssistantSessionStore, sess types.AssistantSession, after int64) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonAssistantSessionNotFound, "Streaming is not available here.") // PROPOSED
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fl.Flush()
	hold := time.NewTimer(assistantStreamHold)
	defer hold.Stop()
	tick := time.NewTicker(assistantPollInterval)
	defer tick.Stop()
	lastWrite := time.Now()
	for {
		evs, err := ss.ListAssistantEvents(r.Context(), sess.ID, after, assistantEventsPage)
		if err != nil {
			return
		}
		for _, e := range evs {
			body, _ := json.Marshal(assistantEventView{Seq: e.Seq, Kind: e.Kind, Payload: e.Payload, CreatedAt: e.CreatedAt})
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.Seq, e.Kind, body)
			after = e.Seq
		}
		cur, cerr := ss.GetAssistantSession(r.Context(), sess.ID, sess.Principal)
		if len(evs) == 0 && (cerr != nil || !cur.Live()) {
			return
		}
		if len(evs) > 0 || time.Since(lastWrite) >= assistantStreamBeat {
			if len(evs) == 0 {
				fmt.Fprint(w, ": keepalive\n\n")
			}
			fl.Flush()
			lastWrite = time.Now()
		}
		select {
		case <-r.Context().Done():
			return
		case <-hold.C:
			return
		case <-tick.C:
		}
	}
}
