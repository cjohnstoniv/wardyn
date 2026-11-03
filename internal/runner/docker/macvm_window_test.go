// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && unix

package docker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// fakeWindowHelper serves the helper's side of one ctl.sock connection: it answers hello, then reads the
// window request and answers it with answer. It returns the request it read and the connection's reader,
// for a test that goes on to end the window.
func fakeWindowHelper(t *testing.T, c net.Conn, proto int, answer string) (<-chan macvm.ControlRequest, *bufio.Reader) {
	t.Helper()
	rd := bufio.NewReader(c)
	got := make(chan macvm.ControlRequest, 1)
	line := func(v any) {
		b, _ := json.Marshal(v)
		_, _ = c.Write(append(b, '\n'))
	}
	ready := make(chan struct{})
	go func() {
		defer close(ready)
		hello, err := rd.ReadBytes('\n')
		var h macvm.ControlRequest
		if err != nil || json.Unmarshal(hello, &h) != nil || h.Op != macvm.OpHello {
			t.Errorf("first line %q is not a hello: %v", hello, err)
			return
		}
		line(macvm.ControlReply{OK: true, Proto: proto})
		if proto != macvm.ControlProto {
			return
		}
		req, err := rd.ReadBytes('\n')
		if err != nil {
			return
		}
		var r macvm.ControlRequest
		if json.Unmarshal(req, &r) != nil {
			t.Errorf("request %q", req)
			return
		}
		got <- r
		_, _ = c.Write([]byte(answer + "\n"))
	}()
	t.Cleanup(func() { <-ready })
	return got, rd
}

func openFake(t *testing.T, proto int, answer string) (runner.MacWindow, net.Conn, <-chan macvm.ControlRequest, error) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	got, _ := fakeWindowHelper(t, b, proto, answer)
	w, err := startMacWindow(t.Context(), a, runner.MacWindowRequest{GrantID: "g1", Title: "Run, for Pat"})
	return w, b, got, err
}

func waitEnded(t *testing.T, w runner.MacWindow) {
	t.Helper()
	select {
	case <-w.Ended():
	case <-time.After(5 * time.Second):
		t.Fatal("the window did not end")
	}
}

// The window op carries the grant and the title; the window is the connection, which the helper ends with
// one line saying why. Each reason the helper gives reaches the runner as its own.
func TestMacWindowHelperEndReasons(t *testing.T) {
	for helper, want := range map[string]string{
		macvm.WindowClosedPerson:     runner.MacWindowEndPerson,
		macvm.WindowClosedHostLocked: runner.MacWindowEndHostLocked,
		macvm.WindowClosedRunEnded:   runner.MacWindowEndRunEnded,
		"failed":                     runner.MacWindowEndHelperFailed,
		"a newer reason":             runner.MacWindowEndHelperFailed,
	} {
		w, helperConn, got, err := openFake(t, macvm.ControlProto, `{"ok":true}`)
		if err != nil {
			t.Fatal(err)
		}
		if r := <-got; r.Op != macvm.OpWindow || r.Grant != "g1" || r.Title != "Run, for Pat" {
			t.Fatalf("the helper was asked %+v", r)
		}
		select {
		case <-w.Ended():
			t.Fatal("a window that is open reads as ended")
		case <-time.After(100 * time.Millisecond):
		}
		_, _ = helperConn.Write([]byte(`{"ok":true,"event":{"type":"window_closed","detail":"` + helper + `"}}` + "\n"))
		_ = helperConn.Close()
		waitEnded(t, w)
		if w.Reason() != want {
			t.Errorf("helper reason %q = %q, want %q", helper, w.Reason(), want)
		}
	}
}

// A helper whose connection ends without a word is a lost helper.
func TestMacWindowHelperVanishing(t *testing.T) {
	w, helperConn, got, err := openFake(t, macvm.ControlProto, `{"ok":true}`)
	if err != nil {
		t.Fatal(err)
	}
	<-got
	_ = helperConn.Close()
	waitEnded(t, w)
	if w.Reason() != runner.MacWindowEndHelperLost {
		t.Fatalf("reason %q, want %q", w.Reason(), runner.MacWindowEndHelperLost)
	}
}

