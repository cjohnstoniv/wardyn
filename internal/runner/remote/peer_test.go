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
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
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
	link.UsePeer(runnerID, nil, pt)
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
	runnerMu       sync.Mutex
	runner         *runnerwire.Peer // the latest session's runner end, under runnerMu
	calls          atomic.Int32     // calls that reached a runner session
	dials          atomic.Int32
	doors          chan error // one per WebSocket the holder served, when it ended
	serve          runnerwire.CallHandler

	mu     sync.Mutex
	execs  map[uint32]int // the runner's exec table: its exec stream id -> exit status, kept until exec_close
	closes atomic.Int32   // exec_close calls that found their exec
}

// rigSessionKey carries, in a call the rig's runner serves, the runner end of the session the call arrived on.
type rigSessionKey struct{}

// sessionPeer is the runner end of the session serving the call in ctx.
func sessionPeer(ctx context.Context) *runnerwire.Peer {
	p, _ := ctx.Value(rigSessionKey{}).(*runnerwire.Peer)
	return p
}

// runnerPeer is the latest session's runner end.
func (r *peerRig) runnerPeer() *runnerwire.Peer {
	r.runnerMu.Lock()
	defer r.runnerMu.Unlock()
	return r.runner
}

// Exit codes the rig's runner treats as a fault: it resets the exec or the stderr stream before it answers.
const (
	execResetsExec   = 98
	execResetsStderr = 99
)

