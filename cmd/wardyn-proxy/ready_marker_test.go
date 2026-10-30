// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
)

// The real entry point prints the ready marker on stdout once its listener is bound: a Docker runner
// holds the agent back until it sees this line.
func TestMain_PrintsTheReadyMarkerOnceListening(t *testing.T) {
	cfg, err := json.Marshal(&proxy.Config{RunID: uuid.New(), ControlPlaneURL: "http://127.0.0.1:1", RunToken: "tok", Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), runMainEnv+"=1", "WARDYN_PROXY_CONFIG_JSON="+string(cfg))
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Signal(syscall.SIGTERM); _ = cmd.Wait() })
	found := make(chan bool, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if strings.TrimSpace(sc.Text()) == proxy.ReadyMarker {
				found <- true
				return
			}
		}
		found <- false
	}()
	select {
	case ok := <-found:
		if !ok {
			t.Fatal("wardyn-proxy's stdout ended without the ready marker")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("wardyn-proxy printed no ready marker")
	}
}

// A self-lapsing proxy (a Kubernetes runner's) binds only after its first renew, so the "listening" log line
// must not appear while that renew is outstanding, and must appear with the ready marker once it lands.
func TestMain_LogsListeningOnlyAfterTheFirstRenew(t *testing.T) {
	arrived, release := make(chan struct{}, 1), make(chan struct{})
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case arrived <- struct{}{}:
		default:
		}
		<-release
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "tok2", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	}))
	t.Cleanup(cp.Close)
	pin := strings.Repeat("ab", 32)
	cfg, err := json.Marshal(&proxy.Config{
		RunID: uuid.New(), ControlPlaneURL: cp.URL, RunToken: "tok", Listen: "127.0.0.1:0", SelfLapse: true,
		DecisionSink: &proxy.DecisionSinkConfig{URL: "tls://127.0.0.1:1", SPKISHA256: pin, Key: pin},
	})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), runMainEnv+"=1", "WARDYN_PROXY_CONFIG_JSON="+string(cfg))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Signal(syscall.SIGTERM); _ = cmd.Wait() })
	// A failed check leaves the renew held, which would block the proxy's exit and the stand-in's close.
	var once sync.Once
	unhold := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unhold)
	var mu sync.Mutex
	var logged strings.Builder
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			mu.Lock()
			logged.WriteString(sc.Text() + "\n")
			mu.Unlock()
		}
	}()
	listening := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Contains(logged.String(), "wardyn-proxy: listening")
	}
	marker := make(chan bool, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if strings.TrimSpace(sc.Text()) == proxy.ReadyMarker {
				marker <- true
				return
			}
		}
		marker <- false
	}()

	select {
	case <-arrived:
	case <-time.After(30 * time.Second):
		t.Fatal("the proxy never asked to renew")
	}
	// Held at the first renew, the proxy has bound nothing: give a premature line time to appear.
	time.Sleep(500 * time.Millisecond)
	if listening() {
		t.Fatal("the proxy logged that it is listening while its first renew was outstanding")
	}
	unhold()
	select {
	case ok := <-marker:
		if !ok {
			t.Fatal("wardyn-proxy's stdout ended without the ready marker")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("wardyn-proxy printed no ready marker after its first renew")
	}
	for end := time.Now().Add(5 * time.Second); !listening(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("the proxy never logged that it is listening once its first renew landed")
		}
	}
}
