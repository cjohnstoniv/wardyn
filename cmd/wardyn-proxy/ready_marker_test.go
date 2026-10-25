// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
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
