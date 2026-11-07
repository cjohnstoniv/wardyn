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
	"os/exec"
	"strconv"
	"time"
)

// MacKCPasswordPath is the file macOS reads the auto-login password from. Stage 1 writes it, root-owned,
// mode 0600, and holds the agent account's password in it, lightly obfuscated.
const MacKCPasswordPath = "/etc/kcpassword"

// kcpasswordKey is the fixed key macOS XORs /etc/kcpassword with. It repeats every 11 bytes.
var kcpasswordKey = [11]byte{0x7D, 0x89, 0x52, 0x23, 0xD2, 0xBC, 0xDD, 0xEA, 0xA3, 0xB9, 0x1F}

// XORKCPassword applies the kcpassword key to b in place. Obfuscating and recovering are the same
// operation.
func XORKCPassword(b []byte) {
	for i := range b {
		b[i] ^= kcpasswordKey[i%len(kcpasswordKey)]
	}
}

// StdinRunner runs one command to completion with stdin as its standard input, for a value that must
// reach a command on no argv and in no environment. The error says how the command ended and nothing
// it printed, and the command's output is dropped.
type StdinRunner interface {
	RunStdin(ctx context.Context, stdin []byte, name string, args ...string) error
}

// RunStdin is StdinRunner for ExecRunner. A command's stderr can echo what it read, so none of it is
// kept.
func (ExecRunner) RunStdin(ctx context.Context, stdin []byte, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &ee):
		return fmt.Errorf("%s: exit status %d", name, ee.ExitCode())
	default:
		return fmt.Errorf("%s: did not run", name)
	}
}

// ScreenLock turns the agent account's screen lock off in a run's guest. A guest that locks asks for
// a password that only /etc/kcpassword holds, so the run's desktop would be stuck at a lock screen.
type ScreenLock struct {
	Run     CmdRunner
	Stdin   StdinRunner
	Console ConsoleUser
	// KCPassword is the file the password is read from; "" is MacKCPasswordPath.
	KCPassword string
	// Wait pauses between tries at the console user; nil is time.Sleep.
	Wait func(time.Duration)
}

// The console user may not be up when Init arrives (auto-login is still running): Off tries consoleTries
// times, consoleGap apart.
const (
	consoleTries = 5
	consoleGap   = 2 * time.Second
)

// consoleUser asks for the console user, again while nobody is logged in yet.
func (l ScreenLock) consoleUser(ctx context.Context) (uint32, error) {
	wait := l.Wait
	if wait == nil {
		wait = time.Sleep
	}
	for i := 0; ; i++ {
		uid, err := l.Console.Console(ctx)
		if err == nil || i == consoleTries-1 || ctx.Err() != nil {
			return uid, err
		}
		wait(consoleGap)
	}
}

// asUserHome is argv run in the console user's GUI session as that user, with that user's HOME (sudo -H),
// so a preference lands in the user's own domain and not root's.
func asUserHome(uid uint32, argv ...string) (string, []string) {
	n := strconv.FormatUint(uint64(uid), 10)
	args := []string{"asuser", n, "/usr/bin/sudo", "-H", "-u", "#" + n, "--"}
	return "/bin/launchctl", append(args, argv...)
}

// agentPassword reads the agent account's password from the kcpassword file. The caller clears it.
func (l ScreenLock) agentPassword() ([]byte, error) {
	return readKCPassword(l.KCPassword)
}

// readKCPassword reads the agent account's password from the kcpassword file at path ("" is
// MacKCPasswordPath). The caller clears it; the error never holds any of it.
func readKCPassword(path string) ([]byte, error) {
	if path == "" {
		path = MacKCPasswordPath
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("unreadable")
	}
	XORKCPassword(b)
	pw, _, _ := bytes.Cut(b, []byte{0})
	pw = bytes.Clone(pw)
	clear(b)
	if len(pw) == 0 {
		return nil, errors.New("empty")
	}
	return pw, nil
}

// Off writes the console user's screen saver idle time 0 for this machine and turns its screen lock
// off, through sysadminctl reading the password on stdin. It never fails the Init and fails closed:
// a step that fails is logged by name, never with why or with what the password was, and leaves the
// lock as it was. It says whether the lock was turned off.
func (l ScreenLock) Off(ctx context.Context) bool {
	uid, err := l.consoleUser(ctx)
	if err != nil {
		slog.Warn("guest agent: screen lock left as it was", "step", "console user")
		return false
	}
	// Per machine, and a run is a new machine: written here, in the session, it is this run's.
	cmd, args := asUserHome(uid, "/usr/bin/defaults", "-currentHost", "write", "com.apple.screensaver", "idleTime", "-int", "0")
	if _, err := l.Run.Run(ctx, cmd, args...); err != nil {
		slog.Warn("guest agent: screen saver idle time not set", "step", "defaults")
	}
	pw, err := l.agentPassword()
	if err != nil {
		slog.Warn("guest agent: screen lock left as it was", "step", "password file")
		return false
	}
	defer clear(pw)
	stdin := append(pw[:len(pw):len(pw)], '\n')
	defer clear(stdin)
	cmd, args = asUserHome(uid, "/usr/sbin/sysadminctl", "-screenLock", "off", "-password", "-")
	if err := l.Stdin.RunStdin(ctx, stdin, cmd, args...); err != nil {
		slog.Warn("guest agent: screen lock left as it was", "step", "sysadminctl")
		return false
	}
	return true
}
