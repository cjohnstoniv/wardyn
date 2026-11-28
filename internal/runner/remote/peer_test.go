// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// R2b: adjacent fields are length-prefixed, and the version byte is bound into the mac.
func TestPeerTokenEncodingIsLengthPrefixed(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	base := PeerToken{Runner: uuid.New(), Session: uuid.New(), Method: runnerwire.MethodAttach, Exp: 1700000000}
	a, b := base, base
	a.From, a.To = "ab", "c"
	b.From, b.To = "a", "bc"
	if hmac.Equal(a.MAC(key), b.MAC(key)) {
		t.Fatal(`("ab","c") and ("a","bc") give the same mac`)
	}
	enc := a.encode()
	if enc[0] != peerTokenVersion {
		t.Fatalf("encoding starts %#x, want the version byte %#x", enc[0], peerTokenVersion)
	}
	flipped := bytes.Clone(enc)
	flipped[0] = 0x02
	m := hmac.New(sha256.New, key)
	m.Write(flipped)
	if hmac.Equal(m.Sum(nil), a.MAC(key)) {
		t.Fatal("the version byte is not bound into the mac")
	}
	h := http.Header{}
	a.SetHeaders(h, key)
	got, mac, err := ReadPeerToken(h)
	if err != nil || got != a || !got.Verify(key, mac) {
		t.Fatalf("header round trip = %+v, %v; verify %v", got, err, got.Verify(key, mac))
	}
	if got.Verify(bytes.Repeat([]byte{8}, 32), mac) {
		t.Fatal("a mac verified under another key")
	}
	h.Del(HeaderPeerTarget)
	if _, _, err := ReadPeerToken(h); !errors.Is(err, ErrPeerTokenMissing) {
		t.Fatalf("missing target = %v, want ErrPeerTokenMissing", err)
	}
}

// R3: a peer request at a replica with no local session for the runner is errOffline and never forwarded, even
// when that replica's Link would route its own calls through a peer.
func TestPeerNeverForwardsTwice(t *testing.T) {
	var dials atomic.Int32
	runnerID := uuid.New()
	pt := NewPeerTransport(PeerConfig{Key: make([]byte, 32)}, "b", func(context.Context, uuid.UUID) (PeerRoute, bool, error) {
		return PeerRoute{ReplicaID: "c", PeerURL: "https://c", SessionID: uuid.New()}, true, nil
	})
	pt.dial = func(context.Context, PeerRoute, http.Header) (runnerwire.Conn, error) {
		dials.Add(1)
		return nil, errors.New("dialled")
	}
	link := &Link{}
	link.UsePeer(runnerID, pt)
	h := NewHub(t.Context(), HubConfig{})
	h.mu.Lock()
	h.sessions[runnerID] = &hubSession{link: link}
	h.mu.Unlock()
	org, _ := runnerwire.Loopback()
	if err := h.ServePeer(t.Context(), org, runnerID, uuid.New(), runnerwire.MethodAttach, nil); !errors.Is(err, errOffline) {
		t.Fatalf("ServePeer with no local session = %v, want errOffline", err)
	}
	if err := h.ServePeer(t.Context(), org, uuid.New(), uuid.New(), runnerwire.MethodAttach, nil); !errors.Is(err, errOffline) {
		t.Fatalf("ServePeer for an unknown runner = %v, want errOffline", err)
	}
	if n := dials.Load(); n != 0 {
		t.Fatalf("the door dialled another replica %d times", n)
	}
}

// peerRig is a holder Link on a fake runner session, and a caller Link that reaches it through the peer path. Every
// dial opens a fresh WebSocket, served by servePin on the holder's pin as it stood when the rig was made.
type peerRig struct {
	holder, caller *Link
	runner         *runnerwire.Peer
	calls          atomic.Int32 // calls that reached a runner session
	dials          atomic.Int32
	doors          chan error // one per WebSocket the holder served, when it ended
	serve          runnerwire.CallHandler
}

func newPeerRig(t *testing.T, ctx context.Context) *peerRig {
	t.Helper()
	r := &peerRig{holder: &Link{}, caller: &Link{}, doors: make(chan error, 16)}
	r.serve = func(ctx context.Context, id uint32, method string, args json.RawMessage) (any, error) {
		r.calls.Add(1)
		switch method {
		case runnerwire.MethodAttach:
			st, err := r.runner.Open(ctx, runnerwire.Open{Kind: runnerwire.KindPTY, Call: id})
			if err != nil {
				return nil, err
			}
			go func() { _, _ = st.Write([]byte("hello")); _ = st.CloseWrite() }()
			return runnerwire.StreamResult{Stream: st.ID()}, nil
		case runnerwire.MethodResize, runnerwire.MethodStatus:
			return nil, nil
		}
		return nil, errors.New("unexpected method")
	}
	r.attach(ctx)
	pin := pinnedPeer{l: r.holder, p: r.holder.current()}
	pt := NewPeerTransport(PeerConfig{Key: make([]byte, 32)}, "a", func(context.Context, uuid.UUID) (PeerRoute, bool, error) {
		return PeerRoute{ReplicaID: "b", PeerURL: "https://b", SessionID: uuid.New()}, true, nil
	})
	pt.dial = func(_ context.Context, _ PeerRoute, h http.Header) (runnerwire.Conn, error) {
		r.dials.Add(1)
		callerConn, doorConn := runnerwire.Loopback()
		go func() { r.doors <- servePin(ctx, doorConn, pin, h.Get(HeaderPeerMethod), nil) }()
		return callerConn, nil
	}
	r.caller.UsePeer(uuid.New(), pt)
	return r
}