// Closing the window from the runner's side closes the helper's connection, which is what closes the
// window; it ends with no reason, and a second close is harmless.
func TestMacWindowCloseClosesTheHelperConnection(t *testing.T) {
	w, helperConn, got, err := openFake(t, macvm.ControlProto, `{"ok":true}`)
	if err != nil {
		t.Fatal(err)
	}
	<-got
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	_ = helperConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadAll(helperConn); err != nil {
		t.Fatalf("the helper's connection did not reach EOF after the runner closed: %v", err)
	}
	waitEnded(t, w)
	if w.Reason() != "" {
		t.Fatalf("a close of our own has reason %q", w.Reason())
	}
	_ = w.Close()
}

// A helper that does not know the window op (it started before this runner was upgraded), or speaks
// another control protocol, fails closed; so do the helper's own refusals, each its own error.
func TestMacWindowRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		proto  int
		answer string
		is     error
		text   string
	}{
		"helper too old":   {macvm.ControlProto, `{"ok":false,"error":"unknown op window"}`, runner.ErrMacWindowHelperOld, ""},
		"another protocol": {macvm.ControlProto + 1, "", runner.ErrMacWindowHelperOld, ""},
		"already open":     {macvm.ControlProto, `{"ok":false,"error":"This run's window is already open.","code":"window_open"}`, runner.ErrMacWindowOpen, ""},
		"not at the Mac":   {macvm.ControlProto, `{"ok":false,"error":"The window didn't open: you aren't signed in.","code":"window_not_at_mac"}`, nil, "you aren't signed in"},
		"anything else":    {macvm.ControlProto, `{"ok":false,"error":"the VM is not running"}`, nil, "the VM is not running"},
	} {
		w, helperConn, _, err := openFake(t, tc.proto, tc.answer)
		if w != nil {
			t.Errorf("%s: a window opened", name)
			continue
		}
		if tc.is != nil && !errors.Is(err, tc.is) || tc.is == nil && (err == nil || !strings.Contains(err.Error(), tc.text)) {
			t.Errorf("%s: %v, want %v %q", name, err, tc.is, tc.text)
		}
		var rf *runner.MacWindowRefusal
		if name == "not at the Mac" && (!errors.As(err, &rf) || rf.Code != "window_not_at_mac") {
			t.Errorf("%s: %v is not the helper's coded refusal", name, err)
		}
		_ = helperConn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadAll(helperConn); err != nil {
			t.Errorf("%s: the refused connection was left open: %v", name, err)
		}
	}
}

// A helper that never answers does not hold the open for ever, and the connection is closed.
func TestMacWindowStuckHelperFailsClosed(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	ctx, cancel := context.WithCancel(t.Context())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	if w, err := startMacWindow(ctx, a, runner.MacWindowRequest{GrantID: "g"}); err == nil || w != nil {
		t.Fatalf("a helper that never answered = %v, %v", w, err)
	}
	_ = b.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.Copy(io.Discard, b); err != nil {
		t.Fatalf("the stuck helper's connection was left open: %v", err)
	}
}

// watchFeed starts watchWindowEvents on one end of a pipe, answers its hello and events requests, and
// returns the helper's end and the watcher's result.
func watchFeed(t *testing.T, runID uuid.UUID, report func(uuid.UUID, string, time.Time)) (net.Conn, <-chan error) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { _ = b.Close() })
	done := make(chan error, 1)
	go func() { done <- watchWindowEvents(t.Context(), a, runID, report) }()
	rd := bufio.NewReader(b)
	for _, want := range []string{macvm.OpHello, macvm.OpEvents} {
		line, err := rd.ReadBytes('\n')
		var req macvm.ControlRequest
		if err != nil || json.Unmarshal(line, &req) != nil || req.Op != want {
			t.Fatalf("watcher sent %q (%v), want op %s", line, err, want)
		}
		if want == macvm.OpHello {
			_, _ = b.Write([]byte(`{"ok":true,"proto":1}` + "\n"))
		} else {
			_, _ = b.Write([]byte(`{"ok":true}` + "\n"))
		}
	}
	return b, done
}

func windowEvent(detail, grant string) string {
	return `{"ok":true,"event":{"type":"window","detail":"` + detail + `","grant":"` + grant + `"}}` + "\n"
}

