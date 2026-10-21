// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// fakeRunnerd is one runner's local socket (internal/runner/runnerio's ServeLocal, reduced): it answers a
// hello with a nonce (or a refusal), and on the organisation's grant sends the bound frame and a screen,
// then records what the CLI typed.
type fakeRunnerd struct {
	runnerID string
	hellos   atomic.Int32

	mu      sync.Mutex
	refuse  string // set: refuse every hello with this reason
	n       int
	pending map[string]net.Conn
	granted net.Conn
	typed   bytes.Buffer
}

func newFakeRunnerd(t *testing.T, runnerID string) (*fakeRunnerd, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "wa") // short: a unix socket path is at most 104 bytes on macOS
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "attach.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	rd := &fakeRunnerd{runnerID: runnerID, pending: map[string]net.Conn{}}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go rd.hello(c)
		}
	}()
	return rd, sock
}

func (rd *fakeRunnerd) hello(c net.Conn) {
	typ, b, err := readLocalFrame(c)
	var h struct {
		V     int    `json:"v"`
		RunID string `json:"run_id"`
	}
	if err != nil || typ != localHello || json.Unmarshal(b, &h) != nil || h.V != localProtocol {
		_ = c.Close()
		return
	}
	rd.hellos.Add(1)
	rd.mu.Lock()
	refuse := rd.refuse
	rd.mu.Unlock()
	if refuse != "" {
		_ = writeLocalJSONForTest(c, localRefuse, map[string]string{"reason": refuse})
		_ = c.Close()
		return
	}
	rd.mu.Lock()
	rd.n++
	nonce := fmt.Sprintf("N%d", rd.n)
	rd.pending[nonce] = c
	rd.mu.Unlock()
	_ = writeLocalJSONForTest(c, localHelloOK, map[string]string{"runner_id": rd.runnerID, "nonce": nonce})
}

// grant is the organisation's local_attach for nonce: false for a nonce this runner never handed out.
func (rd *fakeRunnerd) grant(nonce string) bool {
	rd.mu.Lock()
	c := rd.pending[nonce]
	delete(rd.pending, nonce)
	rd.granted = c
	rd.mu.Unlock()
	if c == nil {
		return false
	}
	_ = writeLocalJSONForTest(c, localBound, map[string]any{"read_only": false})
	_ = writeLocalFrame(c, localData, []byte("local screen\n"))
	go func() {
		for {
			typ, b, err := readLocalFrame(c)
			if err != nil {
				return
			}
			if typ == localData {
				rd.mu.Lock()
				rd.typed.Write(b)
				rd.mu.Unlock()
			}
		}
	}()
	return true
}

func (rd *fakeRunnerd) end(reason string) {
	rd.mu.Lock()
	c := rd.granted
	rd.mu.Unlock()
	_ = writeLocalJSONForTest(c, localEnd, map[string]string{"reason": reason})
	_ = c.Close()
}

func (rd *fakeRunnerd) setRefuse(reason string) {
	rd.mu.Lock()
	rd.refuse = reason
	rd.mu.Unlock()
}

func (rd *fakeRunnerd) typedString() string {
	rd.mu.Lock()
	defer rd.mu.Unlock()
	return rd.typed.String()
}

func writeLocalJSONForTest(w io.Writer, typ byte, v any) error {
	b, _ := json.Marshal(v)
	return writeLocalFrame(w, typ, b)
}

// fakeOrg is the organisation: the mint, the take-over route, GET /runs/{id} and the attach WebSocket,
// whose every dial onDial answers (n counts dials from 1).
type fakeOrg struct {
	runID, runnerID string
	placement       string
	rd              *fakeRunnerd
	onDial          func(o *fakeOrg, n int, w http.ResponseWriter, r *http.Request)
	takeover        func(w http.ResponseWriter)
	mintRefusal     func(w http.ResponseWriter) // set: the mint refuses

	mints, dials, takeovers, presence atomic.Int32
	mu                                sync.Mutex
	queries                           []url.Values
	order                             []string
}

