// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
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
