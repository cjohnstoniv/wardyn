// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"testing"
	"time"
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
	if last := time.Unix(0, run.lastPong.Load()); !last.After(start.Add(10 * every)) {
		t.Fatalf("no recent PONG: last at %s after start", last.Sub(start))
	}
	if err := org.Call(ctxT(t), "echo", map[string]string{}, nil); err != nil {
		t.Fatalf("a call on the kept-alive link = %v", err)
	}
}
