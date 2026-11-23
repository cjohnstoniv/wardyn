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

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/livebus"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	// assistantLongPoll is how long a long-poll waits for an event.
	assistantLongPoll = 25 * time.Second
	// assistantStreamHold bounds one stream; the browser reconnects with its last seq.
	assistantStreamHold = 5 * time.Minute
	// assistantStreamBeat spaces the keepalive comment that holds a proxy's idle timeout, and the store read that
	// catches up on a lost notice, as the run events stream's beat does (run_events.go).
	assistantStreamBeat = 15 * time.Second
	// assistantEventsPage is the most events one read returns: with the run's 256 KiB cap on an event, one read
	// holds at most about 12 MiB.
	assistantEventsPage = 48
	// assistantStreamsPerPerson is how many event streams and long-polls one person may hold open at once, across
	// their sessions and tabs.
	assistantStreamsPerPerson = 4
	// assistantStreamWrite bounds one batch's writes and flush: a client that stops reading is cut off
	// instead of pinning the stream.
	assistantStreamWrite = 30 * time.Second
)

// assistantLive is this replica's wake-ups for the assistant's readers, and each person's count of open event
// reads. A reader waits for a wake of its session (a stream or a long-poll) or of its run (the relay's turns),
// never on a timer that reads the store. Every change to a session wakes both, here and, over livebus, on the
// other replicas (assistantChanged). A wake is a hint, as every notice is: a reader reads the store before it
// waits, a long-poll's end makes the caller read again, and a stream reads again at each beat, so a lost notice
// costs promptness only. Zero value is ready to use.
type assistantLive struct {
	mu    sync.Mutex
	waits map[uuid.UUID]*assistantWait
	reads map[string]int
}

// assistantWait is one id's waiters: ch is closed at the id's next wake.
type assistantWait struct {
	ch chan struct{}
	n  int
}

// watch returns a channel closed at the next wake of id, and its release. A reader takes it before the store
// read it guards, so a wake between the read and the wait is not lost.
func (l *assistantLive) watch(id uuid.UUID) (<-chan struct{}, func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.waits == nil {
		l.waits = map[uuid.UUID]*assistantWait{}
	}
	w := l.waits[id]
	if w == nil {
		w = &assistantWait{ch: make(chan struct{})}
		l.waits[id] = w
	}
	w.n++
	return w.ch, func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if w.n--; w.n == 0 && l.waits[id] == w {
			delete(l.waits, id)
		}
	}
}

// wake ends the waits on each of ids; uuid.Nil names nothing.
func (l *assistantLive) wake(ids ...uuid.UUID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, id := range ids {
		if w := l.waits[id]; id != uuid.Nil && w != nil {
			close(w.ch)
			delete(l.waits, id)
		}
	}
}

// take takes one of person's read slots and returns its release, or false when they hold
// assistantStreamsPerPerson already.
func (l *assistantLive) take(person string) (func(), bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.reads == nil {
		l.reads = map[string]int{}
	}
	if l.reads[person] >= assistantStreamsPerPerson {
		return nil, false
	}
	l.reads[person]++
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.reads[person]--; l.reads[person] <= 0 {
			delete(l.reads, person)
		}
	}, true
}

// assistantEventNotice is a KindAssistantEvent's data: the session that changed.
type assistantEventNotice struct {
	Session uuid.UUID `json:"s"`
}

// assistantChanged wakes the readers of session and of its run on every replica. Every store write to a session
// that a reader waits on calls it: an event, a replace, an end.
func (s *Server) assistantChanged(session uuid.UUID, run *uuid.UUID) {
	runID := uuid.Nil
	if run != nil {
		runID = *run
	}
	s.assistantLive.wake(session, runID)
	s.publishNotice(livebus.KindAssistantEvent, runID, assistantEventNotice{Session: session})
}

// onAssistantEventNotice wakes this replica's readers of another replica's change.
func (s *Server) onAssistantEventNotice(m livebus.Message) {
	if m.Origin == s.replicaName() {
		return
	}
	var n assistantEventNotice
	if unmarshalNotice(m, &n) != nil {
		return
	}
	s.assistantLive.wake(n.Session, m.Run)
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
// Both count against the person's assistantStreamsPerPerson, and both wait for the session's wake
// (assistantLive), so a replica other than the one that took the event serves it when the notice arrives.
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
	release, ok := s.assistantLive.take(sess.Principal)
	if !ok {
		w.Header().Set("Retry-After", "15")
		writeErrorReason(w, http.StatusTooManyRequests, reasonAssistantRateLimited, "Too many open streams. Close a tab and try again.") // PROPOSED
		return
	}
	defer release()
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		s.assistantStream(w, r, ss, sess, assistantStreamAfter(r, after))
		return
	}
	deadline := time.NewTimer(assistantLongPoll)
	defer deadline.Stop()
	for !s.assistantPollStep(w, r, ss, sess, after, deadline.C) {
	}
}

