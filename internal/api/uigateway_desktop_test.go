// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// desktopFakeRunner is the harness runner plus a VM display: each dial is one end of a pipe whose
// other end is qemuDisplay(after).
type desktopFakeRunner struct {
	*sshFakeRunner
	dialled chan string
	after   func(net.Conn)
}

func (f *desktopFakeRunner) DialDesktop(_ context.Context, ref string) (io.ReadWriteCloser, error) {
	f.dialled <- ref
	a, b := net.Pipe()
	go func() {
		defer b.Close()
		qemuDisplay(b, f.after)
	}()
	return a, nil
}

// qemuDisplay is QEMU's RFB server in miniature: version 3.8, security None, ServerInit, then after
// (nil: it stays open, reading what the viewer sends).
func qemuDisplay(b net.Conn, after func(net.Conn)) {
	in := make([]byte, 12)
	for _, step := range []struct {
		out  []byte
		read int
	}{{[]byte("RFB 003.008\n"), 12}, {[]byte{1, 1}, 1}, {[]byte{0, 0, 0, 0}, 1}} {
		if _, err := b.Write(step.out); err != nil {
			return
		}
		if _, err := io.ReadFull(b, in[:step.read]); err != nil {
			return
		}
	}
	if _, err := b.Write(rfbServerInit()); err != nil {
		return
	}
	if after == nil {
		_, _ = io.Copy(io.Discard, b)
		return
	}
	after(b)
}

// desktopViewer is the viewer's end of the desktop socket, read as a byte stream.
type desktopViewer struct {
	t   *testing.T
	ctx context.Context
	ws  *websocket.Conn
	buf []byte
}

func (v *desktopViewer) read(n int) []byte {
	v.t.Helper()
	for len(v.buf) < n {
		_, msg, err := v.ws.Read(v.ctx)
		if err != nil {
			v.t.Fatalf("reading %d bytes from the desktop (have %q): %v", n, v.buf, err)
		}
		v.buf = append(v.buf, msg...)
	}
	out := v.buf[:n]
	v.buf = v.buf[n:]
	return out
}

func (v *desktopViewer) write(b []byte) {
	v.t.Helper()
	if err := v.ws.Write(v.ctx, websocket.MessageBinary, b); err != nil {
		v.t.Fatal(err)
	}
}

// handshake answers qemuDisplay as the viewer does, through to ServerInit.
func (v *desktopViewer) handshake() {
	v.t.Helper()
	if got := string(v.read(12)); got != "RFB 003.008\n" {
		v.t.Fatalf("greeting = %q", got)
	}
	v.write([]byte("RFB 003.008\n"))
	if got := v.read(2); got[0] != 1 || got[1] != 1 {
		v.t.Fatalf("security types = %v", got)
	}
	v.write([]byte{1})
	if got := v.read(4); string(got) != "\x00\x00\x00\x00" {
		v.t.Fatalf("security result = %v", got)
	}
	v.write([]byte{1})
	if got := v.read(len(rfbServerInit())); string(got) != string(rfbServerInit()) {
		v.t.Fatalf("ServerInit = %x", got)
	}
}

// dialDesktop opens the desktop socket with the session cookie c.
func dialDesktop(t *testing.T, ctx context.Context, h *uiHarness, srvURL string, c *http.Cookie) *desktopViewer {
	t.Helper()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srvURL, "http")+desktopPath(h, uiDesktopRFB),
		&websocket.DialOptions{HTTPHeader: http.Header{"Cookie": {c.Name + "=" + c.Value}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	return &desktopViewer{t: t, ctx: ctx, ws: ws}
}

const desktopTestViewer = `<!doctype html><html lang="en" data-wardyn-desktop=""><head><script type="module" src="./assets/desktop-1.js"></script></head><body></body></html>`

// newDesktopHarness is newUIHarness on a Linux VM run, with a display behind the runner and the
// console bundle's desktop.html in WARDYN_UI_DIR.
func newDesktopHarness(t *testing.T) (*uiHarness, *desktopFakeRunner) {
	t.Helper()
	h := newUIHarness(t, okBackend())
	h.run.MachineKind = types.MachineLinuxVM
	h.store.putRun(h.run)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, uiDesktopViewer), []byte(desktopTestViewer), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "assets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assets", "desktop-1.js"), []byte("// viewer"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.srv.cfg.UIDir = dir
	dr := &desktopFakeRunner{sshFakeRunner: h.runner, dialled: make(chan string, 8)}
	h.srv.cfg.Runner = dr
	h.srv.cfg.Store = &desktopClosedStore{uiMemStore: h.store, closed: map[string]time.Time{}}
	return h, dr
}

