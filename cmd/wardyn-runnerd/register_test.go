// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
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
	if !strings.Contains(out.String(), got.Fingerprint) || strings.Contains(out.String(), token) {
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
