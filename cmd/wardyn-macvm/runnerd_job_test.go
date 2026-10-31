// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/macvmtest"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// runnerdJobEnv marks this package's test binary started as runnerd's helper.
const runnerdJobEnv = "WARDYN_TEST_RUNNERD_JOB"

// runnerdJobPushed is what the binary prints once stage 2 pushed its guest agent.
const runnerdJobPushed = "runnerd job: stage 2 pushed the guest agent beside the helper"

// TestUpdateToolsAsRunnerdStartsIt is cmd/wardyn-runnerd's TestMacOSVMAgentDriftUpdatesWithoutForce's
// helper, not a test of its own: that test builds this binary and starts it as wardyn-macvm, with
// runnerd's argv after "--". It runs the helper's own command on that argv, on the fake engine, and
// plays the guest agent (tools mode on 1026, the control session on 1024) if stage 2 boots.
func TestUpdateToolsAsRunnerdStartsIt(t *testing.T) {
	if os.Getenv(runnerdJobEnv) == "" {
		t.Skip("cmd/wardyn-runnerd's agent drift test runs this as its helper")
	}
	agent := placeGuestAgent(t)
	eng := &macvmtest.Engine{}
	d, _, _ := testDeps(eng)
	d.stdout, d.stderr, d.powerSource = os.Stdout, os.Stderr, onAC{}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- run(ctx, flag.Args(), d) }()
	var mu sync.Mutex
	var pushed []byte
	code := -1
	for served := false; code < 0; {
		select {
		case code = <-done:
			continue
		case <-time.After(10 * time.Millisecond):
		}
		ms := eng.Machines()
		if served || len(ms) == 0 || !ms[0].Listening(macvm.ToolsPort) {
			continue
		}
		served = true
		m := ms[0]
		tools := http.NewServeMux()
		tools.HandleFunc("POST /v1/tools/push", func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			if r.URL.Query().Get("dest") == guestagent.MacAgentBinaryPath {
				mu.Lock()
				pushed = b
				mu.Unlock()
			}
		})
		tools.HandleFunc("POST /v1/tools/exec", func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(guestagent.ToolsExecResult{Output: "No new software available.", ExitCode: 0})
		})
		control := http.NewServeMux()
		control.HandleFunc("POST /v1/exec", passSelftest)
		control.HandleFunc("GET /v1/dial/tools", func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(guestagent.ToolsDial{Class: guestagent.DialConnected, Dials: 1})
		})
		go func() {
			_ = guestagent.ServeVsock(ctx, func(context.Context) (net.Conn, error) { return m.DialAgent(macvm.ToolsPort) }, tools)
		}()
		go func() {
			_ = guestagent.ServeVsock(ctx, func(context.Context) (net.Conn, error) { return m.DialAgent(macvm.AgentPort) }, control)
		}()
	}
	if code != 0 {
		t.Fatalf("update-tools %q: exit %d", flag.Args(), code)
	}
	mu.Lock()
	defer mu.Unlock()
	if !bytes.Equal(pushed, agent) {
		t.Fatalf("update-tools %q: stage 2 pushed %q to %s; want the guest agent beside the helper", flag.Args(), pushed, guestagent.MacAgentBinaryPath)
	}
	fmt.Println(runnerdJobPushed)
}
