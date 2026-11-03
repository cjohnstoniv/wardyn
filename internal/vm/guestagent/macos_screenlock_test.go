// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const lockPw = "Zq9-secret-agent-password-0123456789ab"

type fakeStdin struct {
	calls [][]string
	stdin [][]byte
	err   error
}

func (f *fakeStdin) RunStdin(_ context.Context, stdin []byte, name string, args ...string) error {
	f.calls = append(f.calls, append([]string{name}, args...))
	f.stdin = append(f.stdin, bytes.Clone(stdin))
	return f.err
}

// kcFile writes /etc/kcpassword's content for pw the way stage 1 does.
func kcFile(t *testing.T, pw string) string {
	t.Helper()
	b := make([]byte, (len(pw)+1+11)/12*12)
	copy(b, pw)
	XORKCPassword(b)
	p := filepath.Join(t.TempDir(), "kcpassword")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func logCapture(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

func TestScreenLockOffReadsKcpasswordAndPassesItOnStdinOnly(t *testing.T) {
	logs := logCapture(t)
	run, in := &fakeRunner{}, &fakeStdin{}
	l := ScreenLock{Run: run, Stdin: in, Console: fakeConsole{uid: 502}, KCPassword: kcFile(t, lockPw)}
	if !l.Off(context.Background()) {
		t.Fatal("Off = false")
	}
	wantIdle := []string{"/bin/launchctl", "asuser", "502", "/usr/bin/sudo", "-H", "-u", "#502", "--", "/usr/bin/defaults", "-currentHost", "write", "com.apple.screensaver", "idleTime", "-int", "0"}
	if len(run.calls) != 1 || !slices.Equal(run.calls[0], wantIdle) {
		t.Fatalf("idle time call = %q", run.calls)
	}
	wantLock := []string{"/bin/launchctl", "asuser", "502", "/usr/bin/sudo", "-H", "-u", "#502", "--", "/usr/sbin/sysadminctl", "-screenLock", "off", "-password", "-"}
	if len(in.calls) != 1 || !slices.Equal(in.calls[0], wantLock) {
		t.Fatalf("screenLock call = %q", in.calls)
	}
	if string(in.stdin[0]) != lockPw+"\n" {
		t.Fatalf("stdin = %q", in.stdin[0])
	}
	for _, text := range []string{fmt.Sprint(run.calls), fmt.Sprint(in.calls), logs.String()} {
		if strings.Contains(text, lockPw) {
			t.Fatalf("the password is in argv or the log: %s", text)
		}
	}
}

func TestScreenLockOffFailsClosedAndLogsNoSecret(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "kcpassword-empty")
	keyOnly := make([]byte, 12)
	XORKCPassword(keyOnly) // plaintext of 12 NULs: an empty password
	if err := os.WriteFile(empty, keyOnly, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		console ConsoleUser
		kc      string
		stdin   error
		wantRun bool
	}{
		"nobody at the console": {fakeConsole{err: ErrNoConsoleUser}, kcFile(t, lockPw), nil, false},
		"no kcpassword":         {fakeConsole{uid: 502}, filepath.Join(t.TempDir(), "none"), nil, true},
		"empty password":        {fakeConsole{uid: 502}, empty, nil, true},
		"sysadminctl fails":     {fakeConsole{uid: 502}, kcFile(t, lockPw), errors.New("sysadminctl: exit status 1"), true},
	} {
		t.Run(name, func(t *testing.T) {
			logs := logCapture(t)
			run, in := &fakeRunner{}, &fakeStdin{err: tc.stdin}
			l := ScreenLock{Run: run, Stdin: in, Console: tc.console, KCPassword: tc.kc, Wait: func(time.Duration) {}}
			if l.Off(context.Background()) {
				t.Fatal("Off = true")
			}
			if !strings.Contains(logs.String(), "screen lock left as it was") {
				t.Fatalf("no failure line: %s", logs)
			}
			if strings.Contains(logs.String(), lockPw) {
				t.Fatalf("the password is in the log: %s", logs)
			}
			if tc.wantRun != (len(run.calls) == 1) {
				t.Fatalf("idle time calls = %d", len(run.calls))
			}
		})
	}
}

// The real exec seam: the password reaches the command's stdin and nothing else, and what the command
// prints (here, it echoes what it read) never reaches the error.
func TestExecRunnerRunStdinKeepsThePasswordOffArgvEnvAndErrors(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-sysadminctl")
	body := "#!/bin/sh\nenv > '" + dir + "/env'\nprintf '%s\\n' \"$0\" \"$@\" > '" + dir + "/argv'\ncat > '" + dir + "/stdin'\ncat '" + dir + "/stdin' >&2\nexit 3\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	err := ExecRunner{}.RunStdin(context.Background(), []byte(lockPw+"\n"), script, "-screenLock", "off", "-password", "-")
	if err == nil || err.Error() != script+": exit status 3" {
		t.Fatalf("err = %v", err)
	}
	for f, want := range map[string]bool{"stdin": true, "argv": false, "env": false} {
		b, rerr := os.ReadFile(filepath.Join(dir, f))
		if rerr != nil {
			t.Fatal(rerr)
		}
		if got := strings.Contains(string(b), lockPw); got != want {
			t.Errorf("%s holds the password = %v, want %v", f, got, want)
		}
	}
}

// flakyConsole has nobody at the console for its first n asks.
type flakyConsole struct {
	n     int
	asked *int
}

func (f flakyConsole) Console(context.Context) (uint32, error) {
	*f.asked++
	if *f.asked <= f.n {
		return 0, ErrNoConsoleUser
	}
	return 502, nil
}

func TestScreenLockOffWaitsForTheConsoleUserFiveTriesTwoSecondsApart(t *testing.T) {
	for _, tc := range []struct {
		name      string
		missing   int
		wantOK    bool
		wantAsked int
	}{{"up on the fourth ask", 3, true, 4}, {"never up", 99, false, 5}} {
		t.Run(tc.name, func(t *testing.T) {
			logs := logCapture(t)
			var asked int
			var waits []time.Duration
			l := ScreenLock{Run: &fakeRunner{}, Stdin: &fakeStdin{}, Console: flakyConsole{n: tc.missing, asked: &asked},
				KCPassword: kcFile(t, lockPw), Wait: func(d time.Duration) { waits = append(waits, d) }}
			if got := l.Off(context.Background()); got != tc.wantOK || asked != tc.wantAsked || len(waits) != tc.wantAsked-1 {
				t.Fatalf("Off = %v after %d asks and %d waits; want %v, %d, %d", got, asked, len(waits), tc.wantOK, tc.wantAsked, tc.wantAsked-1)
			}
			for _, d := range waits {
				if d != 2*time.Second {
					t.Fatalf("waited %v", d)
				}
			}
			if failed := strings.Contains(logs.String(), "screen lock left as it was"); failed == tc.wantOK {
				t.Fatalf("log = %s", logs)
			}
		})
	}
}