// The events watcher reports a window open whose grant this runner did not issue (someone ran the op on
// the Mac), never one it issued, never a close, and keeps watching after each.
func TestMacWindowWatcherReportsOnlyStrangers(t *testing.T) {
	runID := uuid.New()
	issueMacWindow(runID, "ours")
	defer forgetMacWindow(runID, "ours")
	seen := make(chan string, 4)
	b, done := watchFeed(t, runID, func(id uuid.UUID, grant string, at time.Time) {
		if id != runID || at.IsZero() {
			t.Errorf("report for %s at %v", id, at)
		}
		seen <- grant
	})
	for _, ev := range []string{
		`{"ok":true,"event":{"type":"guest_agent","detail":"connected"}}` + "\n",
		windowEvent("open", "ours"),
		windowEvent("closed", "stranger"),
		windowEvent("open", "stranger"),
		`{"ok":true,"event":{"type":"window","detail":"open"}}` + "\n",
	} {
		_, _ = b.Write([]byte(ev))
	}
	_ = b.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got := []string{<-seen, <-seen}
	slices.Sort(got)
	if got[0] != "" || got[1] != "stranger" {
		t.Fatalf("reported grants %q, want [\"\", stranger]", got)
	}
	select {
	case g := <-seen:
		t.Fatalf("a third report: %q", g)
	case <-time.After(100 * time.Millisecond):
	}
}

// A report that never returns does not stop the events read: later events are still read, and the helper
// (which drops events for a reader that falls behind) is never kept waiting.
func TestMacWindowWatcherReportNeverBlocksTheRead(t *testing.T) {
	runID := uuid.New()
	issueMacWindow(runID, "ours")
	defer forgetMacWindow(runID, "ours")
	stall := make(chan struct{})
	defer close(stall)
	b, done := watchFeed(t, runID, func(uuid.UUID, string, time.Time) { <-stall })
	wrote := make(chan struct{})
	go func() {
		defer close(wrote)
		for _, ev := range []string{windowEvent("open", "stranger"), windowEvent("closed", "ours"), windowEvent("open", "another")} {
			_, _ = b.Write([]byte(ev)) // a pipe write returns only once the watcher has read it
		}
	}()
	select {
	case <-wrote:
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher stopped reading while a report was stalled")
	}
	if issuedMacWindow(runID, "ours") {
		t.Fatal("an issued grant outlived the helper's closed event for it")
	}
	_ = b.Close()
	<-done
}

// An issued grant is forgotten on the helper's closed event for it, not before: its open is not reported,
// and a later open with the same grant, once it has closed, is.
func TestMacWindowWatcherForgetsAGrantOnTheHelpersClose(t *testing.T) {
	runID := uuid.New()
	issueMacWindow(runID, "g1")
	defer forgetMacWindow(runID, "g1")
	seen := make(chan string, 2)
	b, done := watchFeed(t, runID, func(_ uuid.UUID, grant string, _ time.Time) { seen <- grant })
	_, _ = b.Write([]byte(windowEvent("open", "g1")))
	_, _ = b.Write([]byte(windowEvent("closed", "g1")))
	_, _ = b.Write([]byte(windowEvent("open", "g1")))
	_ = b.Close()
	<-done
	if g := <-seen; g != "g1" {
		t.Fatalf("reported %q", g)
	}
	select {
	case g := <-seen:
		t.Fatalf("the open before the close was reported too: %q", g)
	case <-time.After(100 * time.Millisecond):
	}
}

// A sandbox that is no macOS VM has no window; a macOS VM's window is asked on the ctl.sock of its
// checked socket directory.
func TestMacVMOpenWindowDialsTheRunsControlSocket(t *testing.T) {
	r := newMacRig(t)
	if _, err := r.d.OpenWindow(context.Background(), "not-a-vm", runner.MacWindowRequest{GrantID: "g"}); !errors.Is(err, runner.ErrMacWindowUnsupported) {
		t.Fatalf("a sandbox that is no macOS VM = %v", err)
	}
	spec := macSpec()
	sb, err := r.d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	dialled := make(chan string, 1)
	r.d.vmDial = func(_ context.Context, path string) (net.Conn, error) {
		dialled <- path
		a, b := net.Pipe()
		fakeWindowHelper(t, b, macvm.ControlProto, `{"ok":true}`)
		return a, nil
	}
	w, err := r.d.OpenWindow(context.Background(), sb.Ref, runner.MacWindowRequest{GrantID: "g1"})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if got, want := <-dialled, filepath.Join(r.c.sockDir(spec.RunID), macvm.ControlSock); got != want {
		t.Fatalf("dialled %s, want %s", got, want)
	}
	r.d.vmDial = nil
	if err := r.d.StopSandbox(context.Background(), sb.Ref); err != nil {
		t.Fatal(err)
	}
}
