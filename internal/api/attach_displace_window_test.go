// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// heldTakeoverRecorder is an audit recorder that holds the session.takeover row's writer until hold closes, and
// says when an attach was recorded cancelled. The owner's claim displaces only after that row is written, so while
// it is held the holder is evicted and its socket not yet closed.
type heldTakeoverRecorder struct {
	*sshTestRecorder
	hold      <-chan struct{}
	seen      chan struct{} // closed once the takeover row is in
	cancelled chan struct{} // closed once a session.attach failure row reads "attach cancelled"
	seenOnce  sync.Once
	cancOnce  sync.Once
}

func newHeldTakeoverRecorder(hold <-chan struct{}) *heldTakeoverRecorder {
	return &heldTakeoverRecorder{hold: hold, seen: make(chan struct{}), cancelled: make(chan struct{})}
}

func (g *heldTakeoverRecorder) Record(ctx context.Context, ev types.AuditEvent) error {
	err := g.sshTestRecorder.Record(ctx, ev)
	switch {
	case ev.Action == "session.takeover":
		g.seenOnce.Do(func() { close(g.seen) })
		<-g.hold
	case ev.Action == "session.attach" && ev.Outcome == "failure" && strings.Contains(string(ev.Data), errAttachCancelled.Error()):
		g.cancOnce.Do(func() { close(g.cancelled) })
	}
	return err
}

// heldAttachRunner is the support rig's runner with every Attach answered only once release closes.
type heldAttachRunner struct {
	*supportTermRunner
	release <-chan struct{}
}

func (r *heldAttachRunner) Attach(ctx context.Context, ref string, opts runner.AttachOptions) (runner.Session, error) {
	<-r.release
	return r.supportTermRunner.Attach(ctx, ref, opts)
}

// heldGrantSub is a runner substrate whose local grant answers only once release closes.
type heldGrantSub struct {
	*fakeLocalSub
	release <-chan struct{}
}

func (s heldGrantSub) LocalAttach(ctx context.Context, ref string, req runner.LocalAttachRequest) (runner.LocalSession, error) {
	<-s.release
	return s.fakeLocalSub.LocalAttach(ctx, ref, req)
}

// The owner joins while a helper's web terminal is still opening (#4206): the helper's Attach answers only after it
// was evicted, and the takeover row, written before the displacement, is held until the helper's socket has ended
// or 300ms have passed since its attach was recorded cancelled. The helper still reads 1008 and why.
func TestWebAttach_HelperDisplacedWhileItsTerminalOpensReadsWhy(t *testing.T) {
	hold := make(chan struct{})
	gate := newHeldTakeoverRecorder(hold)
	held := &heldAttachRunner{release: gate.seen}
	rig := newSupportTermRigWith(t, func(c *Config) {
		gate.sshTestRecorder = c.Audit.(*sshTestRecorder)
		c.Audit = gate
		held.supportTermRunner = c.Runner.(*supportTermRunner)
		c.Runner = held
	}, "terminal")
	helperEnd := rig.open(rig.ticket(supportHelperSub, true), false)
	waitFor(t, "the helper to hold the seat", func() bool { w := rig.srv.attachHolderFor(rig.run.ID); return w != nil && w.support == rig.grant.ID })
	var helperErr error
	helperGone := make(chan struct{})
	go func() { helperErr = <-helperEnd; close(helperGone) }()
	go func() {
		defer close(hold)
		select {
		case <-helperGone:
		case <-gate.cancelled:
			select {
			case <-helperGone:
			case <-time.After(300 * time.Millisecond):
			}
		case <-time.After(5 * time.Second):
		}
	}()
	rig.open(rig.ticket(supportOwnerSub, false), false) // registers behind the helper and claims the seat
	select {
	case <-helperGone:
	case <-time.After(5 * time.Second):
		t.Fatal("the helper's socket is still open")
	}
	var ce websocket.CloseError
	if !errors.As(helperErr, &ce) || ce.Code != websocket.StatusPolicyViolation || ce.Reason != supportOwnerJoinedReason {
		t.Fatalf("the helper's socket ended with %v, want %d %q", helperErr, websocket.StatusPolicyViolation, supportOwnerJoinedReason)
	}
}

// A take-over decided while a local grant is in flight (#4206): the holder is evicted, its grant then answers and
// is recorded cancelled, and it is displaced only after its socket has ended or 300ms have passed, the order a
// take-over keeps with its takeover row in between. The client still reads 1008 and why.
func TestAttachLocalDisplacedWhileItsGrantIsInFlightReadsWhy(t *testing.T) {
	rig := newLocalRig(t, localRigOpts{})
	release := make(chan struct{})
	rig.srv.localAttachSubs = func(uuid.UUID) localAttachSub { return heldGrantSub{fakeLocalSub: rig.sub, release: release} }
	end := drainLocal(rig.dial(t))
	waitFor(t, "the local holder to register", func() bool { return rig.srv.attachHolderFor(rig.run.ID) != nil })
	prev, announce := rig.srv.evictAttachHolderFor(rig.run.ID, holderOwner)
	if prev == nil || prev.source != attachSourceLocal {
		t.Fatalf("evicted %+v, want the local holder", prev)
	}
	close(release)
	waitFor(t, "the grant to be recorded cancelled", func() bool {
		for _, ev := range rig.rows("session.attach") {
			if ev.Outcome == "failure" && strings.Contains(string(ev.Data), errAttachCancelled.Error()) {
				return true
			}
		}
		return false
	})
	var got error
	select {
	case got = <-end:
	case <-time.After(300 * time.Millisecond):
	}
	prev.displace(attachTakeoverReason(holderOwner))
	if announce != nil {
		announce()
	}
	if got == nil {
		select {
		case got = <-end:
		case <-time.After(5 * time.Second):
			t.Fatal("the displaced local socket is still open")
		}
	}
	var ce websocket.CloseError
	if !errors.As(got, &ce) || ce.Code != websocket.StatusPolicyViolation || ce.Reason != attachTakeoverReason(holderOwner) {
		t.Fatalf("the local socket ended with %v, want %d %q", got, websocket.StatusPolicyViolation, attachTakeoverReason(holderOwner))
	}
}
