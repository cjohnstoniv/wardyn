// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// roleRunner is sshFakeRunner recording each Attach's role (true: a writer's) and session, and running hook inside it.
type roleRunner struct {
	*sshFakeRunner
	mu       sync.Mutex
	roles    []bool
	sessions []*countingShellSession
	hook     func(writer bool)
}

func (r *roleRunner) Attach(_ context.Context, _ string, opts runner.AttachOptions) (runner.Session, error) {
	sess := newCountingShellSession()
	r.mu.Lock()
	r.roles = append(r.roles, !opts.Observer)
	r.sessions = append(r.sessions, sess)
	hook := r.hook
	r.mu.Unlock()
	if hook != nil {
		hook(!opts.Observer)
	}
	return sess, nil
}

// written is the input bytes each Attach's session received, in Attach order.
func (r *roleRunner) written() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := make([]int, len(r.sessions))
	for i, s := range r.sessions {
		n[i] = s.writtenBytes()
	}
	return n
}

// An issuer portal's ticket check covers its socket's first opening in the role it was registered in, and nothing
// after it. A promoted writer, whether promoted before the first role read, while the first opening is pending or
// after it is installed, opens and is made ready only on a check of its own that passed, run after the promotion
// was counted, and its Attach starts before that check's start plus uiDispatchFresh. A check that does not pass
// opens no writer and leaves the holder attaching, so no input is delivered. A socket that is no issuer portal's is
// unchanged.
func TestEstablishExec_PortalPromotionWaitsOnItsOwnCheck(t *testing.T) {
	const person = "delegate:person:aaaaaaaaaaaaaaaaaaaaaaaaaa:bbbb"
	const noTicket = time.Duration(-1)
	cases := []struct {
		name      string
		ticketAge time.Duration // the ticket check's start is this long ago; noTicket: no issuer portal's ticket
		writer    bool          // registered as the writer
		promote   string        // "": never; "before": before establishExec; "during": in the first Attach;
		// "after": after a first establishExec installed its exec (a writer's is demoted first), then again;
		// "again": before, and demoted and promoted again in the first writer Attach
		checks    []bool        // the own check's successive results; nil: no check of its own
		checkAge  time.Duration // the own check reports a start this long before it ran
		endInside bool          // the socket's context ends while the own check runs
		wantErr   error
		wantAsked int
		wantRoles []bool
		wantReady bool
	}{
		{name: "a registered writer opens on the ticket's check", writer: true, checks: []bool{false},
			wantRoles: []bool{true}, wantReady: true},
		{name: "a registered observer opens on the ticket's check", checks: []bool{false},
			wantRoles: []bool{false}, wantReady: true},
		{name: "promoted before the first role read, its check refuses", promote: "before", checks: []bool{false},
			wantErr: errAttachCancelled, wantAsked: 1},
		{name: "promoted before the first role read, its check passes", promote: "before", checks: []bool{true},
			wantAsked: 1, wantRoles: []bool{true}, wantReady: true},
		{name: "promoted while the first opening is pending, its check refuses", promote: "during", checks: []bool{false},
			wantErr: errAttachCancelled, wantAsked: 1, wantRoles: []bool{false}},
		{name: "promoted while the first opening is pending, its check passes", promote: "during", checks: []bool{true},
			wantAsked: 1, wantRoles: []bool{false, true}, wantReady: true},
		{name: "promoted after its first install, its check refuses", promote: "after", checks: []bool{false},
			wantErr: errAttachCancelled, wantAsked: 1, wantRoles: []bool{false}},
		{name: "promoted after its first install, its check passes", promote: "after", checks: []bool{true},
			wantAsked: 1, wantRoles: []bool{false, true}, wantReady: true},
		{name: "a writer installed, demoted and promoted again, its check refuses", writer: true, promote: "after",
			checks: []bool{false}, wantErr: errAttachCancelled, wantAsked: 1, wantRoles: []bool{true}},
		{name: "a writer installed, demoted and promoted again, its check passes", writer: true, promote: "after",
			checks: []bool{true}, wantAsked: 1, wantRoles: []bool{true, true}, wantReady: true},
		{name: "promoted, its check passes as the socket ends", promote: "before", checks: []bool{true}, endInside: true,
			wantErr: errAttachCancelled, wantAsked: 1},
		{name: "a holder with its own check and no ticket dispatch time", ticketAge: noTicket, writer: true,
			checks: []bool{true}, wantErr: errAttachTimedOut},
		{name: "promoted past the ticket's dispatch time, its own check passes", ticketAge: 2 * uiDispatchFresh,
			promote: "before", checks: []bool{true}, wantAsked: 1, wantRoles: []bool{true}, wantReady: true},
		{name: "promoted, its own check started a dispatch bound ago", promote: "before", checks: []bool{true},
			checkAge: uiDispatchFresh, wantErr: errAttachTimedOut, wantAsked: 1},
		{name: "promoted, no check of its own", promote: "before", wantErr: errAttachTimedOut},
		{name: "promoted again while its writer opening is pending, the second check refuses", promote: "again",
			checks: []bool{true, false}, wantErr: errAttachCancelled, wantAsked: 2, wantRoles: []bool{true}},
		{name: "not an issuer portal's socket: no check asked", ticketAge: noTicket, promote: "before",
			wantRoles: []bool{true}, wantReady: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, run, _ := sshOwnedRunningRun(t)
			fr := &roleRunner{sshFakeRunner: &sshFakeRunner{}}
			srv := New(Config{BaseCtx: testBaseCtx(t), Store: st, Runner: fr})
			ctx, end := context.WithCancel(t.Context())
			defer end()
			if tc.ticketAge != noTicket {
				ctx = withTicketActor(ctx, ticketActor{via: &types.DelegationVia{Delegate: uuid.New(), Grant: uuid.New()},
					principal: person, checkStart: time.Now().Add(-tc.ticketAge)})
			}
			h := &attachHolder{}
			h.writable.Store(tc.writer)
			t.Cleanup(func() { _ = h.mux.Close() })
			var asked int
			if tc.checks != nil {
				h.recheck = func(context.Context) (time.Time, bool) {
					if h.promotions.Load() == 0 || !h.writable.Load() {
						t.Error("the check ran before the promotion it covers was counted")
					}
					ok := tc.checks[min(asked, len(tc.checks)-1)]
					asked++
					if tc.endInside {
						end()
					}
					return time.Now().Add(-tc.checkAge), ok
				}
			}
			var once sync.Once
			fr.hook = func(writer bool) {
				switch {
				case tc.promote == "during" && !writer:
					once.Do(h.promote)
				case tc.promote == "again" && writer:
					once.Do(func() { h.writable.Store(false); h.promote() })
				}
			}
			switch tc.promote {
			case "before", "again":
				h.promote()
			case "after":
				if err := srv.establishExec(ctx, run, h); err != nil || !h.ready.isReady() {
					t.Fatalf("the first opening = %v, ready %v; want it open", err, h.ready.isReady())
				}
				if tc.writer {
					h.writable.Store(false) // the lease's demotion (demoteWriter): the exec and readiness stay
				}
				h.promote()
			}
			err := srv.establishExec(ctx, run, h)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("establishExec = %v, want %v", err, tc.wantErr)
			}
			fr.mu.Lock()
			roles := slices.Clone(fr.roles)
			fr.mu.Unlock()
			if !slices.Equal(roles, tc.wantRoles) || asked != tc.wantAsked {
				t.Fatalf("Attach roles (true: a writer's) = %v after %d checks of its own, want %v after %d",
					roles, asked, tc.wantRoles, tc.wantAsked)
			}
			if got := h.ready.isReady(); got != tc.wantReady {
				t.Fatalf("ready = %v, want %v: input reaches the exec only once it is ready", got, tc.wantReady)
			}
			if tc.wantErr != nil && h.canDeliver() {
				t.Fatal("input can reach the sandbox although the opening was refused")
			}
			if tc.wantReady && h.canDeliver() != h.writable.Load() {
				t.Fatalf("canDeliver = %v for a ready holder with writable %v", h.canDeliver(), h.writable.Load())
			}
		})
	}
}

