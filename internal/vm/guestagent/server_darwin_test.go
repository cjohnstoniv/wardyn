// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// Native darwin tests: they run on the train's hosted macOS job, not on Linux.

func newDarwinServer(t *testing.T) (*Server, *fakeRunner, http.Handler) {
	t.Helper()
	r := &fakeRunner{}
	s := &Server{Version: "t", Home: t.TempDir(), User: "agent", OutDir: t.TempDir(), MarkerDir: t.TempDir(),
		Desktop:   &Desktop{Run: r, Console: fakeConsole{uid: 502}, VNCSettings: filepath.Join(t.TempDir(), "com.apple.VNCSettings.txt")},
		ToolsDial: &ToolsDialRecord{}}
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	return s, r, h
}

func postInit(h http.Handler, in Init) *httptest.ResponseRecorder {
	b, _ := json.Marshal(in)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/init", bytes.NewReader(b)))
	return w
}

func TestDarwinInitRunDiskDesktopAndCA(t *testing.T) {
	s, r, h := newDarwinServer(t)
	w := postInit(h, Init{Env: map[string]string{"A": "b"}, DesktopPassword: "Abcd2345"})
	if w.Code != 200 {
		t.Fatalf("init = %d %s", w.Code, w.Body)
	}
	var res InitResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || res.SignIns != SignInsRunDisk || res.DesktopPasswordSet == nil || !*res.DesktopPasswordSet {
		t.Errorf("InitResult = %+v, %v", res, err)
	}
	if pw, ok := s.Desktop.Password(); !ok || pw != "Abcd2345" {
		t.Errorf("no run password after Init: %q %v", pw, ok)
	}
	if got, err := os.ReadFile(s.Desktop.VNCSettings); err != nil || !bytes.Equal(got, VNCSettingsLine("Abcd2345")) {
		t.Errorf("Init did not write the VNC password: %q %v", got, err)
	}
	noArgvHolds(t, r, "Abcd2345")
	if w := postInit(h, Init{}); w.Code != http.StatusConflict {
		t.Errorf("second Init = %d", w.Code)
	}
}

func TestDarwinInitRefusesWhatMacOSCannotDo(t *testing.T) {
	_, _, h := newDarwinServer(t)
	in := Init{Mounts: []Mount{{Tag: "wardyn0", Target: "/Users/agent/work"}}}
	if w := postInit(h, in); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("folder mount with no mounter: Init = %d", w.Code)
	}
	// A refusal leaves the boot's Init available.
	if w := postInit(h, Init{}); w.Code != 200 {
		t.Errorf("Init after refusals = %d", w.Code)
	}
}

// Shares are mounted in order before the marker; one that fails refuses the Init with 422, names its
// target, and writes no marker, so nothing runs without its folders.
func TestDarwinInitMountsSharesBeforeTheMarker(t *testing.T) {
	s, _, h := newDarwinServer(t)
	marker := filepath.Join(s.MarkerDir, "wardyn-guest-init")
	var got []Mount
	s.Mount = func(_ context.Context, m Mount) error {
		if _, err := os.Stat(marker); err == nil {
			t.Errorf("share %s mounted after the marker", m.Tag)
		}
		got = append(got, m)
		return nil
	}
	in := Init{Mounts: []Mount{{Tag: "wardyn0", Target: "/Users/agent/a", ReadOnly: true}, {Tag: "wardyn1", Target: "/Users/agent/b"}}}
	if w := postInit(h, in); w.Code != 200 {
		t.Fatalf("Init = %d %s", w.Code, w.Body)
	}
	if !slices.Equal(got, in.Mounts) {
		t.Errorf("mounted %+v, want %+v", got, in.Mounts)
	}

	s2, _, h2 := newDarwinServer(t)
	s2.Mount = func(context.Context, Mount) error { return errors.New("mount_virtiofs failed") }
	w := postInit(h2, Init{Mounts: []Mount{{Tag: "wardyn9", Target: "/Users/agent/c"}}})
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "/Users/agent/c") {
		t.Fatalf("failed mount: Init = %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(filepath.Join(s2.MarkerDir, "wardyn-guest-init")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a failed mount left the marker: %v", err)
	}
}

