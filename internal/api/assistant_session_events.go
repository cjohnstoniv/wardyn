// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
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
	// assistantEventsPage is the most events one read returns: with the run's 256 KiB cap on an event, one read
	// holds at most about 12 MiB.
	assistantEventsPage = 48
	// assistantStreamsPerPerson is how many event streams one person may hold open at once, across their
	// sessions and tabs.
	assistantStreamsPerPerson = 4
	// assistantStreamWrite bounds one batch's writes and flush: a client that stops reading is cut off
	// instead of pinning the stream.
	assistantStreamWrite = 30 * time.Second
)

// assistantStreams counts each person's open event streams.
// ponytail: process-wide, so the cap holds per replica; a per-server count needs a Server field.
var assistantStreams = struct {
	sync.Mutex
	open map[string]int
}{open: map[string]int{}}

// assistantStreamTake takes one of person's stream slots and returns its release, or false when they hold
// assistantStreamsPerPerson already.
func assistantStreamTake(person string) (func(), bool) {
	assistantStreams.Lock()
	defer assistantStreams.Unlock()
	if assistantStreams.open[person] >= assistantStreamsPerPerson {
		return nil, false
	}
	assistantStreams.open[person]++
	return func() {
		assistantStreams.Lock()
		defer assistantStreams.Unlock()
		if assistantStreams.open[person]--; assistantStreams.open[person] <= 0 {
			delete(assistantStreams.open, person)
		}
	}, true
}

// assistantStreamAfter is where a stream starts: the cursor, or the browser's own Last-Event-ID when a
// reconnect carries one above it.
func assistantStreamAfter(r *http.Request, after int64) int64 {
	if id, err := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64); err == nil && id > after {
		return id
	}
	return after
}

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
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidRequestBody, "after must be a number.") // PROPOSED
		return
	}
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		release, ok := assistantStreamTake(sess.Principal)
		if !ok {
			w.Header().Set("Retry-After", "15")
			writeErrorReason(w, http.StatusTooManyRequests, reasonAssistantRateLimited, "Too many open streams. Close a tab and try again.") // PROPOSED
			return
		}
		defer release()
		s.assistantStream(w, r, ss, sess, assistantStreamAfter(r, after))
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
	// Each batch sets its own write deadline and clears it before the wait, as the run events stream does: a
	// client that stops reading fails the write instead of holding the stream past its hold.
	rc := http.NewResponseController(w)
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
		cur, cerr := ss.GetAssistantSession(r.Context(), sess.ID, sess.Principal)
		if len(evs) == 0 && (cerr != nil || !cur.Live()) {
			return
		}
		if len(evs) > 0 || time.Since(lastWrite) >= assistantStreamBeat {
			_ = rc.SetWriteDeadline(time.Now().Add(assistantStreamWrite))
			for _, e := range evs {
				body, _ := json.Marshal(assistantEventView{Seq: e.Seq, Kind: e.Kind, Payload: e.Payload, CreatedAt: e.CreatedAt})
				if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.Seq, e.Kind, body); err != nil {
					return
				}
				after = e.Seq
			}
			if len(evs) == 0 {
				if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
					return
				}
			}
			if err := rc.Flush(); err != nil {
				return
			}
			_ = rc.SetWriteDeadline(time.Time{})
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
