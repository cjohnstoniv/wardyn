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
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
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

// TestRunEvents_ResumeAtTheEndOfAnEndedRingStillEnds: the ring a reconnecting
// reader meets can be shorter than the one it followed — synthesized after a
// restart, written by boot reconcile, or rebuilt after pruning — so its
// Last-Event-ID can land exactly on the last id. That reader must still be told
// the run ended, or the SDK reconnects forever.
func TestRunEvents_ResumeAtTheEndOfAnEndedRingStillEnds(t *testing.T) {
	srv, st, _ := statusDetailDispatchFixture(t, &fakeRunner{})
	ts := httptest.NewServer(panicFails(t, srv.Handler()))
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	setState := func(s types.RunState) {
		st.dispatchTestStore.mu.Lock()
		st.state = s
		st.dispatchTestStore.mu.Unlock()
	}

	previous := uuid.New()
	t.Run("ended before this daemon: [ended(1)] resumed at 1", func(t *testing.T) {
		setState(types.RunCompleted)
		s := openRunEvents(t, ctx, ts.URL, previous, "1")
		s.expect(client.RunEvent{ID: 1, Type: client.RunEventEnded, State: types.RunCompleted})
		s.expectClosed()
	})

	t.Run("boot reconcile: [failed(1), ended(2)] resumed at 2", func(t *testing.T) {
		reconciled := uuid.New()
		setState(types.RunRunning)
		if ok, err := srv.casRunState(ctx, reconciled, types.RunRunning, types.RunFailed); !ok || err != nil {
			t.Fatalf("casRunState: %v %v", ok, err)
		}
		s := openRunEvents(t, ctx, ts.URL, reconciled, "2")
		s.expect(client.RunEvent{ID: 2, Type: client.RunEventEnded, State: types.RunFailed})
		s.expectClosed()
	})

	t.Run("pruned ring of three: [ended(1)] resumed at 1", func(t *testing.T) {
		pruned := uuid.New()
		srv.runEvents.moved(pruned, types.RunPending, types.RunStarting)
		srv.runEvents.moved(pruned, types.RunStarting, types.RunRunning)
		srv.runEvents.moved(pruned, types.RunRunning, types.RunCompleted)
		srv.runEvents.mu.Lock()
		delete(srv.runEvents.rings, pruned)
		srv.runEvents.mu.Unlock()
		setState(types.RunCompleted)
		s := openRunEvents(t, ctx, ts.URL, pruned, "1")
		s.expect(client.RunEvent{ID: 1, Type: client.RunEventEnded, State: types.RunCompleted})
		s.expectClosed()
	})

	t.Run("the SDK resuming at the ring's end returns", func(t *testing.T) {
		setState(types.RunCompleted)
		sdkCtx, sdkCancel := context.WithTimeout(ctx, 4*time.Second)
		defer sdkCancel()
		var seen []string
		err := client.New(ts.URL, adminToken).RunEvents(sdkCtx, previous, 1, func(ev client.RunEvent) error {
			seen = append(seen, ev.Type)
			return nil
		})
		if err != nil || len(seen) != 1 || seen[0] != client.RunEventEnded {
			t.Fatalf("err=%v events=%v, want nil after ended", err, seen)
		}
	})
}

