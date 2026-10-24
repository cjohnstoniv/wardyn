// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"cmp"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
)

// The macOS desktop is the guest's own Screen Sharing. The agent owns three things about it: setting
// the per-run VNC password the host's helper made (Init.DesktopPassword), the pointer-position read
// the prepare control check uses, and the desktop session's environment.

// vncPasswordLen is VncAuth's key size: it uses at most eight characters (RFC 6143 7.2.2), so a
// longer password would not be the one the host's shim logs in with.
const vncPasswordLen = 8

// ConsoleUser names who owns the GUI session on the guest's console.
type ConsoleUser interface {
	Console(ctx context.Context) (uid uint32, err error)
}

// Pointer is the pointer position in the console session's screen points, origin at the bottom left
// (NSEvent.mouseLocation).
type Pointer struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// Desktop is the agent's hold on the guest's Screen Sharing. The zero value is not usable.
type Desktop struct {
	Run     CmdRunner
	Console ConsoleUser
	// VNCSettings is the settings file SetPassword writes; "" is MacVNCSettingsPath.
	VNCSettings string

	mu       sync.Mutex
	password string
	disabled bool
	open     map[io.Closer]struct{}
}

// Disable turns the run's desktop off: no password is set, the tunnel refuses, and a tunnel already
// open is closed. Called at Init for a run whose sign-ins cannot be kept (SignInsOff), which has no
// desktop on macOS; the desktop is on until then, because prepare's control check needs it.
func (d *Desktop) Disable() {
	d.mu.Lock()
	d.disabled = true
	open := d.open
	d.open = nil
	d.mu.Unlock()
	for c := range open {
		_ = c.Close()
	}
}

// Enabled says whether the desktop is on.
func (d *Desktop) Enabled() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return !d.disabled
}

// Track registers c as an open tunnel, so Disable closes it; release forgets it. It reports false, and
// registers nothing, once the desktop is disabled.
func (d *Desktop) Track(c io.Closer) (release func(), ok bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.disabled {
		return nil, false
	}
	if d.open == nil {
		d.open = map[io.Closer]struct{}{}
	}
	d.open[c] = struct{}{}
	return func() {
		d.mu.Lock()
		delete(d.open, c)
		d.mu.Unlock()
	}, true
}

// ErrNoConsoleUser is a guest with nobody logged in at the console (the login window).
var ErrNoConsoleUser = errors.New("guestagent: nobody is logged in at the guest's console")

// MacVNCSettingsPath is where Screen Sharing keeps its legacy VNC password: VNCSettingsLine's text,
// root-owned, mode 0600.
const MacVNCSettingsPath = "/Library/Preferences/com.apple.VNCSettings.txt"

// vncSettingsKey is the fixed key Screen Sharing XORs its stored VNC password with.
var vncSettingsKey = [16]byte{0x17, 0x34, 0x51, 0x6E, 0x8B, 0xA8, 0xC5, 0xE2, 0xFF, 0x1C, 0x39, 0x56, 0x73, 0x90, 0xAD, 0xCA}

// VNCSettingsLine is the settings file's text for password: each of the key's 16 bytes XORed with the
// password's (zero past its end), in upper-case hex, and a newline. kickstart -setvncpw writes the same
// text; that the guest's Screen Sharing reads a password written here is unproven (MAC-PROOF P9).
func VNCSettingsLine(password string) []byte {
	var b [16]byte
	for i, k := range vncSettingsKey {
		if i < len(password) {
			k ^= password[i]
		}
		b[i] = k
	}
	return []byte(strings.ToUpper(hex.EncodeToString(b[:])) + "\n")
}

// SetPassword sets Screen Sharing's VNC password to pw by writing the settings file itself, through a
// temp file and a rename (replaceFile), so the password is on no command line: any guest process,
// the agent user's included, can read another process's arguments. Nothing holds pw afterwards, and
// the error holds none of it. Init's run password and tools mode's desktop_password step both set it
// here.
func (d *Desktop) SetPassword(pw string) error {
	if pw == "" || len(pw) > vncPasswordLen {
		return fmt.Errorf("the desktop password is %d bytes, not 1 to %d", len(pw), vncPasswordLen)
	}
	path := cmp.Or(d.VNCSettings, MacVNCSettingsPath)
	err := replaceFile(path, 0o600, func(f *os.File) error {
		_, err := f.Write(VNCSettingsLine(pw))
		return err
	})
	if err != nil {
		return fmt.Errorf("set the VNC password: %w", redact(err, pw))
	}
	return nil
}

