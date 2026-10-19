// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestPG_MintSSHKey_AutoMintOncePerRun: on real Postgres an auto-mint ssh_key
// is issued once per run, a failed first preparation does not count, and two
// concurrent asks yield exactly one value.
func TestPG_MintSSHKey_AutoMintOncePerRun(t *testing.T) {
	pool := pgPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	const key = "-----BEGIN OPENSSH PRIVATE KEY-----\nonce-per-run-key\n-----END OPENSSH PRIVATE KEY-----\n"
	secrets := newMemSecrets()
	b := New(NewPgxStore(pool), secrets, poolAudit{pool}, nil, &FakeGitHubMinter{})

	runID := uuid.New()
	seedRun(ctx, t, pool, runID)
	grantID := pgSeedGrant(ctx, t, pool, runID, sshKeySpec("git.example.test", "once-key", "", ""))
	if _, err := b.MintForGrant(ctx, callerFor(runID), grantID); err == nil {
		t.Fatal("mint with no stored key: want an error")
	}
	secrets.m["once-key"] = []byte(key)
	if got, err := b.MintForGrant(ctx, callerFor(runID), grantID); err != nil || got.Token != key {
		t.Fatalf("first mint after a failed preparation = (%q, %v)", got.Token, err)
	}
	if got, err := b.MintForGrant(ctx, callerFor(runID), grantID); !errors.Is(err, ErrAlreadyMinted) || got.Token != "" {
		t.Fatalf("second mint = (%q, %v), want ErrAlreadyMinted", got.Token, err)
	}
	if s, d := countMintAudits(ctx, t, pool, runID, "success"), countMintAudits(ctx, t, pool, runID, "denied"); s != 1 || d != 1 {
		t.Fatalf("success=%d denied=%d, want 1 and 1", s, d)
	}

	race := uuid.New()
	seedRun(ctx, t, pool, race)
	raceGrant := pgSeedGrant(ctx, t, pool, race, sshKeySpec("git.example.test", "once-key", "", ""))
	const n = 6
	var wg sync.WaitGroup
	var mu sync.Mutex
	values := 0
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := b.MintForGrant(ctx, callerFor(race), raceGrant)
			switch {
			case err == nil && got.Token == key:
				mu.Lock()
				values++
				mu.Unlock()
			case !errors.Is(err, ErrAlreadyMinted):
				t.Errorf("concurrent mint: %v", err)
			}
		}()
	}
	wg.Wait()
	if values != 1 || countMintAudits(ctx, t, pool, race, "success") != 1 {
		t.Fatalf("concurrent asks handed out %d values, want exactly 1", values)
	}
}
