// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"encoding/json"
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
		"--headless=old", "chrome://version",
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
// fromBrowser, and records what it read on seen.
func fakeBrowser(t *testing.T, toBrowser, fromBrowser net.Conn, seen chan<- cdpMessage) {
	t.Helper()
	go func() {
		r := bufio.NewReader(toBrowser)
		for {
			line, err := readNUL(r)
			if err != nil {
				return
			}
			var m cdpMessage
			if json.Unmarshal(line, &m) != nil {
				return
			}
			seen <- m
			rep, _ := json.Marshal(cdpMessage{ID: m.ID, Result: json.RawMessage(`{"ok":true}`)})
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
		seen := make(chan cdpMessage, 4)
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
		if m := <-seen; m.Method != "Browser.getVersion" || m.SessionID != "" {
			t.Fatalf("sessions=%v: the browser got %+v", sessions, m)
		}

		res, err := ctlCallIn(sock, "S1", "Runtime.evaluate", json.RawMessage(`{"expression":"1+1"}`))
		if sessions {
			if err != nil || string(res) != `{"ok":true}` {
				t.Fatalf("agent socket: session call = %s, %v", res, err)
			}
			if m := <-seen; m.Method != "Runtime.evaluate" || m.SessionID != "S1" {
				t.Fatalf("agent socket: the browser got %+v, want Runtime.evaluate in session S1", m)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "browser-level commands only") {
			t.Fatalf("person socket: session call = %s, %v; want a refusal", res, err)
		}
		select {
		case m := <-seen:
			t.Fatalf("person socket: a refused session call reached the browser: %+v", m)
		case <-time.After(100 * time.Millisecond):
		}
	}
}
