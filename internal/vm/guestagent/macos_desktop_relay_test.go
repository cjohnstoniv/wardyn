// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/rfbauth"
	"github.com/cjohnstoniv/wardyn/internal/rfbauth/rfbauthtest"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

const (
	relayRunPW   = "run-pw12" // the run's VNC password, eight bytes
	relayAgentPW = "agent-login-pw"
	relayAgent   = uint32(501)
)

type relayConsole struct {
	uid uint32
	err error
}

func (c relayConsole) Console(context.Context) (uint32, error) { return c.uid, c.err }

// relayRunner records commands; fail names the commands that fail, and answer gives a command's output.
type relayRunner struct {
	mu     sync.Mutex
	calls  [][]string
	fail   map[string]bool
	answer func(name string, args []string) ([]byte, error)
}

func (r *relayRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string{name}, args...))
	r.mu.Unlock()
	if r.answer != nil {
		return r.answer(name, args)
	}
	if r.fail[name] {
		return nil, errors.New("exit status 1")
	}
	return nil, nil
}

func (r *relayRunner) ran(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.ContainsFunc(r.calls, func(c []string) bool { return c[0] == name })
}

// relayKC writes the kcpassword file for pw the way stage 1 does: XORed, NUL-ended, padded to 12 bytes.
func relayKC(t *testing.T, dir, pw string) string {
	t.Helper()
	b := append([]byte(pw), 0)
	for len(b)%12 != 0 {
		b = append(b, 0)
	}
	guestagent.XORKCPassword(b)
	p := filepath.Join(dir, "kcpassword")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func trustPeer(context.Context, net.Conn) error { return nil }

// relayDesktop is a run's desktop: the run password set, the agent at the console, its login password in
// kcpassword. It returns the status file's path.
func relayDesktop(t *testing.T, agentPW string) (*guestagent.Desktop, string) {
	t.Helper()
	dir := t.TempDir()
	d := &guestagent.Desktop{
		Run:         &relayRunner{},
		Console:     relayConsole{uid: relayAgent},
		VNCSettings: filepath.Join(dir, "vnc"),
		AgentUID:    relayAgent,
		KCPassword:  relayKC(t, dir, agentPW),
		ViewStatus:  filepath.Join(dir, "wardyn", "desktop-view.status"),
		PeerCheck:   trustPeer,
	}
	if err := d.SetRunPassword(context.Background(), relayRunPW); err != nil {
		t.Fatal(err)
	}
	return d, d.ViewStatus
}

func appleServer() *rfbauthtest.Server {
	return &rfbauthtest.Server{User: guestagent.MacAgentUser, Password: relayAgentPW, VNCPassword: relayRunPW}
}

// readStatus reads the status file, waiting for it: the relay writes it as the host's handshake ends.
func readStatus(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	for deadline := time.Now().Add(5 * time.Second); err != nil && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
		b, err = os.ReadFile(path)
	}
	if err != nil {
		t.Fatalf("status file: %v", err)
	}
	return strings.TrimSpace(string(b))
}

// hostBroker hands the test the host's end of each desktop tunnel the agent dials.
type hostBroker chan net.Conn

