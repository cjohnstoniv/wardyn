// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// childModeEnv makes the test binary a stand-in MCP server, started by the host as its child.
const childModeEnv = "WARDYN_MCP_HOST_TEST_CHILD"

func TestMain(m *testing.M) {
	switch os.Getenv(childModeEnv) {
	case "":
		os.Exit(m.Run())
	case "check-ca":
		os.Exit(checkCAChild())
	case "echo":
		echoChild()
		os.Exit(0)
	case "exit5":
		os.Exit(5)
	case "ignore-term":
		signal.Ignore(syscall.SIGTERM)
		time.Sleep(time.Minute)
		os.Exit(0)
	case "orphan":
		// A process it started (this binary again, asleep) keeps its stdout open after it exits.
		exe, _ := os.Executable()
		gc := exec.Command(exe, "-test.run=^$")
		gc.Env = append(os.Environ(), childModeEnv+"=sleeper")
		gc.Stdout = os.Stdout
		if gc.Start() != nil || os.WriteFile(os.Getenv("TEST_PID_FILE"), []byte(strconv.Itoa(gc.Process.Pid)), 0o644) != nil {
			os.Exit(98)
		}
		os.Exit(0)
	case "sleeper":
		time.Sleep(time.Minute)
		os.Exit(0)
	case "answer-exit":
		// Answers one request and exits at once. A long stray line first (it fits the pipe, so the
		// write does not wait for the host) leaves the host reading after the exit.
		if bufio.NewScanner(os.Stdin).Scan() {
			fmt.Println(strings.Repeat("x", 60000))
			fmt.Println(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)
		}
		os.Exit(0)
	}
	os.Exit(99)
}

// checkCAChild leaves a marker that it ran, then exits 0 only when both CA files are already in
// place with the content the test expects.
func checkCAChild() int {
	dir := os.Getenv("TEST_CA_DIR")
	_ = os.WriteFile(filepath.Join(dir, "child-ran"), nil, 0o644)
	ca, err1 := os.ReadFile(filepath.Join(dir, "mitm-ca.pem"))
	bundle, err2 := os.ReadFile(filepath.Join(dir, "ca-bundle.pem"))
	if err1 != nil || err2 != nil || string(ca) != os.Getenv("TEST_WANT_CA") || string(bundle) != os.Getenv("TEST_WANT_BUNDLE") {
		return 7
	}
	return 0
}

// echoChild answers each request line with its method; a "notify" request also sends a notification first.
func echoChild() {
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil || m.ID == nil {
			continue
		}
		if m.Method == "notify" {
			fmt.Println(`{"jsonrpc":"2.0","method":"notifications/message","params":{"data":"hello"}}`)
		}
		fmt.Printf(`{"jsonrpc":"2.0","id":%s,"result":{"method":%q}}`+"\n", m.ID, m.Method)
	}
}

const testCA = "-----BEGIN CERTIFICATE-----\nTESTCA\n-----END CERTIFICATE-----\n"

// childLaunch is a launch that starts this test binary as the child.
func childLaunch(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(launch{Command: exe, Args: []string{"-test.run=^$"}})
	return string(b)
}

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// TestMCPHostWritesCAFilesBeforeExec: both CA files are in place when the server starts (the
// system bundle first, then the proxy's CA; the CA alone, with a warning, when there is no bundle),
// and a CA file that cannot be written stops the host before the server starts.
func TestMCPHostWritesCAFilesBeforeExec(t *testing.T) {
	sys := filepath.Join(t.TempDir(), "system.crt")
	if err := os.WriteFile(sys, []byte("SYSTEM ROOTS"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name       string
		bundles    []string
		wantBundle string
		warns      bool
	}{
		{"system bundle then the CA", []string{filepath.Join(t.TempDir(), "absent.crt"), sys}, "SYSTEM ROOTS\n" + testCA, false},
		{"no system bundle", []string{filepath.Join(t.TempDir(), "absent.crt")}, testCA, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "wardyn")
			t.Setenv(childModeEnv, "check-ca")
			t.Setenv("TEST_WANT_CA", testCA)
			t.Setenv("TEST_WANT_BUNDLE", c.wantBundle)
			t.Setenv("TEST_CA_DIR", dir) // where the child checks the files the host wrote
			var stderr bytes.Buffer
			code, err := run(context.Background(), options{caDir: dir, bundles: c.bundles, listen: "127.0.0.1:0", stderr: &stderr,
				env: envOf(map[string]string{launchEnv: childLaunch(t), caPEMEnv: strings.TrimSuffix(testCA, "\n")})})
			if err != nil || code != 0 {
				t.Fatalf("code %d, err %v; the child did not find both CA files in place (stderr %q)", code, err, stderr.String())
			}
			if strings.Contains(stderr.String(), "no system CA bundle") != c.warns {
				t.Fatalf("stderr %q, want a warning: %v", stderr.String(), c.warns)
			}
		})
	}
	t.Run("a write failure starts nothing", func(t *testing.T) {
		root := t.TempDir()
		blocker := filepath.Join(root, "file")
		if err := os.WriteFile(blocker, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv(childModeEnv, "check-ca")
		t.Setenv("TEST_CA_DIR", root)
		for name, env := range map[string]map[string]string{
			"unwritable directory": {launchEnv: childLaunch(t), caPEMEnv: testCA},
			"no CA":                {launchEnv: childLaunch(t)},
		} {
			dir := filepath.Join(blocker, "wardyn")
			if name == "no CA" {
				dir = filepath.Join(root, "wardyn")
			}
			code, err := run(context.Background(), options{caDir: dir, listen: "127.0.0.1:0", stderr: io.Discard, env: envOf(env)})
			if err == nil || code == 0 {
				t.Fatalf("%s: code %d, err %v; want a non-zero exit", name, code, err)
			}
			if _, err := os.Stat(filepath.Join(root, "child-ran")); err == nil {
				t.Fatalf("%s: the server started", name)
			}
		}
	})
}

