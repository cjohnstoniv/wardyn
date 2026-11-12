// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// The run-start check: a run with a desktop waits for the agent account at the console up to LoginWait
// after boot, then fails with the reason. Init answers that reason, so the run fails instead of running.
// The person reads the default wait as "5 minutes".
func TestAwaitAgentConsole(t *testing.T) {
	const short = 30 * time.Millisecond
	cases := []struct {
		name    string
		console []fakeConsole
		wait    time.Duration // LoginWait; 0 is the default
		booted  time.Duration // before now
		want    string        // "" is success
	}{
		{"agent at the console", []fakeConsole{{uid: 502}}, short, 0, ""},
		{"agent signs in during the wait", []fakeConsole{{err: ErrNoConsoleUser}, {err: ErrNoConsoleUser}, {uid: 502}}, short, 0, ""},
		{"still at the login window", []fakeConsole{{err: ErrNoConsoleUser}}, short, 0,
			"the Mac guest never reached the agent's desktop: still at the login window 30ms after boot"},
		{"another account at the console", []fakeConsole{{uid: 501}}, short, 0,
			"the Mac guest never reached the agent's desktop: another account (uid 501) at the console 30ms after boot"},
		{"default wait, booted long before: one look, no wait", []fakeConsole{{err: ErrNoConsoleUser}}, 0, time.Hour,
			"the Mac guest never reached the agent's desktop: still at the login window 5 minutes after boot"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			con := &seqConsole{answers: c.console}
			d := &Desktop{Console: con, SessionEnvEvery: time.Millisecond, LoginWait: c.wait}
			err := d.awaitAgentConsole(context.Background(), 502, time.Now().Add(-c.booted))
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("err = %v, want none", err)
			case c.want != "" && (!errors.Is(err, ErrNeverAtDesktop) || err.Error() != c.want):
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			if c.booted > 0 && con.n != 1 {
				t.Fatalf("console read %d times, want one: the wait is counted from boot", con.n)
			}
		})
	}
}

func TestInWords(t *testing.T) {
	for d, want := range map[time.Duration]string{5 * time.Minute: "5 minutes", time.Minute: "1 minute",
		90 * time.Second: "1m30s", 30 * time.Millisecond: "30ms"} {
		if got := inWords(d); got != want {
			t.Errorf("inWords(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestAwaitAgentConsoleEndsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := &Desktop{Console: fakeConsole{err: ErrNoConsoleUser}, LoginWait: time.Hour}
	if err := d.awaitAgentConsole(ctx, 502, time.Now()); !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "never reached") {
		t.Fatalf("err = %v, want the context's", err)
	}
}