func (b hostBroker) dial(ctx context.Context) (net.Conn, error) {
	near, far := net.Pipe()
	select {
	case b <- far:
		return near, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (b hostBroker) next(t *testing.T) net.Conn {
	t.Helper()
	select {
	case c := <-b:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("the agent did not dial the host")
		return nil
	}
}

// hostLogin is the helper's side: VncAuth with pw as an RFB 3.8 client. It returns SecurityResult and,
// on 0, the ServerInit. Every byte it reads from the agent is copied to seen when set.
func hostLogin(c net.Conn, pw string, seen io.Writer) (uint32, []byte, error) {
	var r io.Reader = c
	if seen != nil {
		r = io.TeeReader(c, seen)
	}
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	v := make([]byte, 12)
	if _, err := io.ReadFull(r, v); err != nil {
		return 0, nil, fmt.Errorf("version: %w", err)
	}
	if !strings.HasPrefix(string(v), "RFB 003.") {
		return 0, nil, fmt.Errorf("version %q", v)
	}
	if _, err := io.WriteString(c, "RFB 003.008\n"); err != nil {
		return 0, nil, err
	}
	n := make([]byte, 1)
	if _, err := io.ReadFull(r, n); err != nil {
		return 0, nil, err
	}
	types := make([]byte, n[0])
	if _, err := io.ReadFull(r, types); err != nil {
		return 0, nil, err
	}
	if !slices.Contains(types, 2) {
		return 0, nil, fmt.Errorf("no VncAuth in %v", types)
	}
	if _, err := c.Write([]byte{2}); err != nil {
		return 0, nil, err
	}
	ch := make([]byte, 16)
	if _, err := io.ReadFull(r, ch); err != nil {
		return 0, nil, err
	}
	resp, err := rfbauth.VNCAuthResponse(pw, ch)
	if err != nil {
		return 0, nil, err
	}
	if _, err := c.Write(resp); err != nil {
		return 0, nil, err
	}
	res := make([]byte, 4)
	if _, err := io.ReadFull(r, res); err != nil {
		return 0, nil, err
	}
	if code := binary.BigEndian.Uint32(res); code != 0 {
		return code, nil, nil
	}
	if _, err := c.Write([]byte{1}); err != nil {
		return 0, nil, err
	}
	si := make([]byte, 24)
	if _, err := io.ReadFull(r, si); err != nil {
		return 0, nil, err
	}
	si = append(si, make([]byte, binary.BigEndian.Uint32(si[20:]))...)
	if _, err := io.ReadFull(r, si[24:]); err != nil {
		return 0, nil, err
	}
	return 0, si, nil
}

// serveOne runs one tunnel through SpliceDesktop with the relay and returns the host's end, past the open byte.
func serveOne(t *testing.T, d *guestagent.Desktop, local guestagent.DialFunc) net.Conn {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	hb := make(hostBroker)
	go func() { _ = guestagent.SpliceDesktop(ctx, hb.dial, local, d.Track, d.ServeConsole) }()
	host := hb.next(t)
	t.Cleanup(func() { _ = host.Close() })
	if _, err := host.Write([]byte{guestagent.DesktopOpenByte}); err != nil {
		t.Fatal(err)
	}
	return host
}

// The real helper shim, the relay and Apple's Screen Sharing in a chain: the shim's VncAuth with the run
// password succeeds, the viewer gets Apple's ServerInit and its updates, and a key from the viewer never
// reaches Apple.
func TestConsoleRelayShowsTheConsoleThroughTheRealShim(t *testing.T) {
	d, status := relayDesktop(t, relayAgentPW)
	got := make(chan byte, 1)
	srv := appleServer()
	srv.Session = func(c net.Conn) {
		defer c.Close()
		first := make([]byte, 1)
		if _, err := io.ReadFull(c, first); err != nil {
			return
		}
		got <- first[0]
		if first[0] == 3 { // FramebufferUpdateRequest
			_, _ = io.ReadFull(c, make([]byte, 9))
			_, _ = c.Write([]byte{0, 0, 0, 0}) // FramebufferUpdate, no rectangles
		}
		_, _ = io.Copy(io.Discard, c)
	}
	host := serveOne(t, d, srv.Dial)
	viewer, shimSide := net.Pipe()
	defer viewer.Close()
	shim := &macvm.RFBShim{ViewOnly: true, Credential: func(context.Context) (string, error) { return relayRunPW, nil },
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	go shim.ServeDesktop(shimSide, host)

	_ = viewer.SetDeadline(time.Now().Add(20 * time.Second))
	vr := bufio.NewReader(viewer)
	step := func(read int, write []byte) []byte {
		t.Helper()
		b := make([]byte, read)
		if _, err := io.ReadFull(vr, b); err != nil {
			t.Fatalf("viewer handshake: %v", err)
		}
		if write != nil {
			if _, err := viewer.Write(write); err != nil {
				t.Fatal(err)
			}
		}
		return b
	}
	step(12, []byte("RFB 003.008\n"))
	step(2, []byte{1})
	step(4, []byte{1})
	si := step(24, nil)
	si = append(si, step(int(binary.BigEndian.Uint32(si[20:])), nil)...)
	if !bytes.Equal(si, rfbauthtest.ServerInit()) {
		t.Fatalf("the viewer got ServerInit %x, want Apple's %x", si, rfbauthtest.ServerInit())
	}
	key := []byte{4, 1, 0, 0, 0, 0, 0, 'a'}
	req := []byte{3, 1, 0, 0, 0, 0, 0x07, 0x80, 0x04, 0x38}
	if _, err := viewer.Write(append(key, req...)); err != nil {
		t.Fatal(err)
	}
	select {
	case b := <-got:
		if b != 3 {
			t.Fatalf("Apple's first message from the viewer is type %d, want the update request (3); a key reached it", b)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no message reached Apple's server")
	}
	if upd := step(4, nil); upd[0] != 0 {
		t.Errorf("the viewer got message type %d, want a FramebufferUpdate", upd[0])
	}
	if srv.ARDSignIns() != 1 {
		t.Errorf("Apple sign-ins = %d, want 1", srv.ARDSignIns())
	}
	if s := readStatus(t, status); s != "view=console" {
		t.Errorf("status %q, want view=console", s)
	}
}

// No Apple authentication offered, or the account refused: the relay dials again and splices raw, so the
// helper's VncAuth reaches Screen Sharing itself; the status says why. A refusal is kept for the boot: the
// next session signs in to nothing.
func TestConsoleRelayFallsBackWhenAppleLoginIsAbsentOrRefused(t *testing.T) {
	for _, tc := range []struct {
		name, agentPW string
		offered       []byte
		want          string
	}{
		{"no apple login", relayAgentPW, []byte{2}, "view=screen_sharing_login reason=no_apple_login offered=[2]"},
		{"refused", "not-the-password", nil, "view=screen_sharing_login reason=refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, status := relayDesktop(t, tc.agentPW)
			srv := appleServer()
			srv.Offered = tc.offered
			host := serveOne(t, d, srv.Dial)
			code, si, err := hostLogin(host, relayRunPW, nil)
			if err != nil || code != 0 {
				t.Fatalf("the helper's login through the fallback: code %d, %v", code, err)
			}
			if !bytes.Equal(si, rfbauthtest.ServerInit()) {
				t.Errorf("ServerInit %x", si)
			}
			if srv.Conns() != 2 {
				t.Errorf("Screen Sharing took %d connections, want 2 (the sign-in, then the raw splice)", srv.Conns())
			}
			if s := readStatus(t, status); s != tc.want {
				t.Errorf("status %q, want %q", s, tc.want)
			}
			signIns, conns := srv.ARDSignIns(), srv.Conns()
			hc, _ := net.Pipe()
			defer hc.Close()
			if d.ServeConsole(context.Background(), hc, srv.Dial) {
				t.Fatal("the second session was handled after a decline")
			}
			if srv.ARDSignIns() != signIns || srv.Conns() != conns {
				t.Errorf("the second session reached Screen Sharing: sign-ins %d→%d, connections %d→%d",
					signIns, srv.ARDSignIns(), conns, srv.Conns())
			}
			if s := readStatus(t, status); s != tc.want {
				t.Errorf("second status %q, want the first reason %q", s, tc.want)
			}
		})
	}
}

// Tools mode never sets a run password: prepare's control check and the base's edit window keep the raw
// splice, and nothing signs in or writes a status.
func TestConsoleRelayOnlyWithARunPassword(t *testing.T) {
	dir := t.TempDir()
	d := &guestagent.Desktop{Run: &relayRunner{}, Console: relayConsole{uid: relayAgent}, AgentUID: relayAgent,
		KCPassword: relayKC(t, dir, relayAgentPW), ViewStatus: filepath.Join(dir, "desktop-view.status"), PeerCheck: trustPeer}
	srv := appleServer()
	hc, _ := net.Pipe()
	defer hc.Close()
	if d.ServeConsole(context.Background(), hc, srv.Dial) {
		t.Fatal("handled a session with no run password")
	}
	if srv.Conns() != 0 {
		t.Errorf("Screen Sharing took %d connections", srv.Conns())
	}
	if _, err := os.Stat(d.ViewStatus); err == nil {
		t.Error("a status was written in tools mode")
	}
}

// The login window, or another account at the console: no sign-in as the agent.
func TestConsoleRelayOnlyWhenTheAgentIsAtTheConsole(t *testing.T) {
	for _, c := range []relayConsole{{uid: 0}, {err: guestagent.ErrNoConsoleUser}, {uid: relayAgent + 1}} {
		d, status := relayDesktop(t, relayAgentPW)
		d.Console = c
		srv := appleServer()
		hc, _ := net.Pipe()
		if d.ServeConsole(context.Background(), hc, srv.Dial) {
			t.Errorf("console %+v: handled", c)
		}
		_ = hc.Close()
		if srv.Conns() != 0 {
			t.Errorf("console %+v: Screen Sharing took %d connections", c, srv.Conns())
		}
		if s := readStatus(t, status); s != "view=screen_sharing_login reason=not_signed_in" {
			t.Errorf("console %+v: status %q", c, s)
		}
	}
}

// An unreadable kcpassword falls back and says so.
func TestConsoleRelayFallsBackWithoutTheAgentPassword(t *testing.T) {
	d, status := relayDesktop(t, relayAgentPW)
	d.KCPassword = filepath.Join(t.TempDir(), "absent")
	srv := appleServer()
	hc, _ := net.Pipe()
	defer hc.Close()
	if d.ServeConsole(context.Background(), hc, srv.Dial) || srv.Conns() != 0 {
		t.Fatalf("signed in without the agent's password (connections %d)", srv.Conns())
	}
	if s := readStatus(t, status); s != "view=screen_sharing_login reason=password_unreadable" {
		t.Errorf("status %q", s)
	}
}

// A wrong VncAuth answer from the host is a refusal, not a fallback: SecurityResult 1, no ServerInit, both
// sides closed, and no second connection to Screen Sharing.
func TestConsoleRelayRefusesAWrongHostResponse(t *testing.T) {
	d, _ := relayDesktop(t, relayAgentPW)
	closed := make(chan struct{})
	srv := appleServer()
	srv.Session = func(c net.Conn) {
		_, _ = io.Copy(io.Discard, c)
		close(closed)
	}
	host := serveOne(t, d, srv.Dial)
	code, si, err := hostLogin(host, "wrong-pw", nil)
	if err != nil || code != 1 || si != nil {
		t.Fatalf("wrong host password: code %d, ServerInit %x, err %v; want SecurityResult 1", code, si, err)
	}
	reason := make([]byte, 4)
	if _, err := io.ReadFull(host, reason); err == nil {
		_, _ = io.ReadFull(host, make([]byte, binary.BigEndian.Uint32(reason)))
	}
	_ = host.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := host.Read(make([]byte, 64)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("the host's side stayed open after the refusal (read %d, %v)", n, err)
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Error("Apple's side stayed open after the refusal")
	}
	if srv.Conns() != 1 {
		t.Errorf("Screen Sharing took %d connections, want 1 (no fallback)", srv.Conns())
	}
}

// The agent's password reaches Screen Sharing only: never a log line, the status file, or a byte to the host.
func TestConsoleRelayPasswordNeverLeaves(t *testing.T) {
	const canary = "Canary-7731-agent"
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })

	for _, apple := range []string{canary, "something-else"} { // console, then a refused fallback
		d, status := relayDesktop(t, canary)
		srv := appleServer()
		srv.Password = apple
		host := serveOne(t, d, srv.Dial)
		var seen bytes.Buffer
		if _, _, err := hostLogin(host, relayRunPW, &seen); err != nil {
			t.Fatal(err)
		}
		_ = host.Close()
		if strings.Contains(seen.String(), canary) {
			t.Errorf("the password reached the host (apple password %q)", apple)
		}
		if strings.Contains(readStatus(t, status), canary) {
			t.Error("the password is in the status file")
		}
	}
	if strings.Contains(logs.String(), canary) {
		t.Error("the password is in a log line")
	}
}

// Disable closes a console session already open, on both sides.
func TestDisableClosesAConsoleRelay(t *testing.T) {
	d, _ := relayDesktop(t, relayAgentPW)
	closed := make(chan struct{})
	srv := appleServer()
	srv.Session = func(c net.Conn) {
		_, _ = io.Copy(io.Discard, c)
		close(closed)
	}
	host := serveOne(t, d, srv.Dial)
	if code, _, err := hostLogin(host, relayRunPW, nil); err != nil || code != 0 {
		t.Fatalf("login: %d %v", code, err)
	}
	d.Disable()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Apple's side stayed open after Disable")
	}
	_ = host.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := host.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("the host's side stayed open after Disable: %v", err)
	}
}