// TestMCPHostLaunchNoShell: the launch is a command and its arguments, run as given: a shell line
// is one program name, never interpreted, and a launch with unknown keys or no command is refused.
func TestMCPHostLaunchNoShell(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "pwned")
	code, err := run(context.Background(), options{caDir: t.TempDir(), listen: "127.0.0.1:0", stderr: io.Discard,
		env: envOf(map[string]string{launchEnv: `{"command":"true; touch ` + marker + `"}`, caPEMEnv: testCA})})
	if err == nil || code == 0 || !strings.Contains(err.Error(), "start the MCP server") {
		t.Fatalf("code %d, err %v; want the shell line refused as a program name", code, err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the launch ran through a shell")
	}
	for _, raw := range []string{`{"command":""}`, `{"command":"x","shell":true}`, `not json`} {
		if _, err := parseLaunch(raw); err == nil {
			t.Errorf("launch %s accepted", raw)
		}
	}
	if l, err := parseLaunch(`{"command":"npx","args":["-y","@scope/server"]}`); err != nil || l.Command != "npx" || len(l.Args) != 2 {
		t.Fatalf("launch %+v, err %v", l, err)
	}
}

// TestMCPHostRelaysStreamableHTTP: requests reach the server's stdin and its answers come back by
// id; initialize starts the one session, an unknown session gets 404, a notification gets 202, a
// stream carries the server's own messages, and the host exits with the server.
func TestMCPHostRelaysStreamableHTTP(t *testing.T) {
	t.Setenv(childModeEnv, "echo")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan string, 1)
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, err := run(ctx, options{caDir: t.TempDir(), listen: "127.0.0.1:0", stderr: io.Discard, ready: func(a string) { ready <- a },
			env: envOf(map[string]string{launchEnv: childLaunch(t), caPEMEnv: testCA})})
		done <- result{code, err}
	}()
	var base string
	select {
	case a := <-ready:
		base = "http://" + a
	case <-time.After(10 * time.Second):
		t.Fatal("the host never listened")
	}
	do := func(method, path, session, body string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		if session != "" {
			req.Header.Set(sessionHeader, session)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}
	read := func(resp *http.Response) string { b, _ := io.ReadAll(resp.Body); return string(b) }

	if resp := do(http.MethodGet, "/healthz", "", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz %d", resp.StatusCode)
	}
	resp := do(http.MethodPost, mcpPath, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	session := resp.Header.Get(sessionHeader)
	if got := read(resp); resp.StatusCode != http.StatusOK || session == "" || !strings.Contains(got, `"id":1`) || !strings.Contains(got, `"initialize"`) {
		t.Fatalf("initialize: %d %q, session %q", resp.StatusCode, got, session)
	}
	resp = do(http.MethodPost, mcpPath, session, `[{"jsonrpc":"2.0","id":"a","method":"tools/list"},{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"x"}}]`)
	var batch []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(read(resp)), &batch); err != nil || len(batch) != 2 || string(batch[0]["id"]) != `"a"` || string(batch[1]["id"]) != "2" {
		t.Fatalf("batch answer %v (%v)", batch, err)
	}
	if resp := do(http.MethodPost, mcpPath, "not-this-session", `{"jsonrpc":"2.0","id":3,"method":"ping"}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown session: %d, want 404", resp.StatusCode)
	}
	if resp := do(http.MethodPost, mcpPath, session, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("notification: %d, want 202", resp.StatusCode)
	}
	if resp := do(http.MethodDelete, mcpPath, "not-this-session", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("DELETE of an unknown session: %d, want 404", resp.StatusCode)
	}
	if resp := do(http.MethodGet, "/other", "", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("other path: %d, want 404", resp.StatusCode)
	}

	stream := do(http.MethodGet, mcpPath, session, "")
	if stream.StatusCode != http.StatusOK || !strings.HasPrefix(stream.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream: %d %q", stream.StatusCode, stream.Header.Get("Content-Type"))
	}
	time.Sleep(50 * time.Millisecond) // the stream is registered before the server speaks
	if got := read(do(http.MethodPost, mcpPath, session, `{"jsonrpc":"2.0","id":4,"method":"notify"}`)); !strings.Contains(got, `"id":4`) {
		t.Fatalf("notify answer %q", got)
	}
	sr := bufio.NewReader(stream.Body)
	line, _ := sr.ReadString('\n')
	for strings.TrimSpace(line) == "event: message" {
		line, _ = sr.ReadString('\n')
	}
	if !strings.Contains(line, "notifications/message") {
		t.Fatalf("stream line %q, want the server's notification", line)
	}

	cancel() // SIGTERM to the server; the host exits with it
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("run: %v", r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the host did not exit with its server")
	}
}

// TestMCPHostExitsWithServerCode: the host's exit code is the server's.
func TestMCPHostExitsWithServerCode(t *testing.T) {
	t.Setenv(childModeEnv, "exit5")
	code, err := run(context.Background(), options{caDir: t.TempDir(), listen: "127.0.0.1:0", stderr: io.Discard,
		env: envOf(map[string]string{launchEnv: childLaunch(t), caPEMEnv: testCA})})
	if err != nil || code != 5 {
		t.Fatalf("code %d, err %v; want 5", code, err)
	}
}

// TestMCPHostExitsOnServerExit: the host ends when its server exits, though a process the server
// started still holds the server's stdout open.
func TestMCPHostExitsOnServerExit(t *testing.T) {
	t.Setenv(childModeEnv, "orphan")
	pidFile := filepath.Join(t.TempDir(), "orphan.pid")
	t.Setenv("TEST_PID_FILE", pidFile)
	t.Cleanup(func() { // the leftover process is this test's to end
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(string(b)); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	start := time.Now()
	code, err := run(context.Background(), options{caDir: t.TempDir(), listen: "127.0.0.1:0", stderr: io.Discard,
		env: envOf(map[string]string{launchEnv: childLaunch(t), caPEMEnv: testCA})})
	if err != nil || code != 0 {
		t.Fatalf("code %d, err %v; want 0", code, err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("the host took %s: it waited for the stdout a leftover process holds", d)
	}
}

// TestMCPHostKillsServerThatIgnoresTerm: on a signal the host sends the server SIGTERM, then SIGKILL
// once killAfter passes, so a server that ignores SIGTERM still ends.
func TestMCPHostKillsServerThatIgnoresTerm(t *testing.T) {
	t.Setenv(childModeEnv, "ignore-term")
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan int, 1)
	go func() {
		code, _ := run(ctx, options{caDir: t.TempDir(), listen: "127.0.0.1:0", stderr: io.Discard, ready: func(a string) { ready <- a },
			killAfter: 300 * time.Millisecond, env: envOf(map[string]string{launchEnv: childLaunch(t), caPEMEnv: testCA})})
		done <- code
	}()
	<-ready
	time.Sleep(200 * time.Millisecond) // the child has set its SIGTERM handling
	cancel()
	select {
	case code := <-done:
		if code == 0 {
			t.Fatal("exit code 0 for a killed server")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the host did not end a server that ignores SIGTERM")
	}
}

// TestMCPHostKeepsLastAnswer: a server that answers and exits at once still has its answer reach
// the POST that asked, every time: the host reads what the server wrote before it ends.
func TestMCPHostKeepsLastAnswer(t *testing.T) {
	t.Setenv(childModeEnv, "answer-exit")
	t.Setenv("GORACE", "atexit_sleep_ms=0") // under -race a child would sleep 1 s before it exits, hiding the race
	const n = 40
	lost := 0
	for range n {
		ready := make(chan string, 1)
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = run(context.Background(), options{caDir: t.TempDir(), listen: "127.0.0.1:0", stderr: io.Discard, ready: func(a string) { ready <- a },
				env: envOf(map[string]string{launchEnv: childLaunch(t), caPEMEnv: testCA})})
		}()
		resp, err := http.Post("http://"+<-ready+mcpPath, "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x"}}`))
		if err != nil {
			lost++
		} else {
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK || !strings.Contains(string(b), `"ok":true`) {
				lost++
			}
		}
		<-done
	}
	if lost > 0 {
		t.Fatalf("lost %d of %d answers the server wrote before it exited", lost, n)
	}
}
