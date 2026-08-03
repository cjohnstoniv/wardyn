// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// fakeTryLock is a controllable tryLock seam for runGroundtruthTokenRotatorLeader
// (mirrors db.TryAdvisoryLock's shape without needing Postgres). held forces
// every acquire attempt to report "someone else has it"; releases counts how
// many times a returned release func was actually invoked.
type fakeTryLock struct {
	mu       sync.Mutex
	held     bool
	attempts int
	releases int
}

func (f *fakeTryLock) try(context.Context) (func(), bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	if f.held {
		return nil, false, nil
	}
	return func() {
		f.mu.Lock()
		f.releases++
		f.mu.Unlock()
	}, true, nil
}

func (f *fakeTryLock) snapshot() (attempts, releases int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts, f.releases
}

// TestRotatorLeader_StandbySkipsRotation covers the lock-held-elsewhere case:
// a replica that never wins the lock must never mint or write the token file,
// no matter how long it parks in the backoff loop.
func TestRotatorLeader_StandbySkipsRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gt-token")
	m := &fakeGTMinter{token: "seed-token", expiry: time.Now().Add(time.Hour)}
	lock := &fakeTryLock{held: true}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { runGroundtruthTokenRotatorLeader(ctx, lock.try, m, path); close(done) }()

	// Give the standby loop a moment to attempt (and lose) the lock at least
	// once before cancelling.
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done

	if m.calls != 0 {
		t.Errorf("standby must never mint a token; minter called %d times", m.calls)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("standby must never write the token file")
	}
	if attempts, _ := lock.snapshot(); attempts == 0 {
		t.Error("standby should have attempted to acquire the lock at least once")
	}
}

// TestRotatorLeader_AcquiresRunsThenReleasesOnce covers the acquired case: the
// winning replica must run the rotator loop (seeding the file, matching
// TestRotator_SeedsFileImmediately) and, once ctx is cancelled and the loop
// returns, release the lock exactly once.
func TestRotatorLeader_AcquiresRunsThenReleasesOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gt-token")
	m := &fakeGTMinter{token: "seed-token", expiry: time.Now().Add(time.Hour)}
	lock := &fakeTryLock{}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { runGroundtruthTokenRotatorLeader(ctx, lock.try, m, path); close(done) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(b)) == "seed-token" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	if m.calls == 0 {
		t.Error("leader must mint at least once")
	}
	if _, releases := lock.snapshot(); releases != 1 {
		t.Errorf("release() called %d times, want exactly 1", releases)
	}
}
