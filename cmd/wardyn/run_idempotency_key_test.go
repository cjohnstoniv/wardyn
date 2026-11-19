// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// keyServer answers POST /api/v1/runs with a 201 (marked as a replay when replayed) and records each Idempotency-Key.
func keyServer(t *testing.T, replayed bool) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		mu.Unlock()
		if replayed {
			w.Header().Set("Idempotent-Replayed", "true")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"11111111-1111-1111-1111-111111111111","state":"PENDING"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), keys...) }
}

func runForOutput(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := rootCmd()
	var out, errOut strings.Builder
	root.SetArgs(args)
	root.SetOut(&out)
	root.SetErr(&errOut)
	err = root.Execute()
	return out.String(), errOut.String(), err
}

// TestRunIdempotencyKeyFlag: --idempotency-key is the request's Idempotency-Key, no flag sends a fresh UUID, and a
// replayed answer adds one stderr line and leaves stdout as for a new run.
func TestRunIdempotencyKeyFlag(t *testing.T) {
	base := func(srv *httptest.Server, extra ...string) []string {
		return append([]string{"run", "--agent", "claude-code", "--url", srv.URL, "--token", "t"}, extra...)
	}

	srv, keys := keyServer(t, false)
	stdout, stderr, err := runForOutput(t, base(srv, "--idempotency-key", "build-9-test")...)
	if err != nil {
		t.Fatal(err)
	}
	if got := keys(); len(got) != 1 || got[0] != "build-9-test" {
		t.Errorf("keys sent = %q, want build-9-test", got)
	}
	if strings.Contains(stderr, "already created") {
		t.Errorf("a first answer printed the replay line: %q", stderr)
	}

	if _, _, err := runForOutput(t, base(srv)...); err != nil {
		t.Fatal(err)
	}
	if got := keys(); len(got) != 2 {
		t.Fatalf("keys sent = %q, want 2", got)
	} else if _, err := uuid.Parse(got[1]); err != nil {
		t.Errorf("no flag sent key %q, want a fresh UUID", got[1])
	}

	rsrv, _ := keyServer(t, true)
	rOut, rErr, err := runForOutput(t, base(rsrv, "--idempotency-key", "build-9-test")...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rErr, "run 11111111-1111-1111-1111-111111111111 was already created by an earlier attempt with this key") {
		t.Errorf("stderr = %q, want the replay line", rErr)
	}
	if rOut != stdout {
		t.Errorf("a replay changed stdout:\n%q\nvs\n%q", rOut, stdout)
	}
}

// TestRunFileTakesTheIdempotencyKeyFromTheFlag: the flag stands beside --file instead of clashing with it.
func TestRunFileTakesTheIdempotencyKeyFromTheFlag(t *testing.T) {
	rig := runFileRig(t)
	file := writeTemplateFile(t, "run.yaml", runDocYAML)
	if _, err := rig.run(t, "run", "--file", file, "--idempotency-key", "ci-7"); err != nil {
		t.Fatal(err)
	}
	if sent := rig.sent(runsPath); len(sent) != 1 {
		t.Errorf("create requests = %q; want one", sent)
	}
}
