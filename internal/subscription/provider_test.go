// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package subscription

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// writeCreds writes a minimal credentials.json with the given access token and
// expiry to path.
func writeCreds(t *testing.T, path, token string, expiresAt time.Time) {
	t.Helper()
	body := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":"rt","expiresAt":%d,"subscriptionType":"max"}}`,
		token, expiresAt.UnixMilli())
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newTestProvider(t *testing.T, credPath, claudeBin string, now time.Time) Provider {
	t.Helper()
	p, err := New(Config{
		CredPath:  credPath,
		ClaudeBin: claudeBin,
		Now:       func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCurrent_Piggyback(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	dir := t.TempDir()
	cred := filepath.Join(dir, ".credentials.json")
	writeCreds(t, cred, "fresh-token", now.Add(time.Hour)) // well beyond the margin

	// claudeBin points at a binary that MUST NOT run on the piggyback path.
	p := newTestProvider(t, cred, filepath.Join(dir, "should-not-exist"), now)
	tok, err := p.Current(context.Background())
	if err != nil {
		t.Fatalf("piggyback Current: %v", err)
	}
	if tok.Value != "fresh-token" {
		t.Errorf("token = %q, want fresh-token", tok.Value)
	}
}

func TestCurrent_DelegatesRefreshWhenNearExpiry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fake-claude is POSIX")
	}
	now := time.Unix(1_700_000_000, 0)
	dir := t.TempDir()
	cred := filepath.Join(dir, ".credentials.json")
	writeCreds(t, cred, "stale-token", now.Add(time.Minute)) // inside the 10m margin

	// Fake claude: rewrites the creds with a fresh token + far expiry, mimicking
	// the resident CLI refreshing + writing back.
	freshExp := now.Add(4 * time.Hour).UnixMilli()
	fake := filepath.Join(dir, "claude")
	script := fmt.Sprintf("#!/bin/sh\ncat > %q <<'EOF'\n"+
		`{"claudeAiOauth":{"accessToken":"refreshed-token","refreshToken":"rt2","expiresAt":%d,"subscriptionType":"max"}}`+
		"\nEOF\n", cred, freshExp)
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	p := newTestProvider(t, cred, fake, now)
	tok, err := p.Current(context.Background())
	if err != nil {
		t.Fatalf("refresh-path Current: %v", err)
	}
	if tok.Value != "refreshed-token" {
		t.Errorf("token = %q, want refreshed-token (delegate refresh should have run)", tok.Value)
	}
}

func TestCurrent_FailsClosedWhenRefreshDoesNotHelp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fake-claude is POSIX")
	}
	now := time.Unix(1_700_000_000, 0)
	dir := t.TempDir()
	cred := filepath.Join(dir, ".credentials.json")
	writeCreds(t, cred, "stale-token", now.Add(-time.Hour)) // already expired

	// Fake claude that does nothing (leaves the expired token in place).
	fake := filepath.Join(dir, "claude")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	p := newTestProvider(t, cred, fake, now)
	if _, err := p.Current(context.Background()); err == nil {
		t.Fatal("expected fail-closed error when the token stays expired after refresh")
	}
}

// Peek is read-only: it returns an EXPIRED token as-is (never refreshes, never
// runs claude) so the status surface can render "expired" rather than "absent".
func TestPeek_ReadsExpiredWithoutRefresh(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	dir := t.TempDir()
	cred := filepath.Join(dir, ".credentials.json")
	writeCreds(t, cred, "expired-token", now.Add(-time.Hour)) // already expired

	// claudeBin points at a binary that MUST NOT run — Peek never delegates.
	p := newTestProvider(t, cred, filepath.Join(dir, "should-not-exist"), now)
	tok, err := p.Peek()
	if err != nil {
		t.Fatalf("Peek(expired): %v", err)
	}
	if tok.Value != "expired-token" {
		t.Errorf("token = %q, want expired-token", tok.Value)
	}
	if tok.ExpiresAt.After(now) {
		t.Errorf("ExpiresAt = %v, want <= now (%v) so the caller sees expiry", tok.ExpiresAt, now)
	}

	// A missing credentials file surfaces as an error, not a panic.
	p2 := newTestProvider(t, filepath.Join(dir, "absent.json"), "claude", now)
	if _, err := p2.Peek(); err == nil {
		t.Fatal("Peek on a missing credentials file must error")
	}
}

func TestRead_RejectsMissingToken(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	dir := t.TempDir()
	cred := filepath.Join(dir, ".credentials.json")
	// Structurally valid JSON but no accessToken.
	if err := os.WriteFile(cred, []byte(`{"claudeAiOauth":{"expiresAt":0}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// A fake claude that also can't help, so Current must fail closed.
	fake := filepath.Join(dir, "claude")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	p := newTestProvider(t, cred, fake, now)
	if _, err := p.Current(context.Background()); err == nil {
		t.Fatal("expected error for credentials with no accessToken")
	}

	// Sanity: a well-formed file parses.
	writeCreds(t, cred, "tok", now.Add(time.Hour))
	var cf credFile
	b, _ := os.ReadFile(cred)
	if err := json.Unmarshal(b, &cf); err != nil || cf.ClaudeAiOauth.AccessToken != "tok" {
		t.Fatalf("parse sanity failed: %v / %q", err, cf.ClaudeAiOauth.AccessToken)
	}
}
