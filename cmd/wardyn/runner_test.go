// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

func TestRunnerClaimUsesLocalFingerprintAndOrgBinding(t *testing.T) {
	id := uuid.New()
	srv := newCmdServer(t, http.StatusOK, types.RunnerRegistration{RunnerID: id, State: types.RunnerClaimed})
	state := filepath.Join(t.TempDir(), "runner")
	key, err := runneridentity.Generate(state)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := runnerwire.Fingerprint(key.Public().(ed25519.PublicKey))
	if err := runneridentity.Save(state, runneridentity.Identity{RunnerID: id, PrivateKey: key, Fingerprint: fingerprint, OrgURLSHA256: federation.OrgURLSHA256(srv.URL)}); err != nil {
		t.Fatal(err)
	}
	root := rootCmd()
	var out strings.Builder
	root.SetOut(&out)
	root.SetErr(&strings.Builder{})
	root.SetArgs([]string{"runner", "claim", "--state-dir", state, "--url", srv.URL, "--token", "personal-token"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	req := srv.last()
	var body types.RunnerClaimRequest
	if err := json.Unmarshal(req.body, &body); err != nil {
		t.Fatal(err)
	}
	if req.path != "/api/v1/me/runners/"+id.String()+"/claim" || body.Fingerprint != fingerprint {
		t.Fatalf("claim request:%s %+v", req.path, body)
	}
	if err := execCmd(t, "runner", "claim", "--state-dir", state, "--url", "https://other.example.com", "--token", "personal-token"); err == nil {
		t.Fatal("sent local fingerprint to another organisation")
	}
}
