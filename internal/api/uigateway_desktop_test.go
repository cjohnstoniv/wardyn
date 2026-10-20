// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
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
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// desktopFakeRunner is the harness runner plus a VM display: each dial is one end of a pipe whose
// other end greets as QEMU's RFB server does and then echoes.
type desktopFakeRunner struct {
	*sshFakeRunner
	dialled chan string
}

func (f *desktopFakeRunner) DialDesktop(_ context.Context, ref string) (io.ReadWriteCloser, error) {
	f.dialled <- ref
	a, b := net.Pipe()
	go func() {
		defer b.Close()
		if _, err := b.Write([]byte("RFB 003.008\n")); err != nil {
			return
		}
		_, _ = io.Copy(b, b)
	}()
	return a, nil
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
	return h, dr
}

func (h *uiHarness) enterDesktop(ticket string) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.enter(url.Values{"run": {h.run.ID.String()}, "app": {uiDesktopApp}, "ticket": {ticket}})
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
	return uiRelayPrefix(h.run.ID, uiDesktopApp) + rest
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
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+desktopPath(h, uiDesktopRFB),
		&websocket.DialOptions{HTTPHeader: http.Header{"Cookie": {c.Name + "=" + c.Value}}})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	if ref := <-dr.dialled; ref != h.run.SandboxRef {
		t.Fatalf("dialled %q, want the run's sandbox", ref)
	}
	_, msg, err := ws.Read(ctx)
	if err != nil || string(msg) != "RFB 003.008\n" {
		t.Fatalf("greeting = %q, %v", msg, err)
	}
	if err := ws.Write(ctx, websocket.MessageBinary, []byte("RFB 003.008\n")); err != nil {
		t.Fatal(err)
	}
	if _, msg, err = ws.Read(ctx); err != nil || string(msg) != "RFB 003.008\n" {
		t.Fatalf("echo = %q, %v", msg, err)
	}

	req, _ := http.NewRequest(http.MethodPost, srv.URL+desktopPath(h, uiDesktopClose), nil)
	req.AddCookie(c)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("close = %v %v", resp, err)
	}
	_ = resp.Body.Close()
	_, _, err = ws.Read(ctx)
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