func newPeerRig(t *testing.T, ctx context.Context) *peerRig {
	t.Helper()
	r := &peerRig{holder: &Link{}, caller: &Link{}, doors: make(chan error, 16), execs: map[uint32]int{}}
	r.serve = func(ctx context.Context, id uint32, method string, args json.RawMessage) (any, error) {
		r.calls.Add(1)
		switch method {
		case runnerwire.MethodAttach:
			st, err := sessionPeer(ctx).Open(ctx, runnerwire.Open{Kind: runnerwire.KindPTY, Call: id})
			if err != nil {
				return nil, err
			}
			go func() { _, _ = st.Write([]byte("hello")); _ = st.CloseWrite() }()
			return runnerwire.StreamResult{Stream: st.ID()}, nil
		case runnerwire.MethodResize, runnerwire.MethodStatus:
			return nil, nil
		case runnerwire.MethodExecStream: // the exec exits at once with the ref's last segment as the status
			var a runnerwire.ExecStreamArgs
			_ = json.Unmarshal(args, &a)
			code, err := strconv.Atoi(a.Ref[strings.LastIndex(a.Ref, "/")+1:])
			if err != nil {
				return nil, err
			}
			ex, err := sessionPeer(ctx).Open(ctx, runnerwire.Open{Kind: runnerwire.KindExec, Call: id})
			if err != nil {
				return nil, err
			}
			se, err := sessionPeer(ctx).Open(ctx, runnerwire.Open{Kind: runnerwire.KindExecStderr, Call: id})
			if err != nil {
				return nil, err
			}
			r.mu.Lock()
			r.execs[ex.ID()] = code
			r.mu.Unlock()
			switch code {
			case execResetsExec:
				_ = ex.Reset(runnerwire.ResetCancelled)
			case execResetsStderr:
				_ = se.Reset(runnerwire.ResetCancelled)
			}
			go func() { _, _ = ex.Write([]byte("out " + a.Ref)); _ = ex.CloseWrite(); _ = se.CloseWrite() }()
			return runnerwire.ExecStreamResult{Exec: ex.ID(), Stderr: se.ID()}, nil
		case runnerwire.MethodExecWait, runnerwire.MethodExecClose:
			var a runnerwire.ExecWaitArgs
			_ = json.Unmarshal(args, &a)
			r.mu.Lock()
			defer r.mu.Unlock()
			code, ok := r.execs[a.Stream]
			if !ok {
				return nil, errors.New("no such exec")
			}
			if method == runnerwire.MethodExecClose {
				delete(r.execs, a.Stream)
				r.closes.Add(1)
				return nil, nil
			}
			return runnerwire.WaitResult{ExitCode: code}, nil
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
	r.caller.UsePeer(uuid.New(), nil, pt)
	return r
}

// attach connects a new runner session to the holder Link, replacing any before it.
func (r *peerRig) attach(ctx context.Context) {
	org, run := runnerwire.Loopback()
	var p *runnerwire.Peer
	p = runnerwire.NewPeer(run, runnerwire.PeerConfig{OnCall: func(ctx context.Context, id uint32, method string, args json.RawMessage) (any, error) {
		return r.serve(context.WithValue(ctx, rigSessionKey{}, p), id, method, args)
	}})
	r.runnerMu.Lock()
	r.runner = p
	r.runnerMu.Unlock()
	go func() { _ = p.Run(ctx) }()
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

// A pin's WebSocket ends at both peers once its last stream is closed, and after a failed first operation,
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
	if !r.runnerPeer().Online() || !r.holder.Online() {
		t.Fatal("releasing a pin ended the runner's session")
	}
}

// The start's cutoff crosses the hop and is enforced at the holder's writer. Values: no cutoff; a cutoff
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

// A pin is the session's id, READY and exact peer read as one snapshot, and is checked again before every
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

// A cancelled request stops waiting on the holder lookup, and no peer connection is opened.
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
	l.UsePeer(uuid.New(), nil, pt)
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

// peerExec is one exec started through a pin, with both of its byte streams ended.
type peerExec struct {
	sess session
	ex   *runnerwire.Stream
}

// startPeerExec starts an exec that exits with code on a pin of its own, as Substrate.ExecStream does, and ends
// both byte directions of its exec and stderr streams.
func startPeerExec(t *testing.T, ctx context.Context, r *peerRig, code int) peerExec {
	t.Helper()
	sess, err := r.caller.pinSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var res runnerwire.ExecStreamResult
	if err := sess.Call(ctx, runnerwire.MethodExecStream, runnerwire.ExecStreamArgs{Ref: strconv.Itoa(code)}, &res); err != nil {
		t.Fatal(err)
	}
	ex, err := sess.TakeStream(res.Exec)
	if err != nil {
		t.Fatal(err)
	}
	se, err := sess.TakeStream(res.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range []*runnerwire.Stream{ex, se} {
		_ = st.CloseWrite()
		_, _ = io.ReadAll(st)
		select {
		case <-st.Done():
		case <-time.After(5 * time.Second):
			t.Fatalf("exec %d: stream %d did not end", code, st.ID())
		}
	}
	return peerExec{sess: sess, ex: ex}
}

func (e peerExec) wait(t *testing.T, ctx context.Context, want int) {
	t.Helper()
	var w runnerwire.WaitResult
	if err := e.sess.Call(ctx, runnerwire.MethodExecWait, runnerwire.ExecWaitArgs{Stream: e.ex.ID()}, &w); err != nil || w.ExitCode != want {
		t.Fatalf("wait on exec stream %d = %d, %v; want %d", e.ex.ID(), w.ExitCode, err, want)
	}
}

// An exec through a pin keeps its WebSocket after both of its byte streams end, so Wait reaches its own exec and
// Close disposes only it; the WebSocket ends at the Close. Two execs overlap on one runner, each on its own pin,
// so their WebSocket stream ids repeat while the runner's differ. A call naming a stream its WebSocket did not
// open is refused at the holder and reaches no runner session.
func TestPeerExecControlOutlivesItsStreams(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	r := newPeerRig(t, ctx)
	a := startPeerExec(t, ctx, r, 3)
	b := startPeerExec(t, ctx, r, 5)
	for _, e := range []peerExec{a, b} {
		if p := e.sess.(*peerPinned); func() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.p == nil }() {
			t.Fatalf("exec stream %d: the pin released its WebSocket before the exec was closed", e.ex.ID())
		}
	}
	b.wait(t, ctx, 5)
	a.wait(t, ctx, 3)
	if err := a.sess.Call(ctx, runnerwire.MethodExecClose, runnerwire.ExecWaitArgs{Stream: a.ex.ID()}, nil); err != nil {
		t.Fatalf("close the first exec: %v", err)
	}
	r.doorEnded(t)
	b.wait(t, ctx, 5)
	r.mu.Lock()
	left := len(r.execs)
	r.mu.Unlock()
	if left != 1 {
		t.Fatalf("the runner holds %d execs after the first was closed, want 1", left)
	}

	stray, err := r.caller.pinSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	before := r.calls.Load()
	if err := stray.Call(ctx, runnerwire.MethodExecWait, runnerwire.ExecWaitArgs{Stream: b.ex.ID()}, nil); err == nil {
		t.Fatal("a wait naming a stream its WebSocket never opened succeeded")
	}
	if got := r.calls.Load(); got != before {
		t.Fatalf("a call naming an unopened stream reached a runner session (%d calls, want %d)", got, before)
	}
	r.doorEnded(t)

	if err := b.sess.Call(ctx, runnerwire.MethodExecClose, runnerwire.ExecWaitArgs{Stream: b.ex.ID()}, nil); err != nil {
		t.Fatalf("close the second exec: %v", err)
	}
	r.doorEnded(t)
	if !r.runnerPeer().Online() {
		t.Fatal("closing the execs ended the runner's session")
	}
}

// redirectingDoor answers every request with one redirect and records each request it sees.
type redirectingDoor struct {
	code     int
	location string
	mu       sync.Mutex
	seen     []*http.Request
}

func (d *redirectingDoor) RoundTrip(req *http.Request) (*http.Response, error) {
	d.mu.Lock()
	d.seen = append(d.seen, req)
	d.mu.Unlock()
	return &http.Response{StatusCode: d.code, Header: http.Header{"Location": {d.location}}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
}

// A peer dial never follows a redirect: the dial fails with errPeerRedirect after the one request, and the signed
// headers never reach the redirect's address. Values: 302 to another host over https, 302 to another host over
// plain http, 307 to the same host over https, 308 to the same host over plain http, and 301.
func TestPeerDialRefusesRedirects(t *testing.T) {
	for _, tc := range []struct {
		code     int
		location string
	}{
		{http.StatusFound, "https://c.example/api/v1/internal/runner-peer"},
		{http.StatusFound, "http://c.example/api/v1/internal/runner-peer"},
		{http.StatusTemporaryRedirect, "https://b.example/elsewhere"},
		{http.StatusPermanentRedirect, "http://b.example/api/v1/internal/runner-peer"},
		{http.StatusMovedPermanently, "https://b.example/elsewhere"},
	} {
		door := &redirectingDoor{code: tc.code, location: tc.location}
		pt := NewPeerTransport(PeerConfig{Key: make([]byte, 32)}, "a", nil)
		pt.rt = door
		h := http.Header{}
		PeerToken{From: "a", To: "b", Runner: uuid.New(), Session: uuid.New(), Method: runnerwire.MethodStatus, Exp: time.Now().Add(10 * time.Second).Unix()}.SetHeaders(h, bytes.Repeat([]byte{7}, 32))
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		conn, err := pt.dialWebSocket(ctx, PeerRoute{ReplicaID: "b", PeerURL: "https://b.example"}, h)
		cancel()
		if conn != nil || !errors.Is(err, errPeerRedirect) {
			t.Fatalf("%d to %s: dial = %v, %v; want errPeerRedirect", tc.code, tc.location, conn, err)
		}
		door.mu.Lock()
		seen := append([]*http.Request(nil), door.seen...)
		door.mu.Unlock()
		if len(seen) != 1 {
			t.Fatalf("%d to %s: the transport saw %d requests, want 1", tc.code, tc.location, len(seen))
		}
		if got := seen[0].URL.String(); got != "https://b.example"+PeerPath {
			t.Fatalf("%d to %s: the one request went to %s", tc.code, tc.location, got)
		}
		for k := range h {
			if seen[0].Header.Get(k) != h.Get(k) {
				t.Fatalf("%d to %s: the first request lacks the signed header %s", tc.code, tc.location, k)
			}
		}
	}
}

// An exec one of whose two streams cannot be delivered fails as a whole, on a local session and through the peer
// path, while the runner stays connected: the caller gets an error and no stream, the runner's exec is closed
// (its command and result are not left without a Close), and a peer pin's WebSocket ends. Values: the exec stream
// reset before the answer, and the stderr stream reset before it; on the peer path either reset fails the
// holder's relay of that stream, so the holder unwinds the stream it did relay.
func TestPeerExecOpensAllStreamsOrNone(t *testing.T) {
	for _, code := range []int{execResetsExec, execResetsStderr} {
		for _, peer := range []bool{false, true} {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			r := newPeerRig(t, ctx)
			via := r.holder
			if peer {
				via = r.caller
			}
			sub := New("r1", via, Options{})
			es, err := sub.ExecStream(ctx, placement.RunnerRefPrefix("r1")+"s/"+strconv.Itoa(code), runner.ExecSpec{})
			if err == nil || es != nil {
				t.Fatalf("code %d peer=%v: exec = %v, %v; want an error", code, peer, es, err)
			}
			r.mu.Lock()
			left := len(r.execs)
			r.mu.Unlock()
			if left != 0 || r.closes.Load() != 1 {
				t.Fatalf("code %d peer=%v: the runner holds %d execs after %d closes, want 0 after 1", code, peer, left, r.closes.Load())
			}
			if peer {
				r.doorEnded(t)
			}
			if !r.runnerPeer().Online() || !r.holder.Online() {
				t.Fatalf("code %d peer=%v: the failed open ended the runner's session", code, peer)
			}
			cancel()
		}
	}
}

// TestPeerDroppedSocketClosesItsExecs: a caller's WebSocket that drops after its exec's streams ended but before
// its exec_close leaves the runner no exec: the holder sends exec_close for each exec the socket opened, and only
// for those.
func TestPeerDroppedSocketClosesItsExecs(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	r := newPeerRig(t, ctx)
	kept := startPeerExec(t, ctx, r, 4)
	dropped := startPeerExec(t, ctx, r, 3)
	dropped.wait(t, ctx, 3)
	r.mu.Lock()
	if n := len(r.execs); n != 2 {
		r.mu.Unlock()
		t.Fatalf("the runner holds %d execs before the drop, want 2", n)
	}
	r.mu.Unlock()
	p := dropped.sess.(*peerPinned)
	p.mu.Lock()
	ws := p.p
	p.mu.Unlock()
	if ws == nil {
		t.Fatal("the pin holds no WebSocket while its exec is open")
	}
	ws.Close() // the caller's socket drops with the exec not closed
	r.doorEnded(t)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		r.mu.Lock()
		n := len(r.execs) // the runner's own exec ids, not the sockets'
		r.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the drop the runner holds %d execs, want only the other socket's", n)
		}
	}
	kept.wait(t, ctx, 4)
	if err := kept.sess.Call(ctx, runnerwire.MethodExecClose, runnerwire.ExecWaitArgs{Stream: kept.ex.ID()}, nil); err != nil {
		t.Fatalf("close the other socket's exec: %v", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.execs) != 0 {
		t.Fatalf("the runner holds %d execs at the end, want 0", len(r.execs))
	}
}

// TestLinkOnlineReadsHolderView: a Link with no session here reads Online from the holder view: true for a fresh
// row naming another replica, with HeldHere false; false once the row ages out of a refresh, for a row naming this
// replica, and after a failed refresh, which empties the view rather than keep a stale answer.
func TestLinkOnlineReadsHolderView(t *testing.T) {
	ctx := t.Context()
	runnerID := uuid.New()
	rows := map[uuid.UUID]PeerRoute{runnerID: {ReplicaID: "b", PeerURL: "https://b", SessionID: uuid.New()}}
	var fail bool
	v := NewHolderView("a", func(context.Context) (map[uuid.UUID]PeerRoute, error) {
		if fail {
			return nil, errors.New("database down")
		}
		return maps.Clone(rows), nil
	})
	var elsewhere []uuid.UUID
	v.OnRefresh = func(_ context.Context, ids []uuid.UUID) { elsewhere = ids }
	l := &Link{}
	l.UsePeer(runnerID, v, NewPeerTransport(PeerConfig{}, "a", nil))
	if l.Online() {
		t.Fatal("online before the first refresh")
	}
	if err := v.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if !l.Online() || l.HeldHere() {
		t.Fatalf("a fresh row on another replica: Online=%v HeldHere=%v, want true, false", l.Online(), l.HeldHere())
	}
	if len(elsewhere) != 1 || elsewhere[0] != runnerID {
		t.Fatalf("OnRefresh got %v, want the runner held elsewhere", elsewhere)
	}
	fail = true
	elsewhere = nil
	if err := v.Refresh(ctx); err == nil {
		t.Fatal("a failed read refreshed")
	}
	if l.Online() || elsewhere != nil {
		t.Fatalf("after a failed refresh: Online=%v OnRefresh=%v, want offline and no call", l.Online(), elsewhere)
	}
	fail = false
	rows[runnerID] = PeerRoute{ReplicaID: "a", PeerURL: "https://a", SessionID: uuid.New()}
	_ = v.Refresh(ctx)
	if l.Online() {
		t.Fatal("a row naming this replica, with no session here, reads online")
	}
	delete(rows, runnerID) // the row aged out of the 30 s window
	_ = v.Refresh(ctx)
	if l.Online() {
		t.Fatal("a row gone stale still reads online")
	}
}

// shorten sets *v to d for the test.
func shorten(t *testing.T, v *time.Duration, d time.Duration) {
	t.Helper()
	old := *v
	*v = d
	t.Cleanup(func() { *v = old })
}

// stopSinkChecks, called after shorten, takes every sink off s's sink check when the test ends and waits until the
// check's loop and every lease read it started have returned. Cleanups run last first, so this runs before
// shorten's restore, and no check reads a duration the restore writes.
func stopSinkChecks(t *testing.T, s *Substrate) {
	t.Helper()
	t.Cleanup(func() {
		s.mu.Lock()
		sinks := slices.Collect(maps.Values(s.outputs))
		clear(s.outputs)
		s.mu.Unlock()
		waitUntil(t, "the sink check stops", func() bool {
			s.mu.Lock()
			checking := s.checking
			s.mu.Unlock()
			if checking {
				return false
			}
			for _, sink := range sinks {
				sink.mu.Lock()
				reading := sink.leaseRead
				sink.mu.Unlock()
				if reading {
					return false
				}
			}
			return true
		})
	})
}

// waitUntil polls cond every 10 ms until it holds or 5 s pass.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting: %s", what)
		}
	}
}

