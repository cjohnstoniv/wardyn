// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// setSignal replaces the rig's kill(2) with f.
func (r *rig) setSignal(f func(pgid int, sig unix.Signal) error) {
	r.srv.mu.Lock()
	r.srv.signal = f
	r.srv.mu.Unlock()
}

// A resume let through the fence holds the guest agent's one lock until its continue is sent: a newer
// pause waits behind it and stops the agent after it, so the older continue never lands after the
// newer stop.
func TestANewerPauseWaitsForAContinueAlreadyLetThrough(t *testing.T) {
	r := newRig(t)
	c := r.client(t)
	pid := startGroup(t, r, c)
	signal(t, c, true, 1)
	waitGroup(t, pid, 2, true)
	admitted, release := make(chan struct{}), make(chan struct{})
	r.setSignal(func(pgid int, sig unix.Signal) error {
		if sig == unix.SIGCONT {
			close(admitted)
			<-release
		}
		return unix.Kill(-pgid, sig)
	})
	resumed := make(chan error, 1)
	go func() { _, err := c.ResumeAgent(ctxT(t), 1); resumed <- err }()
	<-admitted
	paused := make(chan error, 1)
	go func() { _, err := c.PauseAgent(ctxT(t), 2); paused <- err }()
	select {
	case err := <-paused:
		t.Fatalf("pause 2 answered (%v) while pause 1's continue was let through and not yet sent", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if rerr, perr := <-resumed, <-paused; rerr != nil || perr != nil {
		t.Fatalf("pause 1's continue = %v, pause 2's stop = %v; want both to act", rerr, perr)
	}
	waitGroup(t, pid, 2, true)
}

// A request let through moves the fence even when its signal fails: an older one stays dropped, and
// the same request sent again acts.
func TestAFailedSignalKeepsTheOrderAndARetryActs(t *testing.T) {
	r := newRig(t)
	c := r.client(t)
	pid := startGroup(t, r, c)
	signal(t, c, true, 1)
	signal(t, c, false, 1)
	failed := false
	r.setSignal(func(pgid int, sig unix.Signal) error {
		if !failed {
			failed = true
			return errors.New("injected")
		}
		return unix.Kill(-pgid, sig)
	})
	if _, err := c.PauseAgent(ctxT(t), 2); err == nil {
		t.Fatal("pause 2 with a failing signal answered no error")
	}
	waitGroup(t, pid, 2, false)
	if signal(t, c, false, 1) {
		t.Fatal("pause 1's continue acted after pause 2 was let through")
	}
	if !signal(t, c, true, 2) {
		t.Fatal("pause 2 sent again was dropped")
	}
	waitGroup(t, pid, 2, true)
}
