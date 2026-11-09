// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	pkillTerm = []string{"/usr/bin/pkill", "-TERM", "-U", "502", "-x", "Setup Assistant"}
	noMatch   = errors.New("exit status 1")
)

// setupAssistantOnDesktop is a Desktop whose step is turned on by a switch file, with short polls.
func setupAssistantOnDesktop(t *testing.T, c ConsoleUser) *Desktop {
	t.Helper()
	sw := filepath.Join(t.TempDir(), "setup-assistant-quit")
	if err := os.WriteFile(sw, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return &Desktop{Console: c, SetupAssistantSwitch: sw, SessionEnvEvery: time.Millisecond,
		SessionEnvWait: 200 * time.Millisecond, SetupAssistantWatch: 100 * time.Millisecond}
}

// desktopRunner answers pgrep for Dock and Finder from dock and finder (called with the count of that
// app's checks so far), and pkill with quit. It fails the test on a pkill sent while the last console read
// was not the agent's or the last Dock or Finder check said it was not running.
type desktopRunner struct {
	fakeRunner
	t             *testing.T
	console       *seqConsole
	dock, finder  func(n int) error
	quit          error
	mu            sync.Mutex
	nDock, nFind  int
	dockUp, finUp bool
}

func newDesktopRunner(t *testing.T, c *seqConsole, dock, finder func(int) error, quit error) *desktopRunner {
	r := &desktopRunner{t: t, console: c, dock: dock, finder: finder, quit: quit}
	r.out = r.answer
	return r
}

func (r *desktopRunner) answer(argv []string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case argv[0] == "/usr/bin/pgrep" && argv[len(argv)-1] == "Dock":
		r.nDock++
		err := r.dock(r.nDock)
		r.dockUp = err == nil
		return nil, err
	case argv[0] == "/usr/bin/pgrep" && argv[len(argv)-1] == "Finder":
		r.nFind++
		err := r.finder(r.nFind)
		r.finUp = err == nil
		return nil, err
	case argv[0] == "/usr/bin/pkill":
		r.console.mu.Lock()
		last := r.console.answers[min(r.console.n, len(r.console.answers))-1]
		r.console.mu.Unlock()
		if last.err != nil || last.uid != 502 || !r.dockUp || !r.finUp {
			r.t.Errorf("pkill sent with the desktop not up: console %+v, Dock %v, Finder %v", last, r.dockUp, r.finUp)
		}
		return nil, r.quit
	}
	r.t.Errorf("unexpected command %q", argv)
	return nil, nil
}

func always(err error) func(int) error { return func(int) error { return err } }

// noKill fails on any signal but SIGTERM, and on a pkill whose argv is not pkillTerm.
func noKill(t *testing.T, calls [][]string) (pkills int) {
	t.Helper()
	for _, argv := range calls {
		if slices.Contains(argv, "-KILL") || slices.Contains(argv, "-9") || slices.Contains(argv, "-SIGKILL") {
			t.Fatalf("a kill was sent: %q", argv)
		}
		if argv[0] == "/usr/bin/pkill" {
			pkills++
			if !reflect.DeepEqual(argv, pkillTerm) {
				t.Fatalf("argv = %q, want %q", argv, pkillTerm)
			}
		}
	}
	return pkills
}

// Without the switch file, the default, the step reads nothing and sends nothing.
func TestEndSetupAssistantIsOffByDefault(t *testing.T) {
	if MacSetupAssistantSwitchPath != "/Library/Wardyn/setup-assistant-quit" {
		t.Fatalf("switch path = %q, which the docs do not name", MacSetupAssistantSwitchPath)
	}
	for name, sw := range map[string]string{
		"switch file absent":          filepath.Join(t.TempDir(), "setup-assistant-quit"),
		"switch is a directory":       t.TempDir(),
		"default path (absent there)": "",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := os.Lstat(MacSetupAssistantSwitchPath); sw == "" && err == nil {
				t.Skip("this machine has the switch file")
			}
			c := &seqConsole{answers: []fakeConsole{{uid: 502}}}
			r := newDesktopRunner(t, c, always(nil), always(nil), nil)
			d := &Desktop{Console: c, SetupAssistantSwitch: sw, SessionEnvEvery: time.Millisecond,
				SessionEnvWait: 20 * time.Millisecond, SetupAssistantWatch: 20 * time.Millisecond}
			d.endSetupAssistant(context.Background(), 502, r)
			if len(r.calls) != 0 || c.n != 0 {
				t.Fatalf("step off: calls = %q, console reads = %d, want none", r.calls, c.n)
			}
		})
	}
}