func (h *uiHarness) enterDesktop(ticket string) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.enter(url.Values{"run": {h.run.ID.String()}, "app": {uiDesktopAppName}, "ticket": {ticket}})
}

func desktopCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == uiCookieName {
			return c
		}
	}
	t.Fatalf("enter set no session cookie: %d %s", rec.Code, rec.Body.String())
	return nil
}

func desktopPath(h *uiHarness, rest string) string {
	return uiRelayPrefix(h.run.ID, uiDesktopAppName) + rest
}

// The desktop of a VM run opens on its own path for one hour, under the relay's 8 h default; the
// cookie is refused once that hour is up, and a container run has no desktop at all.
func TestDesktopEnterOpensAnHourLongSessionOnAVMRunOnly(t *testing.T) {
	h, _ := newDesktopHarness(t)
	rec := h.enterDesktop(h.ticket(h.run.ID, h.owner, oidc.RoleUser))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != desktopPath(h, "/") {
		t.Fatalf("enter = %d to %q: %s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	c := desktopCookie(t, rec)
	if c.Path != desktopPath(h, "/") {
		t.Fatalf("cookie path %q", c.Path)
	}
	if left := time.Until(c.Expires); left > time.Hour || left < 59*time.Minute {
		t.Fatalf("the desktop session lasts %v, want one hour", left)
	}
	if got := uiGet(h, desktopPath(h, "/"), c); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `&#34;state&#34;:&#34;open&#34;`) {
		t.Fatalf("viewer = %d %s", got.Code, got.Body.String())
	}
	h.clock.advance(time.Hour)
	if got := uiGet(h, desktopPath(h, "/"), c); got.Code != http.StatusForbidden {
		t.Fatalf("an hour later the viewer = %d, want 403", got.Code)
	}
	// The desktop's session is not an app's: the same cookie opens no declared app.
	if got := uiGet(h, uiRelayPrefix(h.run.ID, "code")+"/", c); got.Code != http.StatusForbidden {
		t.Fatalf("a desktop cookie on an app = %d, want 403", got.Code)
	}

	h.run.MachineKind = ""
	h.store.putRun(h.run)
	rec = h.enterDesktop(h.ticket(h.run.ID, h.owner, oidc.RoleUser))
	var body errorBody
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusForbidden || body.Error != uiNoDesktopMsg {
		t.Fatalf("a container run's desktop = %d %+v", rec.Code, body)
	}
}

// The desktop door is shut by either limit a person's run can carry: deny_interactive, which shuts
// the terminal, and deny_ui_apps, which shuts the UI apps.
func TestDesktopDoorObeysDenyInteractiveAndDenyUIApps(t *testing.T) {
	for name, limits := range map[string]types.GovernanceLimits{
		"deny_interactive": {DenyInteractive: true},
		"deny_ui_apps":     {DenyUIApps: true},
		"neither":          {},
	} {
		t.Run(name, func(t *testing.T) {
			h, _ := newDesktopHarness(t)
			p := types.GovernanceProfile{ID: uuid.New(), Name: "p", Limits: limits}
			h.store.profiles = []types.GovernanceProfile{p}
			h.run.GovernanceProfileID = &p.ID
			h.store.putRun(h.run)
			rec := h.enterDesktop(h.ticket(h.run.ID, h.owner, oidc.RoleUser))
			want := http.StatusForbidden
			if name == "neither" {
				want = http.StatusFound
			}
			if rec.Code != want {
				t.Fatalf("enter = %d, want %d: %s", rec.Code, want, rec.Body.String())
			}
		})
	}
}

// A desktop link works once: the second use, and a copied tab address in a browser with no session,
// get the viewer's refusal page with the gateway's own words, and its assets resolve under the run.
func TestDesktopLinkWorksOnce(t *testing.T) {
	h, _ := newDesktopHarness(t)
	ticket := h.ticket(h.run.ID, h.owner, oidc.RoleUser)
	if rec := h.enterDesktop(ticket); rec.Code != http.StatusFound {
		t.Fatalf("first enter = %d", rec.Code)
	}
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"the link again":      h.enterDesktop(ticket),
		"the tab, no session": uiGet(h, desktopPath(h, "/"), nil),
	} {
		body := rec.Body.String()
		if rec.Code != http.StatusForbidden || !strings.Contains(body, `&#34;state&#34;:&#34;refused&#34;`) {
			t.Fatalf("%s = %d %s", name, rec.Code, body)
		}
		if !strings.Contains(body, `src="`+desktopPath(h, "/assets/desktop-1.js")+`"`) {
			t.Fatalf("%s: assets not rooted under the run: %s", name, body)
		}
		if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Fatalf("%s: no CSP", name)
		}
	}
	if !strings.Contains(h.enterDesktop(ticket).Body.String(), htmlAttr(uiTicketInvalidMsg)) ||
		!strings.Contains(uiGet(h, desktopPath(h, "/"), nil).Body.String(), htmlAttr(uiNoSessionMsg)) {
		t.Fatal("a refusal page does not carry the gateway's words")
	}
	if rec := uiGet(h, desktopPath(h, "/assets/desktop-1.js"), nil); rec.Code != http.StatusOK || rec.Body.String() != "// viewer" {
		t.Fatalf("viewer asset = %d %q", rec.Code, rec.Body.String())
	}
	if rec := uiGet(h, desktopPath(h, "/assets/..%2fdesktop.html"), nil); rec.Code != http.StatusNotFound {
		t.Fatalf("an asset path climbing out = %d", rec.Code)
	}
}

