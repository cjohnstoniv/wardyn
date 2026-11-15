// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// shortDir is a directory whose path leaves room for a unix socket's.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "wn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func stagedConfig(t *testing.T, listen string, allowed ...string) []byte {
	t.Helper()
	b, err := json.Marshal(&proxy.Config{
		RunID: uuid.New(), ControlPlaneURL: "http://127.0.0.1:1", RunToken: "tok", Listen: listen,
		Policy: types.RunPolicySpec{AllowedDomains: allowed},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func writeList(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "live-denies.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBoot_NoOverlayKeepsBytes(t *testing.T) {
	staged := stagedConfig(t, "127.0.0.1:0")
	eff, base, err := applyLiveDenies(staged, shortDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if &eff[0] != &staged[0] || &base[0] != &staged[0] {
		t.Fatal("with no list file the staged bytes were changed")
	}
	if got := loadedReport(eff).ConfigSHA256; got != proxy.ConfigSHA256(staged) {
		t.Fatalf("report hash %s, want the staged bytes' %s", got, proxy.ConfigSHA256(staged))
	}
}

func TestBoot_OverlayApplied(t *testing.T) {
	staged := stagedConfig(t, "127.0.0.1:0", "blocked.example", "fine.example")
	dir := shortDir(t)
	writeList(t, dir, `["Blocked.example"," b.example "]`)
	eff, base, err := applyLiveDenies(staged, dir)
	if err != nil {
		t.Fatal(err)
	}
	want, err := proxy.WithLiveDenies(staged, []string{"b.example", "blocked.example"})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(eff, want) || !bytes.Equal(base, staged) {
		t.Fatalf("effective bytes %s\nwant %s", eff, want)
	}
	if got := loadedReport(eff).ConfigSHA256; got != proxy.ConfigSHA256(want) {
		t.Fatalf("report hash %s, want %s", got, proxy.ConfigSHA256(want))
	}
	cfg, err := proxy.LoadConfigBytes(eff)
	if err != nil || !slices.Equal(cfg.LiveDenies, []string{"b.example", "blocked.example"}) {
		t.Fatalf("config live denies %v, %v", cfg.LiveDenies, err)
	}
	// The proxy built from it denies the host its allowlist names.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv, err := proxy.NewServer(ctx, cfg, &http.Client{Timeout: time.Second}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	listening := make(chan struct{})
	srv.OnListening(func() { close(listening) })
	served := make(chan error, 1)
	go func() { served <- srv.ListenAndServe() }()
	t.Cleanup(func() {
		sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer scancel()
		_ = srv.Shutdown(sctx)
		<-served
	})
	// The config listens on port 0, so the kernel picks the port at the bind and no other process holds it first.
	select {
	case <-listening:
	case <-time.After(10 * time.Second):
		t.Fatal("the proxy never listened")
	}
	addr := srv.BoundAddr().String()
	// The list denies blocked.example by policy; fine.example, allowed and not listed, is never denied by policy
	// (this machine cannot resolve it, so its CONNECT still fails, for another reason).
	for host, denied := range map[string]bool{"blocked.example:443": true, "fine.example:443": false} {
		var status string
		deadline := time.Now().Add(10 * time.Second)
		for {
			c, err := net.Dial("tcp", addr)
			if err == nil {
				_, _ = io.WriteString(c, "CONNECT "+host+" HTTP/1.1\r\nHost: "+host+"\r\n\r\n")
				_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
				buf := make([]byte, 512)
				n, _ := c.Read(buf)
				status = string(buf[:n])
				c.Close()
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the proxy never listened: %v", err)
			}
		}
		if got := strings.Contains(status, "policy:denied"); got != denied {
			t.Errorf("CONNECT %s = %q; denied by policy = %v, want %v", host, status, got, denied)
		}
	}
}

func TestBoot_CorruptOverlayExits(t *testing.T) {
	staged := stagedConfig(t, "127.0.0.1:0")
	for name, content := range map[string]string{"not json": `{"deny":`, "not an array": `{"a":1}`, "not strings": `[1,2]`, "truncated": `["a.example"`} {
		dir := shortDir(t)
		writeList(t, dir, content)
		if _, _, err := applyLiveDenies(staged, dir); err == nil {
			t.Errorf("%s: no error", name)
		}
		if code := runMainWithList(t, staged, dir); code != 1 {
			t.Errorf("%s: wardyn-proxy exited %d, want 1", name, code)
		}
	}
	// An unreadable list (a directory in its place) is the same refusal.
	dir := shortDir(t)
	if err := os.Mkdir(filepath.Join(dir, "live-denies.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := applyLiveDenies(staged, dir); err == nil {
		t.Error("an unreadable list: no error")
	}
}

func TestBoot_InvalidEntryExits(t *testing.T) {
	staged := stagedConfig(t, "127.0.0.1:0")
	for name, content := range map[string]string{"invalid host": `["a.example","bad host"]`, "empty entry": `[""]`, "too long": `["` + strings.Repeat("a", 300) + `.example"]`} {
		dir := shortDir(t)
		writeList(t, dir, content)
		if _, _, err := applyLiveDenies(staged, dir); err == nil {
			t.Errorf("%s: no error", name)
		}
		if code := runMainWithList(t, staged, dir); code != 1 {
			t.Errorf("%s: wardyn-proxy exited %d, want 1", name, code)
		}
	}
}

// runMainWithList runs the real entry point over staged and dir and answers its exit code.
func runMainWithList(t *testing.T, staged []byte, dir string) int {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), runMainEnv+"=1", "WARDYN_PROXY_CONFIG_JSON="+string(staged), proxy.LiveDenyDirEnv+"="+dir)
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		if err != nil {
			t.Fatal(err)
		}
		return 0
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatal("wardyn-proxy did not exit")
		return -1
	}
}

// runNarrowChild runs `wardyn-proxy -narrow` with the given stdin and the live deny directory dir (unset when "").
func runNarrowChild(t *testing.T, dir string, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], append([]string{"-narrow"}, args...)...)
	cmd.Env = append(os.Environ(), runMainEnv+"=1")
	if dir != "" {
		cmd.Env = append(cmd.Env, proxy.LiveDenyDirEnv+"="+dir)
	}
	cmd.Stdin = strings.NewReader(stdin)
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return code, so.String(), se.String()
}