// SetRunPassword sets pw, the run's VNC password from Init, in the guest (SetPassword) and holds it.
// The password is held in the agent's memory and in the guest's Screen Sharing settings file, which
// lives on the run's clone disk and goes with it; the agent never logs it or writes it anywhere else.
// It is called once, at Init. On failure the run has no desktop password (Password reports none) and
// the error holds none.
func (d *Desktop) SetRunPassword(_ context.Context, pw string) error {
	if err := d.SetPassword(pw); err != nil {
		return err
	}
	d.mu.Lock()
	d.password = pw
	d.mu.Unlock()
	return nil
}

// redact removes secret from err's text.
func redact(err error, secret string) error {
	if secret == "" || !strings.Contains(err.Error(), secret) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), secret, "***"))
}

// Password is this run's VNC password, if SetRunPassword succeeded.
func (d *Desktop) Password() (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.password, d.password != ""
}

// pointerScript prints the pointer position as "x y".
const pointerScript = `ObjC.import('AppKit'); var p = $.NSEvent.mouseLocation; Math.round(p.x) + ' ' + Math.round(p.y)`

// asUser is the command line that runs argv in the console user's GUI session as that user. A root
// daemon outside the session reads the wrong pointer, so launchctl moves the command into the
// session's bootstrap namespace and sudo drops it to the user's own identity.
func asUser(uid uint32, argv ...string) (string, []string) {
	// sudo takes a numeric uid as #uid, so the account's name is never looked up or quoted.
	args := []string{"asuser", strconv.FormatUint(uint64(uid), 10), "/usr/bin/sudo", "-u", "#" + strconv.FormatUint(uint64(uid), 10)}
	return "/bin/launchctl", append(args, argv...)
}

// PointerCommand is the command that reads the pointer in the console user's session.
func PointerCommand(uid uint32) (string, []string) {
	return asUser(uid, "/usr/bin/osascript", "-l", "JavaScript", "-e", pointerScript)
}

// Pointer reads the pointer position in the console user's session, for the prepare control check: the
// helper's own RFB client moves the pointer to a set point and compares. That osascript needs no
// permission for this is unproven (MAC-PROOF P9).
func (d *Desktop) Pointer(ctx context.Context) (Pointer, error) {
	uid, err := d.Console.Console(ctx)
	if err != nil {
		return Pointer{}, err
	}
	cmd, args := PointerCommand(uid)
	out, err := d.Run.Run(ctx, cmd, args...)
	if err != nil {
		return Pointer{}, fmt.Errorf("read the pointer: %w", err)
	}
	return parsePointer(string(out))
}

func parsePointer(s string) (Pointer, error) {
	f := strings.Fields(s)
	if len(f) != 2 {
		return Pointer{}, fmt.Errorf("the pointer read answered %q", strings.TrimSpace(s))
	}
	var v [2]float64
	for i := range v {
		n, err := strconv.ParseFloat(f[i], 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || math.Abs(n) > 1e6 {
			return Pointer{}, fmt.Errorf("the pointer read answered %q", strings.TrimSpace(s))
		}
		v[i] = n
	}
	return Pointer{X: int(math.Round(v[0])), Y: int(math.Round(v[1]))}, nil
}

// SetSessionEnv sets env (KEY=VALUE) in the console user's GUI session, so apps started from it after
// this see it. A session that is not up yet (ErrNoConsoleUser) is skipped, not an error: the caller
// logs it. That a launched app reads the variables is unproven (MAC-PROOF P11).
func (d *Desktop) SetSessionEnv(ctx context.Context, env []string) error {
	if len(env) == 0 {
		return nil
	}
	uid, err := d.Console.Console(ctx)
	if err != nil {
		return err
	}
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("session variable %q is not KEY=VALUE", k)
		}
		// launchctl asuser keeps root's identity in the user's namespace, which is what setenv needs.
		args := []string{"asuser", strconv.FormatUint(uint64(uid), 10), "/bin/launchctl", "setenv", k, v}
		if _, err := d.Run.Run(ctx, "/bin/launchctl", args...); err != nil {
			return fmt.Errorf("set %s in the desktop session: %w", k, err)
		}
	}
	return nil
}

// Register adds the desktop's request to mux:
//
//	GET /v1/desktop/pointer      Pointer, read in the console user's session
//
// The password is never served back: the host's helper made it and logs in with it itself.
func (d *Desktop) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/desktop/pointer", d.servePointer)
}

// servePointer answers the pointer position (Pointer); 409 with nobody at the console.
func (d *Desktop) servePointer(w http.ResponseWriter, r *http.Request) {
	p, err := d.Pointer(r.Context())
	if err != nil {
		slog.Warn("guest agent: pointer read", "err", err)
		status := http.StatusInternalServerError
		if errors.Is(err, ErrNoConsoleUser) {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeJSONAny(w, p)
}

func writeJSONAny(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
