// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

func TestRegisterCreatesPrivateBoundIdentity(t *testing.T) {
	var org string
	id := uuid.New()
	token := "wdr_" + strings.Repeat("a", 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/runners/register" || r.Header.Get("Authorization") != "" {
			t.Error("wrong route or human bearer")
		}
		var req types.RunnerRegisterRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if req.Token != token || len(req.PublicKey) != ed25519.PublicKeySize || req.Name != "laptop" {
			t.Error("invalid registration request")
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(types.RunnerRegistration{RunnerID: id, Fingerprint: runnerwire.Fingerprint(req.PublicKey), State: types.RunnerUnclaimed, OrgURLSHA256: federation.OrgURLSHA256(org)})
	}))
	defer srv.Close()
	org = srv.URL
	root := t.TempDir()
	tokenFile := filepath.Join(root, "token")
	if err := os.WriteFile(tokenFile, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "identity")
	cmd := newRegisterCommand()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--org", org, "--token-file", tokenFile, "--state-dir", state, "--name", "laptop"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	got, err := runneridentity.Load(state, org)
	if err != nil || got.RunnerID != id {
		t.Fatalf("identity:%+v %v", got, err)
	}
	text := out.String()
	if !strings.Contains(text, "registered laptop; key fingerprint "+got.Fingerprint+"\n  limits: runs at once ") ||
		!strings.HasSuffix(text, "\n  claim it: wardyn runner claim   (or in Your account → My runner)\n") || strings.Contains(text, token) {
		t.Fatal("fingerprint absent or token printed")
	}
}

func TestRegisterRefusesRedirectWithoutSendingTokenToTarget(t *testing.T) {
	called := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	root := t.TempDir()
	file := filepath.Join(root, "token")
	if err := os.WriteFile(file, []byte("wdr_"+strings.Repeat("a", 64)), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := newRegisterCommand()
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	cmd.SetArgs([]string{"--org", srv.URL, "--token-file", file, "--state-dir", filepath.Join(root, "state")})
	if err := cmd.Execute(); err == nil {
		t.Fatal("followed registration redirect")
	}
	if called {
		t.Fatal("redirect target received registration token")
	}
}

// The server's refusal reaches the person, and only a refused or spent token
// is called unusable; a server that never judged the token leaves it usable.
func TestRegisterSurfacesRefusalAndAdvisesFreshTokenOnlyWhenSpent(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		spent      bool
	}{
		{"token-refused", `{"error":"registration token is invalid, expired or already used","reason":"runner_token_invalid"}`, http.StatusUnauthorized, true},
		{"key-conflict-after-consume", `{"error":"this key was already registered; generate a new identity and registration token","reason":"runner_key_already_registered"}`, http.StatusConflict, true},
		{"runners-switched-off", `{"error":"runners are switched off on this organisation","reason":"runners_disabled"}`, http.StatusServiceUnavailable, false},
		{"rate-limited", `{"error":"too many registration attempts; retry later","reason":"runner_registration_rate_limited"}`, http.StatusTooManyRequests, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			root := t.TempDir()
			file := filepath.Join(root, "token")
			if err := os.WriteFile(file, []byte("wdr_"+strings.Repeat("a", 64)), 0600); err != nil {
				t.Fatal(err)
			}
			cmd := newRegisterCommand()
			cmd.SetOut(&strings.Builder{})
			cmd.SetErr(&strings.Builder{})
			cmd.SetArgs([]string{"--org", srv.URL, "--token-file", file, "--state-dir", filepath.Join(root, "state")})
			err := cmd.Execute()
			if err == nil {
				t.Fatal("refused registration succeeded")
			}
			var env struct{ Error string }
			if jerr := json.Unmarshal([]byte(tc.body), &env); jerr != nil {
				t.Fatal(jerr)
			}
			msg := err.Error()
			if !strings.Contains(msg, env.Error) {
				t.Fatalf("server reason missing: %s", msg)
			}
			if strings.Contains(msg, "obtain a fresh token") != tc.spent || strings.Contains(msg, "wdr_") {
				t.Fatalf("token advice wrong (spent=%v): %s", tc.spent, msg)
			}
		})
	}
}

// testHost is the computer the register tests measure: 8 CPUs, 16 GB memory, 500 GB of free disk.
var testHost = types.RunnerAmounts{CPUMillis: 8000, MemoryMiB: 16384, DiskMiB: 500 * 1024}

func init() {
	measureHost = func(context.Context) (types.RunnerAmounts, error) { return testHost, nil }
}