// Screen Sharing's "only these users" group gets the agent account; with no group every user may sign in
// and nothing runs.
func TestScreenSharingAccessListAddsTheAgentOnlyWhenTheGroupExists(t *testing.T) {
	absent := &relayRunner{fail: map[string]bool{"/usr/bin/dscl": true}}
	(&guestagent.Desktop{Run: absent}).AllowScreenSharing(context.Background(), "agent")
	if absent.ran("/usr/sbin/dseditgroup") {
		t.Error("edited a group that does not exist")
	}
	present := &relayRunner{}
	(&guestagent.Desktop{Run: present}).AllowScreenSharing(context.Background(), "agent")
	want := []string{"/usr/sbin/dseditgroup", "-o", "edit", "-a", "agent", "-t", "user", "com.apple.access_screensharing"}
	if !slices.ContainsFunc(present.calls, func(c []string) bool { return slices.Equal(c, want) }) {
		t.Errorf("calls %q, want %q", present.calls, want)
	}
	failing := &relayRunner{fail: map[string]bool{"/usr/sbin/dseditgroup": true}}
	(&guestagent.Desktop{Run: failing}).AllowScreenSharing(context.Background(), "agent") // logged, never fatal
}

// The credentials go only to a Screen Sharing whose end of the connection root holds: a process that
// bound 127.0.0.1:5900 itself never gets the agent's password.
func TestConsoleRelaySignsInOnlyWhereRootHoldsScreenSharingsEnd(t *testing.T) {
	for _, tc := range []struct {
		name    string
		holders []int // uids lsof names for Screen Sharing's end; nil is lsof failing
		signIn  bool
	}{
		{"root", []int{0}, true},
		{"the agent", []int{int(relayAgent)}, false},
		{"root and another", []int{0, int(relayAgent)}, false},
		{"lsof fails", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := appleServer()
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			go func() {
				for {
					c, err := l.Accept()
					if err != nil {
						return
					}
					s, _ := srv.Dial(context.Background())
					go func() { _, _ = io.Copy(s, c); _ = s.Close() }()
					go func() { _, _ = io.Copy(c, s); _ = c.Close() }()
				}
			}()
			listen := l.Addr().String()
			run := &relayRunner{answer: func(name string, args []string) ([]byte, error) {
				if name != "/usr/sbin/lsof" || tc.holders == nil {
					return nil, errors.New("exit status 1")
				}
				var at string
				for _, a := range args {
					if v, ok := strings.CutPrefix(a, "-iTCP@"); ok {
						at = v
					}
				}
				var b strings.Builder
				fmt.Fprintf(&b, "p%d\nu0\nf7\nn%s->%s\n", 900, at, listen) // the agent's own end
				for i, uid := range tc.holders {
					fmt.Fprintf(&b, "p%d\nu%d\nf4\nn%s->%s\n", 100+i, uid, listen, at)
				}
				return []byte(b.String()), nil
			}}
			d, status := relayDesktop(t, relayAgentPW)
			d.Run, d.PeerCheck = run, nil
			dial := func(ctx context.Context) (net.Conn, error) {
				var nd net.Dialer
				return nd.DialContext(ctx, "tcp", listen)
			}
			host := serveOne(t, d, dial)
			code, _, err := hostLogin(host, relayRunPW, nil)
			if err != nil || code != 0 {
				t.Fatalf("login: %d %v", code, err)
			}
			if got := srv.ARDSignIns() == 1; got != tc.signIn {
				t.Errorf("signed in = %v, want %v", got, tc.signIn)
			}
			if !run.ran("/usr/sbin/lsof") {
				t.Error("nobody checked who holds Screen Sharing's end")
			}
			want := "view=console"
			if !tc.signIn {
				want = "view=screen_sharing_login reason=screen_sharing_unverified"
			}
			if s := readStatus(t, status); s != want {
				t.Errorf("status %q, want %q", s, want)
			}
		})
	}
}
