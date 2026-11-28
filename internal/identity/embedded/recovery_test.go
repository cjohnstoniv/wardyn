// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package embedded

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/identity/identitytest"
)

// countingStore counts revocation reads and fails each one, so a test sees whether VerifyExpired reached the store.
type countingStore struct{ reads int }

func (c *countingStore) IsRevoked(context.Context, string, uuid.UUID) (bool, error) {
	c.reads++
	return false, errors.New("revocation store read")
}
func (c *countingStore) RevokeRun(context.Context, uuid.UUID) error         { return nil }
func (c *countingStore) RevokeJTI(context.Context, string, uuid.UUID) error { return nil }

// mintAgo mints a run token as if it were ago in the past, so it expires ago minus the token's lifetime from now.
func mintAgo(t *testing.T, p *Provider, runID uuid.UUID, ago time.Duration) identity.RunIdentity {
	t.Helper()
	p.now = func() time.Time { return time.Now().Add(-ago) }
	id, err := p.MintRunIdentity(context.Background(), runID, "alice", "bob", testAudience, false)
	p.now = time.Now
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return id
}

func TestVerifyExpired_UnexpiredIsErrNotExpired(t *testing.T) {
	p := newProvider(t, identitytest.NewMemRevocationStore(), &recordingRecorder{})
	id := mintAgo(t, p, uuid.New(), 0)
	if _, err := p.VerifyExpired(context.Background(), id.Token, testAudience, 24*time.Hour); !errors.Is(err, identity.ErrNotExpired) {
		t.Fatalf("VerifyExpired(live token) err = %v, want ErrNotExpired", err)
	}
	// An expired one is accepted, with the claims Verify would have built.
	runID := uuid.New()
	old := mintAgo(t, p, runID, 2*time.Hour)
	claims, err := p.VerifyExpired(context.Background(), old.Token, testAudience, 24*time.Hour)
	if err != nil {
		t.Fatalf("VerifyExpired(expired token): %v", err)
	}
	if claims.RunID != runID || claims.JTI != old.JTI || claims.Sub != "alice" || claims.Sponsor != "bob" || !claims.Expiry.Equal(old.Expiry.Truncate(time.Second)) {
		t.Fatalf("claims = %+v, want run %s jti %s expiry %s", claims, runID, old.JTI, old.Expiry)
	}
}

func TestVerifyExpired_AudienceCheckedAtValidTime(t *testing.T) {
	p := newProvider(t, identitytest.NewMemRevocationStore(), &recordingRecorder{})
	id := mintAgo(t, p, uuid.New(), 2*time.Hour)
	// Verify refuses it as expired; VerifyExpired judges audience and nbf just before the expiry and accepts it.
	if _, err := p.Verify(context.Background(), id.Token, testAudience); err == nil {
		t.Fatal("Verify accepted an expired token")
	}
	if _, err := p.VerifyExpired(context.Background(), id.Token, testAudience, 24*time.Hour); err != nil {
		t.Fatalf("VerifyExpired(right audience): %v", err)
	}
	_, err := p.VerifyExpired(context.Background(), id.Token, "some-other-audience", 24*time.Hour)
	if err == nil || errors.Is(err, identity.ErrNotExpired) || errors.Is(err, identity.ErrTokenTooOld) {
		t.Fatalf("VerifyExpired(wrong audience) err = %v, want an audience refusal", err)
	}
	if _, err := p.VerifyExpired(context.Background(), id.Token, "", 24*time.Hour); err == nil {
		t.Fatal("VerifyExpired accepted an empty expected audience")
	}
	// A token signed by another key is refused at the signature.
	other := newProvider(t, identitytest.NewMemRevocationStore(), &recordingRecorder{})
	forged := mintAgo(t, other, uuid.New(), 2*time.Hour)
	if _, err := p.VerifyExpired(context.Background(), forged.Token, testAudience, 24*time.Hour); err == nil {
		t.Fatal("VerifyExpired accepted a token signed by another key")
	}
}

func TestVerifyExpired_TooOldBeforeRevocationRead(t *testing.T) {
	store := &countingStore{}
	p := newProvider(t, store, &recordingRecorder{})
	id := mintAgo(t, p, uuid.New(), 26*time.Hour) // expired 25 h ago
	if _, err := p.VerifyExpired(context.Background(), id.Token, testAudience, 24*time.Hour); !errors.Is(err, identity.ErrTokenTooOld) {
		t.Fatalf("VerifyExpired(25 h expired) err = %v, want ErrTokenTooOld", err)
	}
	if store.reads != 0 {
		t.Fatalf("revocation store read %d times for a too-old token, want 0", store.reads)
	}
	// Inside the limit the store is read, and its failure is an error, never a revocation and never a pass.
	young := mintAgo(t, p, uuid.New(), 2*time.Hour)
	_, err := p.VerifyExpired(context.Background(), young.Token, testAudience, 24*time.Hour)
	var revoked *identity.RevokedTokenError
	if err == nil || errors.As(err, &revoked) {
		t.Fatalf("VerifyExpired with a failing revocation store err = %v, want a plain error", err)
	}
	if store.reads != 1 {
		t.Fatalf("revocation store read %d times, want 1", store.reads)
	}
}

func TestVerifyExpired_RevokedIsRevokedTokenError(t *testing.T) {
	mem := identitytest.NewMemRevocationStore()
	p := newProvider(t, mem, &recordingRecorder{})
	runID := uuid.New()
	id := mintAgo(t, p, runID, 2*time.Hour)
	if err := mem.RevokeRun(context.Background(), runID); err != nil {
		t.Fatalf("RevokeRun: %v", err)
	}
	_, err := p.VerifyExpired(context.Background(), id.Token, testAudience, 24*time.Hour)
	var revoked *identity.RevokedTokenError
	if !errors.As(err, &revoked) || revoked.Claims == nil || revoked.Claims.RunID != runID {
		t.Fatalf("VerifyExpired(revoked) err = %v, want *RevokedTokenError for run %s", err, runID)
	}
}