// attach connects a new runner session to the holder Link, replacing any before it.
func (r *peerRig) attach(ctx context.Context) {
	org, run := runnerwire.Loopback()
	r.runner = runnerwire.NewPeer(run, runnerwire.PeerConfig{OnCall: r.serve})
	go func() { _ = r.runner.Run(ctx) }()
	r.holder.Attach(ctx, org)
}

// doorEnded waits for one served WebSocket to end at the holder.
func (r *peerRig) doorEnded(t *testing.T) {
	t.Helper()
	select {
	case <-r.doors:
	case <-time.After(5 * time.Second):
		t.Fatal("the holder's end of the peer WebSocket did not finish")
	}
}

// TestPeerPinRelaysCallsAndStreams: a pin made with no local session reaches the holder's session, the stream the
// runner opens for the call reaches the caller under its own id, and once the runner's session is replaced every
// call on the pin is errOffline and reaches no session.
func TestPeerPinRelaysCallsAndStreams(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	r := newPeerRig(t, ctx)
	sess, err := r.caller.pinSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sess.(*peerPinned); !ok {
		t.Fatalf("pin with no local session is %T, want *peerPinned", sess)
	}
	var res runnerwire.StreamResult
	if err := sess.Call(ctx, runnerwire.MethodAttach, runnerwire.AttachArgs{Ref: "r"}, &res); err != nil {
		t.Fatal(err)
	}
	st, err := sess.TakeStream(res.Stream)
	if err != nil {
		t.Fatalf("take relayed stream %d: %v", res.Stream, err)
	}
	b, err := io.ReadAll(st)
	if err != nil || string(b) != "hello" {
		t.Fatalf("relayed stream read %q, %v", b, err)
	}
	if err := sess.Call(ctx, runnerwire.MethodResize, runnerwire.ResizeArgs{Stream: st.ID(), Cols: 80, Rows: 24}, nil); err != nil {
		t.Fatalf("resize on the pin: %v", err)
	}
	r.attach(ctx) // the runner reconnects: a new session replaces the pinned one
	before := r.calls.Load()
	if err := sess.Call(ctx, runnerwire.MethodResize, runnerwire.ResizeArgs{Stream: st.ID(), Cols: 1, Rows: 1}, nil); !errors.Is(err, errOffline) {
		t.Fatalf("call on the pin after the session moved = %v, want errOffline", err)
	}
	if got := r.calls.Load(); got != before {
		t.Fatalf("a call after the move reached a runner session (%d calls, want %d)", got, before)
	}
}

// CX-824: a pin's WebSocket ends at both peers once its last stream is closed, and after a failed first operation,
// while the runner stays connected.
func TestPeerPinReleasedWhenUnused(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	r := newPeerRig(t, ctx)
	sess, _ := r.caller.pinSession(ctx)
	var res runnerwire.StreamResult
	if err := sess.Call(ctx, runnerwire.MethodAttach, runnerwire.AttachArgs{Ref: "r"}, &res); err != nil {
		t.Fatal(err)
	}
	st, err := sess.TakeStream(res.Stream)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(st)
	_ = st.Close()
	r.doorEnded(t)
	if p := sess.(*peerPinned); func() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.p != nil }() {
		t.Fatal("the caller kept its WebSocket after the last stream closed")
	}
	sess2, _ := r.caller.pinSession(ctx)
	if err := sess2.Call(ctx, "no_such_method", nil, nil); err == nil {
		t.Fatal("a failed first operation succeeded")
	}
	r.doorEnded(t)
	if !r.runner.Online() || !r.holder.Online() {
		t.Fatal("releasing a pin ended the runner's session")
	}
}

