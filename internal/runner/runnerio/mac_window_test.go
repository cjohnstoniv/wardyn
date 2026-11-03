// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerio

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
)

// fakeWindow is the helper's connection: closed is shut when the runner closes it, ended when the window
// is over (a close of the runner's own ends it too, with no reason).
type fakeWindow struct {
	ended, closed chan struct{}
	reason        string
	once, endOnce sync.Once
}

func newFakeWindow() *fakeWindow {
	return &fakeWindow{ended: make(chan struct{}), closed: make(chan struct{})}
}

func (w *fakeWindow) Ended() <-chan struct{} { return w.ended }
func (w *fakeWindow) Reason() string         { return w.reason }
func (w *fakeWindow) Close() error {
	w.once.Do(func() { close(w.closed) })
	w.endOnce.Do(func() { close(w.ended) })
	return nil
}

// helperEnds is the helper closing the window itself, for reason.
func (w *fakeWindow) helperEnds(reason string) {
	w.endOnce.Do(func() { w.reason = reason; close(w.ended) })
}

// windowSub is a substrate that can open windows.
type windowSub struct {
	contextSubstrate
	mu   sync.Mutex
	reqs []runner.MacWindowRequest
	wins []*fakeWindow
	err  error
}

func (s *windowSub) OpenWindow(_ context.Context, _ string, req runner.MacWindowRequest) (runner.MacWindow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	w := newFakeWindow()
	s.reqs, s.wins = append(s.reqs, req), append(s.wins, w)
	return w, nil
}

func (s *windowSub) opens() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.reqs) }
func (s *windowSub) win(i int) *fakeWindow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wins[i]
}

type windowRig struct {
	server *Server
	sub    *windowSub
	org    *runnerwire.Peer
	rp     *runnerwire.Peer
	refs   []string // two held sandboxes, as the org names them
}

