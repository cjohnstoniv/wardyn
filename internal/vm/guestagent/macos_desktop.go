// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// The macOS desktop is the guest's own Screen Sharing. The agent owns three things about it: the
// per-run VNC password, the pointer-position read the prepare control check uses, and the desktop
// session's environment.

// Kickstart is Apple's remote-management tool.
const kickstart = "/System/Library/CoreServices/RemoteManagement/ARDAgent.app/Contents/Resources/kickstart"

// vncPasswordLen is VncAuth's key size: it uses at most eight characters (RFC 6143 7.2.2).
const vncPasswordLen = 8

// vncAlphabet is printable and needs no quoting anywhere it travels. Eight characters of it are about
// 47 bits, which is enough for what the password is: a lock on a door only the host's helper can
// reach over vsock, not the security boundary.
const vncAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"

// NewVNCPassword is vncPasswordLen random characters of vncAlphabet.
func NewVNCPassword() (string, error) {
	max := big.NewInt(int64(len(vncAlphabet)))
	b := make([]byte, vncPasswordLen)
	for i := range b {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		b[i] = vncAlphabet[n.Int64()]
	}
	return string(b), nil
}

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
	// NewPassword makes a run's password; nil is NewVNCPassword. A seam for tests.
	NewPassword func() (string, error)

	mu       sync.Mutex
	password string
	disabled bool
	open     map[io.Closer]struct{}
}

// Disable turns the run's desktop off: no password is set, the credential request answers 404, the
// tunnel refuses, and a tunnel already open is closed. Called at Init for a run whose sign-ins cannot
// be kept (SignInsOff), which has no desktop on macOS; the desktop is on until then, because
// prepare's control check needs it.
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

// KickstartArgs is the command line that sets Screen Sharing's legacy VNC password. That the setting
// exists on a macOS 27 guest, and that root can change it per run, are unproven (MAC-PROOF P9).
func KickstartArgs(password string) []string {
	return []string{"-configure", "-clientopts", "-setvnclegacy", "-vnclegacy", "yes", "-setvncpw", "-vncpw", password}
}

// SetRunPassword makes this run's VNC password and sets it in the guest. The password is held in the
// agent's memory and in the guest's own Screen Sharing setting, which lives on the run's clone disk
// and goes with it; the agent never logs it or writes it anywhere else. It is called once, at Init.
// On failure the run has no desktop password (Password reports none) and the error holds none.
func (d *Desktop) SetRunPassword(ctx context.Context) error {
	gen := d.NewPassword
	if gen == nil {
		gen = NewVNCPassword
	}
	pw, err := gen()
	if err != nil {
		return err
	}
	if _, err := d.Run.Run(ctx, kickstart, KickstartArgs(pw)...); err != nil {
		return fmt.Errorf("set the VNC password: %w", redact(err, pw))
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

// Register adds the desktop's two requests to mux:
//
//	GET /v1/desktop/credential   the run's VNC password, for the helper's RFB shim; 404 without one
//	GET /v1/desktop/pointer      Pointer, read in the console user's session
//
// The agent's only client is the host's helper over vsock; the password is never logged.
func (d *Desktop) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/desktop/credential", func(w http.ResponseWriter, _ *http.Request) {
		pw, ok := d.Password()
		if !ok {
			http.Error(w, "this run has no desktop password", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(DesktopCredential{Password: pw})
	})
	mux.HandleFunc("GET /v1/desktop/pointer", func(w http.ResponseWriter, r *http.Request) {
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
	})
}

// DesktopCredential is GET /v1/desktop/credential's answer.
type DesktopCredential struct {
	Password string `json:"password"`
}

func writeJSONAny(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
