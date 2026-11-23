// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// An SSH shell's output reaches the session recording before the client, and nothing is sent to the client once
// the pump has ended, so a chunk the person saw is always in the recording.

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
)

// pumpMarker is the one chunk the fake shell prints.
const pumpMarker = "marker-output"

// pumpShell is a runner.Session whose Read returns pumpMarker once, then io.EOF.
type pumpShell struct{ sent bool }

func (s *pumpShell) Read(p []byte) (int, error) {
	if s.sent {
		return 0, io.EOF
	}
	s.sent = true
	return copy(p, pumpMarker), nil
}
func (s *pumpShell) Write(p []byte) (int, error)                  { return len(p), nil }
func (s *pumpShell) Resize(context.Context, uint16, uint16) error { return nil }
func (s *pumpShell) Close() error                                 { return nil }

// pumpTee is a cast tee that can be asked what it holds.
type pumpTee struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (t *pumpTee) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.Write(p)
}

func (t *pumpTee) holds(s string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return bytes.Contains(t.buf.Bytes(), []byte(s))
}

// pumpChannel is a fakeSSHChannel that records, at each write to the client, whether the tee already held the
// marker.
type pumpChannel struct {
	*fakeSSHChannel
	tee *pumpTee

	mu          sync.Mutex
	writes      int
	teedAtWrite []bool
}

func (c *pumpChannel) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.writes++
	c.teedAtWrite = append(c.teedAtWrite, c.tee.holds(pumpMarker))
	c.mu.Unlock()
	return c.fakeSSHChannel.Write(p)
}

func newPumpFixture() (*sshPumpState, *pumpChannel, *pumpTee) {
	tee := &pumpTee{}
	ch := &pumpChannel{fakeSSHChannel: newFakeSSHChannel(), tee: tee}
	ctx, cancel := context.WithCancel(context.Background())
	p := &sshPumpState{ctx: ctx, cancel: cancel, channel: ch, castTee: tee, sess: &pumpShell{}, reasonC: make(chan string, 2)}
	return p, ch, tee
}

func TestSSHShellPump_TheCastHoldsAChunkBeforeTheClientDoes(t *testing.T) {
	p, ch, _ := newPumpFixture()

	p.output()

	ch.mu.Lock()
	defer ch.mu.Unlock()
	if len(ch.teedAtWrite) != 1 || !ch.teedAtWrite[0] {
		t.Fatalf("channel writes saw the recording holding the chunk = %v, want one write after the tee", ch.teedAtWrite)
	}
	if reason := <-p.reasonC; reason != "shell exited" {
		t.Errorf("end reason = %q, want shell exited", reason)
	}
}

func TestSSHShellPump_AfterThePumpEndsNothingMoreIsSent(t *testing.T) {
	p, ch, tee := newPumpFixture()
	p.cancel()

	p.output()

	ch.mu.Lock()
	defer ch.mu.Unlock()
	if ch.writes != 0 {
		t.Errorf("channel writes after the pump ended = %d, want none", ch.writes)
	}
	if !tee.holds(pumpMarker) {
		t.Error("the recording lost a chunk read before the pump ended")
	}
}
