// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runMainEnv makes the test binary run main() itself, so a test can drive the
// real entry point (flags, os.Exit) as a subprocess.
const runMainEnv = "WARDYN_PROXY_TEST_RUN_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(runMainEnv) == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

// TestApplyLLMScanSwitch: a disable token nils the policy's inspection, an
// enable token or unset leaves it exactly as authored, and anything else is
// exit 2 with the policy untouched — a typo must never be read as "off" or
// silently ignored.
func TestApplyLLMScanSwitch(t *testing.T) {
	for _, tc := range []struct {
		v        string
		wantCode int
		wantNil  bool
	}{
		{"off", 0, true},
		{"0", 0, true},
		{"false", 0, true},
		{" OFF ", 0, true},
		{"", 0, false},
		{"on", 0, false},
		{"true", 0, false},
		{"of", 2, false},
		{"maybe", 2, false},
	} {
		cfg := &proxy.Config{Policy: types.RunPolicySpec{LLMInspection: &types.LLMInspectionSpec{}}}
		code := applyLLMScanSwitch(cfg, tc.v)
		if code != tc.wantCode || (cfg.Policy.LLMInspection == nil) != tc.wantNil {
			t.Errorf("applyLLMScanSwitch(%q) = %d, inspection nil = %v; want %d, nil = %v",
				tc.v, code, cfg.Policy.LLMInspection == nil, tc.wantCode, tc.wantNil)
		}
	}
}

// TestCgroupMemoryLimitBytes: "max", a missing file and v1's unlimited
// sentinel are no limit; a number is the limit; the first usable file wins.
func TestCgroupMemoryLimitBytes(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	unlimited := write("max", "max\n")
	limited := write("limited", "268435456\n")
	sentinel := write("v1-sentinel", "9223372036854771712\n")
	missing := filepath.Join(dir, "missing")

	for _, tc := range []struct {
		name   string
		paths  []string
		want   int64
		wantOK bool
	}{
		{"max", []string{unlimited}, 0, false},
		{"number", []string{limited}, 268435456, true},
		{"missing", []string{missing}, 0, false},
		{"v1 unlimited sentinel", []string{sentinel}, 0, false},
		{"falls through to the next file", []string{missing, unlimited, limited}, 268435456, true},
	} {
		if got, ok := cgroupMemoryLimitBytes(tc.paths...); got != tc.want || ok != tc.wantOK {
			t.Errorf("%s: cgroupMemoryLimitBytes = (%d, %v), want (%d, %v)", tc.name, got, ok, tc.want, tc.wantOK)
		}
	}
}

// TestEgressCanaryExitCodes runs the real binary entry point: -egress-canary
// exits 0 when the TCP dial connects and 1 when it is refused. The k8s
// substrate reads "NetworkPolicy enforced" from that exit code, so a canary
// that exited 0 on a refused dial would report enforcement that is not there.
func TestEgressCanaryExitCodes(t *testing.T) {
	open, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = open.Close() })
	closedLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := closedLn.Addr().String()
	_ = closedLn.Close()

	for _, tc := range []struct {
		name, addr string
		want       int
	}{
		{"listening port", open.Addr().String(), 0},
		{"closed port", closedAddr, 1},
	} {
		cmd := exec.Command(os.Args[0], "-egress-canary", tc.addr)
		cmd.Env = append(os.Environ(), runMainEnv+"=1")
		err := cmd.Run()
		code := 0
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else if err != nil {
			t.Fatalf("%s: run canary: %v", tc.name, err)
		}
		if code != tc.want {
			t.Errorf("%s: -egress-canary %s exited %d, want %d", tc.name, tc.addr, code, tc.want)
		}
	}
}