func (o *fakeOrg) note(s string) {
	o.mu.Lock()
	o.order = append(o.order, s)
	o.mu.Unlock()
}

func (o *fakeOrg) handler(w http.ResponseWriter, r *http.Request) {
	base := "/api/v1/runs/" + o.runID
	switch {
	case r.Method == http.MethodPost && r.URL.Path == base+"/attach/ticket":
		n := o.mints.Add(1)
		if o.mintRefusal != nil {
			o.mintRefusal(w)
			return
		}
		o.note("mint")
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"ticket":"T%d"}`, n)
	case r.Method == http.MethodPost && r.URL.Path == base+"/attach/takeover":
		o.takeovers.Add(1)
		o.note("takeover")
		o.takeover(w)
	case r.Method == http.MethodGet && r.URL.Path == base:
		o.note("get")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": o.runID, "placement": o.placement,
			"runner_id": o.runnerID, "runner_name": "mbp-dev"})
	case r.Method == http.MethodGet && r.URL.Path == base+"/attach":
		n := int(o.dials.Add(1))
		o.mu.Lock()
		o.queries = append(o.queries, r.URL.Query())
		o.order = append(o.order, "dial")
		o.mu.Unlock()
		o.onDial(o, n, w, r)
	default:
		http.NotFound(w, r)
	}
}

func (o *fakeOrg) calls() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return strings.Join(o.order, ",")
}

func (o *fakeOrg) query(n int) url.Values {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.queries[n-1]
}

// grantLocal is a new organisation's local session: the runner gets the grant, the first frame says
// transport "local", and the socket carries control frames until the CLI leaves.
func grantLocal(o *fakeOrg, _ int, w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	if !o.rd.grant(r.URL.Query().Get("local_nonce")) {
		_ = c.Close(4409, "attach_local_grant_failed")
		return
	}
	_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"attach-mode","read_only":false,"transport":"local"}`))
	readControl(o, c, r.Context())
}

func readControl(o *fakeOrg, c *websocket.Conn, ctx context.Context) {
	for {
		typ, b, err := c.Read(ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageText && strings.Contains(string(b), `"presence"`) {
			o.presence.Add(1)
		}
	}
}

// serveRelay is the relay: the first frame names no transport, then the screen, then input is read.
func serveRelay(_ *fakeOrg, _ int, w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"attach-mode","read_only":false}`))
	_ = c.Write(r.Context(), websocket.MessageBinary, []byte("relay screen\n"))
	for {
		if _, _, err := c.Read(r.Context()); err != nil {
			return
		}
	}
}

// localThenRelay answers a transport=local dial with local and any other with serveRelay.
func localOr(local func(*fakeOrg, int, http.ResponseWriter, *http.Request)) func(*fakeOrg, int, http.ResponseWriter, *http.Request) {
	return func(o *fakeOrg, n int, w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("transport") == "local" {
			local(o, n, w, r)
			return
		}
		serveRelay(o, n, w, r)
	}
}

func refuseDial(status int, reason, msg string) func(*fakeOrg, int, http.ResponseWriter, *http.Request) {
	return func(_ *fakeOrg, _ int, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": msg, "reason": reason})
	}
}

func close4409(_ *fakeOrg, _ int, w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	_ = c.Close(4409, "attach_local_grant_failed")
}

// localFixture is a run on runner mbp-dev of this computer: the organisation, the runner's socket and
// its pointer file (0600 in a 0700 directory, naming this organisation). It shortens the drain so a
// piped session ends soon after its input.
type localFixture struct {
	org     *fakeOrg
	rd      *fakeRunnerd
	srv     *httptest.Server
	dir     string
	sock    string
	stdout  func() string
	stderr  func() string
	pointer map[string]string
}

// tweak runs before the organisation serves its first request.
func newLocalFixture(t *testing.T, onDial func(*fakeOrg, int, http.ResponseWriter, *http.Request), tweak ...func(*fakeOrg)) *localFixture {
	t.Helper()
	runnerID := uuid.New().String()
	rd, sock := newFakeRunnerd(t, runnerID)
	org := &fakeOrg{runID: uuid.New().String(), runnerID: runnerID, placement: "local", rd: rd, onDial: onDial,
		takeover: func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"taken_over":true,"previous_holder":"alice"}`)) }}
	for _, fn := range tweak {
		fn(org)
	}
	srv := httptest.NewServer(http.HandlerFunc(org.handler))
	t.Cleanup(srv.Close)
	dir := filepath.Join(t.TempDir(), "local-attach")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f := &localFixture{org: org, rd: rd, srv: srv, dir: dir, sock: sock,
		pointer: map[string]string{"runner_id": runnerID, "org_url": srv.URL + "/", "socket": sock}}
	f.writePointer(t)
	oldDir, oldIn, oldIdle, oldGrace := localAttachDir, attachStdin, attachDrainIdle, localEndGrace
	t.Cleanup(func() { localAttachDir, attachStdin, attachDrainIdle, localEndGrace = oldDir, oldIn, oldIdle, oldGrace })
	localAttachDir = func() (string, error) { return dir, nil }
	attachStdin = strings.NewReader("typed\n")
	attachDrainIdle = 300 * time.Millisecond
	localEndGrace = 500 * time.Millisecond
	_, f.stdout, f.stderr = redirectAttachIO(t)
	return f
}

