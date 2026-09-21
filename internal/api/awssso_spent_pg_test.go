// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_SpentMarkSurvivesRestart is #149's acceptance proof: a refresh
// token's spent mark — written by markAWSSSOTokenSpent, persisted to Postgres
// via store.AWSSSOSpentTokenStore (internal/store/pagination.go) — is graded
// correctly by a SECOND Server instance over the SAME pool, whose
// ssoRefreshSpent map starts nil exactly as a freshly restarted daemon's
// does. Before #149 the mark lived only in that in-memory map, so a restart
// forgot it and a refresh token already known dead graded "renewable" again.
//
// fakeOIDC here exists only to FAIL the test if anything reaches the OIDC
// endpoint: setupHarnessCreds is a read-only probe and must answer from the
// persisted row alone, with no CreateToken round trip.
func TestPG_SpentMarkSurvivesRestart(t *testing.T) {
	pool := throwawayPGPool(t)

	fakeOIDC(t, func(http.ResponseWriter, map[string]string, int) {
		t.Fatal("CreateToken was called; grading a spent token must never redeem it — it only reads the persisted mark")
	})

	newSrv := func() *Server {
		return &Server{cfg: Config{
			Store:        store.NewPG(pool),
			Secrets:      &memSecrets{m: map[string][]byte{}},
			MaskRegistry: secretmask.NewRegistry(),
			Now:          func() time.Time { return awsSSOTestFixedNow },
		}}
	}

	// Process 1: captures a session, then learns its refresh token is spent
	// (a redeem AWS itself retired, or a redeem whose persist back to the
	// secret store failed — refreshAWSSSOBlob's two mark sites) and marks it.
	s1 := newSrv()
	blob := putAWSSSOBlob(t, s1, awsSSOTestFixedNow.Add(-time.Minute))
	fingerprint := awsSSOTokenFingerprint(blob.RefreshToken)
	s1.markAWSSSOTokenSpent(context.Background(), fingerprint, "")

	// Process 2 simulates the restart: a brand-new Server, a brand-new
	// (nil) ssoRefreshSpent map, the SAME Postgres pool, and the SAME
	// fingerprint — putAWSSSOBlob's fixture refresh token is fixed.
	s2 := newSrv()
	if s2.ssoRefreshSpent != nil {
		t.Fatalf("a fresh Server must start with a nil spent map, got %v", s2.ssoRefreshSpent)
	}
	putAWSSSOBlob(t, s2, awsSSOTestFixedNow.Add(-time.Minute))

	rows, _, _ := s2.setupHarnessCreds(context.Background(), types.SiteConfig{}, awsSSOScope{})
	var row SetupHarness
	for _, r := range rows {
		if r.Provider == awsSSOProvider {
			row = r
		}
	}
	if !row.Captured || !row.Expired {
		t.Fatalf("row = %+v; want a captured, expired AWS SSO row", row)
	}
	if row.Renewable {
		t.Fatal("a second Server over the same pool graded Renewable=true for a fingerprint already marked spent and persisted before it started — the restart lost the mark")
	}
	if !s2.awsSSOTokenSpentFor(blob) {
		t.Fatal("awsSSOTokenSpentFor must read the persisted row on a cache miss")
	}
	// Read-once memoize: the answer is now cached in-process, off the map
	// alone — no second store read needed.
	if !s2.ssoRefreshSpent[fingerprint] {
		t.Fatal("the first read must memoize the persisted answer into the in-memory map")
	}
}