func resetCode(err error) uint32 {
	if re := (*runnerwire.ResetError)(nil); errors.As(err, &re) {
		return re.Code
	}
	return 0
}

// endBuf is a sink writer that records its bytes and how its drain ended.
type endBuf struct {
	mu  sync.Mutex
	b   bytes.Buffer
	end chan error
}

func newEndBuf() *endBuf { return &endBuf{end: make(chan error, 1)} }

func (e *endBuf) Write(p []byte) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.b.Write(p)
}
func (e *endBuf) BeginDrain()        {}
func (e *endBuf) EndDrain(err error) { e.end <- err }
func (e *endBuf) String() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.b.String()
}

// launcherSession is a launcher's transport: a real runnerwire session to a fake runner that answers
// recover_output with recover, whose OPENs reach the Substrate, with the output ACKs recorded and HeldHere and
// Online set by the test.
type launcherSession struct {
	org, run     *runnerwire.Peer
	h            Handler
	held, online atomic.Bool
	ack          atomic.Int64
	recovers     atomic.Int32
	recover      func(ctx context.Context, p *runnerwire.Peer, id uint32, a runnerwire.RecoverOutputArgs) (any, error)
}

func (l *launcherSession) Online() bool         { return l.online.Load() }
func (l *launcherSession) HeldHere() bool       { return l.held.Load() }
func (l *launcherSession) SetHandler(h Handler) { l.h = h }
func (l *launcherSession) TakeStream(id uint32) (*runnerwire.Stream, error) {
	return l.org.TakeStream(id)
}
func (l *launcherSession) Call(ctx context.Context, method string, args, result any, opts ...runnerwire.CallOption) error {
	if method == runnerwire.MethodOutputAck {
		l.ack.Store(args.(runnerwire.OutputAckArgs).Offset)
		return nil
	}
	return l.org.Call(ctx, method, args, result, opts...)
}

func newLauncher(ctx context.Context, opts Options, recover func(ctx context.Context, p *runnerwire.Peer, id uint32, a runnerwire.RecoverOutputArgs) (any, error)) (*Substrate, *launcherSession) {
	ls := &launcherSession{recover: recover}
	ls.online.Store(true)
	s := New(uuid.NewString(), ls, opts)
	o, r := runnerwire.Loopback()
	ls.org = runnerwire.NewPeer(o, runnerwire.PeerConfig{Org: true, OnOpen: ls.h.HandleOpen})
	ls.run = runnerwire.NewPeer(r, runnerwire.PeerConfig{OnCall: func(ctx context.Context, id uint32, method string, raw json.RawMessage) (any, error) {
		if method != runnerwire.MethodRecoverOutput || ls.recover == nil {
			return nil, errors.New("unexpected method")
		}
		ls.recovers.Add(1)
		var a runnerwire.RecoverOutputArgs
		_ = json.Unmarshal(raw, &a)
		return ls.recover(ctx, ls.run, id, a)
	}})
	go func() { _ = ls.org.Run(ctx) }()
	go func() { _ = ls.run.Run(ctx) }()
	return s, ls
}

// addSink registers run's sink on s, with a ref when ref is set, holding prefix already.
func addSink(t *testing.T, s *Substrate, run uuid.UUID, ref string, w *endBuf, prefix string) *outputSink {
	t.Helper()
	sink := &outputSink{writer: w}
	if prefix != "" {
		if _, err := sink.accept(0, []byte(prefix)); err != nil {
			t.Fatal(err)
		}
	}
	s.mu.Lock()
	s.outputs[run] = sink
	s.mu.Unlock()
	if ref != "" {
		s.setSinkRef(sink, ref)
	}
	return sink
}

