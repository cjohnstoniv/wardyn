// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
)

// revokeRowData is the data of the credential.revoke row that carries jti.
func revokeRowData(t *testing.T, au *fakeAudit, jti string) map[string]any {
	t.Helper()
	for _, ev := range au.byAction("credential.revoke") {
		var d map[string]any
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatalf("decode revoke data: %v", err)
		}
		if d["jti"] == jti {
			return d
		}
	}
	t.Fatalf("no credential.revoke row carries jti %q", jti)
	return nil
}

// A sealed token opens only under its own run, jti and key version.
func TestTokenSeal_AADBindsRunJTIVersion(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	runID, jti, version := uuid.New(), "jti-1", 3
	sealed, err := kek.Seal(key, []byte("ghs_secret_value"), tokenSealAAD(runID, jti, version))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("ghs_secret_value")) {
		t.Fatal("the sealed copy carries the token in clear")
	}
	plain, err := kek.Open(key, sealed, tokenSealAAD(runID, jti, version))
	if err != nil || string(plain) != "ghs_secret_value" {
		t.Fatalf("open under its own AAD = %q, %v", plain, err)
	}
	for name, aad := range map[string][]byte{
		"another run":     tokenSealAAD(uuid.New(), jti, version),
		"another jti":     tokenSealAAD(runID, "jti-2", version),
		"another version": tokenSealAAD(runID, jti, version+1),
	} {
		if _, err := kek.Open(key, sealed, aad); err == nil {
			t.Errorf("the sealed token opened under %s", name)
		}
	}
	// The label separates this AAD from every other purpose's.
	if bytes.Equal(tokenSealAAD(runID, jti, version), kek.Encode("wardyn/ado-run-pat/v1", runID.String(), jti, "3")) {
		t.Fatal("the github token seal's AAD equals another purpose's")
	}
}

// The retry wait doubles from 30 s per failed attempt and stops at 10 minutes.
func TestGitHubRevokeBackoff(t *testing.T) {
	for attempts, want := range map[int]time.Duration{
		1: 30 * time.Second, 2: time.Minute, 3: 2 * time.Minute, 4: 4 * time.Minute, 5: 8 * time.Minute,
		6: 10 * time.Minute, 40: 10 * time.Minute,
	} {
		if got := githubRevokeBackoff(attempts); got != want {
			t.Errorf("githubRevokeBackoff(%d) = %s, want %s", attempts, got, want)
		}
	}
}

// A broker with no token seal: RevokeRun reads and queues nothing and stamps none; the worker returns at once.
func TestRevokeRun_NoTokenSealStampsNone(t *testing.T) {
	b, db, au, gh := newTestBroker(t)
	runID := uuid.New()
	gid := seedGrant(db, runID, githubGrantSpec(t, false))
	minted, err := b.MintForGrant(t.Context(), callerFor(runID), gid)
	if err != nil {
		t.Fatalf("github mint: %v", err)
	}
	if err := b.RevokeRun(t.Context(), runID); err != nil {
		t.Fatalf("RevokeRun: %v", err)
	}
	if got := revokeRowData(t, au, minted.JTI)["target_revoke"]; got != targetRevokeNone {
		t.Fatalf("target_revoke = %v, want %s", got, targetRevokeNone)
	}
	done := make(chan struct{})
	go func() { b.RunGitHubTokenRevokes(t.Context()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunGitHubTokenRevokes did not return at once on a broker with no token seal")
	}
	if gh.Revoked != 0 {
		t.Fatalf("revoked = %d, want 0", gh.Revoked)
	}
	if n, err := b.DeleteOwnerTokenSeals(t.Context(), "anyone"); n != 0 || err != nil {
		t.Fatalf("DeleteOwnerTokenSeals with no seal = %d, %v", n, err)
	}
}