func newWindowRig(t *testing.T, sub func(*runnertest.Fake) *windowSub) *windowRig {
	t.Helper()
	fake := runnertest.NewFake()
	r := &windowRig{sub: sub(fake)}
	var local []string
	for range 2 {
		sb, err := fake.CreateSandbox(t.Context(), runner.SandboxSpec{RunID: uuid.New()})
		if err != nil {
			t.Fatal(err)
		}
		local = append(local, sb.Ref)
		r.refs = append(r.refs, placement.RunnerRefPrefix("laptop")+sb.Ref)
	}
	server, err := New(r.sub, "laptop", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r.server = server
	org, run := runnerwire.Loopback()
	r.org = runnerwire.NewPeer(org, runnerwire.PeerConfig{Org: true})
	go func() { _ = r.org.Run(t.Context()) }()
	r.rp = runnerwire.NewPeer(run, runnerwire.PeerConfig{OnCall: func(ctx context.Context, id uint32, method string, args json.RawMessage) (any, error) {
		return server.Call(ctx, r.rp, id, method, args)
	}})
	go func() { _ = r.rp.Run(t.Context()) }()
	server.HoldSandboxes(func(ref string) bool { return ref == local[0] || ref == local[1] })
	server.Online(r.rp)
	t.Cleanup(func() { r.rp.Close(); r.org.Close(); server.Offline(r.rp) })
	return r
}

func plainWindowSub(f *runnertest.Fake) *windowSub {
	return &windowSub{contextSubstrate: contextSubstrate{f}}
}

// open is the org's mac_window call for ref under grant; the stream is nil when the call is refused.
func (r *windowRig) open(t *testing.T, ref, grant string) (*runnerwire.Stream, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var res runnerwire.StreamResult
	if err := r.org.Call(ctx, runnerwire.MethodMacWindow, runnerwire.MacWindowArgs{Ref: ref, GrantID: grant, Title: "Run, for Pat"}, &res); err != nil {
		return nil, err
	}
	st, err := r.org.TakeStream(res.Stream)
	if err != nil {
		t.Fatal(err)
	}
	if st.Kind() != runnerwire.KindMacWindow {
		t.Fatalf("the window's stream is %q", st.Kind())
	}
	return st, nil
}

func waitClosed(t *testing.T, w *fakeWindow, what string) {
	t.Helper()
	select {
	case <-w.closed:
	case <-time.After(5 * time.Second):
		t.Fatalf("the helper's connection stayed open: %s", what)
	}
}

func notClosed(t *testing.T, w *fakeWindow, what string) {
	t.Helper()
	select {
	case <-w.closed:
		t.Fatalf("the helper's connection closed: %s", what)
	case <-time.After(150 * time.Millisecond):
	}
}

// The organisation's close, or a reset (a revocation, a failed re-check, the run's end), tears the view
// down: the stream's end closes the helper's connection, which closes the window. The run's slot is free
// afterwards, and the helper was asked once with the grant and the title.
func TestMacWindowOrgEndClosesTheHelperConnection(t *testing.T) {
	for name, end := range map[string]func(*runnerwire.Stream){
		"close": func(s *runnerwire.Stream) { _ = s.Close() },
		"reset": func(s *runnerwire.Stream) { _ = s.Reset(runnerwire.ResetCancelled) },
	} {
		t.Run(name, func(t *testing.T) {
			r := newWindowRig(t, plainWindowSub)
			st, err := r.open(t, r.refs[0], "g1")
			if err != nil {
				t.Fatal(err)
			}
			w := r.sub.win(0)
			notClosed(t, w, "the window is open")
			if got := r.sub.reqs[0]; got.GrantID != "g1" || got.Title != "Run, for Pat" {
				t.Fatalf("the helper was asked %+v", got)
			}
			end(st)
			waitClosed(t, w, "the org ended its stream")
			st2, err := r.open(t, r.refs[0], "g2")
			if err != nil {
				t.Fatalf("a reopen after the org's end = %v", err)
			}
			_ = st2.Close()
		})
	}
}

// The window ending first (the person closed it, the screen locked) tells the org why on the stream, then
// ends it; the run's slot is free before the org hears.
func TestMacWindowHelperEndTellsTheOrg(t *testing.T) {
	r := newWindowRig(t, plainWindowSub)
	st, err := r.open(t, r.refs[0], "g1")
	if err != nil {
		t.Fatal(err)
	}
	r.sub.win(0).helperEnds(runner.MacWindowEndHostLocked)
	b, err := io.ReadAll(st)
	if err != nil {
		t.Fatal(err)
	}
	var end runnerwire.MacWindowEnd
	if err := json.Unmarshal(b, &end); err != nil || end.Reason != runner.MacWindowEndHostLocked {
		t.Fatalf("the org read %q = %+v, %v", b, end, err)
	}
	waitClosed(t, r.sub.win(0), "the window is over")
	if st2, err := r.open(t, r.refs[0], "g2"); err != nil {
		t.Fatalf("a reopen after the window ended = %v", err)
	} else {
		_ = st2.Close()
	}
}

// One window per run, on the runner too: a second grant's open is refused with the shared sentence and
// never reaches the helper, the first window is undisturbed, another run is not affected, and once the
// first ends a new grant opens.
func TestMacWindowRunnerRefusesASecondGrantOnTheSameRun(t *testing.T) {
	r := newWindowRig(t, plainWindowSub)
	first, err := r.open(t, r.refs[0], "g1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.open(t, r.refs[0], "g2")
	if err == nil || !strings.Contains(err.Error(), runnerwire.MacWindowOpenSentence) {
		t.Fatalf("the second grant's open = %v, want %q", err, runnerwire.MacWindowOpenSentence)
	}
	if r.sub.opens() != 1 {
		t.Fatalf("the helper was asked %d times, want 1", r.sub.opens())
	}
	notClosed(t, r.sub.win(0), "a refused second grant")
	other, err := r.open(t, r.refs[1], "g3")
	if err != nil {
		t.Fatalf("another run's window = %v", err)
	}
	_ = first.Close()
	waitClosed(t, r.sub.win(0), "the first window ended")
	again, err := r.open(t, r.refs[0], "g4")
	if err != nil {
		t.Fatalf("a new grant after the first ended = %v", err)
	}
	_ = again.Close()
	_ = other.Close()
}

// A second open that reaches the helper while the first is still being asked is refused too: the slot is
// taken before the helper is asked.
func TestMacWindowSlotIsTakenBeforeTheHelperIsAsked(t *testing.T) {
	gate := make(chan struct{})
	r := newWindowRig(t, func(f *runnertest.Fake) *windowSub {
		return &windowSub{contextSubstrate: contextSubstrate{f}}
	})
	slow := &slowWindowSub{windowSub: r.sub, gate: gate, asked: make(chan struct{}, 1)}
	r.server.mu.Lock()
	r.server.sub = slow
	r.server.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		st, err := r.open(t, r.refs[0], "g1")
		if st != nil {
			defer st.Close()
		}
		done <- err
	}()
	<-slow.asked
	if _, err := r.open(t, r.refs[0], "g2"); err == nil || !strings.Contains(err.Error(), runnerwire.MacWindowOpenSentence) {
		t.Fatalf("a second grant while the first is being asked = %v", err)
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatalf("the first grant = %v", err)
	}
	if r.sub.opens() != 1 {
		t.Fatalf("the helper opened %d windows, want 1", r.sub.opens())
	}
}

type slowWindowSub struct {
	*windowSub
	gate  chan struct{}
	asked chan struct{}
}

func (s *slowWindowSub) OpenWindow(ctx context.Context, ref string, req runner.MacWindowRequest) (runner.MacWindow, error) {
	s.asked <- struct{}{}
	<-s.gate
	return s.windowSub.OpenWindow(ctx, ref, req)
}

// The runner opens a window only on a run it holds, and only on a macOS VM.
func TestMacWindowRefusals(t *testing.T) {
	r := newWindowRig(t, plainWindowSub)
	for name, tc := range map[string]struct {
		ref, grant, want string
	}{
		"a ref of another runner":       {"runner:other/abc", "g", "ref does not belong to this runner"},
		"a sandbox it does not hold":    {placement.RunnerRefPrefix("laptop") + "not-held", "g", "ref is not a sandbox this runner holds"},
		"no grant id":                   {r.refs[0], "", "a window needs a grant id"},
		"a grant id the helper refuses": {r.refs[0], "g 1/..", "a window needs a grant id"},
	} {
		if _, err := r.open(t, tc.ref, tc.grant); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	if r.sub.opens() != 0 {
		t.Fatalf("a refused call reached the helper (%d)", r.sub.opens())
	}

	r.sub.err = runner.ErrMacWindowUnsupported
	if _, err := r.open(t, r.refs[0], "g"); err == nil || !strings.Contains(err.Error(), runnerwire.MacWindowNotMacSentence) {
		t.Errorf("a sandbox that is no macOS VM: %v", err)
	}
	plain := newWindowRig(t, plainWindowSub)
	plain.server.mu.Lock()
	plain.server.sub = contextSubstrate{runnertest.NewFake()}
	plain.server.mu.Unlock()
	if _, err := plain.open(t, plain.refs[0], "g"); err == nil || !strings.Contains(err.Error(), runnerwire.MacWindowNotMacSentence) {
		t.Errorf("a substrate with no windows: %v", err)
	}
}

// A helper too old to know the window op fails closed with a sentence, and the failed open leaves no
// slot taken.
func TestMacWindowOldHelperFailsClosed(t *testing.T) {
	r := newWindowRig(t, plainWindowSub)
	r.sub.err = runner.ErrMacWindowHelperOld
	if _, err := r.open(t, r.refs[0], "g1"); err == nil || !strings.Contains(err.Error(), runnerwire.MacWindowHelperOldSentence) {
		t.Fatalf("an old helper: %v, want %q", err, runnerwire.MacWindowHelperOldSentence)
	}
	r.sub.mu.Lock()
	r.sub.err = &runner.MacWindowRefusal{Code: "window_not_at_mac", Sentence: "The window didn't open: you aren't signed in at this Mac's screen, or it is locked."}
	r.sub.mu.Unlock()
	if _, err := r.open(t, r.refs[0], "g2"); err == nil || !strings.Contains(err.Error(), "you aren't signed in at this Mac's screen") {
		t.Fatalf("a person not at the Mac: %v", err)
	}
	r.sub.mu.Lock()
	r.sub.err = nil
	r.sub.mu.Unlock()
	st, err := r.open(t, r.refs[0], "g3")
	if err != nil {
		t.Fatalf("an open after two failed ones = %v", err)
	}
	_ = st.Close()
}

// The runner's link dropping ends the window: the helper's connection closes and the slot is free.
func TestMacWindowLinkLossClosesTheHelperConnection(t *testing.T) {
	r := newWindowRig(t, plainWindowSub)
	if _, err := r.open(t, r.refs[0], "g1"); err != nil {
		t.Fatal(err)
	}
	r.server.Offline(r.rp)
	waitClosed(t, r.sub.win(0), "the runner's link dropped")
	r.server.mu.Lock()
	n := len(r.server.windows)
	r.server.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d windows still registered after the link dropped", n)
	}
}

// The runner's link going while the helper is still being asked must not leave the window open with
// nobody owning it: Offline ends the (still empty) session, the helper then opens the window, and the
// call closes that window itself and leaves no slot taken.
func TestMacWindowOfflineWhileTheHelperIsBeingAskedClosesTheWindow(t *testing.T) {
	gate := make(chan struct{})
	r := newWindowRig(t, plainWindowSub)
	slow := &slowWindowSub{windowSub: r.sub, gate: gate, asked: make(chan struct{}, 1)}
	r.server.mu.Lock()
	r.server.sub = slow
	r.server.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		st, err := r.open(t, r.refs[0], "g1")
		if st != nil {
			_ = st.Close()
		}
		done <- err
	}()
	<-slow.asked
	r.server.Offline(r.rp)
	close(gate)
	if err := <-done; err == nil {
		t.Fatal("a window opened across the link going down")
	}
	waitClosed(t, r.sub.win(0), "the link went while the helper was asked")
	r.server.mu.Lock()
	n := len(r.server.windows)
	r.server.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d windows registered after the failed open", n)
	}
}