// With the switch on, nothing is signalled until the agent is at the console and Dock and Finder run as
// the agent: the console alone, or only one of the two apps, is still a login coming up.
func TestEndSetupAssistantSendsNothingUntilDockAndFinderRun(t *testing.T) {
	cases := []struct {
		name         string
		console      []fakeConsole
		dock, finder func(int) error
	}{
		{"at the login window", []fakeConsole{{err: ErrNoConsoleUser}}, always(nil), always(nil)},
		{"another account at the console", []fakeConsole{{uid: 501}}, always(nil), always(nil)},
		{"console read fails", []fakeConsole{{err: errors.New("stat /dev/console: boom")}}, always(nil), always(nil)},
		{"agent at the console, no Dock or Finder", []fakeConsole{{uid: 502}}, always(noMatch), always(noMatch)},
		{"agent at the console, Dock but no Finder", []fakeConsole{{uid: 502}}, always(nil), always(noMatch)},
		{"agent at the console, Finder but no Dock", []fakeConsole{{uid: 502}}, always(noMatch), always(nil)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			con := &seqConsole{answers: c.console}
			r := newDesktopRunner(t, con, c.dock, c.finder, nil)
			setupAssistantOnDesktop(t, con).endSetupAssistant(context.Background(), 502, r)
			if n := noKill(t, r.calls); n != 0 {
				t.Fatalf("%d signals sent before the desktop was up: %q", n, r.calls)
			}
			if con.n < 3 {
				t.Fatalf("console read %d times: the step stopped looking", con.n)
			}
		})
	}
}

// Once the desktop is up, the step asks Setup Assistant to quit with SIGTERM, never a kill, checks the
// desktop again before each signal, and keeps looking for the watch, so a relaunch is asked too.
func TestEndSetupAssistantQuitsGentlyOnceTheDesktopIsUp(t *testing.T) {
	cases := []struct {
		name         string
		console      []fakeConsole
		dock, finder func(int) error
		quit         error
	}{
		{"desktop up from the start, pane there", []fakeConsole{{uid: 502}}, always(nil), always(nil), nil},
		{"desktop up from the start, no pane", []fakeConsole{{uid: 502}}, always(nil), always(nil), noMatch},
		{"login window, then the agent, then Dock, then Finder",
			[]fakeConsole{{err: ErrNoConsoleUser}, {err: ErrNoConsoleUser}, {uid: 502}},
			func(n int) error { return map[bool]error{true: noMatch}[n < 3] },
			func(n int) error { return map[bool]error{true: noMatch}[n < 3] }, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			con := &seqConsole{answers: c.console}
			r := newDesktopRunner(t, con, c.dock, c.finder, c.quit)
			setupAssistantOnDesktop(t, con).endSetupAssistant(context.Background(), 502, r)
			if n := noKill(t, r.calls); n < 3 {
				t.Fatalf("pkill -TERM sent %d times, want it to keep asking for the watch: %q", n, r.calls)
			}
		})
	}
}

func TestEndSetupAssistantReturnsOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	con := &seqConsole{answers: []fakeConsole{{uid: 502}}}
	r := newDesktopRunner(t, con, always(nil), always(nil), nil)
	d := setupAssistantOnDesktop(t, con)
	d.SessionEnvWait = time.Hour
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
