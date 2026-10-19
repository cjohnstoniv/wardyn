// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// TestPG_RefusedMarkIsSharedBetweenReplicas is #2090's proof: a refusal one replica recorded grades
// the session on a second Server over the same pool (its own map empty, as on another replica or
// after a restart), and a renewal the second one makes clears it for the first. The mark is the
// store's alone, so the first cannot keep a stale copy.
func TestPG_RefusedMarkIsSharedBetweenReplicas(t *testing.T) {
	pool := throwawayPGPool(t)
	newSrv := func() *Server {
		return &Server{cfg: Config{
			Store:        store.NewPG(pool),
			Secrets:      &memSecrets{m: map[string][]byte{}},
			MaskRegistry: secretmask.NewRegistry(),
			Now:          func() time.Time { return awsSSOTestFixedNow },
		}}
	}
	ctx := context.Background()
	a, b := newSrv(), newSrv()
	blob := putAWSSSOBlob(t, a, awsSSOTestFixedNow.Add(-time.Minute))
	fingerprint := awsSSOTokenFingerprint(blob.RefreshToken)

	a.markAWSSSORefused(ctx, fingerprint, awsSSOTestOwner, "access_denied: nope")
	if a.ssoRefreshRefused != nil || b.ssoRefreshRefused != nil {
		t.Fatalf("the in-memory fallback was used beside a store that shares the mark: %v / %v", a.ssoRefreshRefused, b.ssoRefreshRefused)
	}
	if detail, refused := b.awsSSORefusedFor(blob); !refused || detail != "access_denied: nope" {
		t.Fatalf("the second replica read (%q, %v), want the first's refusal", detail, refused)
	}

	b.clearAWSSSORefused(ctx, fingerprint)
	if _, refused := a.awsSSORefusedFor(blob); refused {
		t.Fatal("the first replica still reads the refusal the second cleared with a renewal")
	}
}