func htmlAttr(s string) string { return strings.ReplaceAll(s, `"`, "&#34;") }

// The stream carries the display both ways through the runner, and Close ends the session: the open
// stream is told why and cut, and the cookie, wherever it was copied, opens nothing more.
func TestDesktopStreamCarriesTheDisplayAndCloseEndsTheSession(t *testing.T) {
	h, dr := newDesktopHarness(t)
	srv := httptest.NewServer(h.gateway)
	t.Cleanup(srv.Close)
	c := desktopCookie(t, h.enterDesktop(h.ticket(h.run.ID, h.owner, oidc.RoleUser)))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	v := dialDesktop(t, ctx, h, srv.URL, c)
	if ref := <-dr.dialled; ref != h.run.SandboxRef {
		t.Fatalf("dialled %q, want the run's sandbox", ref)
	}
	v.handshake() // both directions carried, through the clipboard filter

	req, _ := http.NewRequest(http.MethodPost, srv.URL+desktopPath(h, uiDesktopClose), nil)
	req.AddCookie(c)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("close = %v %v", resp, err)
	}
	_ = resp.Body.Close()
	_, _, err = v.ws.Read(ctx)
	var ce websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != desktopCloseSessionEnded {
		t.Fatalf("the open stream after Close = %v, want close %d", err, desktopCloseSessionEnded)
	}
	if got := uiGet(h, desktopPath(h, "/"), c); got.Code != http.StatusForbidden {
		t.Fatalf("the closed session's cookie = %d, want 403", got.Code)
	}
}

// A stream on a run that has stopped is told the run ended, and never reaches the runner.
func TestDesktopStreamOnAnEndedRunIsToldSo(t *testing.T) {
	h, dr := newDesktopHarness(t)
	srv := httptest.NewServer(h.gateway)
	t.Cleanup(srv.Close)
	c := desktopCookie(t, h.enterDesktop(h.ticket(h.run.ID, h.owner, oidc.RoleUser)))
	h.run.State = types.RunCompleted
	h.store.putRun(h.run)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+desktopPath(h, uiDesktopRFB),
		&websocket.DialOptions{HTTPHeader: http.Header{"Cookie": {c.Name + "=" + c.Value}}})
	if err != nil {
		// The relay's own re-check may refuse before the upgrade; either way nothing is dialled.
		if len(dr.dialled) != 0 {
			t.Fatal("an ended run's display was dialled")
		}
		return
	}
	defer ws.CloseNow()
	_, _, err = ws.Read(ctx)
	var ce websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != desktopCloseRunEnded {
		t.Fatalf("stream on an ended run = %v, want close %d", err, desktopCloseRunEnded)
	}
	if len(dr.dialled) != 0 {
		t.Fatal("an ended run's display was dialled")
	}
}

