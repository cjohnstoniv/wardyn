// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/localviewer"
	"github.com/cjohnstoniv/wardyn/internal/types"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// windowFixture is a Linux VM run on a runner of this computer: the runner's socket hands out a window
// nonce, and the organisation's grant for it sends the bound frame, then the runner's side of the display.
type windowFixture struct {
	runID, runnerID uuid.UUID
	srv             *httptest.Server

	mu     sync.Mutex
	hello  map[string]any
	conn   net.Conn // the CLI's connection, once it said hello
	posted string   // the local_nonce the CLI posted
	posts  int      // how many opens the CLI posted
	got    []byte   // what the viewer wrote to the runner
	done   chan struct{}
	// answer, when set before the CLI says hello, answers the hello in place of a window nonce.
	answer func(c net.Conn)
}

func newWindowFixture(t *testing.T, grant func(w http.ResponseWriter, c net.Conn), end string) *windowFixture {
	t.Helper()
	f := &windowFixture{runID: uuid.New(), runnerID: uuid.New(), done: make(chan struct{})}
	sockDir, err := os.MkdirTemp("", "ww") // short: a unix socket path is at most 104 bytes on macOS
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "attach.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		typ, b, err := readLocalFrame(c)
		var h map[string]any
		if err != nil || typ != localHello || json.Unmarshal(b, &h) != nil {
			_ = c.Close()
			return
		}
		f.mu.Lock()
		f.hello, f.conn = h, c
		answer := f.answer
		f.mu.Unlock()
		if answer != nil {
			answer(c)
			return
		}
		_ = writeLocalJSONForTest(c, localHelloOK, map[string]string{"runner_id": f.runnerID.String(), "nonce": "nonce-1"})
	}()
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/runs/"+f.runID.String():
			_ = json.NewEncoder(w).Encode(types.AgentRun{ID: f.runID, State: types.RunRunning, MachineKind: types.MachineLinuxVM,
				Placement: types.PlacementLocal, RunnerID: &f.runnerID, RunnerName: "laptop", Title: "build"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/runs/"+f.runID.String()+"/desktop/local-window":
			var body struct {
				Nonce string `json:"local_nonce"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.posted = body.Nonce
			f.posts++
			c := f.conn
			f.mu.Unlock()
			if grant != nil {
				grant(w, c)
				return
			}
			_ = writeLocalJSONForTest(c, localBound, map[string]any{})
			go f.runner(c, end)
			_ = json.NewEncoder(w).Encode(map[string]any{"grant_id": uuid.New(), "runner_id": f.runnerID, "runner": "laptop"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	dir := filepath.Join(t.TempDir(), "local-attach")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]string{"runner_id": f.runnerID.String(), "org_url": f.srv.URL + "/", "socket": sock})
	if err := os.WriteFile(filepath.Join(dir, f.runnerID.String()+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	oldDir, oldRun, oldAvail := localAttachDir, localViewerRun, localViewerAvailable
	t.Cleanup(func() { localAttachDir, localViewerRun, localViewerAvailable = oldDir, oldRun, oldAvail })
	localAttachDir = func() (string, error) { return dir, nil }
	localViewerAvailable = func() error { return nil }
	return f
}

// runner is the runner's side of a granted window: the display's first bytes, the viewer's reply, then
// the end frame with reason (none: it waits for the CLI to close the socket).
func (f *windowFixture) runner(c net.Conn, end string) {
	defer close(f.done)
	_ = writeLocalFrame(c, localData, []byte("RFB 003.008\n"))
	typ, b, err := readLocalFrame(c)
	if err != nil || typ != localData {
		return
	}
	f.mu.Lock()
	f.got = b
	f.mu.Unlock()
	if end == "" {
		_, _ = io.Copy(io.Discard, c)
		return
	}
	_ = writeLocalJSONForTest(c, localEnd, map[string]string{"reason": end})
	_ = c.Close()
}

// TestWindowLocalGrantBoundEnd drives `wardyn window` on a Linux VM run through the window hello, the
// organisation's grant for its nonce, the bound frame, the display both ways and the runner's end.
func TestWindowLocalGrantBoundEnd(t *testing.T) {
	// viewer reads the display's first bytes and answers them, then reads until the stream ends (closed
	// false) or closes the window itself (closed true).
	viewer := func(closed bool, saw *[]byte) func(context.Context, io.ReadWriteCloser, string) error {
		return func(_ context.Context, rw io.ReadWriteCloser, title string) error {
			defer rw.Close()
			if title != "Wardyn: build" {
				return errors.New("title " + title)
			}
			b := make([]byte, 12)
			if _, err := io.ReadFull(rw, b); err != nil {
				return err
			}
			*saw = b
			if _, err := rw.Write([]byte("RFB 003.008\n")); err != nil {
				return err
			}
			if closed {
				return nil
			}
			_, err := io.Copy(io.Discard, rw)
			return err
		}
	}
	for _, tc := range []struct {
		name, end string
		closed    bool
		wantErr   string
		wantOut   string
	}{
		{name: "the run's display closes", end: "run_ended", wantOut: "display closed"},
		{name: "the person closes the window", closed: true, wantOut: "The window closed. The run goes on."},
		{name: "the organisation ends it", end: "ended_by_organisation", wantErr: "The organisation closed the window"},
		{name: "an unknown end", end: "something_new", wantErr: "The runner closed the window: something_new."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWindowFixture(t, nil, tc.end)
			var saw []byte
			localViewerRun = viewer(tc.closed, &saw)
			out, err := execCmdCapture(t, "window", f.runID.String(), "--url", f.srv.URL, "--token", "t")
			<-f.done
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.hello["kind"] != "window" || f.hello["run_id"] != f.runID.String() {
				t.Fatalf("hello = %v, want kind window for the run", f.hello)
			}
			if f.posted != "nonce-1" {
				t.Fatalf("posted local_nonce %q, want the socket's nonce-1", f.posted)
			}
			if string(saw) != "RFB 003.008\n" || string(f.got) != "RFB 003.008\n" {
				t.Fatalf("viewer saw %q, runner got %q", saw, f.got)
			}
			if !strings.Contains(out, "The window is open on laptop.") {
				t.Fatalf("output = %q", out)
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || !strings.Contains(out, tc.wantOut) {
				t.Fatalf("err = %v, output = %q, want %q", err, out, tc.wantOut)
			}
		})
	}
}

// The organisation's refusal is its own sentence, and the viewer never starts.
func TestWindowLocalRefusalIsTheServersSentence(t *testing.T) {
	const sentence = "Windows onto Linux VM runs are turned off for this organisation."
	f := newWindowFixture(t, func(w http.ResponseWriter, _ net.Conn) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": sentence, "reason": "local_window_site_off"})
	}, "")
	localViewerRun = func(context.Context, io.ReadWriteCloser, string) error {
		t.Error("the viewer started on a refusal")
		return nil
	}
	_, err := execCmdCapture(t, "window", f.runID.String(), "--url", f.srv.URL, "--token", "t")
	if err == nil || err.Error() != sentence || exitCodeFor(err) != 2 {
		t.Fatalf("err = %v (exit %d), want exactly %q with exit 2", err, exitCodeFor(err), sentence)
	}
}

// A computer that cannot draw the window says so before it asks the runner for anything.
func TestWindowLocalNeedsLinux(t *testing.T) {
	f := newWindowFixture(t, nil, "")
	localViewerAvailable = func() error { return localviewer.ErrUnsupportedOS }
	_, err := execCmdCapture(t, "window", f.runID.String(), "--url", f.srv.URL, "--token", "t")
	if err == nil || err.Error() != localWindowOSSentence {
		t.Fatalf("err = %v, want %q", err, localWindowOSSentence)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hello != nil {
		t.Fatalf("the runner was asked for a nonce: %v", f.hello)
	}
}

// refuse answers an open as the organisation does: status, reason and sentence.
func refuse(status int, reason, sentence string) func(http.ResponseWriter, net.Conn) {
	return func(w http.ResponseWriter, _ net.Conn) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": sentence, "reason": reason})
	}
}

// With no window nonce on this computer (here, no pointer file), the CLI still asks the organisation, with
// no nonce, and shows its refusal; only the organisation's missing-nonce answer leaves the local reason.
func TestWindowLocalNoNonceAsksTheOrganisation(t *testing.T) {
	const siteOff = "Opening a Linux VM run in a window on your own computer is turned off for this install."
	for _, tc := range []struct {
		name    string
		grant   func(http.ResponseWriter, net.Conn)
		wantErr string
	}{
		{"the organisation refuses first", refuse(http.StatusForbidden, "local_window_site_off", siteOff), siteOff},
		{"the organisation needs the nonce", refuse(http.StatusBadRequest, "invalid_request_body", "The request needs local_nonce."),
			"a window onto a Linux VM run opens only on the computer its runner is on, and that runner handed out no window nonce here (no usable pointer file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWindowFixture(t, tc.grant, "")
			empty := t.TempDir()
			localAttachDir = func() (string, error) { return empty, nil }
			localViewerRun = func(context.Context, io.ReadWriteCloser, string) error {
				t.Error("the viewer started with no nonce")
				return nil
			}
			_, err := execCmdCapture(t, "window", f.runID.String(), "--url", f.srv.URL, "--token", "t")
			if err == nil || !strings.HasPrefix(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to start %q", err, tc.wantErr)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.posts != 1 || f.posted != "" || f.hello != nil {
				t.Fatalf("posts=%d posted=%q hello=%v, want one open with no nonce and no hello", f.posts, f.posted, f.hello)
			}
		})
	}
}

// A macOS VM run still opens through the Mac window route, and its refusal is that open's sentence: the run
// read answers 200, so the refusal comes from the open.
func TestWindowMacRefusalIsTheOpensSentence(t *testing.T) {
	const sentence = "This run's Mac isn't connected to this server."
	id := uuid.New()
	var mu sync.Mutex
	var last string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		last = r.Method + " " + r.URL.Path
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(types.AgentRun{ID: id, State: types.RunRunning, MachineKind: types.MachineMacOSVM})
			return
		}
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": sentence})
	}))
	t.Cleanup(srv.Close)
	_, err := execCmdCapture(t, "window", id.String(), "--url", srv.URL, "--token", "t")
	if err == nil || err.Error() != sentence || exitCodeFor(err) != 3 {
		t.Fatalf("err = %v (exit %d), want exactly %q with exit 3", err, exitCodeFor(err), sentence)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := "POST /api/v1/runs/" + id.String() + "/desktop/window"; last != want {
		t.Fatalf("last request = %q, want %q", last, want)
	}
}

// The window hello's answers, as openLocalWindowSocket reads them.
func TestOpenLocalWindowSocket(t *testing.T) {
	for _, tc := range []struct {
		name    string
		remote  bool
		answer  func(f *windowFixture) func(net.Conn)
		wantWhy string
	}{
		{name: "the run is not on a person's runner", remote: true, wantWhy: "the run is not on a person's runner"},
		{name: "the runner refuses the hello", answer: func(*windowFixture) func(net.Conn) {
			return func(c net.Conn) {
				_ = writeLocalJSONForTest(c, localRefuse, map[string]string{"reason": "too_many_pending"})
			}
		}, wantWhy: "the runner refused: too_many_pending"},
		{name: "the socket answers for another runner", answer: func(*windowFixture) func(net.Conn) {
			return func(c net.Conn) {
				_ = writeLocalJSONForTest(c, localHelloOK, map[string]string{"runner_id": uuid.NewString(), "nonce": "nonce-1"})
			}
		}, wantWhy: "the runner's socket answered for another runner"},
		{name: "the socket closes", answer: func(*windowFixture) func(net.Conn) {
			return func(c net.Conn) { _ = c.Close() }
		}, wantWhy: "the runner's socket closed: "},
		{name: "a window nonce", wantWhy: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWindowFixture(t, nil, "")
			if tc.answer != nil {
				f.mu.Lock()
				f.answer = tc.answer(f)
				f.mu.Unlock()
			}
			run := types.AgentRun{ID: f.runID, Placement: types.PlacementLocal, RunnerID: &f.runnerID, RunnerName: "laptop"}
			if tc.remote {
				run.Placement = types.PlacementRemote
			}
			lc, why := openLocalWindowSocket(context.Background(), &sdk.Client{BaseURL: f.srv.URL}, run)
			if lc != nil {
				defer lc.conn.Close()
			}
			if tc.wantWhy == "" {
				if lc == nil || lc.nonce != "nonce-1" || lc.runner != "laptop" {
					t.Fatalf("lc = %+v, why = %q", lc, why)
				}
				f.mu.Lock()
				defer f.mu.Unlock()
				if f.hello["kind"] != "window" {
					t.Fatalf("hello = %v, want kind window", f.hello)
				}
				return
			}
			if lc != nil || !strings.HasPrefix(why, tc.wantWhy) {
				t.Fatalf("lc = %v, why = %q, want it to start %q", lc, why, tc.wantWhy)
			}
		})
	}
}

// The frames before the bind: the runner's refusal or end is its sentence, anything else is a protocol
// failure, and the viewer never starts.
func TestWindowLocalFramesBeforeBind(t *testing.T) {
	for _, tc := range []struct {
		name    string
		send    func(c net.Conn)
		wantErr string
	}{
		{"the runner ends the window", func(c net.Conn) {
			_ = writeLocalJSONForTest(c, localEnd, map[string]string{"reason": "runner_link_lost"})
		}, "The runner lost its link to the organisation"},
		{"the runner refuses", func(c net.Conn) {
			_ = writeLocalJSONForTest(c, localRefuse, map[string]string{"reason": "nonce_kind_mismatch"})
		}, "The runner was asked for a terminal on a window's nonce"},
		{"an unexpected frame", func(c net.Conn) { _ = writeLocalFrame(c, localData, []byte("RFB")) },
			"the runner's socket sent an unexpected frame before the window opened"},
		{"the socket closes", func(c net.Conn) { _ = c.Close() }, "the runner's socket closed before the window opened"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWindowFixture(t, func(w http.ResponseWriter, c net.Conn) {
				tc.send(c)
				_ = json.NewEncoder(w).Encode(map[string]any{"grant_id": uuid.New(), "runner_id": uuid.New(), "runner": "laptop"})
			}, "")
			localViewerRun = func(context.Context, io.ReadWriteCloser, string) error {
				t.Error("the viewer started before the bind")
				return nil
			}
			_, err := execCmdCapture(t, "window", f.runID.String(), "--url", f.srv.URL, "--token", "t")
			if err == nil || !strings.HasPrefix(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to start %q", err, tc.wantErr)
			}
		})
	}
}
