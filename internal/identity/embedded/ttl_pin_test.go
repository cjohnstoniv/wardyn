// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package embedded

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/identity/identitytest"
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
	p, err := New(nil, "wardyn.local", identitytest.NewMemRevocationStore(), &recordingRecorder{})
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

// TestB5_ExpiredVerifyCarriesTheRunID is the control-plane half of B5's quieting
// rule: the sidecar backs off and stops hammering a refused renew, so the case
// underneath — a HEALTHY run whose renews were refused through a control-plane
// outage and which now holds a dead identity for the rest of its life — must be
// visible somewhere, keyed to the run. internal/api's run.identity.expired row is
// where, and it can only exist because Verify reports EXPIRY as a typed error
// carrying the run id.
//
// The two negative halves matter as much as the positive one:
//   - a token that fails for any OTHER reason (forged signature, wrong audience)
//     names no run of ours, so it must NOT come back as an expiry — that would
//     let a prober write rows against a run id it chose;
//   - the error is still an error. Verify returns nil claims with it, and every
//     caller keeps failing closed.
func TestB5_ExpiredVerifyCarriesTheRunID(t *testing.T) {
	ctx := context.Background()
	base := time.Now()
	p, err := New(nil, "wardyn.local", identitytest.NewMemRevocationStore(), &recordingRecorder{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p.now = func() time.Time { return base }

	runID := uuid.New()
	id, err := p.MintRunIdentity(ctx, runID, "alice@example.com", "", "wardyn-internal")
	if err != nil {
		t.Fatalf("MintRunIdentity: %v", err)
	}

	p.now = func() time.Time { return base.Add(tokenTTL + time.Minute + time.Second) }
	claims, verr := p.Verify(ctx, id.Token, "wardyn-internal")
	if verr == nil {
		t.Fatal("expired token was ACCEPTED — expiry must fail closed")
	}
	if claims != nil {
		t.Errorf("Verify returned claims alongside the expiry error (%+v); the typed error says WHICH RUN to "+
			"tell about the refusal, never that the refusal was soft", claims)
	}
	var exp *identity.ExpiredTokenError
	if !errors.As(verr, &exp) {
		t.Fatalf("expired Verify returned %T (%v); internal/api cannot key run.identity.expired to a run "+
			"without the typed error", verr, verr)
	}
	if exp.RunID != runID {
		t.Errorf("ExpiredTokenError.RunID = %s, want %s", exp.RunID, runID)
	}
	if !errors.Is(verr, jwt.ErrExpired) {
		t.Errorf("the typed error must still unwrap to jwt.ErrExpired; got %v", verr)
	}

	// A WRONG-AUDIENCE token is not an expiry, even when it is also expired-ish:
	// the row this type feeds is about a run of ours whose renews stopped landing,
	// not about anything a caller managed to present.
	p.now = func() time.Time { return base }
	fresh, err := p.MintRunIdentity(ctx, uuid.New(), "alice@example.com", "", "wardyn-internal")
	if err != nil {
		t.Fatalf("MintRunIdentity: %v", err)
	}
	_, verr = p.Verify(ctx, fresh.Token, "wardyn-groundtruth")
	if verr == nil {
		t.Fatal("a token for another audience was ACCEPTED")
	}
	if errors.As(verr, &exp) {
		t.Errorf("a wrong-audience refusal came back as an expiry (run %s) — only expiry names a run", exp.RunID)
	}
}
