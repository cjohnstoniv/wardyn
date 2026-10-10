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
	"time"

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

func TestRunnerListAndTokensCommands(t *testing.T) {
	id := uuid.New()
	seen := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	views := []types.RunnerView{{ID: id, Owner: "alice@example.com", Name: "laptop-7", State: types.RunnerClaimed, Online: true, LastSeenAt: &seen, RunsActive: 2, KeyFingerprint: "abcd1234"}}
	srv := newCmdServer(t, http.StatusOK, views)

	out, err := execCmdCapture(t, "runner", "list", "--all", "--state", "revoked", "--url", srv.URL, "--token", "t")
	if err != nil {
		t.Fatal(err)
	}
	if req := srv.last(); req.path != "/api/v1/runners" || req.query != "state=revoked" {
		t.Fatalf("--all request %s?%s", req.path, req.query)
	}
	for _, want := range []string{"alice@example.com", "laptop-7", "online", "abcd1234"} {
		if !strings.Contains(out, want) {
			t.Errorf("admin list lacks %q:\n%s", want, out)
		}
	}

	out, err = execCmdCapture(t, "runner", "list", "--url", srv.URL, "--token", "t")
	if err != nil || srv.last().path != "/api/v1/me/runners" || strings.Contains(out, "OWNER") {
		t.Fatalf("own list: %v path %s\n%s", err, srv.last().path, out)
	}
	if _, err := execCmdCapture(t, "runner", "list", "--state", "revoked", "--url", srv.URL, "--token", "t"); err == nil {
		t.Fatal("--state without --all was accepted")
	}

	tokens := []types.RunnerRegistrationToken{{ID: id, Owner: "dan@example.com", MintedBy: "admin", CreatedAt: seen, ExpiresAt: seen.Add(72 * time.Hour)}}
	tsrv := newCmdServer(t, http.StatusOK, tokens)
	out, err = execCmdCapture(t, "runner", "tokens", "list", "--url", tsrv.URL, "--token", "t")
	if err != nil || tsrv.last().path != "/api/v1/runners/tokens" || !strings.Contains(out, "dan@example.com") {
		t.Fatalf("tokens list: %v\n%s", err, out)
	}
	rsrv := newCmdServer(t, http.StatusNoContent, nil)
	out, err = execCmdCapture(t, "runner", "tokens", "revoke", id.String(), "--url", rsrv.URL, "--token", "t")
	if req := rsrv.last(); err != nil || req.method != http.MethodDelete || req.path != "/api/v1/runners/tokens/"+id.String() || !strings.Contains(out, id.String()) {
		t.Fatalf("tokens revoke: %v %+v\n%s", err, req, out)
	}
	if _, err := execCmdCapture(t, "runner", "tokens", "revoke", "not-a-uuid", "--url", rsrv.URL, "--token", "t"); err == nil {
		t.Fatal("a malformed token id was sent")
	}
}