// registerWith runs register with args against a server that records the request and answers it.
func registerWith(t *testing.T, args ...string) (types.RunnerRegisterRequest, bool, string, error) {
	t.Helper()
	var org string
	var sent types.RunnerRegisterRequest
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(types.RunnerRegistration{RunnerID: uuid.New(), Fingerprint: runnerwire.Fingerprint(sent.PublicKey),
			State: types.RunnerUnclaimed, OrgURLSHA256: federation.OrgURLSHA256(org), Limits: sent.Limits})
	}))
	defer srv.Close()
	org = srv.URL
	root := t.TempDir()
	tokenFile := filepath.Join(root, "token")
	if err := os.WriteFile(tokenFile, []byte("wdr_"+strings.Repeat("b", 64)), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := newRegisterCommand()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs(append([]string{"--org", org, "--token-file", tokenFile, "--state-dir", filepath.Join(root, "identity")}, args...))
	err := cmd.Execute()
	return sent, called, out.String(), err
}

// Registration measures the computer and sends the proposal (owner ruling RP Q3): half of each as the
// total, and per run the total divided by Runs at once, at least 1 CPU and 1 GB; a flag overrides its
// own amount, and the per-run amounts follow a changed total. A limit past what the computer reports,
// or Runs at once of 0, is refused before anything is sent (the mock's register lines).
func TestRegisterSendsMeasuredProposals(t *testing.T) {
	sent, _, out, err := registerWith(t)
	want := types.RunnerLimits{RunsAtOnce: 4, Total: types.RunnerAmounts{CPUMillis: 4000, MemoryMiB: 8192, DiskMiB: 250 * 1024},
		PerRun: types.RunnerAmounts{CPUMillis: 1000, MemoryMiB: 2048, DiskMiB: 62 * 1024}}
	if err != nil || sent.Limits == nil || *sent.Limits != want || !strings.Contains(out, "this computer reports 8 CPUs, 16 GB memory, 500 GB disk") {
		t.Fatalf("measured proposal sent %+v, %v\n%s", sent.Limits, err, out)
	}
	sent, _, _, err = registerWith(t, "--max-runs", "2", "--total-cpu", "4", "--total-memory", "8", "--total-disk", "250",
		"--per-run-cpu", "2", "--per-run-memory", "4", "--per-run-disk", "125")
	want = types.RunnerLimits{RunsAtOnce: 2, Total: types.RunnerAmounts{CPUMillis: 4000, MemoryMiB: 8192, DiskMiB: 250 * 1024},
		PerRun: types.RunnerAmounts{CPUMillis: 2000, MemoryMiB: 4096, DiskMiB: 125 * 1024}}
	if err != nil || *sent.Limits != want {
		t.Fatalf("every limit as a flag sent %+v, %v", sent.Limits, err)
	}
	if sent, _, _, err = registerWith(t, "--max-runs", "2", "--total-memory", "12"); err != nil || sent.Limits.Total.MemoryMiB != 12*1024 || sent.Limits.PerRun.MemoryMiB != 6*1024 {
		t.Fatalf("a changed total = %+v, %v; want its per-run share to follow", sent.Limits, err)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--total-memory", "64"}, "--total-memory 64 is more than this computer reports (16 GB)"},
		{[]string{"--max-runs", "0"}, "--max-runs is a whole number, 1 or more"},
		{[]string{"--max-runs", "65"}, "runs at once is a whole number from 1 to 64"},
		{[]string{"--per-run-memory", "16", "--total-memory", "8"}, "a per-run limit cannot be more than the runner's total"},
	} {
		if _, called, _, err := registerWith(t, tc.args...); err == nil || !strings.Contains(err.Error(), tc.want) || called {
			t.Errorf("%v = %v (sent %v), want %q before anything is sent", tc.args, err, called, tc.want)
		}
	}
	measureHost = func(context.Context) (types.RunnerAmounts, error) {
		return types.RunnerAmounts{CPUMillis: 8000, MemoryMiB: 16384}, nil
	}
	t.Cleanup(func() { measureHost = func(context.Context) (types.RunnerAmounts, error) { return testHost, nil } })
	if _, called, _, err := registerWith(t); err == nil || !strings.Contains(err.Error(), "--total-disk") || called {
		t.Fatalf("an unmeasured disk = %v, want --total-disk asked for before anything is sent", err)
	}
	if sent, _, _, err := registerWith(t, "--total-disk", "100"); err != nil || sent.Limits.Total.DiskMiB != 100*1024 {
		t.Fatalf("an unmeasured disk given = %+v, %v", sent.Limits, err)
	}
}