// TestRunEvents_SettleSynthesizesTheMissingFailed: when the store already says
// FAILED before the CAS's own emit lands, the ring still gets its failed event,
// with the phase its last event implies. With no events at all (a previous
// daemon's run) the phase is unknown, so ended stands alone.
func TestRunEvents_SettleSynthesizesTheMissingFailed(t *testing.T) {
	for name, tc := range map[string]struct {
		moves [][2]types.RunState
		want  []client.RunEvent
	}{
		"after ready": {
			moves: [][2]types.RunState{{types.RunPending, types.RunStarting}, {types.RunStarting, types.RunRunning}},
			want: []client.RunEvent{{Type: client.RunEventProvisioning}, {Type: client.RunEventReady},
				{Type: client.RunEventFailed, Reason: client.RunFailedRunFailed}, {Type: client.RunEventEnded, State: types.RunFailed}},
		},
		"while provisioning": {
			moves: [][2]types.RunState{{types.RunPending, types.RunStarting}},
			want: []client.RunEvent{{Type: client.RunEventProvisioning},
				{Type: client.RunEventFailed, Reason: client.RunFailedStartFailed}, {Type: client.RunEventEnded, State: types.RunFailed}},
		},
		"no feed": {want: []client.RunEvent{{Type: client.RunEventEnded, State: types.RunFailed}}},
	} {
		t.Run(name, func(t *testing.T) {
			var h runEventHub
			id := uuid.New()
			for _, m := range tc.moves {
				h.moved(id, m[0], m[1])
			}
			h.settle(id, types.RunFailed)
			h.moved(id, types.RunRunning, types.RunFailed) // the CAS's own emit, landing late
			got, _, _ := h.since(id, 0)
			if len(got) != len(tc.want) {
				t.Fatalf("feed = %+v, want %+v", got, tc.want)
			}
			for i, w := range tc.want {
				if got[i].Type != w.Type || got[i].Reason != w.Reason || got[i].State != w.State {
					t.Fatalf("feed[%d] = %+v, want %+v", i, got[i], w)
				}
			}
		})
	}
}

// flippableRevocations is a SessionRevocations double whose answer a test
// changes mid-stream.
type flippableRevocations struct {
	fakeAuthzSessionRevocations
	revoked atomic.Bool
}

func (f *flippableRevocations) IsSessionRevoked(context.Context, string, string, time.Time) (bool, error) {
	return f.revoked.Load(), nil
}

// TestRunEvents_RevokedSessionEndsTheStreamAtTheNextKeepalive: authentication
// runs once per request, so the keepalive re-checks the session cutoff and a
// revoke lands within one beat rather than one hold.
func TestRunEvents_RevokedSessionEndsTheStreamAtTheNextKeepalive(t *testing.T) {
	revs := &flippableRevocations{}
	srv, ast, _, _ := newAuthzMatrixServer(t, func(c *Config) { c.SessionRevocations = revs })
	srv.runEvents.hold, srv.runEvents.beat = 0, 10*time.Millisecond
	id := uuid.New()
	ast.mu.Lock()
	ast.runs[id] = types.AgentRun{ID: id, CreatedBy: "sub-member", State: types.RunRunning, Agent: "claude-code"}
	ast.mu.Unlock()
	ts := httptest.NewServer(panicFails(t, srv.Handler()))
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/runs/"+id.String()+"/events", nil)
	req.AddCookie(ssoSession(t, "sub-member", "member@corp.example", oidc.RoleUser))
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("open: %v %v", resp, err)
	}
	defer resp.Body.Close()
	lines := make(chan string)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	// Held across several beats while the session is live.
	for beats := 0; beats < 3; {
		if l, ok := <-lines; !ok {
			t.Fatal("stream closed while the session was live")
		} else if l == ": keepalive" {
			beats++
		}
	}
	revs.revoked.Store(true)
	for range lines {
		// drain until the server closes the stream
	}
	if ctx.Err() != nil {
		t.Fatal("stream outlived the revoked session")
	}
}

