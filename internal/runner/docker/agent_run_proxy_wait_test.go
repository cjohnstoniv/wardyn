// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// agentRunExec runs agent-base's real agent-run as an exec task `date +%s%N` with the proxy URL given,
// and returns the task's start time, its stderr and how long agent-run took.
func agentRunExec(t *testing.T, proxyURL, waitSecs string) (started time.Time, stderr string, took time.Duration) {
	t.Helper()
	script := baseRunnableAgentRun(t)
	start := t.TempDir()
	cmd := exec.Command("bash", script, "date +%s%N")
	cmd.Dir = start
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir(), "WARDYN_TASK_MODE=exec", "WARDYN_START_FOLDER="+start,
		"WARDYN_REPOS=", "WARDYN_REPO_URL=", "WARDYN_MITM_CA_PEM=", "WARDYN_GITHUB_GRANT_ID=",
		"WARDYN_PROXY_URL="+proxyURL, "WARDYN_PROXY_WAIT_SECS="+waitSecs)
	t0 := time.Now()
	out, err := cmd.Output()
	took = time.Since(t0)
	if err != nil {
		t.Fatalf("agent-run exec task: %v\n%s", err, errBuf.String())
	}
	ns, perr := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if perr != nil {
		t.Fatalf("task output %q: %v", out, perr)
	}
	return time.Unix(0, ns), errBuf.String(), took
}

// The belt: an agent image whose proxy is not listening yet holds the task until it is, instead of
// starting it into "connection refused".
func TestAgentRun_WaitsForALateProxy(t *testing.T) {
	addr, listen := refusingAddr(t) // nothing listens on addr until the goroutine below opens it
	opened := make(chan time.Time, 1)
	go func() {
		time.Sleep(1500 * time.Millisecond)
		ln, err := listen()
		if err != nil {
			opened <- time.Time{}
			return
		}
		opened <- time.Now()
		t.Cleanup(func() { _ = ln.Close() })
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	started, stderr, _ := agentRunExec(t, "http://"+addr, "10")
	at := <-opened
	if at.IsZero() {
		t.Fatalf("the late listener could not bind %s", addr)
	}
	if started.Before(at) {
		t.Errorf("the task started at %s, before the proxy listened at %s: the belt did not wait", started, at)
	}
	if strings.Contains(stderr, "did not accept connections") {
		t.Errorf("a proxy that came up in time was reported late: %s", stderr)
	}
}

// refusingAddr is a loopback address that refuses connections until listen is called. A socket that does
// not listen yet keeps its port bound, so no other listener on the host can take it, as one did a closed
// port's.
func refusingAddr(t *testing.T) (addr string, listen func() (net.Listener, error)) {
	t.Helper()
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	f := os.NewFile(uintptr(fd), "proxy-port")
	t.Cleanup(func() { _ = f.Close() })
	if err := unix.Bind(fd, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	sa, err := unix.Getsockname(fd)
	if err != nil {
		t.Fatal(err)
	}
	return "127.0.0.1:" + strconv.Itoa(sa.(*unix.SockaddrInet4).Port), func() (net.Listener, error) {
		if err := unix.Listen(fd, 16); err != nil {
			return nil, err
		}
		return net.FileListener(f)
	}
}

// Past its bound the belt fails open: one stderr line, then the task runs, as before the wait existed.
func TestAgentRun_ProxyNeverUpFailsOpenWithOneLine(t *testing.T) {
	addr, _ := refusingAddr(t)
	_, stderr, took := agentRunExec(t, "http://"+addr, "1")
	if n := strings.Count(stderr, "did not accept connections within 1s"); n != 1 {
		t.Errorf("want exactly one fail-open line, got %d in:\n%s", n, stderr)
	}
	if took < time.Second {
		t.Errorf("agent-run returned after %s, before the 1s bound", took)
	}
}

// Every agent image's agent-run waits, so a derived image that copies one inherits the belt.
func TestAgentRun_EveryImageCallsTheWait(t *testing.T) {
	for _, p := range []string{"claude-code/agent-run", "codex-cli/agent-run", "aws-sso/agent-run", "common/agent-run-stub"} {
		body := ccRead(t, ccAbs(t, "../../../deploy/images/"+p))
		src := strings.Index(body, "\nsource /usr/local/bin/agent-run-lib.sh\n")
		call := strings.Index(body, "\nwait_for_proxy\n")
		if src < 0 || call < src {
			t.Errorf("%s does not call wait_for_proxy right after sourcing the lib", p)
		}
	}
}
