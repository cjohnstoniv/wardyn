// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// TestPeerLeaseSentOnlyToPinnedSession: a holder door method authorised for one session sends its lease only on
// that session. The runner reconnects to the same replica while the method's read is pending; the send then answers
// errOffline, and no LEASE reaches the replacement session.
func TestPeerLeaseSentOnlyToPinnedSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	l := &Link{}
	var oldLeases, newLeases atomic.Int32
	attach := func(n *atomic.Int32) {
		org, run := runnerwire.Loopback()
		p := runnerwire.NewPeer(run, runnerwire.PeerConfig{OnControl: func(f runnerwire.Frame) {
			if f.Type == runnerwire.TypeLease {
				n.Add(1)
			}
		}})
		go func() { _ = p.Run(ctx) }()
		l.Attach(ctx, org)
	}
	attach(&oldLeases)

	sid := uuid.New()
	id := sid.String()
	runnerID := uuid.New()
	h := NewHub(ctx, HubConfig{})
	hs := &hubSession{id: id, link: l, peer: l.current()}
	hs.readyID.Store(&id)
	h.mu.Lock()
	h.sessions[runnerID] = hs
	h.mu.Unlock()

	reading, swapped := make(chan struct{}), make(chan struct{})
	sent := make(chan error, 1)
	answer := func(ctx context.Context, _ json.RawMessage) (any, error) {
		close(reading)
		<-swapped // the run read is pending while the runner reconnects
		err := l.Send(context.WithoutCancel(ctx), runnerwire.Frame{Type: runnerwire.TypeLease, Payload: json.RawMessage(`{}`)})
		sent <- err
		return nil, err
	}
	callerConn, doorConn := runnerwire.Loopback()
	go func() { _ = h.ServeHolderMethod(ctx, doorConn, runnerID, sid, PeerMethodLease, answer, nil) }()
	cp := runnerwire.NewPeer(callerConn, runnerwire.PeerConfig{Org: true})
	go func() { _ = cp.Run(ctx) }()
	called := make(chan error, 1)
	go func() { called <- peerSend(ctx, cp, PeerMethodLease, map[string]string{}, nil) }()

	<-reading
	attach(&newLeases) // the replacement session, on the same Link
	close(swapped)
	if err := <-sent; !errors.Is(err, errOffline) {
		t.Fatalf("the lease send after the session was replaced = %v, want errOffline", err)
	}
	if err := <-called; !errors.Is(err, errOffline) {
		t.Fatalf("the caller's answer = %v, want errOffline", err)
	}
	// A LEASE the replacement got would be read before a later frame: one round trip on the new session settles it.
	_ = l.Call(ctx, runnerwire.MethodStatus, map[string]string{"ref": "r"}, nil)
	if n := newLeases.Load(); n != 0 {
		t.Fatalf("%d LEASE frames reached the replacement session", n)
	}
	if n := oldLeases.Load(); n != 0 {
		t.Fatalf("%d LEASE frames reached the replaced session", n)
	}
}

// The holder's offline answer over a link that stays up reaches the caller as errOffline, as a closed link's does.
func TestPeerHolderOfflineAnswerIsErrOffline(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	callerConn, doorConn := runnerwire.Loopback()
	door := runnerwire.NewPeer(doorConn, runnerwire.PeerConfig{OnCall: func(context.Context, uint32, string, json.RawMessage) (any, error) {
		return nil, errOffline
	}})
	go func() { _ = door.Run(ctx) }()
	defer door.Close()
	cp := runnerwire.NewPeer(callerConn, runnerwire.PeerConfig{Org: true})
	go func() { _ = cp.Run(ctx) }()
	defer cp.Close()
	err := peerSend(ctx, cp, PeerMethodLease, map[string]string{}, nil)
	if !errors.Is(err, errOffline) {
		t.Fatalf("the caller's answer to the holder's offline = %v, want errOffline", err)
	}
	if !cp.Online() {
		t.Fatal("the link went down; the case is a link that stays up")
	}
}