// serveSocket listens on dir's narrowing socket and runs handle on each connection after reading its request.
func serveSocket(t *testing.T, dir string, handle func(req []byte, c net.Conn)) {
	t.Helper()
	ln, err := net.Listen("unix", proxy.NarrowSocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(10 * time.Second))
				req, _ := io.ReadAll(c)
				handle(req, c)
			}()
		}
	}()
}

func TestNarrowFlag_LoadsNoConfig(t *testing.T) {
	missing := filepath.Join(shortDir(t), "no-such-config.json")
	dir := shortDir(t)

	// No socket: nothing was delivered. The nonexistent -config is never opened.
	if code, out, _ := runNarrowChild(t, dir, `{"deny":[]}`, "-config", missing); code != 3 || out != "" {
		t.Errorf("no socket: exit %d, stdout %q; want 3 and nothing", code, out)
	}
	// The directory variable unset is the same.
	if code, _, _ := runNarrowChild(t, "", `{"deny":[]}`); code != 3 {
		t.Errorf("no directory: exit %d, want 3", code)
	}
	// A stale socket file nobody listens on is a dial error.
	stale := shortDir(t)
	ln, err := net.Listen("unix", proxy.NarrowSocketPath(stale))
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	if code, _, _ := runNarrowChild(t, stale, `{"deny":[]}`); code != 3 {
		t.Errorf("stale socket: exit %d, want 3", code)
	}

	// A listener that closes without answering: the request may have been delivered.
	silent := shortDir(t)
	got := make(chan string, 1)
	serveSocket(t, silent, func(req []byte, c net.Conn) { got <- string(req) })
	if code, out, _ := runNarrowChild(t, silent, `{"deny":["a.example"]}`, "-config", missing); code != 4 || out != "" {
		t.Errorf("no answer: exit %d, stdout %q; want 4 and nothing", code, out)
	}
	if req := <-got; req != `{"deny":["a.example"]}` {
		t.Errorf("the socket got %q, want the stdin bytes as read", req)
	}
	// An answer that is not JSON.
	junk := shortDir(t)
	serveSocket(t, junk, func(_ []byte, c net.Conn) { _, _ = io.WriteString(c, "not json\n") })
	if code, out, _ := runNarrowChild(t, junk, `{"deny":[]}`); code != 4 || out != "" {
		t.Errorf("junk answer: exit %d, stdout %q; want 4", code, out)
	}
	// An answer over the bound.
	big := shortDir(t)
	serveSocket(t, big, func(_ []byte, c net.Conn) {
		_, _ = io.WriteString(c, `{"state":"current","error":"`+strings.Repeat("x", proxy.MaxNarrowMessageBytes)+"\"}\n")
	})
	if code, out, _ := runNarrowChild(t, big, `{"deny":[]}`); code != 4 || out != "" {
		t.Errorf("oversized answer: exit %d, %d bytes of stdout; want 4 and nothing", code, len(out))
	}
	// An answer: printed, exit 0.
	ok := shortDir(t)
	const line = `{"state":"current","changed":false,"live_denies":[]}`
	serveSocket(t, ok, func(_ []byte, c net.Conn) { _, _ = io.WriteString(c, line+"\n") })
	if code, out, _ := runNarrowChild(t, ok, `{"deny":[]}`, "-config", missing); code != 0 || out != line+"\n" {
		t.Errorf("answer: exit %d, stdout %q; want 0 and the line", code, out)
	}
	// A request over the bound is sent as one byte over, whole, for the proxy to refuse.
	sizes := make(chan int, 1)
	long := shortDir(t)
	serveSocket(t, long, func(req []byte, c net.Conn) { sizes <- len(req); _, _ = io.WriteString(c, line+"\n") })
	if code, _, _ := runNarrowChild(t, long, strings.Repeat(" ", proxy.MaxNarrowMessageBytes+5000)); code != 0 {
		t.Errorf("over-bound request: exit %d, want 0", code)
	}
	if n := <-sizes; n != proxy.MaxNarrowMessageBytes+1 {
		t.Errorf("the socket got %d bytes, want %d", n, proxy.MaxNarrowMessageBytes+1)
	}
}