// openEventsAs opens GET /runs/{id}/events with bearer and returns the
// response whatever its status; the stream is closed when ctx ends.
func openEventsAs(t *testing.T, ctx context.Context, base string, runID uuid.UUID, bearer string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/runs/"+runID.String()+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// TestRunEvents_DelegatedTokenReadsItsPersonsRun (#1407): a portal's delegated
// token streams its person's run like the person would; the same token on
// another person's run gets GET /runs/{id}'s byte-identical 404, and the
// not_owner row names the person with data.via naming the portal.
func TestRunEvents_DelegatedTokenReadsItsPersonsRun(t *testing.T) {
	srv, ast, _, _ := newAuthzMatrixServer(t)
	rec := srv.cfg.Audit.(*recRecorder)
	const person = "sub-person"
	tok, via := seedDelegation(t, ast.fakeDelegateStore, person)
	seed := func(owner string) uuid.UUID {
		id := uuid.New()
		ast.mu.Lock()
		ast.runs[id] = types.AgentRun{ID: id, CreatedBy: owner, State: types.RunCompleted, Agent: "claude-code"}
		ast.mu.Unlock()
		return id
	}
	own, foreign := seed(person), seed("sub-someone-else")

	w := do(t, srv, http.MethodGet, "/api/v1/runs/"+own.String()+"/events", tok, "")
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "text/event-stream" ||
		!strings.Contains(w.Body.String(), `"type":"ended"`) {
		t.Fatalf("own run: status %d type %q body %q, want 200 text/event-stream carrying ended",
			w.Code, w.Header().Get("Content-Type"), w.Body)
	}

	before := len(rec.snapshot())
	events := do(t, srv, http.MethodGet, "/api/v1/runs/"+foreign.String()+"/events", tok, "")
	rows := rec.snapshot()[before:]
	get := do(t, srv, http.MethodGet, "/api/v1/runs/"+foreign.String(), tok, "")
	if events.Code != http.StatusNotFound || get.Code != http.StatusNotFound || events.Body.String() != get.Body.String() {
		t.Fatalf("another person's run: events %d %q, GET run %d %q; want the same 404 for both",
			events.Code, events.Body, get.Code, get.Body)
	}
	if len(rows) != 1 || rows[0].Action != authz.AuditAction || rows[0].Actor != person ||
		!strings.Contains(string(rows[0].Data), `"reason":"not_owner"`) ||
		!strings.Contains(string(rows[0].Data), `"delegate":"`+via.Delegate.String()+`"`) {
		t.Fatalf("refusal rows = %+v, want one not_owner row naming the person and the portal", rows)
	}
}

// TestRunEvents_StreamCapPerPrincipal (#1407): one principal holds at most
// maxRunEventStreams streams; the next is refused 422 event_stream_cap, another
// principal is unaffected, and a disconnect frees its slot.
func TestRunEvents_StreamCapPerPrincipal(t *testing.T) {
	srv, ast, _, _ := newAuthzMatrixServer(t)
	srv.runEvents.hold = 0 // hold the streams open
	const person = "sub-person"
	tok, _ := seedDelegation(t, ast.fakeDelegateStore, person)
	id := uuid.New()
	ast.mu.Lock()
	ast.runs[id] = types.AgentRun{ID: id, CreatedBy: person, State: types.RunRunning, Agent: "claude-code"}
	ast.mu.Unlock()
	ts := httptest.NewServer(panicFails(t, srv.Handler()))
	t.Cleanup(ts.Close)

	cancels := make([]context.CancelFunc, 0, maxRunEventStreams)
	for i := range maxRunEventStreams {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		cancels = append(cancels, cancel)
		if resp := openEventsAs(t, ctx, ts.URL, id, tok); resp.StatusCode != http.StatusOK {
			t.Fatalf("stream %d of %d: status %d, want 200", i+1, maxRunEventStreams, resp.StatusCode)
		}
	}

	over := openEventsAs(t, context.Background(), ts.URL, id, tok)
	var body errorBody
	_ = json.NewDecoder(over.Body).Decode(&body)
	if over.StatusCode != http.StatusUnprocessableEntity || body.Reason != string(authz.ReasonEventStreamCap) {
		t.Fatalf("stream %d: status %d reason %q, want 422 event_stream_cap", maxRunEventStreams+1, over.StatusCode, body.Reason)
	}

	// The cap is per principal: the admin token still streams the same run.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if resp := openEventsAs(t, ctx, ts.URL, id, adminToken); resp.StatusCode != http.StatusOK {
		t.Fatalf("another principal at the person's cap: status %d, want 200", resp.StatusCode)
	}

	// A disconnect frees a slot.
	cancels[0]()
	who := runEventStreamer{types.ActorHuman, person}
	deadline := time.Now().Add(5 * time.Second)
	for {
		srv.runEvents.mu.Lock()
		n := srv.runEvents.streams[who]
		srv.runEvents.mu.Unlock()
		if n < maxRunEventStreams {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a closed stream never released its slot")
		}
		time.Sleep(5 * time.Millisecond)
	}
	ctx, cancel = context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if resp := openEventsAs(t, ctx, ts.URL, id, tok); resp.StatusCode != http.StatusOK {
		t.Fatalf("after a disconnect: status %d, want 200", resp.StatusCode)
	}
}