// The VM display rides CTL-A's clipboard filter: a ServerCutText from the VM's side is dropped when
// the run's desktop rules block copy out and passed when they allow it, a ClientCutText is passed or
// dropped by paste in, and each is audited by direction, size and hash, never by its text.
func TestDesktopRelayFiltersCutTextByTheRunsDesktopRules(t *testing.T) {
	keyEvent := []byte{4, 1, 0, 0, 0, 0, 0, 'a'}
	for _, tc := range []struct {
		name             string
		rules            *types.DesktopControls
		copyOut, pasteIn bool
	}{
		{"defaults block copy out and allow paste in", nil, false, true},
		{"copy out allowed, paste in blocked", &types.DesktopControls{CopyOut: types.TransferAllowed, PasteIn: types.TransferBlocked}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, dr := newDesktopHarness(t)
			if tc.rules != nil {
				h.store.putEffectivePolicy(h.run.ID, types.RunPolicySpec{MinConfinementClass: types.CC2, Desktop: tc.rules})
			}
			paste := rfbCut(6, pasteSentinel)
			reached := make(chan []byte, 1)
			dr.after = func(b net.Conn) {
				if _, err := b.Write(append(rfbCut(3, copySentinel), 2)); err != nil { // the copy, then a Bell
					return
				}
				want := len(keyEvent)
				if tc.pasteIn {
					want += len(paste)
				}
				got := make([]byte, want)
				if _, err := io.ReadFull(b, got); err == nil {
					reached <- got
				}
			}
			srv := httptest.NewServer(h.gateway)
			t.Cleanup(srv.Close)
			c := desktopCookie(t, h.enterDesktop(h.ticket(h.run.ID, h.owner, oidc.RoleUser)))
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			v := dialDesktop(t, ctx, h, srv.URL, c)
			<-dr.dialled
			v.handshake()
			next := []byte{2}
			if tc.copyOut {
				next = append(rfbCut(3, copySentinel), 2)
			}
			if got := v.read(len(next)); string(got) != string(next) {
				t.Fatalf("after ServerInit the viewer got %q, want %q", got, next)
			}
			v.write(append(paste, keyEvent...))
			want := keyEvent
			if tc.pasteIn {
				want = append(append([]byte(nil), paste...), keyEvent...)
			}
			select {
			case got := <-reached:
				if string(got) != string(want) {
					t.Fatalf("the VM got %q, want %q", got, want)
				}
			case <-ctx.Done():
				t.Fatal("the viewer's bytes never reached the VM")
			}
			outcome := map[bool]string{true: "success", false: "denied"}
			wantRows := map[string]struct {
				outcome   string
				bytes     int
				sum, text string
			}{
				"copy_out": {outcome[tc.copyOut], len(copySentinel), digest(copySentinel), copySentinel},
				"paste_in": {outcome[tc.pasteIn], len(pasteSentinel), digest(pasteSentinel), pasteSentinel},
			}
			rows := clipboardRows(t, h, 2)
			if len(rows) != 2 {
				t.Fatalf("ui.clipboard rows = %+v, want 2", rows)
			}
			for _, row := range rows {
				var d struct {
					App       string `json:"app"`
					Direction string `json:"direction"`
					Bytes     int    `json:"bytes"`
					SHA256    string `json:"sha256"`
				}
				_ = json.Unmarshal(row.Data, &d)
				w := wantRows[d.Direction]
				if row.Outcome != w.outcome || d.Bytes != w.bytes || d.SHA256 != w.sum || row.Target != d.Direction || d.App != uiDesktopAppName {
					t.Errorf("ui.clipboard row %s/%s %s, want %+v", row.Outcome, row.Target, row.Data, w)
				}
				if strings.Contains(string(row.Data), w.text) {
					t.Errorf("a ui.clipboard row carries the text: %s", row.Data)
				}
			}
		})
	}
}

// The viewer cannot see a refused handshake's status, so it asks with a plain GET of the socket path:
// 403 while the session is gone (no cookie, or Closed) and 426 while it is good, which it retries.
func TestDesktopStreamPlainGetTellsAGoneSessionFromAGoodOne(t *testing.T) {
	h, _ := newDesktopHarness(t)
	c := desktopCookie(t, h.enterDesktop(h.ticket(h.run.ID, h.owner, oidc.RoleUser)))
	if got := uiGet(h, desktopPath(h, uiDesktopRFB), c); got.Code != http.StatusUpgradeRequired {
		t.Fatalf("plain GET with a good session = %d, want 426", got.Code)
	}
	if got := uiGet(h, desktopPath(h, uiDesktopRFB), nil); got.Code != http.StatusForbidden {
		t.Fatalf("plain GET without a session = %d, want 403", got.Code)
	}
	req := httptest.NewRequest(http.MethodPost, desktopPath(h, uiDesktopClose), nil)
	req.AddCookie(c)
	h.gateway.ServeHTTP(httptest.NewRecorder(), req)
	if got := uiGet(h, desktopPath(h, uiDesktopRFB), c); got.Code != http.StatusForbidden {
		t.Fatalf("plain GET after Close = %d, want 403", got.Code)
	}
}