func (s *Substrate) sinkFor(run uuid.UUID) *outputSink {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.outputs[run]
}

func (sink *outputSink) state() (offset int64, attached int) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return sink.offset, len(sink.attached)
}

// forwardRig is a holder Substrate whose Link holds a fake runner's session, with its output opens forwarded to
// launcher Substrates by replica id behind a fake door that refuses as the real one does.
type forwardRig struct {
	holder *Substrate
	link   *Link
	runner *runnerwire.Peer
	run    uuid.UUID
	dials  atomic.Int32

	mu        sync.Mutex
	at        OutputLauncher
	atErr     error
	launchers map[string]*Substrate
	refuse    string
	door      func(ctx context.Context, d runnerwire.Conn) // replaces the launcher's door when set
}

func newForwardRig(t *testing.T, ctx context.Context) *forwardRig {
	t.Helper()
	r := &forwardRig{run: uuid.New(), launchers: map[string]*Substrate{}}
	runnerID, sess := uuid.New(), uuid.New()
	view := NewHolderView("a", func(context.Context) (map[uuid.UUID]PeerRoute, error) {
		return map[uuid.UUID]PeerRoute{runnerID: {ReplicaID: "a", PeerURL: "https://a", SessionID: sess}}, nil
	})
	if err := view.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	pt := NewPeerTransport(PeerConfig{Key: make([]byte, 32)}, "a", nil)
	pt.dial = func(ctx context.Context, route PeerRoute, h http.Header) (runnerwire.Conn, error) {
		r.dials.Add(1)
		if h.Get(HeaderPeerMethod) != PeerMethodOutput || h.Get(HeaderPeerTarget) == "" {
			t.Errorf("forwarded open dialled method %q target %q", h.Get(HeaderPeerMethod), h.Get(HeaderPeerTarget))
		}
		r.mu.Lock()
		l, refuse, door := r.launchers[route.ReplicaID], r.refuse, r.door
		r.mu.Unlock()
		c, d := runnerwire.Loopback()
		if door != nil {
			go door(ctx, d)
			return c, nil
		}
		if refuse == "" && (l == nil || !l.ServableOutput(ctx, r.run)) {
			refuse = "no_sink"
		}
		if refuse != "" {
			return nil, &peerRefusedError{class: refuse}
		}
		go func() { _ = l.ServeOutputDoor(context.WithoutCancel(ctx), d, r.run, nil) }()
		return c, nil
	}
	r.link = &Link{}
	r.link.UsePeer(runnerID, view, pt)
	r.holder = New(runnerID.String(), r.link, Options{RunOutputLauncher: func(context.Context, uuid.UUID) (OutputLauncher, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.at, r.atErr
	}})
	r.connect(ctx)
	return r
}

// connect attaches a new runner session to the holder, replacing any before it.
func (r *forwardRig) connect(ctx context.Context) {
	o, rc := runnerwire.Loopback()
	p := runnerwire.NewPeer(rc, runnerwire.PeerConfig{OnCall: func(context.Context, uint32, string, json.RawMessage) (any, error) {
		return nil, nil
	}})
	go func() { _ = p.Run(ctx) }()
	r.link.Attach(ctx, o)
	r.runner = p
}

func (r *forwardRig) set(at OutputLauncher) {
	r.mu.Lock()
	r.at = at
	r.mu.Unlock()
}

func (r *forwardRig) open(ctx context.Context, offset int64) (*runnerwire.Stream, error) {
	return r.runner.Open(ctx, runnerwire.Open{Kind: runnerwire.KindOutput, Target: r.run.String(), Offset: offset})
}

