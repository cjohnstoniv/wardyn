// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// dispatchAttachRunner is holderTestRunner with each Attach's start recorded and an optional stall, and a thaw.
type dispatchAttachRunner struct {
	*holderTestRunner
	mu       sync.Mutex
	attaches []time.Time
	thaws    []time.Time
	stall    func(ctx context.Context) // runs inside Attach; it may outlive ctx
	thaw     func(ctx context.Context) error
	late     []*countingShellSession // sessions Attach returned after a stall
}

func (r *dispatchAttachRunner) Attach(ctx context.Context, ref string, opts runner.AttachOptions) (runner.Session, error) {
	r.mu.Lock()
	r.attaches = append(r.attaches, time.Now())
	stall := r.stall
	r.mu.Unlock()
	if stall != nil {
		stall(ctx)
	}
	sess, err := r.holderTestRunner.Attach(ctx, ref, opts)
	if stall != nil {
		r.mu.Lock()
		r.late = append(r.late, sess.(*countingShellSession))
		r.mu.Unlock()
	}
	return sess, err
}

func (r *dispatchAttachRunner) FreezeSandbox(context.Context, string) error { return nil }

func (r *dispatchAttachRunner) ThawSandbox(ctx context.Context, _ string) error {
	r.mu.Lock()
	r.thaws = append(r.thaws, time.Now())
	thaw := r.thaw
	r.mu.Unlock()
	if thaw != nil {
		return thaw(ctx)
	}
	return nil
}

func atOrAfter(ts []time.Time, by time.Time) int {
	n := 0
	for _, t := range ts {
		if !t.Before(by) {
			n++
		}
	}
	return n
}

// pausingAttachStore is the attach rig's store as a run pauser that reports the run paused.
type pausingAttachStore struct{ issuerAttachStore }

func (pausingAttachStore) ListPauseCandidates(context.Context) ([]store.PauseCandidate, time.Time, error) {
	return nil, time.Now(), nil
}
func (pausingAttachStore) StampRunActive(context.Context, uuid.UUID) (bool, error) { return true, nil }
func (pausingAttachStore) MarkRunPaused(context.Context, uuid.UUID, types.PauseReason, *time.Time) (bool, error) {
	return false, nil
}
func (pausingAttachStore) ClearRunPaused(context.Context, uuid.UUID) (bool, error) { return true, nil }
func (pausingAttachStore) RunHasOpenRequest(context.Context, uuid.UUID) (bool, error) {
	return false, nil
}

// newDispatchAttachRig is the issuer attach rig with the dispatch bound shortened and the recording runner.
func newDispatchAttachRig(t *testing.T) (*issuerAttachRig, *dispatchAttachRunner) {
	t.Helper()
	rig := newIssuerAttachRig(t)
	dr := &dispatchAttachRunner{holderTestRunner: rig.runner}
	rig.srv.cfg.Runner = dr
	setDispatchFresh(t, dispatchFresh)
	return rig, dr
}

// frames reads c until it closes and counts its binary (sandbox output) frames and its text frames.
func frames(c *websocket.Conn, within time.Duration) (binary, text int) {
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	for {
		typ, _, err := c.Read(ctx)
		if err != nil {
			return binary, text
		}
		if typ == websocket.MessageBinary {
			binary++
		} else {
			text++
		}
	}
}