// TestRunEvents_RevokedPersonEndsTheDelegatedStream (#1407): revoking the
// person's sessions ends a portal's open stream at the next keepalive, as it
// does the person's own.
func TestRunEvents_RevokedPersonEndsTheDelegatedStream(t *testing.T) {
	revs := &flippableRevocations{}
	srv, ast, _, _ := newAuthzMatrixServer(t, func(c *Config) { c.SessionRevocations = revs })
	srv.runEvents.hold, srv.runEvents.beat = 0, 10*time.Millisecond
	const person = "sub-person"
	tok, _ := seedDelegation(t, ast.fakeDelegateStore, person)
	id := uuid.New()
	ast.mu.Lock()
	ast.runs[id] = types.AgentRun{ID: id, CreatedBy: person, State: types.RunRunning, Agent: "claude-code"}
	ast.mu.Unlock()
	ts := httptest.NewServer(panicFails(t, srv.Handler()))
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp := openEventsAs(t, ctx, ts.URL, id, tok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("open: status %d", resp.StatusCode)
	}
	lines := make(chan string)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	for beats := 0; beats < 3; {
		if l, ok := <-lines; !ok {
			t.Fatal("delegated stream closed while the person's session was live")
		} else if l == ": keepalive" {
			beats++
		}
	}
	revs.revoked.Store(true)
	for range lines {
		// drain until the server closes the stream
	}
	if ctx.Err() != nil {
		t.Fatal("delegated stream outlived the person's revoked sessions")
	}
}

// TestRunEvents_DeadDelegatedTokenEndsTheStream (#1413): the portal is checked
// once at open, so the keepalive re-asks the store — a portal revoked
// mid-stream, or a delegated token past its ten-minute TTL, ends the stream at
// the next beat instead of the hold, and a store that cannot answer ends it
// too.
func TestRunEvents_DeadDelegatedTokenEndsTheStream(t *testing.T) {
	for name, kill := range map[string]func(ast *authzStore, via types.DelegationVia, clock *atomic.Int64){
		"portal revoked": func(ast *authzStore, via types.DelegationVia, _ *atomic.Int64) {
			if _, err := ast.RevokeDelegate(context.Background(), via.Delegate, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
		},
		"token expired": func(_ *authzStore, _ types.DelegationVia, clock *atomic.Int64) {
			clock.Store(int64(delegatedTokenTTL + time.Minute))
		},
	} {
		t.Run(name, func(t *testing.T) {
			var skew atomic.Int64 // nanoseconds the server clock is ahead
			srv, ast, _, _ := newAuthzMatrixServer(t, func(c *Config) {
				c.Now = func() time.Time { return time.Now().Add(time.Duration(skew.Load())) }
			})
			srv.runEvents.hold, srv.runEvents.beat = 0, 10*time.Millisecond
			const person = "sub-person"
			tok, via := seedDelegation(t, ast.fakeDelegateStore, person)
			id := uuid.New()
			ast.mu.Lock()
			ast.runs[id] = types.AgentRun{ID: id, CreatedBy: person, State: types.RunRunning, Agent: "claude-code"}
			ast.mu.Unlock()
			ts := httptest.NewServer(panicFails(t, srv.Handler()))
			t.Cleanup(ts.Close)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			resp := openEventsAs(t, ctx, ts.URL, id, tok)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("open: status %d", resp.StatusCode)
			}
			lines := make(chan string)
			go func() {
				defer close(lines)
				sc := bufio.NewScanner(resp.Body)
				for sc.Scan() {
					lines <- sc.Text()
				}
			}()
			for beats := 0; beats < 3; {
				if l, ok := <-lines; !ok {
					t.Fatal("stream closed while the token was live")
				} else if l == ": keepalive" {
					beats++
				}
			}
			kill(ast, via, &skew)
			for range lines {
				// drain until the server closes the stream
			}
			if ctx.Err() != nil {
				t.Fatal("stream outlived its dead delegated token")
			}
		})
	}
}
