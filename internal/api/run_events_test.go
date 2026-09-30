// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// runEventStream is one open GET /runs/{id}/events connection.
type runEventStream struct {
	t    *testing.T
	resp *http.Response
	sc   *bufio.Scanner
}

func openRunEvents(t *testing.T, ctx context.Context, base string, runID uuid.UUID, lastEventID string) *runEventStream {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/runs/"+runID.String()+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+adminToken)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET events: status %d, body %s", resp.StatusCode, b)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	return &runEventStream{t: t, resp: resp, sc: bufio.NewScanner(resp.Body)}
}

// next reads one event, checking that its id: and event: lines agree with the
// data it carries. ok is false when the server closed the stream.
func (s *runEventStream) next() (ev client.RunEvent, ok bool) {
	s.t.Helper()
	var id, typ string
	for s.sc.Scan() {
		line := s.sc.Text()
		switch {
		case strings.HasPrefix(line, "id: "):
			id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			typ = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
				s.t.Fatalf("decode %q: %v", line, err)
			}
			if id != strconv.FormatUint(ev.ID, 10) || typ != ev.Type {
				s.t.Fatalf("id:/event: lines (%q, %q) disagree with data %+v", id, typ, ev)
			}
			return ev, true
		}
	}
	return client.RunEvent{}, false
}

func (s *runEventStream) expect(want ...client.RunEvent) {
	s.t.Helper()
	for _, w := range want {
		got, ok := s.next()
		if !ok {
			s.t.Fatalf("stream closed; want %+v", w)
		}
		if got.ID != w.ID || got.Type != w.Type || got.Reason != w.Reason || got.State != w.State {
			s.t.Fatalf("event = {id:%d type:%s reason:%q state:%q}, want {id:%d type:%s reason:%q state:%q}",
				got.ID, got.Type, got.Reason, got.State, w.ID, w.Type, w.Reason, w.State)
		}
	}
}

func (s *runEventStream) expectClosed() {
	s.t.Helper()
	if ev, ok := s.next(); ok {
		s.t.Fatalf("stream carried %+v after ended; want it closed", ev)
	}
	if err := s.sc.Err(); err != nil {
		s.t.Fatalf("stream ended with %v, want a clean close", err)
	}
}

// TestRunEvents_LaunchStreamsInOrderAndResumesWithoutGaps is #1144's done-when:
// a real dispatch emits provisioning -> pulling -> ready in order, a reconnect
// with Last-Event-ID resumes at the next id with nothing repeated or skipped,
// a failure carries its machine reason, and the stream ends after ended.
func TestRunEvents_LaunchStreamsInOrderAndResumesWithoutGaps(t *testing.T) {
	rn := &waitingRunner{fakeRunner: &fakeRunner{}, details: []string{
		"image: Pulling: wardyn/claude-code:latest",
		"agent: ContainerCreating",
		"image: Pulling: wardyn/proxy:latest", // a second pull is not a second step
	}}
	srv, st, run := statusDetailDispatchFixture(t, rn)
	dispatchOnce(srv, run)
	if st.State() != types.RunRunning {
		t.Fatalf("dispatch left the run %s, want RUNNING", st.State())
	}
	ts := httptest.NewServer(panicFails(t, srv.Handler()))
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	first := openRunEvents(t, ctx, ts.URL, run.ID, "")
	first.expect(
		client.RunEvent{ID: 1, Type: client.RunEventProvisioning},
		client.RunEvent{ID: 2, Type: client.RunEventPulling},
	)
	first.resp.Body.Close() // the client drops after id 2

	resumed := openRunEvents(t, ctx, ts.URL, run.ID, "2")
	resumed.expect(client.RunEvent{ID: 3, Type: client.RunEventReady})

	// Live delivery to the open stream, through the one CAS every path uses.
	if ok, err := srv.casRunState(ctx, run.ID, types.RunRunning, types.RunFailed); !ok || err != nil {
		t.Fatalf("casRunState: %v %v", ok, err)
	}
	resumed.expect(
		client.RunEvent{ID: 4, Type: client.RunEventFailed, Reason: client.RunFailedRunFailed},
		client.RunEvent{ID: 5, Type: client.RunEventEnded, State: types.RunFailed},
	)
	resumed.expectClosed()

	// A reader arriving after the end gets the whole feed and the same close.
	late := openRunEvents(t, ctx, ts.URL, run.ID, "")
	late.expect(
		client.RunEvent{ID: 1, Type: client.RunEventProvisioning},
		client.RunEvent{ID: 2, Type: client.RunEventPulling},
		client.RunEvent{ID: 3, Type: client.RunEventReady},
		client.RunEvent{ID: 4, Type: client.RunEventFailed, Reason: client.RunFailedRunFailed},
		client.RunEvent{ID: 5, Type: client.RunEventEnded, State: types.RunFailed},
	)
	late.expectClosed()
}