func TestAttachIssuerPortal_LateThawRefused_PG(t *testing.T) {
	rig, dr := newDispatchAttachRig(t)
	st := rig.srv.cfg.Store.(issuerAttachStore)
	ast := st.authorityUnits.touchCountingStore.authzStore
	now := time.Now().UTC()
	ast.mu.Lock()
	run := ast.runs[rig.run.ID]
	run.PausedAt, run.PausedReason = &now, types.PauseIdle
	ast.runs[rig.run.ID] = run
	ast.mu.Unlock()
	rig.srv.cfg.Store = pausingAttachStore{st}
	issuerAdmitWaitNS.Store(int64(10 * time.Second)) // the opening reads' own bound is past the dispatch time
	t.Cleanup(func() { issuerAdmitWaitNS.Store(0) })
	var by time.Time
	dr.thaw = func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() } // stalls until it is cut
	tok, err := mintAttachTicket(audit.WithDelegation(context.Background(), rig.via), rig.srv.cfg.Store, rig.run.ID, types.ActorHuman,
		rig.person, oidc.RoleUser, oidc.Issue{At: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	by = time.Now().Add(dispatchFresh)
	url := rig.ts.URL + "/api/v1/runs/" + rig.run.ID.String() + "/attach?ticket=" + tok
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var eb errorBody
	_ = json.Unmarshal(body, &eb)
	if resp.StatusCode != http.StatusBadGateway || eb.Reason != reasonAttachResumeFailed {
		t.Fatalf("a thaw stalled past the dispatch time: %d %s, want the resume-failed refusal", resp.StatusCode, body)
	}
	dr.mu.Lock()
	defer dr.mu.Unlock()
	if n := atOrAfter(dr.thaws, by); n != 0 || len(dr.attaches) != 0 {
		t.Fatalf("%d thaws at or after the dispatch time, %d attaches; want 0, 0", n, len(dr.attaches))
	}
}

func TestAttachIssuerPortal_LateOpeningRefusedBeforeServe_PG(t *testing.T) {
	rig, dr := newDispatchAttachRig(t)
	release := make(chan struct{})
	by := time.Now().Add(dispatchFresh)
	dr.stall = func(context.Context) { <-release } // the opening returns its session late, whatever its context
	c := rig.dial(t)
	time.Sleep(time.Until(by) + 200*time.Millisecond)
	close(release)
	binary, _ := frames(c, 10*time.Second)
	dr.mu.Lock()
	defer dr.mu.Unlock()
	if n := atOrAfter(dr.attaches, by); n != 0 {
		t.Fatalf("%d openings started at or after the dispatch time", n)
	}
	if binary != 0 {
		t.Fatalf("%d sandbox output frames from an opening returned late, want 0", binary)
	}
	for _, s := range dr.late {
		waitWithin(t, 5*time.Second, "the late session closed by its identity", s.isClosed)
	}
}

func TestAttachIssuerPortal_OpenedSessionOutlivesDeadline_PG(t *testing.T) {
	rig, _ := newDispatchAttachRig(t)
	by := time.Now().Add(dispatchFresh)
	writer := rig.dial(t)
	// The writer's client reads all along, as a browser does: a client that never reads answers no ping, and the
	// same person's second socket, admitted read-only, would evict it as stale (probeStaleWriter) within ~4 s.
	closed := make(chan time.Time, 1)
	go func() {
		drainClient(writer)
		closed <- time.Now()
	}()
	observer := rig.dial(t)
	waitWithin(t, 5*time.Second, "both openings", func() bool {
		rig.runner.mu.Lock()
		defer rig.runner.mu.Unlock()
		return len(rig.runner.sessions) >= 2
	})
	// The observer still receives output after the dispatch time (scaled: 2 bounds past it).
	time.Sleep(time.Until(by) + 2*dispatchFresh)
	rig.runner.mu.Lock()
	sessions := append([]*countingShellSession(nil), rig.runner.sessions...)
	rig.runner.mu.Unlock()
	for _, s := range sessions {
		go s.feed("output after the dispatch time\r\n")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		typ, _, err := observer.Read(ctx)
		if err != nil {
			t.Fatalf("the observer's output after the dispatch time: %v", err)
		}
		if typ == websocket.MessageBinary {
			break
		}
	}
	// An idle writer's first keystroke, after the dispatch time, reaches the exec.
	if err := writer.Write(ctx, websocket.MessageBinary, []byte("k")); err != nil {
		t.Fatalf("the writer's first keystroke: %v", err)
	}
	waitWithin(t, 5*time.Second, "the keystroke at the exec", func() bool {
		for _, s := range sessions {
			if s.writtenBytes() > 0 {
				return true
			}
		}
		return false
	})
	// A revoke after a timely opening is torn down by the watch, as before.
	at := rig.suspend(t)
	select {
	case got := <-closed:
		cutBy(t, "terminal opened in time, suspend after its dispatch time", at, got, relayEvery+relayBound+relaySlack)
	case <-time.After(10 * time.Second):
		t.Fatal("the socket outlived a suspend after a timely opening")
	}
}

func TestAttachIssuerPortal_LocalTransportRefused_PG(t *testing.T) {
	rig, dr := newDispatchAttachRig(t)
	rig.srv.cfg.AttachLocal = true
	ask := func(via *types.DelegationVia) (int, errorBody) {
		ctx := context.Background()
		if via != nil {
			ctx = audit.WithDelegation(ctx, *via)
		}
		tok, err := mintAttachTicket(ctx, rig.srv.cfg.Store, rig.run.ID, types.ActorHuman, rig.person, oidc.RoleUser, oidc.Issue{At: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest(http.MethodGet, rig.ts.URL+"/api/v1/runs/"+rig.run.ID.String()+"/attach?transport=local&local_nonce=n&ticket="+tok, nil)
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Sec-WebSocket-Version", "13")
		req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var eb errorBody
		_ = json.NewDecoder(resp.Body).Decode(&eb)
		return resp.StatusCode, eb
	}
	code, eb := ask(&rig.via)
	if code != http.StatusConflict || eb.Reason != reasonAttachLocalUnavailable || eb.Detail != localAttachIssuerPortal {
		t.Fatalf("an issuer portal's local attach: %d %+v, want 409 with detail %q", code, eb, localAttachIssuerPortal)
	}
	dr.mu.Lock()
	n := len(dr.attaches)
	dr.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d openings for a refused local attach, want 0", n)
	}
	// A ticket with no portal on it is not refused for being a portal's.
	if _, eb := ask(nil); eb.Detail == localAttachIssuerPortal {
		t.Fatalf("a plain ticket's local attach was refused as an issuer portal's: %+v", eb)
	}
}

// TestAttachIssuerPortal_PromotionRechecksAuthority_PG: a promoted issuer-portal observer's writer exec is a new
// opening, whether the promotion lands after its first opening or while that is still pending. Its socket's
// authority is asked again first, a refusal cuts the socket with no writer opened and no input delivered, and the
// writer's Runner.Attach is bounded by that re-check's start plus the dispatch bound, never by the ticket's.
func TestAttachIssuerPortal_PromotionRechecksAuthority_PG(t *testing.T) {
	open := func(t *testing.T, rig *issuerAttachRig) (writer, observer *websocket.Conn) {
		t.Helper()
		writer = rig.dial(t)
		if readAttachMode(t, writer).ReadOnly {
			t.Fatal("the first socket opened read-only")
		}
		observer = rig.dial(t)
		if !readAttachMode(t, observer).ReadOnly {
			t.Fatal("the second socket opened as the writer")
		}
		waitWithin(t, 5*time.Second, "both openings", func() bool {
			rig.runner.mu.Lock()
			defer rig.runner.mu.Unlock()
			return len(rig.runner.sessions) == 2
		})
		return writer, observer
	}
	attaches := func(dr *dispatchAttachRunner) []time.Time {
		dr.mu.Lock()
		defer dr.mu.Unlock()
		return append([]time.Time(nil), dr.attaches...)
	}
	promoted := func(rig *issuerAttachRig) bool {
		for _, ev := range rig.audit.snapshot() {
			if ev.Action == "session.promote" {
				return true
			}
		}
		return false
	}
	written := func(rig *issuerAttachRig) int {
		rig.runner.mu.Lock()
		defer rig.runner.mu.Unlock()
		n := 0
		for _, s := range rig.runner.sessions {
			n += s.writtenBytes()
		}
		return n
	}

	t.Run("suspended before the promotion", func(t *testing.T) {
		rig, dr := newDispatchAttachRig(t)
		rig.srv.authorityTest = attachAuthorityTimings{every: time.Minute, lookup: relayBound} // the watch never ticks here
		writer, observer := open(t, rig)
		_ = rig.suspend(t)
		closed := make(chan time.Time, 1)
		go func() {
			for {
				if _, _, err := observer.Read(context.Background()); err != nil {
					closed <- time.Now()
					return
				}
			}
		}()
		at := time.Now()
		_ = writer.Close(websocket.StatusNormalClosure, "")
		waitWithin(t, 5*time.Second, "the session.promote row", func() bool { return promoted(rig) })
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = observer.Write(ctx, websocket.MessageBinary, []byte("k")) // held until a writer exec is ready, which never comes
		select {
		case got := <-closed:
			cutBy(t, "terminal promoted after a suspend", at, got, 2*time.Second)
		case <-time.After(10 * time.Second):
			t.Fatal("the promoted socket outlived a suspend that came before its promotion")
		}
		if n := len(attaches(dr)); n != 2 {
			t.Fatalf("%d openings, want 2: a writer exec was opened for a suspended person", n)
		}
		if n := written(rig); n != 0 {
			t.Fatalf("%d bytes of input reached an exec after the suspend, want 0", n)
		}
	})

	t.Run("live, promoted after the ticket's dispatch time", func(t *testing.T) {
		rig, dr := newDispatchAttachRig(t)
		by := time.Now().Add(dispatchFresh)
		writer, observer := open(t, rig)
		time.Sleep(time.Until(by) + dispatchFresh)
		_ = writer.Close(websocket.StatusNormalClosure, "")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for {
			typ, data, err := observer.Read(ctx)
			if err != nil {
				t.Fatalf("the promoted socket before its writer frame: %v", err)
			}
			var m attachModeMsg
			if typ == websocket.MessageText && json.Unmarshal(data, &m) == nil && m.Type == "attach-mode" && !m.ReadOnly {
				break
			}
		}
		if n := len(attaches(dr)); n != 3 {
			t.Fatalf("%d openings, want 3: the writer exec of a promotion past the ticket's dispatch time", n)
		}
		if err := observer.Write(ctx, websocket.MessageBinary, []byte("k")); err != nil {
			t.Fatalf("the promoted writer's keystroke: %v", err)
		}
		waitWithin(t, 5*time.Second, "the keystroke at the writer exec", func() bool { return written(rig) > 0 })
	})

	t.Run("the re-open outlives its own dispatch time", func(t *testing.T) {
		rig, dr := newDispatchAttachRig(t)
		writer, observer := open(t, rig)
		release := make(chan struct{})
		dr.mu.Lock()
		dr.stall = func(context.Context) { <-release } // returns its session late, whatever its context
		dr.mu.Unlock()
		_ = writer.Close(websocket.StatusNormalClosure, "")
		waitWithin(t, 5*time.Second, "the re-open's Runner.Attach", func() bool { return len(attaches(dr)) == 3 })
		// The re-check started before this Attach did, so its dispatch time lies before started + dispatchFresh.
		started := attaches(dr)[2]
		time.Sleep(time.Until(started.Add(dispatchFresh)) + 200*time.Millisecond)
		close(release)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = observer.Write(ctx, websocket.MessageBinary, []byte("k"))
		rctx, rcancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer rcancel()
		for {
			_, _, err := observer.Read(rctx)
			if err == nil {
				continue
			}
			if rctx.Err() != nil {
				t.Fatal("the socket was still open 10s after its writer exec came back past its dispatch time")
			}
			if code := websocket.CloseStatus(err); code != websocket.StatusInternalError {
				t.Fatalf("the socket ended with %v (close %d), want the attach-failed close %d", err, code, websocket.StatusInternalError)
			}
			break
		}
		dr.mu.Lock()
		late := append([]*countingShellSession(nil), dr.late...)
		dr.mu.Unlock()
		if len(late) != 1 {
			t.Fatalf("%d late sessions, want the re-open's 1", len(late))
		}
		waitWithin(t, 5*time.Second, "the late writer exec closed by its identity", late[0].isClosed)
		if n := late[0].writtenBytes(); n != 0 {
			t.Fatalf("%d bytes of input reached a writer exec returned after its dispatch time, want 0", n)
		}
	})

	t.Run("suspended while the first opening is pending", func(t *testing.T) {
		rig, dr := newDispatchAttachRig(t)
		rig.srv.authorityTest = attachAuthorityTimings{every: time.Minute, lookup: relayBound} // the watch never ticks here
		// The ticket's bound alone would not refuse the writer exec here.
		setDispatchFresh(t, 10*time.Second)
		writer := rig.dial(t)
		if readAttachMode(t, writer).ReadOnly {
			t.Fatal("the first socket opened read-only")
		}
		waitWithin(t, 5*time.Second, "the writer's opening", func() bool {
			rig.runner.mu.Lock()
			defer rig.runner.mu.Unlock()
			return len(rig.runner.sessions) == 1
		})
		release := make(chan struct{})
		dr.mu.Lock()
		dr.stall = func(context.Context) { <-release } // the observer's first opening is still pending at its promotion
		dr.mu.Unlock()
		observer := rig.dial(t)
		if !readAttachMode(t, observer).ReadOnly {
			t.Fatal("the second socket opened as the writer")
		}
		waitWithin(t, 5*time.Second, "the observer's pending opening", func() bool { return len(attaches(dr)) == 2 })
		_ = rig.suspend(t)
		closed := make(chan time.Time, 1)
		go func() {
			for {
				if _, _, err := observer.Read(context.Background()); err != nil {
					closed <- time.Now()
					return
				}
			}
		}()
		_ = writer.Close(websocket.StatusNormalClosure, "")
		waitWithin(t, 5*time.Second, "the session.promote row", func() bool { return promoted(rig) })
		at := time.Now()
		close(release) // the observer's session comes back; the promotion's writer exec would be next
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = observer.Write(ctx, websocket.MessageBinary, []byte("k")) // held until a writer exec is ready, which never comes
		select {
		case got := <-closed:
			cutBy(t, "terminal promoted during its first opening after a suspend", at, got, 2*time.Second)
		case <-time.After(10 * time.Second):
			t.Fatal("the socket promoted during its first opening outlived a suspend that came before the promotion")
		}
		if n := len(attaches(dr)); n != 2 {
			t.Fatalf("%d openings, want 2: a writer exec was opened for a suspended person", n)
		}
		if n := written(rig); n != 0 {
			t.Fatalf("%d bytes of input reached an exec after the suspend, want 0", n)
		}
	})
}