// A Close holds across a restart and on every replica: a new Server on the same store (and cookie
// key) refuses the closed cookie, while an open one still works after the restart.
func TestDesktopCloseHoldsAcrossARestartAndReplicas(t *testing.T) {
	t.Run("restart", func(t *testing.T) {
		h, _ := newDesktopHarness(t)
		closed := desktopCookie(t, h.enterDesktop(h.ticket(h.run.ID, h.owner, oidc.RoleUser)))
		h.clock.advance(time.Second) // a session's key carries its issue second: a later one is another session
		open := desktopCookie(t, h.enterDesktop(h.ticket(h.run.ID, h.owner, oidc.RoleUser)))
		if rec := postDesktopClose(h.gateway, h, closed); rec.Code != http.StatusNoContent {
			t.Fatalf("close = %d %s", rec.Code, rec.Body.String())
		}
		h.srv = New(h.srv.cfg)
		h.gateway = h.srv.UIGatewayHandler()
		if got := uiGet(h, desktopPath(h, uiDesktopRFB), closed); got.Code != http.StatusForbidden {
			t.Fatalf("the closed cookie after the restart = %d, want 403", got.Code)
		}
		if got := uiGet(h, desktopPath(h, uiDesktopRFB), open); got.Code != http.StatusUpgradeRequired {
			t.Fatalf("the open cookie after the restart = %d, want 426", got.Code)
		}
	})
	t.Run("replica", func(t *testing.T) {
		h, _ := newDesktopHarness(t)
		b := desktopReplica(h)
		c := desktopCookie(t, h.enterDesktop(h.ticket(h.run.ID, h.owner, oidc.RoleUser)))
		if got := uiGet(b, desktopPath(h, uiDesktopRFB), c); got.Code != http.StatusUpgradeRequired {
			t.Fatalf("replica B before the Close = %d, want 426", got.Code)
		}
		if rec := postDesktopClose(h.gateway, h, c); rec.Code != http.StatusNoContent {
			t.Fatalf("close on A = %d %s", rec.Code, rec.Body.String())
		}
		if got := uiGet(b, desktopPath(h, uiDesktopRFB), c); got.Code != http.StatusForbidden {
			t.Fatalf("replica B after a Close on A = %d, want 403", got.Code)
		}
	})
}

// A stream open on replica B ends with the session-ended code within the re-check bound of a Close
// on replica A.
func TestDesktopStreamOnAnotherReplicaEndsAfterAClose(t *testing.T) {
	h, dr := newDesktopHarness(t)
	b := desktopReplica(h)
	b.srv.uiDesktopClosedEvery = 50 * time.Millisecond
	srvB := httptest.NewServer(b.gateway)
	t.Cleanup(srvB.Close)
	c := desktopCookie(t, h.enterDesktop(h.ticket(h.run.ID, h.owner, oidc.RoleUser)))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	v := dialDesktop(t, ctx, h, srvB.URL, c)
	<-dr.dialled
	v.handshake()
	time.Sleep(200 * time.Millisecond) // several re-checks pass while the session is open

	if rec := postDesktopClose(h.gateway, h, c); rec.Code != http.StatusNoContent {
		t.Fatalf("close on A = %d %s", rec.Code, rec.Body.String())
	}
	closedAt := time.Now()
	_, _, err := v.ws.Read(ctx)
	var ce websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != desktopCloseSessionEnded {
		t.Fatalf("B's stream after a Close on A = %v, want close %d", err, desktopCloseSessionEnded)
	}
	if took := time.Since(closedAt); took > 2*time.Second {
		t.Fatalf("B's stream ended %v after the Close, want within the re-check bound", took)
	}
}

