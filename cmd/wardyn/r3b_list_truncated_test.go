// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// r3bTruncatedListServer answers every list GET with one row and the server's
// X-Wardyn-Truncated marker.
func r3bTruncatedListServer(t *testing.T, body any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Wardyn-Truncated", "true")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// r3bCaptureStdout runs fn with os.Stdout redirected to a pipe and returns what
// it wrote. emitJSON encodes to os.Stdout directly, NOT cmd.OutOrStdout(), so
// cobra's own out buffer never sees the --json payload — capturing the real fd
// is the only way to assert the shape a script actually parses.
func r3bCaptureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	saved := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		_, _ = io.Copy(&b, r)
		done <- b.String()
	}()
	fn()
	os.Stdout = saved
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}

// TestR3BListCmdsWarnOnTruncation is F265's CLI half: a list command against a
// page the server flagged truncated printed the rows, exited 0, wrote nothing to
// stderr and left no marker in --json — indistinguishable from a complete list.
// `wardyn audit` has warned on exactly this signal since W16-S1-2; the four list
// families named in the finding now do the same.
//
// ALL FOUR, because the first pass covered two. `policy list` and `workspace
// list` kept calling the plain SDK wrappers, so the truncation bit the server
// set was discarded before the CLI could see it and neither command had an
// --offset to page with — the same defect, unfixed, on a binary whose sibling
// commands were fixed. A table with two of the four entries is what let that
// read as done.
//
// STDERR is asserted, and stdout is asserted NOT to carry it: --json output
// must keep the plain array shape existing scripts parse.
func TestR3BListCmdsWarnOnTruncation(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		body any
	}{
		{"run list", []string{"run", "list"}, []types.AgentRun{{ID: uuid.New(), State: types.RunRunning}}},
		{"approvals list", []string{"approvals", "list"}, []types.ApprovalRequest{{ID: uuid.New(), RunID: uuid.New(), Kind: types.ApprovalEgressDomain}}},
		// The two the first pass skipped. Both were named in this finding's own
		// blast radius and both still reproduced it verbatim on the FIXED
		// binary: rows, exit 0, EMPTY stderr, no marker in --json — which is
		// this finding's definition of the defect, not a lesser version of it.
		{"policy list", []string{"policy", "list"}, []types.RunPolicy{{ID: uuid.New(), Name: "example"}}},
		{"workspace list", []string{"workspace", "list"}, []types.Workspace{{ID: uuid.New(), Name: "example"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := r3bTruncatedListServer(t, tc.body)
			root := rootCmd()
			errBuf := &strings.Builder{}
			root.SetArgs(append(append([]string{}, tc.args...), "--limit", "1", "--offset", "1", "--json", "--url", srv.URL, "--token", "tok"))
			root.SetOut(&strings.Builder{})
			root.SetErr(errBuf)
			var execErr error
			stdout := r3bCaptureStdout(t, func() { execErr = root.Execute() })
			if execErr != nil {
				t.Fatalf("%s returned error: %v", tc.name, execErr)
			}
			if !strings.Contains(errBuf.String(), "truncated") {
				t.Errorf("%s: stderr = %q, want a truncation warning — a silently incomplete list at exit 0 "+
					"is the whole finding", tc.name, errBuf.String())
			}
			// The wording is auditCmd's, noun swapped: one family of warning, and
			// the remedy it names has to be a flag these commands actually carry,
			// which is why --offset was added alongside it.
			if !strings.Contains(errBuf.String(), "--offset=2") {
				t.Errorf("%s: stderr = %q, want it to name --offset=2 (offset 1 + the 1 row shown), the way "+
					"`wardyn audit` already does", tc.name, errBuf.String())
			}
			// --json stays BYTE-FOR-BYTE the plain array scripts/ci-run.sh parses:
			// decoded as []any it must be exactly the rows, with no wrapper object
			// and no warning text anywhere in the stream.
			if strings.Contains(stdout, "truncated") {
				t.Errorf("%s: the warning leaked into stdout (%q) — --json must stay the plain array shape",
					tc.name, stdout)
			}
			if trimmed := strings.TrimSpace(stdout); !strings.HasPrefix(trimmed, "[") || !strings.HasSuffix(trimmed, "]") {
				t.Errorf("%s: --json stdout is not a bare array (%q) — scripts/ci-run.sh parses the plain shape",
					tc.name, stdout)
			}
			var rows []any
			if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
				t.Errorf("%s: --json stdout is not valid JSON (%v): %q", tc.name, err, stdout)
			} else if len(rows) != 1 {
				t.Errorf("%s: --json array has %d rows, want the 1 the server served", tc.name, len(rows))
			}
		})
	}

	// The control: no header, no warning. Otherwise every list would cry wolf.
	t.Run("a complete page warns about nothing", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]types.AgentRun{{ID: uuid.New(), State: types.RunRunning}})
		}))
		t.Cleanup(srv.Close)
		root := rootCmd()
		errBuf := &strings.Builder{}
		root.SetArgs([]string{"run", "list", "--json", "--url", srv.URL, "--token", "tok"})
		root.SetOut(&strings.Builder{})
		root.SetErr(errBuf)
		if err := root.Execute(); err != nil {
			t.Fatalf("run list returned error: %v", err)
		}
		if strings.Contains(errBuf.String(), "truncated") {
			t.Errorf("stderr = %q on a COMPLETE page, want nothing", errBuf.String())
		}
	})
}
