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
	r.caller.UsePeer(uuid.New(), pt)
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
