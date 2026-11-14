// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/federation"
)

// TestRunnerSetupThatCannotReadTheRunnerSaysSo: when the organisation refuses the credential at the final wait,
// setup stops at once and says the connection could not be confirmed, and neither blames the runner nor offers
// the restore line; with no read answering until the wait ends, it says the same.
func TestRunnerSetupThatCannotReadTheRunnerSaysSo(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		polls  func(n int) bool
	}{
		// Two reads come before the wait: the registered runner's version, and its claim state.
		{"refused", http.StatusUnauthorized, func(n int) bool { return n == 3 }},
		{"never answered", http.StatusBadGateway, func(n int) bool { return n > 3 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newSetupRig(t)
			bin, sum := r.fakeRunnerd(r.binDir, 0)
			pinnedRunnerdSHA256 = sum
			if err := os.MkdirAll(r.stateDir(), 0o700); err != nil {
				t.Fatal(err)
			}
			id := fmt.Sprintf(`{"runner_id":%q,"key_fingerprint":"fp-1","org_url_sha256":%q}`, r.runnerID, federation.OrgURLSHA256(r.srv.URL))
			if err := os.WriteFile(filepath.Join(r.stateDir(), "runner.json"), []byte(id), 0o600); err != nil {
				t.Fatal(err)
			}
			binDir := filepath.Join(r.home, ".local", "share", "wardyn", "bin")
			if err := os.MkdirAll(binDir, 0o755); err != nil {
				t.Fatal(err)
			}
			prev := filepath.Join(binDir, "wardyn-runnerd.previous")
			if err := os.WriteFile(prev, []byte("old"), 0o755); err != nil {
				t.Fatal(err)
			}
			r.claimed, r.version, r.readStatus = true, "0.9.6", tc.status
			_, err := r.run("--token", "wdn_personal", "--runnerd", bin)
			prefix := strings.TrimSuffix(errSetupNotConfirmed, "%w")
			if err == nil || !strings.HasPrefix(err.Error(), prefix) || strings.Contains(err.Error(), "wardyn-runnerd.previous") {
				t.Fatalf("err = %v, want %q and no restore line", err, prefix)
			}
			if n := len(r.called("GET /api/v1/me/runners/" + r.runnerID.String())); !tc.polls(n) {
				t.Errorf("read the runner %d times", n)
			}
		})
	}
}