// An issuer portal's holder delivers input and resizes only through a writer exec installed for its current
// promotion (canDeliver), checked again before every chunk. A promotion is writable before its writer exec is open,
// and before the holder's ready state is cleared, so a drain that is already past its ready wait, or mid-frame,
// still holds input here. That input reaches neither the observer's exec nor a writer exec from before the
// promotion; it waits for the promotion's own writer, and is never delivered when that writer's check refuses.
// The lease is the single-replica one (nil, always live), so canWrite alone admits it in every case below.
func TestWriteGated_PortalPromotionDeliversOnlyThroughItsCheckedWriter(t *testing.T) {
	const person = "delegate:person:aaaaaaaaaaaaaaaaaaaaaaaaaa:bbbb"
	cases := []struct {
		name   string
		writer bool // registered as the writer: its first exec is a writer's, delivering, then it is demoted
		check  bool // the promotion's own check result
		want   []int
	}{
		{name: "a ready observer promoted, its check refuses", check: false, want: []int{0}},
		{name: "a ready observer promoted, its check passes", check: true, want: []int{0, 1}},
		{name: "a delivering writer demoted and promoted again, its check refuses", writer: true, check: false,
			want: []int{1}},
		{name: "a delivering writer demoted and promoted again, its check passes", writer: true, check: true,
			want: []int{1, 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, run, _ := sshOwnedRunningRun(t)
			fr := &roleRunner{sshFakeRunner: &sshFakeRunner{}}
			srv := New(Config{BaseCtx: testBaseCtx(t), Store: st, Runner: fr})
			ctx := withTicketActor(t.Context(), ticketActor{via: &types.DelegationVia{Delegate: uuid.New(), Grant: uuid.New()},
				principal: person, checkStart: time.Now()})
			pending := make(chan struct{})
			entered := make(chan struct{})
			var enterOnce, releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(pending) }) }
			h := &attachHolder{recheck: func(context.Context) (time.Time, bool) {
				enterOnce.Do(func() { close(entered) })
				<-pending
				return time.Now(), tc.check
			}}
			h.writable.Store(tc.writer)
			t.Cleanup(func() { _ = h.mux.Close() })
			t.Cleanup(release) // on every exit, a failed assertion included, so the check never stays blocked
			if err := srv.establishExec(ctx, run, h); err != nil || !h.ready.isReady() {
				t.Fatalf("the first opening = %v, ready %v; want it open", err, h.ready.isReady())
			}
			if tc.writer {
				if err := h.writeGated(&h.mux, []byte("a")); err != nil {
					t.Fatal(err)
				}
				h.writable.Store(false) // the lease's demotion (demoteWriter)
			}
			h.promote()
			// Between its check and its install, the promotion's Attach asks again: a drain past its ready wait
			// delivers nothing and no resize reaches the old exec, whatever the check returned.
			var hookRan, hookDeliverable atomic.Bool
			var hookErr atomic.Value
			fr.mu.Lock()
			fr.hook = func(bool) {
				hookRan.Store(true)
				hookDeliverable.Store(h.canDeliver())
				if err := h.writeGated(&h.mux, []byte("h")); err != nil {
					hookErr.Store(err)
				}
			}
			fr.mu.Unlock()
			opened := make(chan error, 1)
			go func() { opened <- srv.establishExec(ctx, run, h) }()
			select {
			case <-entered: // the promotion's check is running, blocked on pending
			case err := <-opened:
				t.Fatalf("the promotion's opening returned %v without running its own check", err)
			case <-time.After(5 * time.Second):
				t.Fatal("the promotion's own check did not start within 5s")
			}
			// A drain past its ready wait, or mid-frame, while the promotion's check is running: every chunk
			// is asked again, and none is delivered, nor any resize.
			if err := h.writeGated(&h.mux, []byte("k")); err != nil {
				t.Fatal(err)
			}
			if h.canDeliver() {
				t.Fatal("canDeliver while the promotion's check is running: resizes would reach the old exec")
			}
			release()
			if err := <-opened; (err == nil) != tc.check {
				t.Fatalf("the promotion's opening = %v with its check %v", err, tc.check)
			}
			if hookRan.Load() != tc.check {
				t.Fatalf("the promotion's Attach ran = %v with its check %v", hookRan.Load(), tc.check)
			}
			if err, _ := hookErr.Load().(error); err != nil {
				t.Fatal(err)
			}
			if hookDeliverable.Load() {
				t.Fatal("canDeliver inside the promotion's Attach, after its check and before its install: input would reach the old exec")
			}
			if err := h.writeGated(&h.mux, []byte("k")); err != nil && tc.check {
				t.Fatal(err)
			}
			if got := fr.written(); !slices.Equal(got, tc.want) {
				t.Fatalf("input bytes per session, in Attach order = %v, want %v", got, tc.want)
			}
		})
	}
}
