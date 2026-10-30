// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// The gateway's seed wait is for a shell startup command, which may be bringing the declared app up. A harness
// prompt seed starts no server, so a missing launcher there is the old immediate 502 (#3391). The probe runs under a
// real sh, as the sandbox runs it, with a fake socat on PATH and the sandbox's WARDYN_INTERACTIVE_START set the way
// the run's interactive_start sets it; the port opens on the third probe.
func TestUIGateway_OnlyAShellStartupCommandWaitsForTheApp(t *testing.T) {
	sh, err := osexec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on this host")
	}
	for _, tc := range []struct {
		name         string
		start        string // WARDYN_INTERACTIVE_START in the sandbox: "" is a shell startup command
		wantCode     int
		wantAtLeast  time.Duration
		wantAtMost   time.Duration
		wantContains string
	}{
		{"a shell startup command still waits", "", http.StatusOK, 900 * time.Millisecond, 0, ""},
		{"a harness prompt seed answers at once", "agent", http.StatusBadGateway, 0, 2 * time.Second, "no UI launcher in this image"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			socat := "#!/bin/sh\nn=$(cat \"" + dir + "/n\" 2>/dev/null || echo 0)\nn=$((n+1))\necho $n >\"" + dir + "/n\"\n[ $n -ge 3 ]\n"
			if err := os.WriteFile(filepath.Join(dir, "socat"), []byte(socat), 0o755); err != nil {
				t.Fatal(err)
			}
			h := newUIHarness(t, okBackend())
			h.run.Interactive, h.run.Task = true, "npm run dev"
			h.store.putRun(h.run)
			next := h.runner.execFn
			h.runner.execFn = func(spec runner.ExecSpec) (*runner.ExecSession, error) {
				if spec.Argv[0] != "sh" {
					return next(spec)
				}
				cmd := osexec.Command(sh, spec.Argv[1:]...)
				cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "WARDYN_INTERACTIVE_START="+tc.start)
				return &runner.ExecSession{Wait: func() (int, error) {
					if err := cmd.Run(); err != nil {
						if ee, ok := err.(*osexec.ExitError); ok {
							return ee.ExitCode(), nil
						}
						return -1, err
					}
					return 0, nil
				}}, nil
			}
			cookie := h.openSession()
			start := time.Now()
			rec := h.relay("/ide", cookie, nil)
			elapsed := time.Since(start)
			if rec.Code != tc.wantCode || !strings.Contains(rec.Body.String(), tc.wantContains) {
				t.Fatalf("relay = %d %s, want %d containing %q", rec.Code, rec.Body.String(), tc.wantCode, tc.wantContains)
			}
			if elapsed < tc.wantAtLeast || (tc.wantAtMost > 0 && elapsed > tc.wantAtMost) {
				t.Fatalf("relay took %s, want between %s and %s", elapsed, tc.wantAtLeast, tc.wantAtMost)
			}
		})
	}
}
