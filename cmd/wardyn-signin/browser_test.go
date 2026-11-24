// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestBrowserFlagsUseThePipeOnly(t *testing.T) {
	flags := browserFlags("/profile")
	if !slices.Contains(flags, "--remote-debugging-pipe") {
		t.Fatalf("browserFlags has no --remote-debugging-pipe: %v", flags)
	}
	for _, f := range flags {
		for _, bad := range []string{"--remote-debugging-port", "--remote-debugging-address", "--load-extension", "--disable-extensions-except"} {
			if strings.HasPrefix(f, bad) {
				t.Errorf("browserFlags carries %s", f)
			}
		}
	}
}

func TestBrowserArgsRefused(t *testing.T) {
	for _, a := range []string{
		"--remote-debugging-port=1", "--remote-debugging-port", "--remote-debugging-address=0.0.0.0",
		"--load-extension=x", "--disable-extensions-except=x", "--user-data-dir=x", "file:///etc/passwd",
		"--headless=old", "chrome://version", "about:flags", "javascript:alert(1)", "data:text/html,x",
		"view-source:https://x", "HTTPS://x", "", "--",
	} {
		if out, err := browserArgs([]string{"--headless", a}); err == nil {
			t.Errorf("browserArgs accepted %q: %v", a, out)
		}
	}
	got, err := browserArgs([]string{"--headless", "https://example.com/x", "http://localhost:3000", "about:blank"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--headless=new", "https://example.com/x", "http://localhost:3000", "about:blank"}
	if !slices.Equal(got, want) {
		t.Fatalf("browserArgs = %v, want %v", got, want)
	}
}

// fakeInfo is a file the fake stat reports.
type fakeInfo struct {
	mode fs.FileMode
	uid  uint32
}

func (f fakeInfo) Name() string       { return "chromium" }
func (f fakeInfo) Size() int64        { return 0 }
func (f fakeInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeInfo) Sys() any           { return &syscall.Stat_t{Uid: f.uid} }

func fakeStat(files map[string]fakeInfo) func(string) (fs.FileInfo, error) {
	return func(p string) (fs.FileInfo, error) {
		if fi, ok := files[p]; ok {
			return fi, nil
		}
		return nil, fs.ErrNotExist
	}
}

func TestBrowserBinary_FixedAllowlistNeverPATH(t *testing.T) {
	const lib, wrapper = "/usr/lib/chromium/chromium", "/usr/bin/chromium"
	if !slices.Equal(browserBinaries, []string{lib, wrapper}) {
		t.Fatalf("browserBinaries = %v", browserBinaries)
	}
	good := fakeInfo{mode: 0o755}
	for name, tc := range map[string]struct {
		lib  *fakeInfo
		want string
	}{
		"both present":        {lib: &good, want: lib},
		"first absent":        {want: wrapper},
		"first agent-owned":   {lib: &fakeInfo{mode: 0o755, uid: 1000}, want: wrapper},
		"first group-written": {lib: &fakeInfo{mode: 0o775}, want: wrapper},
		"first other-written": {lib: &fakeInfo{mode: 0o757}, want: wrapper},
		"first a directory":   {lib: &fakeInfo{mode: fs.ModeDir | 0o755}, want: wrapper},
	} {
		files := map[string]fakeInfo{wrapper: good}
		if tc.lib != nil {
			files[lib] = *tc.lib
		}
		got, err := browserBinary(browserBinaries, fakeStat(files))
		if err != nil || got != tc.want {
			t.Errorf("%s: browserBinary = %q, %v; want %q", name, got, err, tc.want)
		}
	}
	if got, err := browserBinary(browserBinaries, fakeStat(map[string]fakeInfo{lib: {mode: 0o755, uid: 1000}})); err == nil {
		t.Errorf("no root-owned binary: browserBinary = %q, want an error", got)
	}

	// A chromium on the agent's PATH is never chosen, even one the stat calls root-owned.
	dir := t.TempDir()
	planted := filepath.Join(dir, "chromium")
	if err := os.WriteFile(planted, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	if got, err := browserBinary(browserBinaries, fakeStat(map[string]fakeInfo{planted: good})); err == nil || got == planted {
		t.Errorf("PATH chromium: browserBinary = %q, %v; want an error", got, err)
	}
	if got, err := browserBinary(browserBinaries, os.Stat); got == planted || (err == nil && !slices.Contains(browserBinaries, got)) {
		t.Errorf("real stat: browserBinary = %q, %v; want an allowlisted binary or an error", got, err)
	}
}

func TestBrowserEnv_Scrubbed(t *testing.T) {
	kept := []string{"HTTPS_PROXY=http://10.0.0.2:3128", "https_proxy=http://10.0.0.2:3128", "NO_PROXY=localhost,127.0.0.1",
		"SSL_CERT_FILE=/tmp/wardyn/ca.pem", "HOME=/home/agent", "WAYLAND_DISPLAY=wayland-1"}
	dropped := []string{"CHROMIUM_FLAGS=--remote-debugging-port=9222", "CHROME_FLAGS=--load-extension=/x",
		"LD_PRELOAD=/tmp/x.so", "LD_LIBRARY_PATH=/tmp", "PATH=/tmp/x"}
	got := browserEnv(append(append([]string{}, dropped...), kept...))
	if len(got) == 0 || got[0] != "PATH=/usr/bin:/bin" {
		t.Fatalf("browserEnv does not start with the fixed PATH: %v", got)
	}
	for _, kv := range dropped {
		if slices.Contains(got, kv) {
			t.Errorf("browserEnv kept %s", kv)
		}
	}
	for _, kv := range kept {
		if !slices.Contains(got, kv) {
			t.Errorf("browserEnv dropped %s", kv)
		}
	}
	if len(got) != 1+len(kept) {
		t.Errorf("browserEnv = %v, want the fixed PATH and the %d kept variables only", got, len(kept))
	}
}

// fakeBrowser answers every CDP message the cdp writes into toBrowser with {"ok":true} on
// fromBrowser, and records what it read on seen, by its raw JSON keys: a wrong tag on
// cdpMessage must not decode back into the same field.
func fakeBrowser(t *testing.T, toBrowser, fromBrowser net.Conn, seen chan<- map[string]json.RawMessage) {
	t.Helper()
	go func() {
		r := bufio.NewReader(toBrowser)
		for {
			line, err := readNUL(r)
			if err != nil {
				return
			}
			var m map[string]json.RawMessage
			var id int
			if json.Unmarshal(line, &m) != nil || json.Unmarshal(m["id"], &id) != nil {
				return
			}
			seen <- m
			rep, _ := json.Marshal(map[string]any{"id": id, "result": map[string]bool{"ok": true}})
			if _, err := fromBrowser.Write(append(rep, 0)); err != nil {
				return
			}
		}
	}()
}

func TestServeCtl_SessionsOnlyWhereAllowed(t *testing.T) {
	for _, sessions := range []bool{true, false} {
		cdpW, browserR := net.Pipe()
		browserW, cdpR := net.Pipe()
		t.Cleanup(func() { _ = cdpW.Close(); _ = browserR.Close(); _ = browserW.Close(); _ = cdpR.Close() })
		seen := make(chan map[string]json.RawMessage, 4)
		fakeBrowser(t, browserR, browserW, seen)
		sock := filepath.Join(t.TempDir(), "s.sock")
		ln, err := listenCtl(sock)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		go serveCtl(ln, newCDP(cdpW, cdpR), sessions)

		// A browser-level command works on both sockets and carries no session.
		if _, err := ctlCall(sock, "Browser.getVersion", nil); err != nil {
			t.Fatalf("sessions=%v: browser-level call: %v", sessions, err)
		}
		if m := <-seen; string(m["method"]) != `"Browser.getVersion"` || m["sessionId"] != nil {
			t.Fatalf("sessions=%v: the browser got %s", sessions, rawKeys(m))
		}

		res, err := ctlCallIn(sock, "S1", "Runtime.evaluate", json.RawMessage(`{"expression":"1+1"}`))
		if sessions {
			if err != nil || string(res) != `{"ok":true}` {
				t.Fatalf("agent socket: session call = %s, %v", res, err)
			}
			if m := <-seen; string(m["method"]) != `"Runtime.evaluate"` || string(m["sessionId"]) != `"S1"` {
				t.Fatalf("agent socket: the browser got %s, want Runtime.evaluate with \"sessionId\": \"S1\"", rawKeys(m))
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "browser-level commands only") {
			t.Fatalf("person socket: session call = %s, %v; want a refusal", res, err)
		}
		select {
		case m := <-seen:
			t.Fatalf("person socket: a refused session call reached the browser: %s", rawKeys(m))
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// rawKeys prints a message fakeBrowser recorded.
func rawKeys(m map[string]json.RawMessage) string {
	b, _ := json.Marshal(m)
	return string(b)
}

func TestCDPArgs_Session(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		refusal string
	}{
		{[]string{"--session", "S1", "Runtime.evaluate"}, "--session goes with --agent"},
		{[]string{"--agent", "--session"}, "usage:"},
		{[]string{"--agent", "--session", "", "Runtime.evaluate"}, "usage:"},
		{[]string{"--agent", "--session", "S1"}, "usage:"},
		{[]string{"--agent"}, "usage:"},
	} {
		if _, _, _, err := cdpArgs(tc.args); err == nil || !strings.Contains(err.Error(), tc.refusal) {
			t.Errorf("cdpArgs(%q): err = %v, want %q", tc.args, err, tc.refusal)
		}
	}
	// The person-side refusal comes before any uid or store check.
	if err := cmdCDP([]string{"--session", "S1", "Runtime.evaluate"}, io.Discard); err == nil || !strings.Contains(err.Error(), "--session goes with --agent") {
		t.Errorf("cmdCDP --session without --agent: err = %v", err)
	}

	agent, session, rest, err := cdpArgs([]string{"--agent", "--session", "S1", "Runtime.evaluate", `{"expression":"1+1"}`})
	if err != nil || !agent || session != "S1" || !slices.Equal(rest, []string{"Runtime.evaluate", `{"expression":"1+1"}`}) {
		t.Fatalf("cdpArgs with a session = %v, %q, %q, %v", agent, session, rest, err)
	}
	agent, session, rest, err = cdpArgs([]string{"--agent", "Browser.getVersion"})
	if err != nil || !agent || session != "" || !slices.Equal(rest, []string{"Browser.getVersion"}) {
		t.Fatalf("cdpArgs browser-level = %v, %q, %q, %v", agent, session, rest, err)
	}
	agent, session, rest, err = cdpArgs([]string{"Browser.getVersion"})
	if err != nil || agent || session != "" || !slices.Equal(rest, []string{"Browser.getVersion"}) {
		t.Fatalf("cdpArgs person = %v, %q, %q, %v", agent, session, rest, err)
	}
}

// A command the browser never answers fails on its own after cdpCallTimeout, and the calls after it
// still get their own replies; its late reply goes to nobody.
func TestCallIn_DeadlineFreesTheBroker(t *testing.T) {
	old := cdpCallTimeout
	cdpCallTimeout = 100 * time.Millisecond
	t.Cleanup(func() { cdpCallTimeout = old })
	cdpW, browserR := net.Pipe()
	browserW, cdpR := net.Pipe()
	t.Cleanup(func() { _ = cdpW.Close(); _ = browserR.Close(); _ = browserW.Close(); _ = cdpR.Close() })
	go func() {
		r := bufio.NewReader(browserR)
		stuck := 0
		for {
			line, err := readNUL(r)
			if err != nil {
				return
			}
			var m cdpMessage
			if json.Unmarshal(line, &m) != nil {
				return
			}
			if m.Method == "Runtime.evaluate" {
				stuck = m.ID
				continue
			}
			if stuck != 0 {
				late, _ := json.Marshal(cdpMessage{ID: stuck, Result: json.RawMessage(`{"late":true}`)})
				if _, err := browserW.Write(append(late, 0)); err != nil {
					return
				}
				stuck = 0
			}
			rep, _ := json.Marshal(cdpMessage{ID: m.ID, Result: json.RawMessage(fmt.Sprintf(`{"id":%d}`, m.ID))})
			if _, err := browserW.Write(append(rep, 0)); err != nil {
				return
			}
		}
	}()
	c := newCDP(cdpW, cdpR)

	start := time.Now()
	res, err := c.callIn("S1", "Runtime.evaluate", json.RawMessage(`{"expression":"new Promise(() => {})","awaitPromise":true}`))
	if err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("an unanswered command = %s, %v; want the deadline's error", res, err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("the unanswered command held its caller %s", d)
	}
	for id := 2; id <= 3; id++ {
		res, err := c.call("Browser.getVersion", nil)
		if want := fmt.Sprintf(`{"id":%d}`, id); err != nil || string(res) != want {
			t.Fatalf("call %d after the unanswered one = %s, %v; want %s", id, res, err, want)
		}
	}
}

// A browser that exits while a call waits fails that call at once, not after cdpCallTimeout.
func TestCallIn_PipeEndFailsAWaitingCall(t *testing.T) {
	old := cdpCallTimeout
	cdpCallTimeout = 30 * time.Second
	t.Cleanup(func() { cdpCallTimeout = old })
	cdpW, browserR := net.Pipe()
	browserW, cdpR := net.Pipe()
	t.Cleanup(func() { _ = cdpW.Close(); _ = browserR.Close(); _ = browserW.Close(); _ = cdpR.Close() })
	go func() {
		// Read the one command, then exit without answering it.
		_, _ = readNUL(bufio.NewReader(browserR))
		_ = browserW.Close()
	}()
	c := newCDP(cdpW, cdpR)
	done := make(chan error, 1)
	start := time.Now()
	go func() { _, err := c.call("Browser.getVersion", nil); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a call whose browser exited returned no error")
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("the call failed only after %s", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a call whose browser exited still waits after 5s")
	}
	if _, err := c.call("Browser.getVersion", nil); err == nil {
		t.Fatal("a call after the browser exited returned no error")
	}
}

// A browser that stops reading its pipe fails the stalled write by the call's deadline, and the
// calls after it run once the browser reads again.
func TestCallIn_WriteDeadlineFreesAStalledPipe(t *testing.T) {
	old := cdpCallTimeout
	cdpCallTimeout = 100 * time.Millisecond
	t.Cleanup(func() { cdpCallTimeout = old })
	cdpW, browserR := net.Pipe()
	browserW, cdpR := net.Pipe()
	t.Cleanup(func() { _ = cdpW.Close(); _ = browserR.Close(); _ = browserW.Close(); _ = cdpR.Close() })
	c := newCDP(cdpW, cdpR)

	done := make(chan error, 1)
	go func() { _, err := c.call("Browser.getVersion", nil); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a write to a browser that reads nothing returned no error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a write to a browser that reads nothing still blocks after 5s")
	}

	go func() {
		r := bufio.NewReader(browserR)
		for {
			line, err := readNUL(r)
			if err != nil {
				return
			}
			var m cdpMessage
			if json.Unmarshal(line, &m) != nil {
				return
			}
			rep, _ := json.Marshal(cdpMessage{ID: m.ID, Result: json.RawMessage(`{"ok":true}`)})
			if _, err := browserW.Write(append(rep, 0)); err != nil {
				return
			}
		}
	}()
	if res, err := c.call("Browser.getVersion", nil); err != nil || string(res) != `{"ok":true}` {
		t.Fatalf("the call after the stalled one = %s, %v", res, err)
	}
}