func TestDarwinInitSignInsOffRunsAgentOnlyWithNoDesktop(t *testing.T) {
	s, r, h := newDarwinServer(t)
	w0 := postInit(h, Init{SignInsOff: true, DesktopPassword: "Abcd2345"})
	if w0.Code != 200 || !strings.Contains(w0.Body.String(), `"desktop_password_set":false`) {
		t.Fatalf("Init = %d %s, want desktop_password_set false", w0.Code, w0.Body)
	}
	if s.Desktop.Enabled() {
		t.Error("the desktop stayed on")
	}
	if _, ok := s.Desktop.Password(); ok {
		t.Error("a password was set")
	}
	if _, err := os.Stat(s.Desktop.VNCSettings); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the VNC settings file was written: %v", err)
	}
	noArgvHolds(t, r, "Abcd2345")
}

func TestDarwinInitSignInsOffDisablesTheDesktopEvenIfWritingFails(t *testing.T) {
	s, _, h := newDarwinServer(t)
	w := postInit(h, Init{SignInsOff: true, Files: []File{{Path: "relative/path", Mode: 0o600}}})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("Init = %d, want the file write to fail", w.Code)
	}
	if s.Desktop.Enabled() {
		t.Error("the desktop stayed on after a failed Init")
	}
}

// signalRig is a darwin Server whose latest exec is running and whose signals are recorded, not sent.
type signalRig struct {
	s    *Server
	h    http.Handler
	sent []unix.Signal
}

func newSignalRig(t *testing.T, markerDir string) *signalRig {
	t.Helper()
	r := &signalRig{s: &Server{Version: "t", Home: t.TempDir(), User: "agent", OutDir: t.TempDir(), MarkerDir: markerDir}}
	h, err := r.s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	r.h = h
	r.s.mu.Lock()
	r.s.execs = map[string]*detached{"a": {id: "a", pid: 4242, changed: make(chan struct{})}}
	r.s.latest = "a"
	r.s.signal = func(pgid int, sig unix.Signal) error {
		if pgid != 4242 {
			t.Errorf("signalled process group %d, want the agent's 4242", pgid)
		}
		r.sent = append(r.sent, sig)
		return nil
	}
	r.s.mu.Unlock()
	return r
}

// post sends one pause or resume with body and returns its status, its body, and whether the agent
// answered it ordered (fenced_pause).
func (r *signalRig) post(t *testing.T, pause bool, body string) (int, string, bool) {
	t.Helper()
	path := "/v1/agent/resume"
	if pause {
		path = "/v1/agent/pause"
	}
	w := httptest.NewRecorder()
	r.h.ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader(body)))
	var res AgentSignalResult
	if w.Code == http.StatusOK {
		_ = json.Unmarshal(w.Body.Bytes(), &res)
	}
	return w.Code, w.Body.String(), res.FencedPause
}

func (r *signalRig) signals() []unix.Signal {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	return slices.Clone(r.sent)
}

func gen(n int) string { b, _ := json.Marshal(AgentSignal{Generation: int64(n)}); return string(b) }

