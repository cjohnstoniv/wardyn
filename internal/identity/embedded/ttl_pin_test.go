// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package embedded

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestRenewU070_TokenTTLStaysShortAndExpiredIsRefused pins the invariant the
// renew route exists to PRESERVE.
//
// The bug renew fixes — a run outliving its token loses every /internal/* call —
// has an obvious wrong fix: raise tokenTTL until runs stop noticing. That trades
// away the whole point of a short-lived JWT-SVID (a bounded window in which a
// stolen or post-revocation token is useful) for convenience. This test fails if
// anyone does that, so the cheap wrong fix cannot land quietly.
//
// It also asserts the other half: a token past its TTL really is refused. That is
// what makes renewal load-bearing rather than decorative — if expiry were not
// enforced, nothing would need renewing.
func TestRenewU070_TokenTTLStaysShortAndExpiredIsRefused(t *testing.T) {
	// The ceiling this design rests on. Renewal is the sanctioned way to outlive
	// it; raising it is not.
	const maxAcceptableTTL = time.Hour
	if tokenTTL > maxAcceptableTTL {
		t.Fatalf("tokenTTL = %s, want <= %s — renewal, not a longer TTL, is how a run outlives its token",
			tokenTTL, maxAcceptableTTL)
	}

	ctx := context.Background()
	base := time.Now()
	p, err := New(nil, "wardyn.local", NewMemRevocationStore(), &recordingRecorder{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p.now = func() time.Time { return base }

	runID := uuid.New()
	id, err := p.MintRunIdentity(ctx, runID, "alice@example.com", "", "wardyn-internal")
	if err != nil {
		t.Fatalf("MintRunIdentity: %v", err)
	}
	if got := id.Expiry.Sub(base); got != tokenTTL {
		t.Fatalf("minted TTL = %s, want tokenTTL (%s)", got, tokenTTL)
	}

	// Fresh: accepted.
	if _, err := p.Verify(ctx, id.Token, "wardyn-internal"); err != nil {
		t.Fatalf("fresh token rejected: %v", err)
	}

	// One second past expiry (plus jose's validation leeway): refused. This is the
	// 401 a long run hits with no renew producer.
	p.now = func() time.Time { return base.Add(tokenTTL + time.Minute + time.Second) }
	if _, err := p.Verify(ctx, id.Token, "wardyn-internal"); err == nil {
		t.Fatal("expired token was ACCEPTED — expiry must fail closed")
	}
}
