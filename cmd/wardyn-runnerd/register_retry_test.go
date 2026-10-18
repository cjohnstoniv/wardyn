// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestRegisterIfUnregisteredRetriesWithTheSameKey: the chart's init container retries a registration
// the organisation did not answer, a bounded number of times and with the key it already saved, so
// one failure no longer leaves runner.key behind to fail every later start ("file exists").
func TestRegisterIfUnregisteredRetriesWithTheSameKey(t *testing.T) {
	attempts, backoff := registerAttempts, registerBackoff
	registerAttempts, registerBackoff = 3, time.Millisecond
	t.Cleanup(func() { registerAttempts, registerBackoff = attempts, backoff })
	var org string
	var keys [][]byte
	id := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req types.RunnerRegisterRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		keys = append(keys, req.PublicKey)
		if len(keys) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(types.RunnerRegistration{RunnerID: id, Fingerprint: runnerwire.Fingerprint(req.PublicKey), State: types.RunnerUnclaimed, OrgURLSHA256: federation.OrgURLSHA256(org)})
	}))
	defer srv.Close()
	org = srv.URL
	root := t.TempDir()
	tokenFile := filepath.Join(root, "token")
	if err := os.WriteFile(tokenFile, []byte("wdr_"+strings.Repeat("a", 64)), 0600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "identity")
	cmd := newRegisterCommand()
	var out, errOut strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--org", org, "--token-file", tokenFile, "--state-dir", state, "--name", "k6", "--if-unregistered"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("register: %v\n%s", err, errOut.String())
	}
	if len(keys) != 3 || !bytes.Equal(keys[0], keys[1]) || !bytes.Equal(keys[1], keys[2]) {
		t.Fatalf("%d attempts, keys equal: %v", len(keys), len(keys) == 3 && bytes.Equal(keys[0], keys[2]))
	}
	if !strings.Contains(errOut.String(), "registration attempt 2 of 3 failed") {
		t.Fatalf("no retry log: %q", errOut.String())
	}
	if got, err := runneridentity.Load(state, org); err != nil || got.RunnerID != id {
		t.Fatalf("identity %+v %v", got, err)
	}
}
