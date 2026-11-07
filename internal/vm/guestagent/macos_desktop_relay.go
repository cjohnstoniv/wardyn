// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/rfbauth"
)

// The console view. macOS gives a legacy VncAuth connection its own login window, so in a run the agent
// signs in to Screen Sharing itself, as the agent account with Apple authentication, inside the guest
// over loopback, and shows the host's helper that session. Toward the helper it is the VncAuth server it
// always saw, with the run password and Apple's own ServerInit, so the helper's login, its view-only
// filter, recording and clipboard rules are unchanged, and an older helper works with it. It is on by
// default with no switch: wherever it cannot sign in (no run password, the agent not at the console, no
// password to read, Apple authentication absent or refused) it writes nothing to the helper, and the raw
// splice, today's view, runs on a fresh dial.

const (
	// viewHandshakeWait bounds the helper's side of the handshake, as Apple's side is bounded by
	// rfbauth's 15 s.
	viewHandshakeWait = 15 * time.Second
	// screenSharingGroup is Screen Sharing's "only these users" access list; absent, every user may sign in.
	screenSharingGroup = "com.apple.access_screensharing"

	viewNotSignedIn        = "not_signed_in"
	viewPasswordUnreadable = "password_unreadable"
	viewUnverified         = "screen_sharing_unverified"
	viewSignInFailed       = "sign_in_failed"
)

// errNotRootsScreenSharing refuses a Screen Sharing connection whose server end root does not hold alone.
var errNotRootsScreenSharing = errors.New("guestagent: Screen Sharing's end of the connection is not root's alone")

// ServeConsole serves the open desktop tunnel hc with the agent account's console session, and says
// whether it did. It reports false, having written nothing to hc, when the run has no desktop password
// (tools mode: prepare's control check and the base's edit window keep the raw splice), the agent is not
// at the console, its password is unreadable, or Screen Sharing declines Apple authentication; a decline
// is kept for the boot (rfbauth.ARDClient). A wrong VncAuth answer from the host is handled: refused,
// both sides closed, no fallback. The agent's password is read per session from kcpassword, sent only to
// Screen Sharing over loopback, and cleared; it is never logged, written or sent to the host.
func (d *Desktop) ServeConsole(ctx context.Context, hc net.Conn, local DialFunc) bool {
	runPW, ok := d.Password()
	if !ok {
		return false
	}
	if uid, err := d.Console.Console(ctx); err != nil || d.AgentUID == 0 || uid != d.AgentUID {
		d.viewFallback(viewNotSignedIn, "", err)
		return false
	}
	pw, err := readKCPassword(d.KCPassword)
	if err != nil {
		d.viewFallback(viewPasswordUnreadable, "", err)
		return false
	}
	apple, si, err := d.ardClient(local).Open(ctx, MacAgentUser, pw)
	clear(pw)
	if err != nil {
		var declined *rfbauth.DeclinedError
		switch {
		case errors.As(err, &declined) && declined.Reason == rfbauth.ReasonNoAppleLogin:
			d.viewFallback(declined.Reason, " offered="+offeredList(declined.Offered), err)
		case errors.As(err, &declined):
			d.viewFallback(declined.Reason, "", err)
		case errors.Is(err, rfbauth.ErrCredentialTooLong):
			d.viewFallback(viewPasswordUnreadable, "", err)
		case errors.Is(err, errNotRootsScreenSharing):
			d.viewFallback(viewUnverified, "", err)
		default:
			d.viewFallback(viewSignInFailed, "", err)
		}
		return false
	}
	defer apple.Close()
	release, ok := d.Track(pair{hc, apple})
	if !ok {
		return true // turned off meanwhile: the caller closes hc
	}
	defer release()
	cut := time.AfterFunc(viewHandshakeWait, func() { _ = hc.Close(); _ = apple.Close() })
	err = presentVNCAuth(hc, runPW, si, func() {
		d.writeViewStatus("view=console")
		slog.Info("guest agent: desktop view", "view", "console")
	})
	if !cut.Stop() && err == nil {
		err = errors.New("the handshake took too long")
	}
	if err != nil {
		slog.Warn("guest agent: desktop view refused: the host's VncAuth", "err", err)
		return true
	}
	copyBoth(hc, apple)
	return true
}

// presentVNCAuth is the server side of RFB 3.8 with VncAuth toward the host's helper: c's answer to a
// fresh challenge must be password's, compared in constant time, and then ServerInit is si. A wrong
// answer gets SecurityResult 1 with a reason; a right one calls verified before SecurityResult 0.
func presentVNCAuth(c io.ReadWriter, password string, si []byte, verified func()) error {
	if _, err := io.WriteString(c, "RFB 003.008\n"); err != nil {
		return err
	}
	v := make([]byte, 12)
	if _, err := io.ReadFull(c, v); err != nil {
		return err
	}
	if string(v) != "RFB 003.008\n" {
		return fmt.Errorf("the host chose %q, not RFB 3.8", v)
	}
	if _, err := c.Write([]byte{1, 2}); err != nil { // one security type: VncAuth
		return err
	}
	t := make([]byte, 1)
	if _, err := io.ReadFull(c, t); err != nil {
		return err
	}
	if t[0] != 2 {
		return fmt.Errorf("the host chose security type %d, not VncAuth", t[0])
	}
	challenge := make([]byte, 16)
	if _, err := rand.Read(challenge); err != nil {
		return err
	}
	if _, err := c.Write(challenge); err != nil {
		return err
	}
	resp := make([]byte, 16)
	if _, err := io.ReadFull(c, resp); err != nil {
		return err
	}
	want, err := rfbauth.VNCAuthResponse(password, challenge)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(resp, want) != 1 {
		const why = "authentication failed"
		_, _ = c.Write(append([]byte{0, 0, 0, 1, 0, 0, 0, byte(len(why))}, why...))
		return errors.New("the answer is not the run password's")
	}
	verified()
	if _, err := c.Write([]byte{0, 0, 0, 0}); err != nil {
		return err
	}
	if _, err := io.ReadFull(c, t); err != nil { // ClientInit: Apple's side already chose shared
		return err
	}
	_, err = c.Write(si)
	return err
}