// CX-823: the start's cutoff crosses the hop and is enforced at the holder's writer. Values: no cutoff; a cutoff
// ahead of both hops; a cutoff that passes during the second hop; a cutoff already passed at the first.
func TestPeerCutoffEnforcedAtHolder(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	r := newPeerRig(t, ctx)
	call := func(by time.Time, delay time.Duration) error {
		peerReplayHook = func() { time.Sleep(delay) }
		defer func() { peerReplayHook = nil }()
		cctx := ctx
		var opts []runnerwire.CallOption
		if !by.IsZero() {
			cctx = runner.WithStartCutoff(ctx, by)
			opts = append(opts, runnerwire.AdmitBy(by))
		}
		sess, err := r.caller.pinSession(cctx)
		if err != nil {
			return err
		}
		return sess.Call(cctx, runnerwire.MethodStatus, map[string]string{"ref": "r"}, nil, opts...)
	}
	for _, c := range []struct {
		name    string
		by      time.Duration // from now; 0 is no cutoff
		delay   time.Duration
		reached bool
	}{
		{"no cutoff", 0, 0, true},
		{"cutoff ahead", 10 * time.Second, 0, true},
		{"cutoff passes on the second hop", 300 * time.Millisecond, 600 * time.Millisecond, false},
		{"cutoff already passed", -time.Second, 0, false},
	} {
		before := r.calls.Load()
		var by time.Time
		if c.by != 0 {
			by = time.Now().Add(c.by)
		}
		err := call(by, c.delay)
		reached := r.calls.Load() != before
		if reached != c.reached {
			t.Errorf("%s: reached the runner = %v, want %v (err %v)", c.name, reached, c.reached, err)
		}
		if !c.reached && !errors.Is(err, runner.ErrStartCutoff) {
			t.Errorf("%s: err = %v, want the start cutoff refusal", c.name, err)
		}
		if c.reached && err != nil {
			t.Errorf("%s: err = %v", c.name, err)
		}
	}
}

// CX-825: a pin is the session's id, READY and exact peer read as one snapshot, and is checked again before every
// forward. Values: same peer; peer replaced before the capture; replaced between capture and forward; no peer; a
// slot held by an admission.
func TestPeerPinSnapshotRefusesReplacedPeer(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	r := newPeerRig(t, ctx)
	sid := uuid.New()
	id := sid.String()
	runnerID := uuid.New()
	h := NewHub(ctx, HubConfig{})
	hs := &hubSession{id: id, link: r.holder, peer: r.holder.current()}
	hs.readyID.Store(&id)
	h.mu.Lock()
	h.sessions[runnerID] = hs
	h.mu.Unlock()
	if _, ok := h.pinHeld(runnerID, sid); !ok {
		t.Fatal("same peer: refused")
	}
	if _, ok := h.pinHeld(runnerID, uuid.New()); ok {
		t.Fatal("another session id: pinned")
	}
	hs.mu.Lock()
	if _, ok := h.pinHeld(runnerID, sid); ok {
		t.Fatal("a slot held by an admission: pinned")
	}
	hs.mu.Unlock()
	pin, ok := h.pinHeld(runnerID, sid)
	if !ok {
		t.Fatal("same peer: refused")
	}
	r.attach(ctx) // replaced between capture and forward
	before := r.calls.Load()
	callerConn, doorConn := runnerwire.Loopback()
	go func() { _ = servePin(ctx, doorConn, pin, runnerwire.MethodStatus, nil) }()
	cp := runnerwire.NewPeer(callerConn, runnerwire.PeerConfig{Org: true})
	go func() { _ = cp.Run(ctx) }()
	if err := peerSend(ctx, cp, runnerwire.MethodStatus, map[string]string{"ref": "r"}, nil); !errors.Is(err, errOffline) {
		t.Fatalf("forward after the peer was replaced = %v, want errOffline", err)
	}
	if r.calls.Load() != before {
		t.Fatal("a forward after the replacement reached the new session")
	}
	// Replaced before the capture: the slot still names the old peer while the Link carries the new one.
	if _, ok := h.pinHeld(runnerID, sid); ok {
		t.Fatal("peer replaced before the capture: pinned")
	}
	hs.mu.Lock()
	hs.peer = nil
	hs.mu.Unlock()
	if _, ok := h.pinHeld(runnerID, sid); ok {
		t.Fatal("no peer: pinned")
	}
}

// CX-826: a cancelled request stops waiting on the holder lookup, and no peer connection is opened.
func TestPeerLookupHonoursCancel(t *testing.T) {
	var dials atomic.Int32
	pt := NewPeerTransport(PeerConfig{Key: make([]byte, 32)}, "a", func(ctx context.Context, _ uuid.UUID) (PeerRoute, bool, error) {
		<-ctx.Done()
		return PeerRoute{}, false, ctx.Err()
	})
	pt.dial = func(context.Context, PeerRoute, http.Header) (runnerwire.Conn, error) {
		dials.Add(1)
		return nil, errors.New("dialled")
	}
	l := &Link{}
	l.UsePeer(uuid.New(), pt)
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	if _, err := l.pinSession(ctx); !errors.Is(err, errOffline) {
		t.Fatalf("pin with a cancelled lookup = %v, want errOffline", err)
	}
	if err := l.Call(ctx, runnerwire.MethodStatus, nil, nil); !errors.Is(err, errOffline) {
		t.Fatalf("call with a cancelled lookup = %v, want errOffline", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the cancelled request waited %s", d)
	}
	if dials.Load() != 0 {
		t.Fatal("a peer connection was opened")
	}
}