// openRetry retries a capacity answer every 20 ms, as the runner does, until another answer or 5 s; it answers the
// last result and how many capacity answers came before it.
func (r *forwardRig) openRetry(ctx context.Context, offset int64) (*runnerwire.Stream, int, error) {
	n := 0
	for deadline := time.Now().Add(5 * time.Second); ; n++ {
		st, err := r.open(ctx, offset)
		if resetCode(err) != runnerwire.ResetCapacity || time.Now().After(deadline) {
			return st, n, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (r *forwardRig) forward() outputForward {
	r.holder.mu.Lock()
	defer r.holder.mu.Unlock()
	if f := r.holder.forwards[r.run]; f != nil {
		return *f
	}
	return outputForward{}
}

func writeAll(t *testing.T, st *runnerwire.Stream, s string) {
	t.Helper()
	if _, err := st.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
}

// R23 (remote): a runner's output open at the holder reaches the launcher's sink, the target read on every attempt.
func TestPeerOutputOpenForwardedToLauncher(t *testing.T) {
	ctx := t.Context()
	shorten(t, &forwardReadyFor, 300*time.Millisecond)
	shorten(t, &forwardRefusedFor, 300*time.Millisecond)
	r := newForwardRig(t, ctx)
	b, bs := newLauncher(ctx, Options{}, nil)
	w := newEndBuf()
	sink := addSink(t, b, r.run, "", w, "")
	r.launchers["b"] = b
	r.set(OutputLauncher{State: LauncherFresh, ReplicaID: "b", PeerURL: "https://b"})

	// The first open is answered capacity at once; a retry at the same offset is accepted.
	if _, err := r.open(ctx, 0); resetCode(err) != runnerwire.ResetCapacity {
		t.Fatalf("first open: %v, want a capacity reset", err)
	}
	st, _, err := r.openRetry(ctx, 0)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	writeAll(t, st, "hello")
	waitUntil(t, "the bytes reach the launcher's sink and are ACKed", func() bool { return w.String() == "hello" && bs.ack.Load() == 5 })

	// A new connection on the same holder reaches the same sink, at the acknowledged offset.
	r.connect(ctx)
	st, _, err = r.openRetry(ctx, 5)
	if err != nil {
		t.Fatalf("open on the new connection: %v", err)
	}
	writeAll(t, st, " world")
	waitUntil(t, "the new connection's bytes follow", func() bool { return w.String() == "hello world" && bs.ack.Load() == 11 })

	// The launcher's capacity answer reaches the runner as capacity.
	r.connect(ctx)
	sink.mu.Lock()
	sink.pending = true
	sink.mu.Unlock()
	if _, n, err := r.openRetry(ctx, 11); resetCode(err) != runnerwire.ResetCapacity || n < 2 {
		t.Fatalf("while the launcher's sink is pending: %v after %d capacity answers, want capacity until 5 s", err, n)
	}
	// The last capacity answer may have started an attempt still in flight: let it return while the sink is
	// pending, so no ready forward from it meets the next open.
	waitUntil(t, "the pending phase's last attempt returns", func() bool { return !r.forward().attempting })
	if r.forward().ready != nil {
		t.Fatal("an attempt while the launcher's sink was pending left a ready forward")
	}
	sink.mu.Lock()
	sink.pending = false
	sink.mu.Unlock()

	// A ready forward that no retry takes is reset after forwardReadyFor.
	r.connect(ctx)
	if _, err := r.open(ctx, 11); resetCode(err) != runnerwire.ResetCapacity {
		t.Fatalf("open: %v, want a capacity reset", err)
	}
	waitUntil(t, "the launcher accepts the forwarded open", func() bool { _, n := sink.state(); return n == 1 })
	waitUntil(t, "the untaken stream is reset", func() bool { _, n := sink.state(); return n == 0 })

	// A lease moved to another launcher between two attempts is followed.
	c, _ := newLauncher(ctx, Options{}, nil)
	wc := newEndBuf()
	addSink(t, c, r.run, "", wc, "hello world")
	r.mu.Lock()
	r.launchers["c"] = c
	r.mu.Unlock()
	r.set(OutputLauncher{State: LauncherFresh, ReplicaID: "c", PeerURL: "https://c"})
	r.connect(ctx)
	st, _, err = r.openRetry(ctx, 11)
	if err != nil {
		t.Fatalf("open after the move: %v", err)
	}
	writeAll(t, st, "!")
	waitUntil(t, "the moved lease's sink takes the bytes", func() bool { return wc.String() == "hello world!" })
	if w.String() != "hello world" {
		t.Fatalf("the old launcher's sink = %q, want it unchanged", w.String())
	}

	// The launcher's no_sink makes the next retry refused, and a refusal answers at once while it lasts.
	r.mu.Lock()
	r.refuse = "no_sink"
	r.mu.Unlock()
	r.connect(ctx)
	if _, _, err := r.openRetry(ctx, 12); resetCode(err) != runnerwire.ResetRefused {
		t.Fatalf("after no_sink: %v, want a refused reset", err)
	}
	dials := r.dials.Load()
	if _, err := r.open(ctx, 12); resetCode(err) != runnerwire.ResetRefused || r.dials.Load() != dials {
		t.Fatalf("within the refusal: %v with %d new dials, want refused with none", err, r.dials.Load()-dials)
	}
	// Any other class records nothing: the next retry starts a new attempt.
	time.Sleep(forwardRefusedFor)
	r.mu.Lock()
	r.refuse = "not_live_session"
	r.mu.Unlock()
	if _, n, err := r.openRetry(ctx, 12); resetCode(err) != runnerwire.ResetCapacity || n < 2 || r.dials.Load() < dials+2 {
		t.Fatalf("after not_live_session: %v after %d capacity answers, want capacity and a new attempt per retry", err, n)
	}

	// The launcher's door takes one OPEN of the token's run: a call first, or an OPEN for another run, is
	// wrong_method, closes the WebSocket, and reaches no sink.
	for name, first := range map[string]func(ctx context.Context, p *runnerwire.Peer) error{
		"a call": func(ctx context.Context, p *runnerwire.Peer) error {
			return p.Call(ctx, runnerwire.MethodStatus, nil, nil)
		},
		"another run": func(ctx context.Context, p *runnerwire.Peer) error {
			_, err := p.Open(ctx, runnerwire.Open{Kind: runnerwire.KindOutput, Target: uuid.NewString()})
			return err
		},
		"another kind": func(ctx context.Context, p *runnerwire.Peer) error {
			_, err := p.Open(ctx, runnerwire.Open{Kind: runnerwire.KindRelay, Target: r.run.String()})
			return err
		},
	} {
		var classes []string
		var cmu sync.Mutex
		h, d := runnerwire.Loopback()
		ended := make(chan error, 1)
		go func() {
			ended <- b.ServeOutputDoor(ctx, d, r.run, func(c string) { cmu.Lock(); classes = append(classes, c); cmu.Unlock() })
		}()
		hp := runnerwire.NewPeer(h, runnerwire.PeerConfig{})
		go func() { _ = hp.Run(ctx) }()
		before := w.String()
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		if err := first(cctx, hp); err == nil {
			t.Errorf("%s: the door served it", name)
		}
		cancel()
		select {
		case <-ended:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the door did not close", name)
		}
		cmu.Lock()
		if len(classes) != 1 || classes[0] != "wrong_method" || w.String() != before {
			t.Errorf("%s: classes %v, sink %q; want one wrong_method and no byte", name, classes, w.String())
		}
		cmu.Unlock()
	}
}

// R23b: HandleOpen never waits on a forward attempt, so the session's read loop keeps serving.
func TestPeerOutputOpenNeverBlocksReadLoop(t *testing.T) {
	ctx := t.Context()
	shorten(t, &forwardAttemptTimeout, 500*time.Millisecond)
	r := newForwardRig(t, ctx)
	r.set(OutputLauncher{State: LauncherFresh, ReplicaID: "b", PeerURL: "https://b"})
	r.door = func(context.Context, runnerwire.Conn) {} // accepts the WebSocket and never answers the OPEN
	for i := range 5 {
		start := time.Now()
		if _, err := r.open(ctx, 0); resetCode(err) != runnerwire.ResetCapacity {
			t.Fatalf("open %d: %v, want a capacity reset", i, err)
		}
		if d := time.Since(start); d > 100*time.Millisecond {
			t.Fatalf("open %d took %v while the attempt hangs", i, d)
		}
		// A call on the same session is answered while the attempt hangs.
		cctx, cancel := context.WithTimeout(ctx, time.Second)
		err := r.link.Call(cctx, runnerwire.MethodOutputAck, runnerwire.OutputAckArgs{RunID: r.run, Offset: 1}, nil)
		cancel()
		if err != nil {
			t.Fatalf("a call while the attempt hangs: %v", err)
		}
	}
	if n := r.dials.Load(); n != 1 {
		t.Fatalf("%d attempts while one hangs, want 1", n)
	}
	waitUntil(t, "the attempt ends at its deadline", func() bool { return !r.forward().attempting })
	if _, err := r.open(ctx, 0); resetCode(err) != runnerwire.ResetCapacity {
		t.Fatalf("open after the deadline: %v", err)
	}
	waitUntil(t, "the next retry starts a new attempt", func() bool { return r.dials.Load() == 2 })
}

// R23d: an open whose launcher reads LauncherWait is answered capacity until forwardWaitCeiling, then refused.
func TestPeerOutputWaitCeilingRefuses(t *testing.T) {
	ctx := t.Context()
	shorten(t, &forwardWaitCeiling, 400*time.Millisecond)
	shorten(t, &forwardRefusedFor, 200*time.Millisecond)
	r := newForwardRig(t, ctx)
	r.set(OutputLauncher{State: LauncherWait})
	start := time.Now()
	_, n, err := r.openRetry(ctx, 0)
	if resetCode(err) != runnerwire.ResetRefused || n < 2 || time.Since(start) < forwardWaitCeiling {
		t.Fatalf("waiting: %v after %d capacity answers in %v, want capacity until the ceiling, then refused", err, n, time.Since(start))
	}
	if f := r.forward(); f.refusedAt.IsZero() || !f.waitSince.IsZero() {
		t.Fatalf("after the ceiling: refusal %v, wait clock %v; want a refusal and no clock", f.refusedAt, f.waitSince)
	}

	// A LauncherFresh read before the ceiling clears the clock.
	time.Sleep(forwardRefusedFor)
	if _, err := r.open(ctx, 0); resetCode(err) != runnerwire.ResetCapacity {
		t.Fatal(err)
	}
	waitUntil(t, "the wait clock starts", func() bool { return !r.forward().waitSince.IsZero() })
	r.set(OutputLauncher{State: LauncherFresh, ReplicaID: "b", PeerURL: "https://b"}) // no sink at b: no_sink
	waitUntil(t, "a fresh read clears the clock", func() bool {
		_, _ = r.open(ctx, 0)
		return r.forward().waitSince.IsZero() && !r.forward().attempting
	})

	// A lost run is refused at its first read.
	time.Sleep(forwardRefusedFor)
	r.set(OutputLauncher{State: LauncherNone})
	dials := r.dials.Load()
	if _, n, err := r.openRetry(ctx, 0); resetCode(err) != runnerwire.ResetRefused || n < 1 || n > 3 || r.dials.Load() != dials {
		t.Fatalf("a lost run: %v after %d capacity answers, %d dials; want refused at the first read", err, n, r.dials.Load()-dials)
	}
	// LauncherFresh naming this replica is a refusal too, with no dial.
	time.Sleep(forwardRefusedFor)
	r.set(OutputLauncher{State: LauncherFresh, ReplicaID: "a", PeerURL: "https://a"})
	if _, n, err := r.openRetry(ctx, 0); resetCode(err) != runnerwire.ResetRefused || n < 1 || n > 3 || r.dials.Load() != dials {
		t.Fatalf("a launcher naming this replica: %v after %d capacity answers, %d dials", err, n, r.dials.Load()-dials)
	}
}

// recoverWith answers recover_output with a stream from start that carries data, then ends.
func recoverWith(start int64, data string) func(ctx context.Context, p *runnerwire.Peer, id uint32, a runnerwire.RecoverOutputArgs) (any, error) {
	return func(ctx context.Context, p *runnerwire.Peer, id uint32, a runnerwire.RecoverOutputArgs) (any, error) {
		if !a.Gap {
			return nil, errors.New("recover_output without Gap")
		}
		st, err := p.Open(ctx, runnerwire.Open{Kind: runnerwire.KindOutput, Call: id})
		if err != nil {
			return nil, err
		}
		go func() { _, _ = st.Write([]byte(data)); _ = st.CloseWrite() }()
		return runnerwire.RecoverResult{Stream: st.ID(), Gap: start}, nil
	}
}

// R23c: a launcher's sink with nothing attached while its runner reads online elsewhere is recovered into itself.
func TestPeerOutputResumesStalledSink(t *testing.T) {
	ctx := t.Context()
	shorten(t, &outputStallAfter, 200*time.Millisecond)
	keepAll := func(context.Context, uuid.UUID) (bool, bool, error) { return true, true, nil }
	opts := Options{OutputLease: keepAll, OutputSinkCheck: 20 * time.Millisecond}

	// The runner stopped sending: the recovery from 0 fills the same sink, each byte once.
	s, ls := newLauncher(ctx, opts, recoverWith(0, "hello world"))
	stopSinkChecks(t, s)
	run, w := uuid.New(), newEndBuf()
	addSink(t, s, run, "runner:x/a", w, "hello")
	select {
	case err := <-w.end:
		if err != nil || w.String() != "hello world" || ls.recovers.Load() != 1 {
			t.Fatalf("recovered sink: %q, end %v, %d recoveries; want every byte once and one recovery", w.String(), err, ls.recovers.Load())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stalled sink was never recovered")
	}

	// A recovery start past the sink's offset ends it unrecoverable.
	s, _ = newLauncher(ctx, opts, recoverWith(9, "ld"))
	stopSinkChecks(t, s)
	run, w = uuid.New(), newEndBuf()
	addSink(t, s, run, "runner:x/b", w, "hello")
	select {
	case err := <-w.end:
		if !errors.Is(err, runner.ErrOutputUnrecoverable) {
			t.Fatalf("a start past the offset ended the sink with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a start past the offset did not end the sink")
	}

	// A sink whose session the launcher holds is never checked for a stall.
	s, ls = newLauncher(ctx, opts, recoverWith(0, "hello"))
	stopSinkChecks(t, s)
	ls.held.Store(true)
	addSink(t, s, uuid.New(), "runner:x/c", newEndBuf(), "hello")
	time.Sleep(3 * outputStallAfter)
	if n := ls.recovers.Load(); n != 0 {
		t.Fatalf("a held session's sink was recovered %d times", n)
	}

	// The count does not run while the runner reads offline: a return starts no recovery for the stall time.
	s, ls = newLauncher(ctx, opts, recoverWith(0, "hello"))
	stopSinkChecks(t, s)
	ls.online.Store(false)
	addSink(t, s, uuid.New(), "runner:x/d", newEndBuf(), "hello")
	time.Sleep(2 * outputStallAfter)
	ls.online.Store(true)
	back := time.Now()
	waitUntil(t, "the sink is recovered after the runner returns", func() bool { return ls.recovers.Load() == 1 })
	if d := time.Since(back); d < outputStallAfter {
		t.Fatalf("recovered %v after the runner returned, want at least %v", d, outputStallAfter)
	}

	// A recover call that hangs past two checks starts no second one, and the sink keeps its own offset.
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	s, ls = newLauncher(ctx, opts, func(ctx context.Context, _ *runnerwire.Peer, _ uint32, _ runnerwire.RecoverOutputArgs) (any, error) {
		select {
		case <-hang:
		case <-ctx.Done():
		}
		return nil, errors.New("hung")
	})
	stopSinkChecks(t, s)
	sink := addSink(t, s, uuid.New(), "runner:x/e", newEndBuf(), "hello")
	waitUntil(t, "the recovery starts", func() bool { return ls.recovers.Load() == 1 })
	time.Sleep(10 * opts.OutputSinkCheck)
	if n := ls.recovers.Load(); n != 1 {
		t.Fatalf("%d recoveries while one hangs, want 1", n)
	}
	if off, _ := sink.state(); off != 5 {
		t.Fatalf("the stalled sink's offset is %d, want its own 5", off)
	}
}

// R23e (remote): a launcher's sink yields to a fresh lease held elsewhere, and serves only while no other process
// holds the lease fresh.
func TestPeerOutputSinkYieldsToFreshLease(t *testing.T) {
	ctx := t.Context()
	shorten(t, &outputStallAfter, 200*time.Millisecond)
	var mu sync.Mutex
	keep, sole, readErr := true, true, error(nil)
	lease := func(context.Context, uuid.UUID) (bool, bool, error) {
		mu.Lock()
		defer mu.Unlock()
		return keep, sole, readErr
	}
	setLease := func(k, so bool, err error) { mu.Lock(); keep, sole, readErr = k, so, err; mu.Unlock() }
	var yielded atomic.Int32
	var w *endBuf
	opts := Options{OutputLease: lease, OutputSinkCheck: 20 * time.Millisecond, OutputYielded: func(uuid.UUID) {
		yielded.Add(1)
		if len(w.end) != 0 {
			panic("the sink was finished before OutputYielded returned")
		}
	}}
	s, ls := newLauncher(ctx, opts, nil)
	stopSinkChecks(t, s)
	run := uuid.New()
	w = newEndBuf()
	addSink(t, s, run, "runner:x/a", w, "")
	st, err := ls.run.Open(ctx, runnerwire.Open{Kind: runnerwire.KindOutput, Target: run.String()})
	if err != nil {
		t.Fatal(err)
	}
	writeAll(t, st, "part")
	waitUntil(t, "the bytes land", func() bool { return w.String() == "part" })

	// keep true, or a failed read, keeps the sink.
	setLease(true, true, errors.New("database down"))
	time.Sleep(5 * opts.OutputSinkCheck)
	if s.sinkFor(run) == nil || yielded.Load() != 0 {
		t.Fatal("a failed read dropped the sink")
	}
	if s.ServableOutput(ctx, run) {
		t.Fatal("the door served a forwarded open on a failed read")
	}

	// keep false: the next check drops the sink, resets the attached stream, yields once, then finishes the sink.
	setLease(false, false, nil)
	select {
	case err := <-w.end:
		if !errors.Is(err, errOutputYielded) || yielded.Load() != 1 {
			t.Fatalf("yield: end %v after %d OutputYielded calls; want errOutputYielded after one", err, yielded.Load())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the sink did not yield")
	}
	<-st.Done()
	if resetCode(st.Err()) != runnerwire.ResetRefused || s.sinkFor(run) != nil {
		t.Fatalf("after the yield: stream %v, sink kept %v; want a refused reset and no sink", st.Err(), s.sinkFor(run) != nil)
	}
	if s.ServableOutput(ctx, run) {
		t.Fatal("the door serves a yielded run")
	}
	time.Sleep(5 * opts.OutputSinkCheck)
	if yielded.Load() != 1 {
		t.Fatalf("OutputYielded ran %d times", yielded.Load())
	}

	// sole false (another process's fresh lease, off the holder): no stall recovery and no_sink at the door; once
	// sole reads true, it recovers. A stale lease (keep and sole true) keeps the sink and its recovery.
	setLease(true, false, nil)
	s, ls = newLauncher(ctx, opts, recoverWith(0, "x"))
	stopSinkChecks(t, s)
	run = uuid.New()
	w = newEndBuf()
	addSink(t, s, run, "runner:x/b", w, "")
	time.Sleep(3 * outputStallAfter)
	if ls.recovers.Load() != 0 || s.ServableOutput(ctx, run) {
		t.Fatalf("sole false: %d recoveries, door serves %v; want none and no_sink", ls.recovers.Load(), s.ServableOutput(ctx, run))
	}
	setLease(true, true, nil)
	waitUntil(t, "the sink recovers once sole reads true", func() bool { return ls.recovers.Load() == 1 })
	if !s.ServableOutput(ctx, run) && s.sinkFor(run) != nil {
		t.Fatal("sole true: the door refuses a kept sink")
	}
}

// agedSink is addSink for a sink that has had nothing attached for an hour, so the next check that keeps it with
// sole set recovers it.
func agedSink(t *testing.T, s *Substrate, run uuid.UUID, ref string, w *endBuf) *outputSink {
	t.Helper()
	sink := &outputSink{writer: w}
	if _, err := sink.accept(0, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	sink.idleSince = time.Now().Add(-time.Hour)
	s.mu.Lock()
	s.outputs[run] = sink
	s.mu.Unlock()
	s.setSinkRef(sink, ref)
	return sink
}

func (sink *outputSink) idle() time.Time {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return sink.idleSince
}

// yieldLog records the runs Options.OutputYielded ran for.
type yieldLog struct {
	mu   sync.Mutex
	runs []uuid.UUID
}

func (y *yieldLog) yielded(run uuid.UUID) {
	y.mu.Lock()
	y.runs = append(y.runs, run)
	y.mu.Unlock()
}

func (y *yieldLog) has(run uuid.UUID) bool {
	y.mu.Lock()
	defer y.mu.Unlock()
	return slices.Contains(y.runs, run)
}

// A slow lease read delays no other sink: the reads of one check run at once, under one deadline of one interval.
// A read that does not answer by then, or that fails, keeps its sink as it was.
func TestPeerOutputSinkCheckSlowLeaseDoesNotDelayOthers(t *testing.T) {
	ctx := t.Context()
	const every = 200 * time.Millisecond
	blocked, yielding, stalled, failing := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	var active, entered, cancelled atomic.Int32
	blockedIn := make(chan struct{})
	var once sync.Once
	var sawInFlight atomic.Bool
	lease := func(ctx context.Context, run uuid.UUID) (bool, bool, error) {
		switch run {
		case blocked:
			active.Add(1)
			entered.Add(1)
			once.Do(func() { close(blockedIn) })
			<-ctx.Done()
			active.Add(-1)
			cancelled.Add(1)
			return false, false, ctx.Err()
		case failing:
			return false, false, errors.New("store read failed")
		}
		// The two healthy reads answer only while the blocked one is in flight, which a serial check never shows.
		select {
		case <-blockedIn:
		case <-ctx.Done():
			return false, false, ctx.Err()
		}
		if run == yielding {
			sawInFlight.Store(active.Load() > 0)
			return false, false, nil
		}
		return true, true, nil
	}
	var y yieldLog
	opts := Options{OutputLease: lease, OutputYielded: y.yielded, OutputSinkCheck: every}
	s, ls := newLauncher(ctx, opts, recoverWith(0, "hello world"))
	stopSinkChecks(t, s)
	ls.held.Store(false)
	start := time.Now()
	bw, yw, sw, fw := newEndBuf(), newEndBuf(), newEndBuf(), newEndBuf()
	blockedSink := addSink(t, s, blocked, "runner:x/blocked", bw, "hello")
	addSink(t, s, yielding, "runner:x/yielding", yw, "hello")
	agedSink(t, s, stalled, "runner:x/stalled", sw)
	failingSink := agedSink(t, s, failing, "runner:x/failing", fw)
	failingIdle := failingSink.idle()

	waitUntil(t, "the keep=false sink yields and the stalled sink is recovered", func() bool {
		return y.has(yielding) && s.sinkFor(yielding) == nil && ls.recovers.Load() >= 1
	})
	if d := time.Since(start); d > 2*every+every/2 {
		t.Fatalf("the healthy sinks were handled %v after the check began, want within two passes of %v", d, every)
	}
	if !sawInFlight.Load() {
		t.Fatal("the keep=false sink was read while no slow read was in flight: the check's reads ran one after another")
	}
	select {
	case err := <-yw.end:
		if !errors.Is(err, errOutputYielded) {
			t.Fatalf("the yielded sink ended with %v, want errOutputYielded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("the yielded sink was never finished")
	}
	select {
	case err := <-sw.end:
		if err != nil || sw.String() != "hello world" {
			t.Fatalf("the stalled sink: %q, end %v; want every byte once", sw.String(), err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stalled sink's recovery never ended")
	}

	// A read past the pass deadline is cancelled, and its sink and the failing one are kept as they were.
	time.Sleep(2 * every)
	if cancelled.Load() == 0 {
		t.Fatal("the blocked read was never cancelled at the pass deadline")
	}
	if entered.Load() < 2 {
		t.Fatalf("the blocked sink was read %d times, want it tried again at the next pass", entered.Load())
	}
	if s.sinkFor(blocked) != blockedSink || y.has(blocked) {
		t.Fatal("the sink whose read did not answer was dropped or yielded")
	}
	if s.sinkFor(failing) != failingSink || y.has(failing) || !failingSink.idle().Equal(failingIdle) {
		t.Fatal("the sink whose read failed was dropped, yielded or had its stall count changed")
	}
	if n := ls.recovers.Load(); n != 1 {
		t.Fatalf("%d recoveries, want only the stalled sink's", n)
	}
	select {
	case err := <-bw.end:
		t.Fatalf("the blocked sink was finished: %v", err)
	case err := <-fw.end:
		t.Fatalf("the failing sink was finished: %v", err)
	default:
	}
}

// With every read slot held by a slow read, the sink a check did not reach is the first the next check reads.
func TestPeerOutputSinkCheckReachesEverySinkPastSlowSlots(t *testing.T) {
	ctx := t.Context()
	const every = 100 * time.Millisecond
	runs := make([]uuid.UUID, sinkCheckReads+1)
	for i := range runs {
		runs[i] = uuid.New()
	}
	slices.SortFunc(runs, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	healthy := runs[sinkCheckReads] // after every blocked one in run id order
	var slowReads atomic.Int32
	var slowAtYield atomic.Int32
	lease := func(ctx context.Context, run uuid.UUID) (bool, bool, error) {
		if run != healthy {
			slowReads.Add(1)
			<-ctx.Done()
			return false, false, ctx.Err()
		}
		slowAtYield.Store(slowReads.Load())
		return false, false, nil
	}
	var y yieldLog
	s, _ := newLauncher(ctx, Options{OutputLease: lease, OutputYielded: y.yielded, OutputSinkCheck: every}, nil)
	stopSinkChecks(t, s)
	for i, run := range runs {
		addSink(t, s, run, "runner:x/"+strconv.Itoa(i), newEndBuf(), "")
	}
	waitUntil(t, "the sink past the slow slots yields", func() bool { return y.has(healthy) })
	// The first pass starts the eight slow reads and cannot reach the healthy sink; the second starts with it.
	if n := slowAtYield.Load(); n > 2*sinkCheckReads {
		t.Fatalf("the healthy sink was read after %d slow reads, want within two passes (at most %d)", n, 2*sinkCheckReads)
	}
}

// A read that ignores its context keeps its slot until it returns: the next passes start no ninth read and never
// read its sink again. Once the held reads return, the sinks they kept from a slot are read.
func TestPeerOutputSinkCheckHoldsEightReadsAtMost(t *testing.T) {
	ctx := t.Context()
	const every = 100 * time.Millisecond
	runs := make([]uuid.UUID, sinkCheckReads+2)
	for i := range runs {
		runs[i] = uuid.New()
	}
	slices.SortFunc(runs, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	held := runs[:sinkCheckReads]
	release := make(chan struct{})
	var inFlight, peak, entered atomic.Int32
	lease := func(_ context.Context, run uuid.UUID) (bool, bool, error) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		if slices.Contains(held, run) {
			entered.Add(1)
			<-release // ignores its context
			return true, true, nil
		}
		return false, false, nil
	}
	var y yieldLog
	s, _ := newLauncher(ctx, Options{OutputLease: lease, OutputYielded: y.yielded, OutputSinkCheck: every}, nil)
	stopSinkChecks(t, s)
	for i, run := range runs {
		addSink(t, s, run, "runner:x/"+strconv.Itoa(i), newEndBuf(), "")
	}
	waitUntil(t, "the eight held reads start", func() bool { return entered.Load() == sinkCheckReads })
	time.Sleep(every*2 + every/2) // two more passes find every slot held
	if p := peak.Load(); p > sinkCheckReads {
		t.Fatalf("%d lease reads in flight at once, want at most %d", p, sinkCheckReads)
	}
	if n := entered.Load(); n != sinkCheckReads {
		t.Fatalf("the held sinks were read %d times, want once each while their reads are out", n)
	}
	if y.has(runs[sinkCheckReads]) || y.has(runs[sinkCheckReads+1]) {
		t.Fatal("a sink was read while every slot was held")
	}
	close(release)
	waitUntil(t, "the sinks past the held slots yield", func() bool {
		return y.has(runs[sinkCheckReads]) && y.has(runs[sinkCheckReads+1])
	})
	if p := peak.Load(); p > sinkCheckReads {
		t.Fatalf("%d lease reads in flight at once, want at most %d", p, sinkCheckReads)
	}
}

// A pass whose deadline passes as it takes a slot gives the slot back: once no read is out, every slot is free.
func TestPeerOutputSinkCheckDeadlineAtAcquireFreesSlot(t *testing.T) {
	var reads atomic.Int32
	lease := func(context.Context, uuid.UUID) (bool, bool, error) {
		reads.Add(1)
		return true, true, nil
	}
	// The deadline is already past when each pass starts, so its slot select finds both a free slot and a done
	// context, and takes either; the sinks get their ref directly, so no check loop runs.
	s, _ := newLauncher(t.Context(), Options{OutputLease: lease, OutputSinkCheck: time.Nanosecond}, nil)
	stopSinkChecks(t, s)
	for i := range sinkCheckReads + 2 {
		sink := &outputSink{writer: newEndBuf(), ref: "runner:x/" + strconv.Itoa(i)}
		s.mu.Lock()
		s.outputs[uuid.New()] = sink
		s.mu.Unlock()
	}
	for range 200 {
		s.checkSinks(context.Background())
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	s.checkSinks(canceled)
	s.mu.Lock()
	held := len(s.readSlot)
	s.mu.Unlock()
	if held != 0 || reads.Load() != 0 {
		t.Fatalf("after passes that each met their deadline: %d slots held, %d reads; want every slot free and no read", held, reads.Load())
	}
}

// Two runners' Substrates on one process's slots hold sinkCheckReads lease reads in flight between them, not that
// many each, and a sink of either is read again once the held reads return.
func TestPeerOutputSinkCheckSlotsSharedAcrossRunners(t *testing.T) {
	ctx := t.Context()
	const every = 100 * time.Millisecond
	release := make(chan struct{})
	var inFlight, peak atomic.Int32
	lease := func(_ context.Context, _ uuid.UUID) (bool, bool, error) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		<-release // ignores its context
		return false, false, nil
	}
	slots := NewSinkCheckReads()
	var ya, yb yieldLog
	a, _ := newLauncher(ctx, Options{OutputLease: lease, OutputYielded: ya.yielded, OutputSinkCheck: every, SinkCheckReads: slots}, nil)
	stopSinkChecks(t, a)
	b, _ := newLauncher(ctx, Options{OutputLease: lease, OutputYielded: yb.yielded, OutputSinkCheck: every, SinkCheckReads: slots}, nil)
	stopSinkChecks(t, b)
	var runs []uuid.UUID
	for i := range sinkCheckReads {
		ra, rb := uuid.New(), uuid.New()
		runs = append(runs, ra, rb)
		addSink(t, a, ra, "runner:a/"+strconv.Itoa(i), newEndBuf(), "")
		addSink(t, b, rb, "runner:b/"+strconv.Itoa(i), newEndBuf(), "")
	}
	waitUntil(t, "every slot is held", func() bool { return len(slots) == sinkCheckReads })
	time.Sleep(every*2 + every/2) // two more passes of each runner find every slot held
	if p := peak.Load(); p > sinkCheckReads {
		t.Fatalf("%d lease reads in flight across two runners, want at most %d", p, sinkCheckReads)
	}
	close(release)
	waitUntil(t, "every sink of both runners yields", func() bool {
		for i, run := range runs {
			y := &yb
			if i%2 == 0 {
				y = &ya
			}
			if !y.has(run) {
				return false
			}
		}
		return true
	})
	if p := peak.Load(); p > sinkCheckReads {
		t.Fatalf("%d lease reads in flight across two runners, want at most %d", p, sinkCheckReads)
	}
}