// The macOS guest orders pauses and resumes by their generation exactly as the Linux guest does
// (PauseFence): a late continue after a newer stop, and a stop after its own continue, are answered
// 409 and signal nothing; a resume with no generation after a fenced pause is the stale_unfenced_resume
// 409; a pause with none is still obeyed.
func TestDarwinPauseAndResumeHonourTheGeneration(t *testing.T) {
	r := newSignalRig(t, t.TempDir())
	for i, step := range []struct {
		pause  bool
		body   string
		status int
	}{
		{true, gen(2), 200},  // pause 2's stop
		{false, gen(1), 409}, // pause 1's continue, delivered late
		{false, gen(2), 200}, // pause 2's resume
		{true, gen(2), 409},  // pause 2's stop again, after its continue
		{true, gen(3), 200},  // pause 3
		{false, "", 409},     // a resume from a side that predates the generation
		{true, "", 200},      // a pause from one is obeyed
		{false, gen(3), 200}, // pause 3's resume still runs the agent
	} {
		code, body, fenced := r.post(t, step.pause, step.body)
		if code != step.status {
			t.Fatalf("step %d: pause=%v %q = %d %s, want %d", i, step.pause, step.body, code, body, step.status)
		}
		if code == 200 && fenced != (step.body != "") {
			t.Fatalf("step %d: fenced_pause = %v for body %q", i, fenced, step.body)
		}
		if step.body == "" && code == 409 && !strings.Contains(body, ReasonStaleUnfencedResume) {
			t.Fatalf("step %d: the unfenced resume's 409 = %q, want %s", i, body, ReasonStaleUnfencedResume)
		}
	}
	want := []unix.Signal{unix.SIGSTOP, unix.SIGCONT, unix.SIGSTOP, unix.SIGSTOP, unix.SIGCONT}
	if got := r.signals(); !slices.Equal(got, want) {
		t.Fatalf("signals sent = %v, want %v", got, want)
	}
}

// A guest image's agent before the generation got empty bodies; on a fresh boot they act as before.
func TestDarwinPauseWithNoGenerationIsSignalledAsBefore(t *testing.T) {
	r := newSignalRig(t, t.TempDir())
	for _, pause := range []bool{true, false, true, false} {
		if code, body, fenced := r.post(t, pause, ""); code != 200 || fenced {
			t.Fatalf("pause=%v with no generation = %d %s fenced %v, want applied unfenced", pause, code, body, fenced)
		}
	}
	if got := r.signals(); !slices.Equal(got, []unix.Signal{unix.SIGSTOP, unix.SIGCONT, unix.SIGSTOP, unix.SIGCONT}) {
		t.Fatalf("signals sent = %v", got)
	}
}

// The order outlives a restart of the agent's service (the marker directory keeps it), not a boot.
func TestDarwinThePauseOrderOutlivesAServiceRestartNotABoot(t *testing.T) {
	boot := t.TempDir()
	r := newSignalRig(t, boot)
	r.post(t, true, gen(2))
	r.post(t, false, gen(2))

	restarted := newSignalRig(t, boot)
	if code, _, _ := restarted.post(t, true, gen(2)); code != 409 {
		t.Fatalf("after a service restart, a stop after its own continue = %d, want 409", code)
	}
	if code, _, _ := restarted.post(t, false, gen(1)); code != 409 {
		t.Fatalf("after a service restart, an older continue = %d, want 409", code)
	}
	if got := restarted.signals(); len(got) != 0 {
		t.Fatalf("the restarted agent signalled %v", got)
	}

	booted := newSignalRig(t, t.TempDir())
	if code, body, _ := booted.post(t, true, gen(1)); code != 200 {
		t.Fatalf("a new boot's first pause = %d %s", code, body)
	}
}