// ardClient is the boot's one Apple authentication client, so a decline is kept for the boot.
func (d *Desktop) ardClient(local DialFunc) *rfbauth.ARDClient {
	d.ardOnce.Do(func() {
		d.ard = &rfbauth.ARDClient{Dial: func(ctx context.Context) (net.Conn, error) { return d.dialScreenSharing(ctx, local) }}
	})
	return d.ard
}

// dialScreenSharing dials local and refuses the connection unless root alone holds its server end, so the
// agent's password never goes to a process that bound 127.0.0.1:5900 itself. That does not rest on the
// kernel's port-reuse rules: it checks the very connection the password will go on, after Screen Sharing
// sent its version (so it has accepted it) and before anything is sent. The version is replayed to the
// caller.
func (d *Desktop) dialScreenSharing(ctx context.Context, local DialFunc) (net.Conn, error) {
	c, err := local(ctx)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	v := make([]byte, 12)
	_, err = io.ReadFull(c, v)
	if err == nil {
		check := d.PeerCheck
		if check == nil {
			check = func(ctx context.Context, c net.Conn) error { return lsofRootHolds(ctx, d.Run, c) }
		}
		if cerr := check(ctx, c); cerr != nil {
			err = fmt.Errorf("%w: %v", errNotRootsScreenSharing, cerr)
		}
	}
	if !stop() && err == nil {
		err = ctx.Err()
	}
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	return &replayConn{Conn: c, r: io.MultiReader(bytes.NewReader(v), c)}, nil
}

// replayConn reads r, then the connection.
type replayConn struct {
	net.Conn
	r io.Reader
}

func (c *replayConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// lsofBound is how long the peer check may take inside Apple authentication's own budget: lsof lists every
// process's files, which a loaded guest can slow past it.
var lsofBound = 5 * time.Second

// lsofRootHolds asks lsof which processes hold the far end of c, a loopback TCP connection, and refuses
// unless there is one and every one is root's.
func lsofRootHolds(ctx context.Context, run CmdRunner, c net.Conn) error {
	local, ok := c.LocalAddr().(*net.TCPAddr)
	remote, rok := c.RemoteAddr().(*net.TCPAddr)
	if !ok || !rok {
		return errors.New("not a TCP connection")
	}
	lctx, cancel := context.WithTimeout(ctx, lsofBound)
	defer cancel()
	out, err := run.Run(lctx, "/usr/sbin/lsof", "-nP", "-iTCP@"+local.String(), "-Fpun")
	if err != nil {
		if ctx.Err() == nil && lctx.Err() != nil {
			return fmt.Errorf("lsof took longer than %s", lsofBound)
		}
		return fmt.Errorf("lsof: %w", err)
	}
	return rootHoldsEnd(out, remote.String()+"->"+local.String())
}

// rootHoldsEnd reads lsof -F p, u and n lines and refuses unless a file named name exists and every
// process holding one is uid 0.
func rootHoldsEnd(out []byte, name string) error {
	uid, held := "", 0
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}
		switch line[0] {
		case 'p':
			uid = ""
		case 'u':
			uid = line[1:]
		case 'n':
			if n, _, _ := strings.Cut(line[1:], " "); n != name {
				continue
			}
			if uid != "0" {
				return fmt.Errorf("uid %q holds Screen Sharing's end", uid)
			}
			held++
		}
	}
	if held == 0 {
		return errors.New("no process holds Screen Sharing's end")
	}
	return nil
}

// viewFallback records that this session shows Screen Sharing's login window, and why.
func (d *Desktop) viewFallback(reason, more string, err error) {
	d.writeViewStatus("view=screen_sharing_login reason=" + reason + more)
	args := []any{"view", "screen_sharing_login", "reason", reason + more}
	if err != nil {
		args = append(args, "err", err.Error())
	}
	slog.Warn("guest agent: desktop view", args...)
}

// writeViewStatus replaces the status file with line, root's at 0644.
func (d *Desktop) writeViewStatus(line string) {
	path := d.ViewStatus
	if path == "" {
		path = MacDesktopViewStatusPath
	}
	err := makeDirAll(filepath.Dir(path), 0o755, nil)
	if err == nil {
		err = replaceFile(path, 0o644, func(f *os.File) error {
			_, err := f.WriteString(line + "\n")
			return err
		})
	}
	if err != nil {
		slog.Warn("guest agent: desktop view status not written", "err", err)
	}
}

func offeredList(types []byte) string {
	s := make([]string, len(types))
	for i, t := range types {
		s[i] = strconv.Itoa(int(t))
	}
	return "[" + strings.Join(s, ",") + "]"
}

// AllowScreenSharing adds user to Screen Sharing's access list when the base has one ("only these
// users"), so the console view can sign in as it. With no list every user may, and nothing runs. A
// failure is logged and never fails the Init; it changes the run's clone only.
func (d *Desktop) AllowScreenSharing(ctx context.Context, user string) {
	if _, err := d.Run.Run(ctx, "/usr/bin/dscl", ".", "-read", "/Groups/"+screenSharingGroup); err != nil {
		return
	}
	if _, err := d.Run.Run(ctx, "/usr/sbin/dseditgroup", "-o", "edit", "-a", user, "-t", "user", screenSharingGroup); err != nil {
		slog.Warn("guest agent: Screen Sharing's access list left as it was", "err", err)
	}
}