// assistantPollStep is one read of the long-poll, then its wait. It reports whether the poll has answered.
func (s *Server) assistantPollStep(w http.ResponseWriter, r *http.Request, ss store.AssistantSessionStore, sess types.AssistantSession, after int64, deadline <-chan time.Time) bool {
	wake, unwatch := s.assistantLive.watch(sess.ID)
	defer unwatch()
	evs, err := ss.ListAssistantEvents(r.Context(), sess.ID, after, assistantEventsPage)
	if err != nil {
		writeServerError(w, r, "read assistant events", err)
		return true
	}
	cur, cerr := ss.GetAssistantSession(r.Context(), sess.ID, sess.Principal)
	if len(evs) > 0 || cerr != nil || !cur.Live() {
		writeJSON(w, http.StatusOK, map[string]any{"events": assistantEventViews(evs), "state": cur.State, "letters": cur.PersonLetters})
		return true
	}
	select {
	case <-r.Context().Done():
		return true
	case <-deadline:
		writeJSON(w, http.StatusOK, map[string]any{"events": []assistantEventView{}, "state": cur.State, "letters": cur.PersonLetters})
		return true
	case <-wake:
		return false
	}
}

// assistantStream writes events as they are stored until the session ends, the hold passes or the client goes.
// It reads the store when the session is woken, and at each beat, which also writes the keepalive.
func (s *Server) assistantStream(w http.ResponseWriter, r *http.Request, ss store.AssistantSessionStore, sess types.AssistantSession, after int64) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonAssistantStreamUnavailable, "Streaming is not available here.") // PROPOSED
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fl.Flush()
	st := assistantStreamState{rc: http.NewResponseController(w), after: after}
	hold := time.NewTimer(assistantStreamHold)
	defer hold.Stop()
	beat := time.NewTicker(assistantStreamBeat)
	defer beat.Stop()
	for s.assistantStreamStep(w, r, ss, sess, &st, hold.C, beat.C) {
	}
}

// assistantStreamState is a stream's cursor, its writer and whether a keepalive is due.
type assistantStreamState struct {
	rc      *http.ResponseController
	after   int64
	beatDue bool
}

// assistantStreamStep is one read and write of the stream, then its wait. It reports whether the stream goes on.
// A full page reads again at once, without waiting.
func (s *Server) assistantStreamStep(w http.ResponseWriter, r *http.Request, ss store.AssistantSessionStore, sess types.AssistantSession, st *assistantStreamState, hold, beat <-chan time.Time) bool {
	wake, unwatch := s.assistantLive.watch(sess.ID)
	defer unwatch()
	evs, err := ss.ListAssistantEvents(r.Context(), sess.ID, st.after, assistantEventsPage)
	if err != nil {
		return false
	}
	cur, cerr := ss.GetAssistantSession(r.Context(), sess.ID, sess.Principal)
	if len(evs) == 0 && (cerr != nil || !cur.Live()) {
		return false
	}
	if len(evs) > 0 || st.beatDue {
		// Each batch sets its own write deadline and clears it before the wait, as the run events stream does: a
		// client that stops reading fails the write instead of holding the stream past its hold.
		_ = st.rc.SetWriteDeadline(time.Now().Add(assistantStreamWrite))
		for _, e := range evs {
			body, _ := json.Marshal(assistantEventView{Seq: e.Seq, Kind: e.Kind, Payload: e.Payload, CreatedAt: e.CreatedAt})
			if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.Seq, e.Kind, body); err != nil {
				return false
			}
			st.after = e.Seq
		}
		if len(evs) == 0 {
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return false
			}
		}
		if err := st.rc.Flush(); err != nil {
			return false
		}
		_ = st.rc.SetWriteDeadline(time.Time{})
	}
	st.beatDue = false
	if len(evs) == assistantEventsPage {
		return true
	}
	select {
	case <-r.Context().Done():
		return false
	case <-hold:
		return false
	case <-beat:
		st.beatDue = true
	case <-wake:
	}
	return true
}
