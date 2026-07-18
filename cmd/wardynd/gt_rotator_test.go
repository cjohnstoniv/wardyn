// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
)

type fakeGTMinter struct {
	token  string
	expiry time.Time
	calls  int
}

func (f *fakeGTMinter) MintRunIdentity(context.Context, uuid.UUID, string, string, string) (identity.RunIdentity, error) {
	f.calls++
	return identity.RunIdentity{Token: f.token, Expiry: f.expiry}, nil
}

func TestWriteTokenFileAtomic_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gt-token")
	if err := writeTokenFileAtomic(path, "tok-abc"); err != nil {
		t.Fatalf("write: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := strings.TrimSpace(string(b)); got != "tok-abc" {
		t.Errorf("token file = %q, want tok-abc", got)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("token file perms = %o, want 600", fi.Mode().Perm())
	}
}

// TestRotator_SeedsFileImmediately is the producer regression: the shipped
// deployment had no process keeping the token file fresh, so the ingest went blind
// ~1h in. The rotator must mint + write a token to the shared file at once (so the
// ingest has one on first read) and then keep it fresh. Cancel after the seed.
func TestRotator_SeedsFileImmediately(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gt-token")
	m := &fakeGTMinter{token: "seed-token", expiry: time.Now().Add(time.Hour)}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { runGroundtruthTokenRotator(ctx, m, path); close(done) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(b)) == "seed-token" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	b, err := os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(b)) != "seed-token" {
		t.Fatalf("rotator must seed the token file immediately; got %q err=%v", string(b), err)
	}
	if m.calls == 0 {
		t.Error("rotator must mint at least once")
	}
}