// A Closed set that cannot be read refuses, never admits: a plain GET is refused as a closed session
// is, and an open stream ends as one does. A Close that cannot be stored is not reported done.
func TestDesktopClosedSetFailureRefuses(t *testing.T) {
	h, dr := newDesktopHarness(t)
	h.srv.uiDesktopClosedEvery = 50 * time.Millisecond
	fake := h.srv.cfg.Store.(*desktopClosedStore)
	srv := httptest.NewServer(h.gateway)
	t.Cleanup(srv.Close)
	c := desktopCookie(t, h.enterDesktop(h.ticket(h.run.ID, h.owner, oidc.RoleUser)))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	v := dialDesktop(t, ctx, h, srv.URL, c)
	<-dr.dialled
	v.handshake()

	fake.fail(errors.New("database unreachable"), nil)
	if got := uiGet(h, desktopPath(h, uiDesktopRFB), c); got.Code != http.StatusForbidden {
		t.Fatalf("plain GET with the Closed set unreadable = %d, want 403", got.Code)
	}
	_, _, err := v.ws.Read(ctx)
	var ce websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != desktopCloseSessionEnded {
		t.Fatalf("the open stream with the Closed set unreadable = %v, want close %d", err, desktopCloseSessionEnded)
	}

	fake.fail(nil, errors.New("database unreachable"))
	rec := postDesktopClose(h.gateway, h, c)
	if rec.Code != http.StatusServiceUnavailable || len(setUICookies(rec)) != 0 {
		t.Fatalf("close with the store refusing the write = %d (cookies %v), want 503 and the cookie kept", rec.Code, setUICookies(rec))
	}
	fake.fail(nil, nil)
	if got := uiGet(h, desktopPath(h, uiDesktopRFB), c); got.Code != http.StatusUpgradeRequired {
		t.Fatalf("plain GET after a Close that was not stored = %d, want 426 (still open)", got.Code)
	}
}

// A Close is held until the cookie's own bound: a session closed in its first second is still refused
// half an hour on, by a fresh process that knows of the Close only from the store.
func TestDesktopCloseHoldsUntilTheCookieBound(t *testing.T) {
	h, _ := newDesktopHarness(t)
	closed := desktopCookie(t, h.enterDesktop(h.ticket(h.run.ID, h.owner, oidc.RoleUser)))
	h.clock.advance(time.Second)
	open := desktopCookie(t, h.enterDesktop(h.ticket(h.run.ID, h.owner, oidc.RoleUser)))
	if rec := postDesktopClose(h.gateway, h, closed); rec.Code != http.StatusNoContent {
		t.Fatalf("close = %d %s", rec.Code, rec.Body.String())
	}
	h.srv = New(h.srv.cfg)
	h.gateway = h.srv.UIGatewayHandler()
	h.clock.advance(30 * time.Minute)
	if got := uiGet(h, desktopPath(h, uiDesktopRFB), closed); got.Code != http.StatusForbidden {
		t.Fatalf("the closed cookie 30 min on = %d, want 403", got.Code)
	}
	if got := uiGet(h, desktopPath(h, uiDesktopRFB), open); got.Code != http.StatusUpgradeRequired {
		t.Fatalf("the open cookie 30 min on = %d, want 426", got.Code)
	}
}

// A store that keeps no Closed set cannot say a desktop is open, so every desktop cookie is refused
// and a Close is not reported done.
func TestDesktopStoreWithoutAClosedSetRefuses(t *testing.T) {
	h, _ := newDesktopHarness(t)
	c := desktopCookie(t, h.enterDesktop(h.ticket(h.run.ID, h.owner, oidc.RoleUser)))
	if got := uiGet(h, desktopPath(h, uiDesktopRFB), c); got.Code != http.StatusUpgradeRequired {
		t.Fatalf("with a Closed set = %d, want 426", got.Code)
	}
	if _, ok := any(h.store).(store.UIDesktopClosedStore); ok {
		t.Fatal("the harness's plain store keeps a Closed set; this test needs one that does not")
	}
	h.srv.cfg.Store = h.store
	h.srv = New(h.srv.cfg)
	h.gateway = h.srv.UIGatewayHandler()
	if got := uiGet(h, desktopPath(h, uiDesktopRFB), c); got.Code != http.StatusForbidden {
		t.Fatalf("with no Closed set = %d, want 403", got.Code)
	}
	if rec := postDesktopClose(h.gateway, h, c); rec.Code == http.StatusNoContent {
		t.Fatalf("close with no Closed set = %d, want it not reported done", rec.Code)
	}
}