// Init sets the host's password in Screen Sharing and says whether it did: an Init with none (from a
// runner that predates it), and one whose settings write fails, answer desktop_password_set false, so the
// helper's shim cannot log in and refuses the desktop. The agent never makes a password of its own.
func TestDarwinInitReportsWhetherTheDesktopPasswordIsSet(t *testing.T) {
	s, _, h := newDarwinServer(t)
	if w := postInit(h, Init{}); w.Code != 200 || !strings.Contains(w.Body.String(), `"desktop_password_set":false`) {
		t.Fatalf("Init with no password = %d %s", w.Code, w.Body)
	}
	if _, ok := s.Desktop.Password(); ok {
		t.Error("the agent made a password of its own")
	}
	if _, err := os.Stat(s.Desktop.VNCSettings); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the VNC settings file was written with no password: %v", err)
	}

	s, _, h = newDarwinServer(t)
	s.Desktop.VNCSettings = filepath.Join(t.TempDir(), "missing", "com.apple.VNCSettings.txt")
	w := postInit(h, Init{DesktopPassword: "Abcd2345"})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"desktop_password_set":false`) || strings.Contains(w.Body.String(), "Abcd2345") {
		t.Fatalf("Init whose settings write fails = %d %s", w.Code, w.Body)
	}
	if _, ok := s.Desktop.Password(); ok {
		t.Error("a password that was not set is held")
	}
}

// toolsRoutes is one request per tools-mode route, for the 404-on-the-control-session test.
var toolsRoutes = []struct{ method, path, body string }{
	{"POST", "/v1/tools/push?dest=/usr/local/bin/x&mode=755&size=1&sha256=" + hex.EncodeToString(make([]byte, 32)), ""}, // 400 before any write
	{"POST", "/v1/tools/exec", `{"argv":["/usr/sbin/softwareupdate","-l"]}`},
	{"GET", "/v1/tools/pointer", ""},
	{"POST", "/v1/tools/desktop_password", `{"password":"Abcd2345"}`},
}

// The control session serves no tools request: each answers 404 there, while the session that arrived
// on the tools port (ToolsHandler) serves them. The control session does answer the tools dial's report.
func TestDarwinToolsRoutesAre404OnTheControlSession(t *testing.T) {
	s, _, h := newDarwinServer(t)
	tools := (&Tools{Desktop: s.Desktop, run: func(context.Context, []string) ([]byte, int, error) { return nil, 0, nil }}).ToolsHandler()
	for _, rt := range toolsRoutes {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(rt.method, rt.path, strings.NewReader(rt.body)))
		if w.Code != http.StatusNotFound {
			t.Errorf("control session: %s %s = %d, want 404", rt.method, rt.path, w.Code)
		}
		w = httptest.NewRecorder()
		tools.ServeHTTP(w, httptest.NewRequest(rt.method, rt.path, strings.NewReader(rt.body)))
		if w.Code == http.StatusNotFound {
			t.Errorf("tools session: %s %s = 404", rt.method, rt.path)
		}
	}
	// The pointer is read through tools mode only: the control session's old route is gone.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/v1/desktop/pointer", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("control session: GET /v1/desktop/pointer = %d, want 404", w.Code)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/v1/dial/tools", nil))
	if w.Code != 200 {
		t.Errorf("GET /v1/dial/tools on the control session = %d", w.Code)
	}
}

// Init trusts the run CA before it answers, so before the host starts any agent process, and says
// how it went in fixed codes. A CA that is not trusted still answers 200: the run goes on. An Init with
// no run CA tries nothing and answers no keychain field.
func TestDarwinInitTrustsTheRunCABeforeItAnswers(t *testing.T) {
	for _, tc := range []struct{ status, reason string }{{KeychainTrusted, ""}, {KeychainNotTrusted, KeychainRefused}} {
		s, _, h := newDarwinServer(t)
		var got []string
		s.TrustRunCA = func(_ context.Context, runCA string) (string, string) {
			if s.inited {
				t.Error("the run CA was trusted after Init answered")
			}
			got = append(got, runCA)
			return tc.status, tc.reason
		}
		w := postInit(h, Init{RunCA: "run-ca-pem"})
		var res InitResult
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &res) != nil {
			t.Fatalf("Init = %d %s", w.Code, w.Body)
		}
		if len(got) != 1 || got[0] != "run-ca-pem" || res.KeychainTrust != tc.status || res.KeychainTrustReason != tc.reason {
			t.Errorf("trusted %q, answered %q %q, want %q %q", got, res.KeychainTrust, res.KeychainTrustReason, tc.status, tc.reason)
		}
	}
	s, _, h := newDarwinServer(t)
	s.TrustRunCA = func(context.Context, string) (string, string) {
		t.Error("an Init with no run CA trusted one")
		return KeychainTrusted, ""
	}
	if w := postInit(h, Init{}); w.Code != 200 || strings.Contains(w.Body.String(), "keychain_trust") {
		t.Errorf("Init with no run CA = %d %s", w.Code, w.Body)
	}
}
