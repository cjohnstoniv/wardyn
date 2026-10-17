// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestPG_LeaverPurgeClearsTheRunnerAttributionOfTheLeaver: the purge's runners step clears the leaver as
// the revoker of a runner that is someone else's and as the minter of a registration token made for
// someone else, and says how many rows it cleared; a token the leaver minted for themselves is theirs and
// is revoked with the rest, not cleared.
func TestPG_LeaverPurgeClearsTheRunnerAttributionOfTheLeaver(t *testing.T) {
	f := newAdmissionFixture(t)
	leaver := "leaver-" + uuid.NewString()
	now := time.Now().UTC()
	if _, err := f.st.RevokeRunner(f.ctx, f.rn.ID, now, leaver, ""); err != nil {
		t.Fatal(err)
	}
	forSomeone := uuid.New()
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO runner_registration_tokens (id, owner, minted_by, token_sha256, org_url_sha256, created_at, expires_at)
		VALUES ($1, 'carol@example.com', $2, $3, 'org', $4::timestamptz, $4::timestamptz + interval '1 hour')`,
		forSomeone, leaver, forSomeone.String(), now); err != nil {
		t.Fatal(err)
	}

	detail, err := f.srv.revokeLeaverRunners(f.ctx, []string{leaver})
	if err != nil {
		t.Fatal(err)
	}
	if detail["runner_attribution_cleared"] != 2 {
		t.Errorf("detail = %v, want the revoked runner and the token made for someone else cleared", detail)
	}
	var revokedBy, mintedBy string
	if err := f.pool.QueryRow(f.ctx, `SELECT revoked_by FROM runners WHERE id = $1`, f.rn.ID).Scan(&revokedBy); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(f.ctx, `SELECT minted_by FROM runner_registration_tokens WHERE id = $1`, forSomeone).Scan(&mintedBy); err != nil {
		t.Fatal(err)
	}
	if revokedBy != "" || mintedBy != "" {
		t.Errorf("the purged person is still named: revoked_by %q, minted_by %q", revokedBy, mintedBy)
	}
}
