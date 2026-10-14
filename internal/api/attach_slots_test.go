// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The per-run cap on live attach sessions (attach_slots.go): writers and observers, web and SSH,
// counted from before the exec opens until the session's recording is finalized.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// attachSlotsHeld is the number of attach sessions runID holds slots for.
func attachSlotsHeld(s *Server, runID uuid.UUID) int {
	reg := s.attachRegistry()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	return reg.slots[runID]
}

func TestAttachSlots_SimultaneousContendersAtTheCapReleaseAndRetry(t *testing.T) {
	s := &Server{}
	run := uuid.New()
	start := make(chan struct{})
	type result struct {
		release func()
		ok      bool
	}
	results := make(chan result)
	for range 2 * maxAttachSessionsPerRun {
		go func() {
			<-start
			release, ok := s.reserveAttachSlot(run)
			results <- result{release, ok}
		}()
	}
	close(start)
	var held []func()
	for range 2 * maxAttachSessionsPerRun {
		if r := <-results; r.ok {
			held = append(held, r.release)
		}
	}
	if len(held) != maxAttachSessionsPerRun || attachSlotsHeld(s, run) != maxAttachSessionsPerRun {
		t.Fatalf("%d contenders won (%d counted), want exactly %d", len(held), attachSlotsHeld(s, run), maxAttachSessionsPerRun)
	}
	if _, ok := s.reserveAttachSlot(uuid.New()); !ok {
		t.Fatal("another run was refused: the cap is per run")
	}

	held[0]()
	held[0]() // idempotent: a second call frees nothing more
	release, ok := s.reserveAttachSlot(run)
	if !ok {
		t.Fatal("a retry after a release was refused")
	}
	if _, ok := s.reserveAttachSlot(run); ok {
		t.Fatal("a release called twice freed two slots")
	}
	release()
	for _, r := range held[1:] {
		r()
	}
	if n := attachSlotsHeld(s, run); n != 0 {
		t.Fatalf("%d slots still counted after every release", n)
	}
}

// recvSlotEvent receives from ch, failing the test if nothing arrives in a bound only a regression hits.
func recvSlotEvent[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

// blockingCastStore holds each SaveCastNamed until gate is closed, signalling entered first.
type blockingCastStore struct {
	recording.Store
	entered chan string
	gate    chan struct{}
}

func (b *blockingCastStore) SaveCastNamed(ctx context.Context, runID, suffix string, r io.Reader) error {
	b.entered <- suffix
	<-b.gate
	return b.Store.SaveCastNamed(ctx, runID, suffix, r)
}

// attachSlotsTestServer is holderTestServer behind a handler that signals each attach handler's
// return, the point at which its slot is released.
func attachSlotsTestServer(t *testing.T) (*Server, *holderTestRunner, *sshTestRecorder, types.AgentRun, *httptest.Server, chan struct{}) {
	t.Helper()
	srv, _, fr, audit, run := holderTestServer(t)
	returned := make(chan struct{}, 4*maxAttachSessionsPerRun)
	inner := panicFails(t, srv.Handler())
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner.ServeHTTP(w, r)
		if strings.HasSuffix(r.URL.Path, "/attach") {
			returned <- struct{}{}
		}
	}))
	t.Cleanup(ts.Close)
	return srv, fr, audit, run, ts, returned
}

// dialAttachRefused dials one attach more and returns the HTTP status it was refused with.
func dialAttachRefused(t *testing.T, ts *httptest.Server, srv *Server, runID uuid.UUID) int {
	t.Helper()
	tok, err := mintAttachTicket(context.Background(), srv.cfg.Store, runID, types.ActorHuman, holderSecond, oidc.RoleAdmin, time.Now())
	if err != nil {
		t.Fatalf("mint ticket: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/runs/"+runID.String()+"/attach?ticket="+tok, nil)
	if err == nil {
		_ = c.CloseNow()
		t.Fatal("an attach past the cap was admitted")
	}
	if resp == nil {
		t.Fatalf("no HTTP response to the refused attach: %v", err)
	}
	return resp.StatusCode
}

// capRefusals is the session.attach rows refused at the cap, and their data.
func capRefusals(audit *sshTestRecorder) []map[string]any {
	var out []map[string]any
	for _, ev := range audit.snapshot() {
		var d map[string]any
		if ev.Action != "session.attach" || ev.Outcome != "failure" || json.Unmarshal(ev.Data, &d) != nil {
			continue
		}
		if d["reason"] == reasonAttachSessionCapReached && d["state"] == "refused" && d["max"] == float64(maxAttachSessionsPerRun) {
			out = append(out, d)
		}
	}
	return out
}

func (r *holderTestRunner) attachCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sessions)
}

