// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The restore runbook's sequence step (docs/OPERATIONS.md, "Restore them", step 3b), run as written: one
// dump restored twice, a session signed in between the two restores, then a cut of that session's subject.
// The cut ends it.
func TestPG_RestoreRunbookKeepsACutAfterTwoRestoresOfOneDump(t *testing.T) {
	doc, err := os.ReadFile("../../docs/OPERATIONS.md")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`"(SELECT setval\('oidc_revocation_seq'[^"]*)"`).FindSubmatch(doc)
	if m == nil {
		t.Fatal("docs/OPERATIONS.md has no setval of oidc_revocation_seq")
	}
	step3b := string(m[1])

	pool := isolatedRevocationPool(t)
	ctx := context.Background()
	rev := &pgSessionRevocations{pool: pool}
	lastValue := func() int64 {
		var v int64
		if err := pool.QueryRow(ctx, `SELECT last_value FROM oidc_revocation_seq`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	cut := func(sub string) {
		if err := store.ExecRevocationCut(ctx, pool, `INSERT INTO oidc_session_revocations (sub, revoked_at) VALUES ($1, clock_timestamp())
			ON CONFLICT (sub) DO UPDATE SET revoked_at = EXCLUDED.revoked_at`, sub, types.CLISessionEndRevoked); err != nil {
			t.Fatal(err)
		}
	}
	dump := lastValue()
	restore := func() {
		// Step 0 reads the mark on the database being replaced; step 3 loads the dump, which rewinds the sequence.
		mark := lastValue()
		if _, err := pool.Exec(ctx, `SELECT setval('oidc_revocation_seq', $1)`, dump); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, strings.ReplaceAll(step3b, "$(cat revocation-hwm.txt)", strconv.FormatInt(mark, 10))); err != nil {
			t.Fatalf("step 3b %q: %v", step3b, err)
		}
	}

	restore()
	cut(seqSub(t, pool, "restore-other-"))
	cut(seqSub(t, pool, "restore-other-"))
	sub := seqSub(t, pool, "restore-")
	seq, err := rev.RevocationSeq(ctx)
	if err != nil {
		t.Fatal(err)
	}
	restore()
	cut(sub)
	if revoked, err := rev.IsSessionRevokedSeq(ctx, sub, "", seq); err != nil || !revoked {
		t.Errorf("a session issued at %d between two restores outlived a cut after the second (cut seq after %d): revoked=%v, %v",
			seq, lastValue(), revoked, err)
	}
}