// The Closed set is keyed by the sha256 of the session's relay key, so the shared table holds no
// principal text and no run ID.
func TestDesktopClosedSetKeyIsADigest(t *testing.T) {
	h, _ := newDesktopHarness(t)
	fake := h.srv.cfg.Store.(*desktopClosedStore)
	c := desktopCookie(t, h.enterDesktop(h.ticket(h.run.ID, h.owner, oidc.RoleUser)))
	req := httptest.NewRequest(http.MethodGet, desktopPath(h, "/"), nil)
	req.AddCookie(c)
	sess, ok := h.srv.decodeUISession(req, h.srv.cfg.Now())
	if !ok {
		t.Fatal("the fresh desktop cookie does not decode")
	}
	if rec := postDesktopClose(h.gateway, h, c); rec.Code != http.StatusNoContent {
		t.Fatalf("close = %d %s", rec.Code, rec.Body.String())
	}
	sum := sha256.Sum256([]byte(uiReassertKey(sess)))
	want := hex.EncodeToString(sum[:])
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.closed) != 1 {
		t.Fatalf("stored keys = %v, want one", fake.closed)
	}
	for key := range fake.closed {
		if key != want || strings.Contains(key, h.owner) || strings.Contains(key, h.run.ID.String()) {
			t.Fatalf("stored key = %q, want the sha256 hex %q of the relay key", key, want)
		}
	}
}

// A Close that lands after a stream's request read the Closed set but before the stream was tracked
// (so the Close had nothing of it to cut) still ends that stream at once, before the runner is dialled.
func TestDesktopStreamClosedBeforeItIsTrackedEndsAtOnce(t *testing.T) {
	h, dr := newDesktopHarness(t)
	fake := h.srv.cfg.Store.(*desktopClosedStore)
	srv := httptest.NewServer(h.gateway)
	t.Cleanup(srv.Close)
	c := desktopCookie(t, h.enterDesktop(h.ticket(h.run.ID, h.owner, oidc.RoleUser)))
	fake.mu.Lock()
	fake.afterRead = func() {
		if rec := postDesktopClose(h.gateway, h, c); rec.Code != http.StatusNoContent {
			t.Errorf("close = %d %s", rec.Code, rec.Body.String())
		}
	}
	fake.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	v := dialDesktop(t, ctx, h, srv.URL, c)
	_, _, err := v.ws.Read(ctx)
	var ce websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != desktopCloseSessionEnded {
		t.Fatalf("the stream closed before it was tracked = %v, want close %d", err, desktopCloseSessionEnded)
	}
	select {
	case ref := <-dr.dialled:
		t.Fatalf("dialled %q after the Close", ref)
	default:
	}
}

// desktopClosedStore is the harness store with a shared Closed set, which every Server built on it
// reads as replicas on one database do; fail makes its reads or its writes fail.
type desktopClosedStore struct {
	*uiMemStore
	mu       sync.Mutex
	closed   map[string]time.Time
	readErr  error
	writeErr error
	// afterRead, when set, runs once after the next read has taken its answer: a Close landing just then.
	afterRead func()
}

func (s *desktopClosedStore) fail(read, write error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readErr, s.writeErr = read, write
}

func (s *desktopClosedStore) CloseUIDesktop(_ context.Context, key string, expiresAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr != nil {
		return s.writeErr
	}
	s.closed[key] = expiresAt
	return nil
}

func (s *desktopClosedStore) UIDesktopClosed(_ context.Context, key string, now time.Time) (bool, error) {
	s.mu.Lock()
	exp, ok := s.closed[key]
	err, hook := s.readErr, s.afterRead
	s.afterRead = nil
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	if err != nil {
		return false, err
	}
	return ok && exp.After(now), nil
}

func (s *desktopClosedStore) PruneUIDesktopClosed(context.Context, time.Time) (int, error) {
	return 0, nil
}

// desktopReplica is a second Server on h's config (its store and cookie key): another replica.
func desktopReplica(h *uiHarness) *uiHarness {
	b := *h
	b.srv = New(h.srv.cfg)
	b.gateway = b.srv.UIGatewayHandler()
	return &b
}

func postDesktopClose(gw http.Handler, h *uiHarness, c *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, desktopPath(h, uiDesktopClose), nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	return rec
}