// TestRunEvents_EndOnASilentPathStillEndsTheStream: a run can end on a path
// that emits nothing (a lease end) or before this daemon started. The stream
// must still end — on open when the run is already terminal, and on the next
// heartbeat when it ends while the stream is held.
func TestRunEvents_EndOnASilentPathStillEndsTheStream(t *testing.T) {
	srv, st, run := statusDetailDispatchFixture(t, &fakeRunner{})
	srv.runEvents.beat = 10 * time.Millisecond
	dispatchOnce(srv, run)
	ts := httptest.NewServer(panicFails(t, srv.Handler()))
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	held := openRunEvents(t, ctx, ts.URL, run.ID, "")
	held.expect(
		client.RunEvent{ID: 1, Type: client.RunEventProvisioning},
		client.RunEvent{ID: 2, Type: client.RunEventReady},
	)
	st.dispatchTestStore.mu.Lock()
	st.state = types.RunStopped // written behind casRunState's back
	st.dispatchTestStore.mu.Unlock()
	held.expect(client.RunEvent{ID: 3, Type: client.RunEventEnded, State: types.RunStopped})
	held.expectClosed()

	// Already terminal with no feed at all (a previous daemon's run).
	other := uuid.New()
	st.dispatchTestStore.mu.Lock()
	st.run.ID, st.state = other, types.RunCompleted
	st.dispatchTestStore.mu.Unlock()
	fresh := openRunEvents(t, ctx, ts.URL, other, "7") // an id from the previous daemon
	fresh.expect(client.RunEvent{ID: 1, Type: client.RunEventEnded, State: types.RunCompleted})
	fresh.expectClosed()
}

// TestRunEvents_IdleStopEmitsIdleStoppedThenEnded: the idle reaper's one call
// into this package after its RUNNING->STOPPED win is what reaches the feed.
func TestRunEvents_IdleStopEmitsIdleStoppedThenEnded(t *testing.T) {
	srv, _, run := statusDetailDispatchFixture(t, &fakeRunner{})
	dispatchOnce(srv, run)
	srv.CancelTerminalRunApprovals(context.Background(), run.ID)
	evs, _, ended := srv.runEvents.since(run.ID, 2)
	if !ended || len(evs) != 2 || evs[0].Type != client.RunEventIdleStopped ||
		evs[1].Type != client.RunEventEnded || evs[1].State != types.RunStopped {
		t.Fatalf("after ready: %+v (ended=%v), want idle_stopped then ended{STOPPED}", evs, ended)
	}
	// Closed: a later move (a KILLED->KILLED re-kill, say) adds nothing.
	srv.runEvents.moved(run.ID, types.RunKilled, types.RunKilled)
	srv.runEvents.settle(run.ID, types.RunKilled)
	if evs, _, _ := srv.runEvents.since(run.ID, 0); len(evs) != 4 {
		t.Fatalf("an ended feed grew to %d events", len(evs))
	}
}

// TestRunEvents_NonReaderGets404: a member who cannot read the run gets the
// byte-identical 404 GET /runs/{id} answers — for a foreign run and for a
// missing one — and the owner is admitted to the stream.
func TestRunEvents_NonReaderGets404(t *testing.T) {
	srv, ast, _, _ := newAuthzMatrixServer(t)
	member := ssoSession(t, "sub-member", "member@corp.example", oidc.RoleUser)
	seed := func(owner string) uuid.UUID {
		id := uuid.New()
		ast.mu.Lock()
		ast.runs[id] = types.AgentRun{ID: id, CreatedBy: owner, State: types.RunCompleted, Agent: "claude-code"}
		ast.mu.Unlock()
		return id
	}
	foreign, own := seed("sub-other-member"), seed("sub-member")

	for name, id := range map[string]uuid.UUID{"foreign": foreign, "missing": uuid.New()} {
		events := doSSO(t, srv, http.MethodGet, "/api/v1/runs/"+id.String()+"/events", member, "")
		get := doSSO(t, srv, http.MethodGet, "/api/v1/runs/"+id.String(), member, "")
		if events.Code != http.StatusNotFound || get.Code != http.StatusNotFound {
			t.Fatalf("%s run: events %d, GET run %d; want 404 for both", name, events.Code, get.Code)
		}
		if events.Body.String() != get.Body.String() {
			t.Fatalf("%s run: events body %q differs from GET /runs/{id}'s %q — an existence oracle", name, events.Body, get.Body)
		}
		if strings.Contains(events.Header().Get("Content-Type"), "event-stream") {
			t.Fatalf("%s run: a refused reader was answered as a stream", name)
		}
	}

	w := doSSO(t, srv, http.MethodGet, "/api/v1/runs/"+own.String()+"/events", member, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"type":"ended"`) {
		t.Fatalf("owner: status %d body %q, want 200 carrying ended", w.Code, w.Body)
	}
}
