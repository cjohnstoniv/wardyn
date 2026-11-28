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

// TestPeerPinRelaysCallsAndStreams: a pin made on a replica with no session reaches the holder's session, the
// stream the runner opens for the call reaches the caller under its own id, and once the runner's session is
// replaced every call on the pin is errOffline.
func TestPeerPinRelaysCallsAndStreams(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	var runnerSide atomic.Pointer[runnerwire.Peer]
	var methods atomic.Int32
	serve := func(ctx context.Context, id uint32, method string, args json.RawMessage) (any, error) {
		methods.Add(1)
		switch method {
		case runnerwire.MethodAttach:
			st, err := runnerSide.Load().Open(ctx, runnerwire.Open{Kind: runnerwire.KindPTY, Call: id})
			if err != nil {
				return nil, err
			}
			go func() { _, _ = st.Write([]byte("hello")); _ = st.CloseWrite() }()
			return runnerwire.StreamResult{Stream: st.ID()}, nil
		case runnerwire.MethodResize:
			var a runnerwire.ResizeArgs
			_ = json.Unmarshal(args, &a)
			return nil, nil
		}
		return nil, errors.New("unexpected method")
	}
	holder := &Link{}
	orgConn, runConn := runnerwire.Loopback()
	runner := runnerwire.NewPeer(runConn, runnerwire.PeerConfig{OnCall: serve})
	runnerSide.Store(runner)
	go func() { _ = runner.Run(ctx) }()
	holder.Attach(ctx, orgConn)
	pin := pinnedPeer{l: holder, p: holder.current()}

	callerConn, doorConn := runnerwire.Loopback()
	go func() { _ = servePin(ctx, doorConn, pin, runnerwire.MethodAttach, nil) }()
	runnerID := uuid.New()
	pt := NewPeerTransport(PeerConfig{Key: make([]byte, 32)}, "a", func(context.Context, uuid.UUID) (PeerRoute, bool, error) {
		return PeerRoute{ReplicaID: "b", PeerURL: "https://b", SessionID: uuid.New()}, true, nil
	})
	pt.dial = func(context.Context, PeerRoute, http.Header) (runnerwire.Conn, error) { return callerConn, nil }
	caller := &Link{}
	caller.UsePeer(runnerID, pt)
	sess, err := caller.pinSession()
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
	// The runner reconnects: a new session replaces the pinned one.
	before := methods.Load()
	org2, run2 := runnerwire.Loopback()
	runner2 := runnerwire.NewPeer(run2, runnerwire.PeerConfig{OnCall: serve})
	go func() { _ = runner2.Run(ctx) }()
	holder.Attach(ctx, org2)
	if err := sess.Call(ctx, runnerwire.MethodResize, runnerwire.ResizeArgs{Stream: st.ID(), Cols: 1, Rows: 1}, nil); !errors.Is(err, errOffline) {
		t.Fatalf("call on the pin after the session moved = %v, want errOffline", err)
	}
	if got := methods.Load(); got != before {
		t.Fatalf("a call after the move reached a runner session (%d calls, want %d)", got, before)
	}
}