func (f *localFixture) writePointer(t *testing.T) {
	t.Helper()
	b, _ := json.Marshal(f.pointer)
	if err := os.WriteFile(filepath.Join(f.dir, f.org.runnerID+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *localFixture) attach(opts attachOptions) (stdout, stderr string, err error) {
	err = runAttach(context.Background(), &sdk.Client{BaseURL: f.srv.URL}, f.org.runID, opts)
	return f.stdout(), f.stderr(), err
}

// TestAttachLocal_OnThisComputer: mint, GET the run, hello the runner's socket, dial transport=local with
// its nonce; the screen and the keystrokes go over the socket, and the transport line names the runner.
func TestAttachLocal_OnThisComputer(t *testing.T) {
	f := newLocalFixture(t, grantLocal)
	stdout, stderr, err := f.attach(attachOptions{})
	if err != nil {
		t.Fatalf("attach = %v, want a clean detach", err)
	}
	q := f.org.query(1)
	if q.Get("transport") != "local" || q.Get("local_nonce") != "N1" || q.Get("ticket") != "T1" {
		t.Errorf("dial query = %v, want transport=local, local_nonce=N1, ticket=T1", q)
	}
	if stdout != "local screen\n" {
		t.Errorf("stdout = %q, want the runner's screen", stdout)
	}
	if got := f.rd.typedString(); got != "typed\n" {
		t.Errorf("runner got %q, want the typed line", got)
	}
	if want := "Attached on this computer (runner mbp-dev). Ctrl-C goes to the sandbox; close the window to detach.\n"; !strings.Contains(stderr, want) {
		t.Errorf("stderr = %q, want the line %q", stderr, want)
	}
	if got := f.org.calls(); got != "mint,get,dial" {
		t.Errorf("calls = %s, want mint first, then the run, then one dial", got)
	}
}

// TestAttachLocal_FallbackTriggers: each transport answer of §6.2 attaches through the organisation, with
// the relay line, and nothing else does.
func TestAttachLocal_FallbackTriggers(t *testing.T) {
	for _, tc := range []struct {
		name         string
		setup        func(t *testing.T, f *localFixture)
		tweak        func(o *fakeOrg)
		onDial       func(*fakeOrg, int, http.ResponseWriter, *http.Request)
		dials, mints int32
		hellos       int32
	}{
		{name: "no pointer file", setup: func(t *testing.T, f *localFixture) {
			_ = os.Remove(filepath.Join(f.dir, f.org.runnerID+".json"))
		}, dials: 1, mints: 1},
		{name: "pointer names another runner", setup: func(t *testing.T, f *localFixture) {
			f.pointer["runner_id"] = uuid.New().String()
			f.writePointer(t)
		}, dials: 1, mints: 1},
		{name: "pointer for another organisation", setup: func(t *testing.T, f *localFixture) {
			f.pointer["org_url"] = "https://other-org.example.com"
			f.writePointer(t)
		}, dials: 1, mints: 1},
		{name: "pointer open to other users", setup: func(t *testing.T, f *localFixture) {
			_ = os.Chmod(filepath.Join(f.dir, f.org.runnerID+".json"), 0o644)
		}, dials: 1, mints: 1},
		{name: "pointer names a file, not a socket", setup: func(t *testing.T, f *localFixture) {
			f.pointer["socket"] = filepath.Join(f.dir, f.org.runnerID+".json")
			f.writePointer(t)
		}, dials: 1, mints: 1},
		{name: "run not on a person's runner", tweak: func(o *fakeOrg) { o.placement = "remote" }, dials: 1, mints: 1},
		{name: "socket refuses", tweak: func(o *fakeOrg) { o.rd.setRefuse("run_not_on_this_runner") }, dials: 1, mints: 1, hellos: 1},
		{name: "409 attach_local_unavailable",
			onDial: localOr(refuseDial(409, "attach_local_unavailable", "this run cannot be attached on its runner's own computer (flag_off); attach through the organisation instead")),
			dials:  2, mints: 2, hellos: 1},
		{name: "close 4409 retries once over the relay", onDial: localOr(close4409), dials: 2, mints: 2, hellos: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			onDial := tc.onDial
			if onDial == nil {
				onDial = localOr(grantLocal)
			}
			var tweaks []func(*fakeOrg)
			if tc.tweak != nil {
				tweaks = append(tweaks, tc.tweak)
			}
			f := newLocalFixture(t, onDial, tweaks...)
			if tc.setup != nil {
				tc.setup(t, f)
			}
			stdout, stderr, err := f.attach(attachOptions{})
			if err != nil {
				t.Fatalf("attach = %v, want the relay attach", err)
			}
			if d, m, h := f.org.dials.Load(), f.org.mints.Load(), f.rd.hellos.Load(); d != tc.dials || m != tc.mints || h != tc.hellos {
				t.Errorf("dials, mints, hellos = %d, %d, %d; want %d, %d, %d", d, m, h, tc.dials, tc.mints, tc.hellos)
			}
			if q := f.org.query(int(tc.dials)); q.Get("transport") != "" || q.Get("ticket") != fmt.Sprintf("T%d", tc.mints) {
				t.Errorf("last dial query = %v, want a relay dial with the newest ticket", q)
			}
			if stdout != "relay screen\n" {
				t.Errorf("stdout = %q, want the relay's screen", stdout)
			}
			if want := "Attached through the organisation (" + strings.TrimPrefix(f.srv.URL, "http://") + "). Ctrl-C goes to the sandbox; close the window to detach.\n"; !strings.Contains(stderr, want) {
				t.Errorf("stderr = %q, want %q", stderr, want)
			}
		})
	}
}

// TestAttachLocal_OldOrganisationRunsAsRelay: an organisation older than the local transport ignores the
// query; its first frame names no transport, and that same socket is the session, a relay attach.
func TestAttachLocal_OldOrganisationRunsAsRelay(t *testing.T) {
	f := newLocalFixture(t, serveRelay)
	stdout, stderr, err := f.attach(attachOptions{})
	if err != nil {
		t.Fatalf("attach = %v", err)
	}
	if f.org.dials.Load() != 1 || f.org.mints.Load() != 1 {
		t.Errorf("dials, mints = %d, %d; want the one socket", f.org.dials.Load(), f.org.mints.Load())
	}
	if stdout != "relay screen\n" || !strings.Contains(stderr, "Attached through the organisation") {
		t.Errorf("stdout = %q, stderr = %q; want the relay session and its line", stdout, stderr)
	}
}

// TestAttachLocal_Second4409IsShown: the relay retry after a 4409 is the only one; a second 4409 is shown.
func TestAttachLocal_Second4409IsShown(t *testing.T) {
	f := newLocalFixture(t, close4409)
	_, _, err := f.attach(attachOptions{})
	if websocket.CloseStatus(err) != 4409 {
		t.Fatalf("attach = %v, want the second 4409 shown", err)
	}
	if f.org.dials.Load() != 2 || f.org.mints.Load() != 2 {
		t.Errorf("dials, mints = %d, %d; want one local try and one relay retry", f.org.dials.Load(), f.org.mints.Load())
	}
}

// parityRows are DESIGN-ATTACH 4.4's refusals the attach WebSocket gives (rows 2-15; row 2's 404 at the
// mint is TestAttachLocal_ForeignRunFailsAtTheMint): each is shown with the server's status, reason and sentence, never retried.
var parityRows = []struct {
	status      int
	reason, msg string
}{
	{403, "run_owner_only", "only the run's owner may attach to it"},
	{404, "run_not_found", "run not found"},
	{409, "run_background_only", "this run is background-only: it has no terminal"},
	{409, "attach_not_running", "run is not running"},
	{409, "attach_run_kept", "run is kept for inspection"},
	{409, "attach_no_sandbox", "run has no sandbox"},
	{503, "attach_session_cap_reached", "too many terminal sessions"},
	{403, "attach_ticket_not_your_run", "this session is no longer authorized"},
	{403, "ui_gateway_ticket_invalid", "ticket invalid"},
	{503, "ui_gateway_ticket_invalid", "ticket could not be verified"},
	{403, "governance_unit", "the governance profile denies interactive sessions"},
	{503, "mask_state_unavailable", "the masking state could not be proved"},
	{502, "attach_resume_failed", "run is paused and could not be resumed; try again"},
	{503, "attach_no_runner", "no runner configured"},
}

func TestAttachLocal_ParityRowsShownVerbatim(t *testing.T) {
	for _, row := range parityRows {
		for _, via := range []string{viaAuto, viaRelay} {
			t.Run(fmt.Sprintf("%s/%d/%s", via, row.status, row.reason), func(t *testing.T) {
				f := newLocalFixture(t, refuseDial(row.status, row.reason, row.msg))
				_, _, err := f.attach(attachOptions{via: via})
				var ae *sdk.APIError
				if !errors.As(err, &ae) || ae.Status != row.status || ae.Reason != row.reason || !strings.Contains(err.Error(), row.msg) {
					t.Fatalf("attach = %v, want %d %s with the server's sentence", err, row.status, row.reason)
				}
				if f.org.dials.Load() != 1 || f.org.mints.Load() != 1 {
					t.Errorf("dials, mints = %d, %d; a refusal gets no second attempt", f.org.dials.Load(), f.org.mints.Load())
				}
			})
		}
	}
}

// TestAttachLocal_OpenSessionClosesAreShown: rows 18-19, a close after the upgrade (authority, take-over,
// lease lost) is shown as the organisation gave it, and not retried.
func TestAttachLocal_OpenSessionClosesAreShown(t *testing.T) {
	for _, tc := range []struct {
		code   websocket.StatusCode
		reason string
	}{{4401, "session ended"}, {websocket.StatusPolicyViolation, "attach lease lost"}} {
		t.Run(tc.reason, func(t *testing.T) {
			f := newLocalFixture(t, func(o *fakeOrg, _ int, w http.ResponseWriter, r *http.Request) {
				c, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				o.rd.grant(r.URL.Query().Get("local_nonce"))
				_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"attach-mode","transport":"local"}`))
				time.Sleep(50 * time.Millisecond)
				o.rd.end("ended_by_organisation")
				_ = c.Close(tc.code, tc.reason)
			})
			attachStdin = blockingReader{}
			_, _, err := f.attach(attachOptions{})
			if websocket.CloseStatus(err) != tc.code || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("attach = %v, want close %d %q", err, tc.code, tc.reason)
			}
			if f.org.dials.Load() != 1 {
				t.Errorf("dials = %d, want 1", f.org.dials.Load())
			}
		})
	}
}

// blockingReader is a stdin that never sends: the session ends from the other side.
type blockingReader struct{}

func (blockingReader) Read([]byte) (int, error) { select {} }

// TestAttachLocal_ShellExitIsCleanDetach: the runner's shell_exited and the organisation's normal close
// end the session with exit 0, as the relay's shell exit does.
func TestAttachLocal_ShellExitIsCleanDetach(t *testing.T) {
	f := newLocalFixture(t, func(o *fakeOrg, _ int, w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		o.rd.grant(r.URL.Query().Get("local_nonce"))
		_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"attach-mode","transport":"local"}`))
		time.Sleep(50 * time.Millisecond)
		o.rd.end("shell_exited")
		_ = c.Close(websocket.StatusNormalClosure, "")
	})
	attachStdin = blockingReader{}
	if _, stderr, err := f.attach(attachOptions{}); err != nil || !strings.Contains(stderr, "detached") {
		t.Fatalf("attach = %v (stderr %q), want a clean detach", err, stderr)
	}
}

// TestAttachLocal_ForeignRunFailsAtTheMint: a pasted run id of someone else's is the mint's 404, before
// the run is read, the socket is asked, or anything is dialled.
func TestAttachLocal_ForeignRunFailsAtTheMint(t *testing.T) {
	f := newLocalFixture(t, grantLocal, func(o *fakeOrg) {
		o.mintRefusal = func(w http.ResponseWriter) { refuseDial(404, "run_not_found", "run not found")(nil, 0, w, nil) }
	})
	_, _, err := f.attach(attachOptions{})
	var ae *sdk.APIError
	if !errors.As(err, &ae) || ae.Status != 404 || ae.Reason != "run_not_found" {
		t.Fatalf("attach = %v, want the mint's 404", err)
	}
	if f.org.dials.Load() != 0 || f.rd.hellos.Load() != 0 || f.org.calls() != "" {
		t.Errorf("after the mint's 404: dials %d, hellos %d, calls %v; want none", f.org.dials.Load(), f.rd.hellos.Load(), f.org.calls())
	}
}

// TestAttachLocal_PresenceAtMostOncePerInterval: typing marks presence on the control socket, at most one
// frame per presenceEvery however many keystrokes.
func TestAttachLocal_PresenceAtMostOncePerInterval(t *testing.T) {
	f := newLocalFixture(t, grantLocal)
	attachStdin = io.MultiReader(strings.NewReader("a"), strings.NewReader("b"), strings.NewReader("c"))
	if _, _, err := f.attach(attachOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := f.rd.typedString(); got != "abc" {
		t.Fatalf("runner got %q, want three keystrokes", got)
	}
	if n := f.org.presence.Load(); n != 1 {
		t.Errorf("presence frames = %d, want 1 for three keystrokes inside %v", n, presenceEvery)
	}
}

// TestAttachVia: --via relay never asks the runner; --via local fails rather than falling back.
func TestAttachVia(t *testing.T) {
	t.Run("relay skips the runner", func(t *testing.T) {
		f := newLocalFixture(t, localOr(grantLocal))
		if stdout, _, err := f.attach(attachOptions{via: viaRelay}); err != nil || stdout != "relay screen\n" {
			t.Fatalf("attach = %v, stdout %q; want the relay", err, stdout)
		}
		if f.rd.hellos.Load() != 0 || f.org.query(1).Get("transport") != "" {
			t.Errorf("hellos = %d, query %v; --via relay must not try local", f.rd.hellos.Load(), f.org.query(1))
		}
	})
	for _, tc := range []struct {
		name   string
		setup  func(f *localFixture)
		tweak  func(o *fakeOrg)
		onDial func(*fakeOrg, int, http.ResponseWriter, *http.Request)
		want   string
		dials  int32
	}{
		{"no pointer file", func(f *localFixture) { _ = os.Remove(filepath.Join(f.dir, f.org.runnerID+".json")) }, nil, nil,
			errLocalNotHere + "no usable pointer file", 0},
		{"socket refuses", nil, func(o *fakeOrg) { o.rd.setRefuse("too_many_pending") }, nil,
			errLocalNotHere + "the runner refused: too_many_pending", 0},
		{"409", nil, nil, refuseDial(409, "attach_local_unavailable", "this run cannot be attached on its runner's own computer (runner_too_old)"),
			"runner_too_old", 1},
		{"4409", nil, nil, close4409, "attach_local_grant_failed", 1},
		{"old organisation", nil, nil, serveRelay, errLocalNotHere + "the organisation does not offer it", 1},
	} {
		t.Run("local/"+tc.name, func(t *testing.T) {
			onDial := tc.onDial
			if onDial == nil {
				onDial = grantLocal
			}
			var tweaks []func(*fakeOrg)
			if tc.tweak != nil {
				tweaks = append(tweaks, tc.tweak)
			}
			f := newLocalFixture(t, onDial, tweaks...)
			if tc.setup != nil {
				tc.setup(f)
			}
			_, _, err := f.attach(attachOptions{via: viaLocal})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("attach --via local = %v, want %q", err, tc.want)
			}
			if f.org.dials.Load() != tc.dials || f.org.mints.Load() != 1 {
				t.Errorf("dials, mints = %d, %d; want %d, 1: --via local never falls back", f.org.dials.Load(), f.org.mints.Load(), tc.dials)
			}
		})
	}
}

// TestAttachTakeOver: --take-over posts the take-over route after the mint and before the dial; nobody
// attached is nothing to take, and any other refusal is shown and stops there.
func TestAttachTakeOver(t *testing.T) {
	t.Run("takes, then attaches", func(t *testing.T) {
		f := newLocalFixture(t, grantLocal)
		_, stderr, err := f.attach(attachOptions{takeOver: true})
		if err != nil {
			t.Fatal(err)
		}
		if got := f.org.calls(); got != "mint,takeover,get,dial" {
			t.Errorf("calls = %s", got)
		}
		if !strings.Contains(stderr, "took the terminal from alice") {
			t.Errorf("stderr = %q", stderr)
		}
	})
	t.Run("nobody attached", func(t *testing.T) {
		f := newLocalFixture(t, grantLocal, func(o *fakeOrg) {
			o.takeover = func(w http.ResponseWriter) {
				refuseDial(409, "attach_takeover_no_holder", "nobody is attached to this run; nothing to take over")(nil, 0, w, nil)
			}
		})
		if _, _, err := f.attach(attachOptions{takeOver: true}); err != nil || f.org.dials.Load() != 1 {
			t.Fatalf("attach = %v, dials %d; want the attach", err, f.org.dials.Load())
		}
	})
	t.Run("refused", func(t *testing.T) {
		f := newLocalFixture(t, grantLocal, func(o *fakeOrg) {
			o.takeover = func(w http.ResponseWriter) {
				refuseDial(403, "run_owner_only", "only the run's owner may take over its terminal")(nil, 0, w, nil)
			}
		})
		_, _, err := f.attach(attachOptions{takeOver: true})
		var ae *sdk.APIError
		if !errors.As(err, &ae) || ae.Reason != "run_owner_only" || f.org.dials.Load() != 0 {
			t.Fatalf("attach = %v, dials %d; want the 403 shown and no dial", err, f.org.dials.Load())
		}
	})
}

// TestLocalAttachOSOnUnix: the local transport is built in everywhere but Windows.
func TestLocalAttachOSOnUnix(t *testing.T) {
	if !localAttachOS {
		t.Fatal("localAttachOS is false on a unix build")
	}
}
