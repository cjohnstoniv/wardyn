// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"context"
	"errors"
	"testing"
	"time"
	"unsafe"
)

// A write stalled on the socket holds the write lock, so no PING (and no
// other frame) leaves and no PONG comes back. The missed-PONG check must still
// fail the link, which closes the socket and returns the stalled sender,
// whether the stalled frame is the keepalive's own PING or any other.
func TestKeepaliveFailsALinkWhoseWriteIsStalled(t *testing.T) {
	const every = 100 * time.Millisecond
	for _, otherSender := range []bool{false, true} {
		name := map[bool]string{false: "PING stalled", true: "another frame stalled"}[otherSender]
		t.Run(name, func(t *testing.T) {
			_, run, sock := newWSPair(t, PeerConfig{})
			sock.hold()
			sent := make(chan error, 1)
			if otherSender {
				go func() { sent <- run.Event(ctxT(t), Event{Kind: EventWaiting}) }()
				select {
				case <-sock.blocked:
				case <-time.After(3 * time.Second):
					t.Fatal("the frame never reached the socket")
				}
			}
			go run.keepalive(every)
			limit := 2 * (MissedPings + 2) * every
			select {
			case <-run.Done():
			case <-time.After(limit):
				t.Fatalf("a stalled write kept the link online past %s", limit)
			}
			if !otherSender {
				return
			}
			select {
			case err := <-sent:
				if err == nil {
					t.Fatal("the stalled sender reported success on a failed link")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("the stalled sender never returned")
			}
		})
	}
}

// Characterization: on a healthy link the keepalive exchanges PING/PONG and
// the link stays up for many intervals.
func TestKeepaliveKeepsAHealthyLinkUp(t *testing.T) {
	const every = 20 * time.Millisecond
	org, run, _ := newWSPair(t, PeerConfig{OnCall: echoHandler})
	start := time.Now()
	go run.keepalive(every)
	time.Sleep(20 * every)
	if !org.Online() || !run.Online() {
		t.Fatalf("a healthy link went down: org %v, runner %v", org.Err(), run.Err())
	}
	if last := *run.lastPong.Load(); !last.After(start.Add(10 * every)) {
		t.Fatalf("no recent PONG: last at %s after start", last.Sub(start))
	}
	if err := org.Call(ctxT(t), "echo", map[string]string{}, nil); err != nil {
		t.Fatalf("a call on the kept-alive link = %v", err)
	}
}

// A wall-clock step moves time.Now's wall reading but not its monotonic one.
// The PONG age must follow the monotonic reading: a backward step must not
// delay failing a silent link, and a forward step must not fail a healthy one.
func TestKeepalivePongAgeIgnoresWallClockSteps(t *testing.T) {
	const every = PingInterval
	limit := MissedPings * every
	pong := time.Now()
	p := &Peer{}
	p.lastPong.Store(&pong)
	for _, c := range []struct {
		name    string
		now     time.Time
		overdue bool
	}{
		{"PONGs stopped, wall stepped back 2s", stepWall(t, pong.Add(limit), -2*time.Second), true},
		{"healthy link, wall stepped forward 60s", stepWall(t, pong.Add(every), 60*time.Second), false},
		{"MissedPings intervals without a PONG", pong.Add(limit), true},
		{"just under MissedPings intervals", pong.Add(limit - time.Nanosecond), false},
	} {
		if got := p.pongOverdue(c.now, every); got != c.overdue {
			t.Errorf("%s: overdue = %v, want %v", c.name, got, c.overdue)
		}
	}
}

// stepWall returns at with its wall reading moved by d and its monotonic
// reading unchanged, as a later time.Now reads after the wall clock steps.
func stepWall(t *testing.T, at time.Time, d time.Duration) time.Time {
	t.Helper()
	s := at.Add(d)
	// time.Time is {wall uint64; ext int64; loc}; ext holds the monotonic reading.
	(*[2]int64)(unsafe.Pointer(&s))[1] = (*[2]int64)(unsafe.Pointer(&at))[1]
	if s.Sub(at) != 0 || s.Round(0).Sub(at.Round(0)) != d {
		t.Fatal("cannot build a wall-stepped time on this Go runtime")
	}
	return s
}

// R2-0103: a peer that stops reading but keeps sending PONGs must not keep a
// link with a stalled write online. The raw end never reads, so once the
// loopback's buffer is full the next write blocks, as on an undrained socket.
func TestKeepaliveFailsAStalledWriteDespiteUnsolicitedPongs(t *testing.T) {
	const every = 50 * time.Millisecond
	r := newRaw(t, PeerConfig{Org: true})
	sent := make(chan error, 1)
	go func() {
		for {
			if err := r.p.Send(context.Background(), Frame{Type: TypeGoaway}); err != nil {
				sent <- err
				return
			}
		}
	}()
	go r.p.keepalive(every)
	forgePongsUntilDown(t, r, every, 2*(MissedPings+2)*every)
	if err := <-sent; err == nil {
		t.Fatal("the stalled sender reported success on a failed link")
	}
}

// R2-0103: keepalive covers the resume replay, which Start writes under the
// write lock before any other frame.
func TestKeepaliveCoversAStalledResumeReplay(t *testing.T) {
	const every = 50 * time.Millisecond
	replay := NewReplayBuffer()
	for range 2 * 64 { // more frames than the loopback buffers
		if err := replay.Reserve(ctxT(t), 2); err != nil {
			t.Fatal(err)
		}
		replay.Assign(Frame{Type: TypePending, Payload: []byte("{}")})
	}
	pc, rc := Loopback()
	r := &rawPeer{p: NewPeer(pc, PeerConfig{Org: true, Replay: replay}), raw: rc}
	t.Cleanup(r.p.Close)
	started := make(chan error, 1)
	go func() { started <- r.p.Start(context.Background(), 0) }()
	waitFor(t, 3*time.Second, "keepalive to start during the replay", func() bool { return r.p.lastPong.Load() != nil })
	go r.p.keepalive(every)
	forgePongsUntilDown(t, r, every, 2*(MissedPings+2)*every)
	if err := <-started; err == nil {
		t.Fatal("Start reported a stalled replay as written")
	}
}

// forgePongsUntilDown sends PONGs that answer no PING until the peer's link
// fails, which it must within limit, as a link down.
func forgePongsUntilDown(t *testing.T, r *rawPeer, every, limit time.Duration) {
	t.Helper()
	tick := time.NewTicker(every / 2)
	defer tick.Stop()
	deadline := time.After(limit)
	for {
		select {
		case <-tick.C:
			_ = r.raw.WriteFrame(ctxT(t), Frame{Type: TypePong, Payload: EncodePing(time.Unix(0, 1))})
		case <-r.p.Done():
			if err := r.p.Err(); !errors.Is(err, ErrLinkDown) {
				t.Fatalf("link failed with %v, want link down", err)
			}
			return
		case <-deadline:
			t.Fatalf("unsolicited PONGs kept a stalled link online past %s", limit)
		}
	}
}