func TestAttachSlots_TheNinthSessionIsRefusedBeforeItsExecOnBothLanes(t *testing.T) {
	srv, fr, audit, run, ts, _ := attachSlotsTestServer(t)
	for i := range maxAttachSessionsPerRun {
		principal := holderSecond // observers of another principal: no stale-writer probe
		if i == 0 {
			principal = holderOwner
		}
		readAttachMode(t, dialAttach(t, ts, srv, run.ID, principal, ""))
		waitForSession(t, fr, i)
	}

	if code := dialAttachRefused(t, ts, srv, run.ID); code != http.StatusServiceUnavailable {
		t.Fatalf("ninth web attach refused with %d, want 503", code)
	}
	ch := newFakeSSHChannel()
	_ = ch.inW.Close() // an admitted shell would end at once on the client's EOF
	srv.bridgeSSHShell(context.Background(), run.ID, holderSecond, ch, 80, 24, make(chan sshWindowChangeMsg))
	if !strings.Contains(ch.stderrString(), attachSessionCapMsg) {
		t.Fatalf("ssh client was told %q, want the cap", ch.stderrString())
	}

	if n := fr.attachCount(); n != maxAttachSessionsPerRun {
		t.Fatalf("Runner.Attach calls = %d, want %d: a refused session opened an exec", n, maxAttachSessionsPerRun)
	}
	got := capRefusals(audit)
	if len(got) != 2 || got[0]["transport"] != nil || got[1]["transport"] != "ssh" {
		t.Fatalf("cap refusals audited = %v, want the web one then the ssh one", got)
	}
	if n := attachSlotsHeld(srv, run.ID); n != maxAttachSessionsPerRun {
		t.Fatalf("%d slots counted, want %d: a refusal changed the count", n, maxAttachSessionsPerRun)
	}
}

func TestAttachSlots_ASlotIsHeldUntilTheRecordingIsPersisted(t *testing.T) {
	srv, fr, _, run, ts, returned := attachSlotsTestServer(t)
	fs, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	casts := &blockingCastStore{Store: fs, entered: make(chan string, 1), gate: make(chan struct{})}
	var unblock sync.Once
	t.Cleanup(func() { unblock.Do(func() { close(casts.gate) }) }) // runs before the server closes
	srv.cfg.RecordingStore = casts

	writer := dialAttach(t, ts, srv, run.ID, holderOwner, "")
	readAttachMode(t, writer)
	out := waitForSession(t, fr, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, b := range []string{"one\r\n", "two\r\n"} { // the second frame proves the first was recorded
		out.feed(b)
		if _, _, err := writer.Read(ctx); err != nil {
			t.Fatalf("read output: %v", err)
		}
	}
	observers := make([]*websocket.Conn, 0, maxAttachSessionsPerRun-1)
	for i := 1; i < maxAttachSessionsPerRun; i++ {
		c := dialAttach(t, ts, srv, run.ID, holderSecond, "")
		if !readAttachMode(t, c).ReadOnly {
			t.Fatal("an observer was admitted writable")
		}
		observers = append(observers, c)
	}

	// The writer leaves; its recording is still being persisted.
	_ = writer.Close(websocket.StatusNormalClosure, "")
	recvSlotEvent(t, casts.entered, "the recording to start persisting")

	// The keyboard moves at once...
	if mode := readAttachMode(t, observers[0]); mode.ReadOnly {
		t.Fatal("the oldest observer was not promoted while the recording persisted")
	}
	// ...but the slot does not: reconnects are refused until the cast has left memory.
	for range 2 {
		if code := dialAttachRefused(t, ts, srv, run.ID); code != http.StatusServiceUnavailable {
			t.Fatalf("a reconnect during persistence was answered %d, want 503", code)
		}
	}
	if n := attachSlotsHeld(srv, run.ID); n != maxAttachSessionsPerRun {
		t.Fatalf("%d slots counted during persistence, want %d", n, maxAttachSessionsPerRun)
	}

	unblock.Do(func() { close(casts.gate) })
	for range 3 { // the writer's handler, and the two refused ones, in any order
		recvSlotEvent(t, returned, "an attach handler to return")
	}
	if n := attachSlotsHeld(srv, run.ID); n != maxAttachSessionsPerRun-1 {
		t.Fatalf("%d slots counted after the recording persisted, want %d", n, maxAttachSessionsPerRun-1)
	}
	readAttachMode(t, dialAttach(t, ts, srv, run.ID, holderOwner, ""))
}

// attachFailRunner opens no exec.
type attachFailRunner struct{ fakeRunner }

func (*attachFailRunner) Attach(context.Context, string, runner.AttachOptions) (runner.Session, error) {
	return nil, errors.New("exec refused")
}

func TestAttachSlots_AFailedSetupReleasesItsSlot(t *testing.T) {
	srv, _, _, run, ts, returned := attachSlotsTestServer(t)
	srv.cfg.Runner = &attachFailRunner{}

	c := dialAttach(t, ts, srv, run.ID, holderOwner, "")
	readAttachMode(t, c)
	if _, _, err := c.Read(t.Context()); websocket.CloseStatus(err) == -1 { // answers the server's close
		t.Fatalf("the failed attach was not closed: %v", err)
	}
	recvSlotEvent(t, returned, "the failed attach's handler to return")

	if n := attachSlotsHeld(srv, run.ID); n != 0 {
		t.Fatalf("%d slots counted after a failed attach, want 0", n)
	}

	ch := newFakeSSHChannel()
	srv.bridgeSSHShell(context.Background(), run.ID, holderOwner, ch, 80, 24, make(chan sshWindowChangeMsg))
	if n := attachSlotsHeld(srv, run.ID); n != 0 {
		t.Fatalf("%d slots counted after a failed ssh attach, want 0", n)
	}
}
