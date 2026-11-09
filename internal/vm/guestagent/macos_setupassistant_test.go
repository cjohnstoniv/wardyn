// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestEndSetupAssistant(t *testing.T) {
	pkill := []string{"/usr/bin/pkill", "-KILL", "-U", "502", "-x", "Setup Assistant"}
	none := errors.New("exit status 1")
	cases := []struct {
		name    string
		console fakeConsole
		answers []error // the answer to each pkill call, in order; the last repeats
		calls   int     // exact pkill calls; 0 with polls set means "at least 2, until the watch ends"
		polls   bool
	}{
		{"found then quiet: one kill plus one quiet poll", fakeConsole{uid: 502}, []error{nil, none}, 2, false},
		{"never found: keeps polling until the watch ends", fakeConsole{uid: 502}, []error{none}, 0, true},
		{"other uid at the console: no call", fakeConsole{uid: 0}, []error{nil}, 0, false},
		{"no console user: no call", fakeConsole{err: ErrNoConsoleUser}, []error{nil}, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := 0
			r := &fakeRunner{out: func([]string) ([]byte, error) {
				err := c.answers[min(n, len(c.answers)-1)]
				n++
				return nil, err
			}}
			d := &Desktop{Console: c.console, SessionEnvEvery: time.Millisecond, SetupAssistantWatch: 50 * time.Millisecond}
			d.endSetupAssistant(context.Background(), 502, r)
			if c.polls && len(r.calls) < 2 || !c.polls && len(r.calls) != c.calls {
				t.Fatalf("pkill calls = %d, want %d (polls=%v)", len(r.calls), c.calls, c.polls)
			}
			for _, got := range r.calls {
				if !reflect.DeepEqual(got, pkill) {
					t.Fatalf("argv = %q, want %q", got, pkill)
				}
			}
		})
	}
}

func TestEndSetupAssistantReturnsOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &fakeRunner{}
	d := &Desktop{Console: fakeConsole{uid: 502}}
	done := make(chan struct{})
	go func() { d.endSetupAssistant(ctx, 502, r); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("endSetupAssistant did not return on a cancelled ctx")
	}
	if len(r.calls) != 0 {
		t.Fatalf("calls = %v, want none", r.calls)
	}
}