// An image whose flag set lacks -narrow: the flag package's own exit 2 and stderr line, which the driver maps to an older image.
func TestNarrowFlag_OldBinaryExitTwo(t *testing.T) {
	if os.Getenv("WARDYN_PROXY_TEST_OLD_FLAGS") == "1" {
		fs := flag.NewFlagSet("wardyn-proxy", flag.ExitOnError)
		fs.String("config", "", "")
		fs.String("egress-canary", "", "")
		_ = fs.Parse([]string{"-narrow"})
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestNarrowFlag_OldBinaryExitTwo$")
	cmd.Env = append(os.Environ(), "WARDYN_PROXY_TEST_OLD_FLAGS=1")
	var se bytes.Buffer
	cmd.Stderr = &se
	err := cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 2 {
		t.Fatalf("exit %v, want 2", err)
	}
	if !strings.Contains(se.String(), "flag provided but not defined: -narrow") {
		t.Fatalf("stderr %q lacks the flag package's line", se.String())
	}
}

// The real proxy over a real list directory: the boot report, a narrowing through -narrow, its report,
// and a restart that applies the list again.
func TestNarrowFlag_EndToEnd(t *testing.T) {
	reports := make(chan string, 16)
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/proxy/loaded") {
			var rep proxy.LoadedReport
			_ = json.NewDecoder(r.Body).Decode(&rep)
			reports <- rep.ConfigSHA256
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(cp.Close)
	staged, err := json.Marshal(&proxy.Config{RunID: uuid.New(), ControlPlaneURL: cp.URL, RunToken: "tok", Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	dir := shortDir(t)
	start := func() *exec.Cmd {
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(), runMainEnv+"=1", "WARDYN_PROXY_CONFIG_JSON="+string(staged), proxy.LiveDenyDirEnv+"="+dir)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		return cmd
	}
	stop := func(cmd *exec.Cmd) { _ = cmd.Process.Signal(syscall.SIGTERM); _ = cmd.Wait() }
	wantReport := func(hash string) {
		t.Helper()
		select {
		case got := <-reports:
			if got != hash {
				t.Fatalf("reported %s, want %s", got, hash)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("no report arrived")
		}
	}

	p1 := start()
	wantReport(proxy.ConfigSHA256(staged))
	code, out, errOut := runNarrowChild(t, dir, `{"deny":["Blocked.example"]}`)
	if code != 0 {
		t.Fatalf("-narrow exited %d: %s", code, errOut)
	}
	var ans proxy.NarrowAnswer
	if err := json.Unmarshal([]byte(out), &ans); err != nil {
		t.Fatalf("answer %q: %v", out, err)
	}
	narrowed, err := proxy.WithLiveDenies(staged, []string{"blocked.example"})
	if err != nil {
		t.Fatal(err)
	}
	if ans.State != "applied" || !ans.Changed || ans.ConfigSHA256 != proxy.ConfigSHA256(narrowed) || !slices.Equal(ans.LiveDenies, []string{"blocked.example"}) {
		t.Fatalf("answer %+v", ans)
	}
	wantReport(ans.ConfigSHA256)
	if b, err := os.ReadFile(filepath.Join(dir, "live-denies.json")); err != nil || string(b) != `["blocked.example"]` {
		t.Fatalf("list file %q, %v", b, err)
	}
	stop(p1)

	// A restart boots from the file and reports the narrowed bytes.
	p2 := start()
	t.Cleanup(func() { stop(p2) })
	wantReport(ans.ConfigSHA256)
	code, out, _ = runNarrowChild(t, dir, `{"deny":[]}`)
	if code != 0 || !strings.Contains(out, `"state":"current"`) || !strings.Contains(out, `"live_denies":["blocked.example"]`) {
		t.Fatalf("probe after restart: exit %d, %q", code, out)
	}
}
