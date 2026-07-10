// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package embedded

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

const testAudience = "wardyn-internal"

// recordingRecorder captures audit events for assertions.
type recordingRecorder struct {
	mu     sync.Mutex
	events []types.AuditEvent
}

func (r *recordingRecorder) Record(_ context.Context, ev types.AuditEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	return nil
}

func (r *recordingRecorder) snapshot() []types.AuditEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]types.AuditEvent, len(r.events))
	copy(out, r.events)
	return out
}

// erroringRecorder always fails Record — used to prove the mint/revoke audit
// write is best-effort (log-loud, not fail-closed): the operation still succeeds.
type erroringRecorder struct{}

func (erroringRecorder) Record(context.Context, types.AuditEvent) error {
	return errors.New("audit sink down")
}

// erroringStore fails IsRevoked to exercise the fail-closed path.
type erroringStore struct{}

func (erroringStore) IsRevoked(context.Context, string, uuid.UUID) (bool, error) {
	return false, errors.New("boom")
}
func (erroringStore) RevokeRun(context.Context, uuid.UUID) error         { return nil }
func (erroringStore) RevokeJTI(context.Context, string, uuid.UUID) error { return nil }

func newProvider(t *testing.T, store RevocationStore, rec *recordingRecorder) *Provider {
	t.Helper()
	p, err := New(nil, "", store, rec)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func TestNew(t *testing.T) {
	rec := &recordingRecorder{}
	t.Run("nil store rejected", func(t *testing.T) {
		if _, err := New(nil, "", nil, rec); err == nil {
			t.Fatal("expected error for nil store")
		}
	})
	t.Run("nil recorder rejected", func(t *testing.T) {
		if _, err := New(nil, "", NewMemRevocationStore(), nil); err == nil {
			t.Fatal("expected error for nil recorder")
		}
	})
	t.Run("non-P256 key rejected", func(t *testing.T) {
		k, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
		if _, err := New(k, "", NewMemRevocationStore(), rec); err == nil {
			t.Fatal("expected error for non-P256 key")
		}
	})
	t.Run("defaults trust domain", func(t *testing.T) {
		p := newProvider(t, NewMemRevocationStore(), rec)
		if p.trustDomain.String() != DefaultTrustDomain {
			t.Fatalf("trust domain = %q, want %q", p.trustDomain.String(), DefaultTrustDomain)
		}
		if p.Name() != "embedded" {
			t.Fatalf("Name() = %q, want embedded", p.Name())
		}
	})
}

func TestMintVerifyRoundtrip(t *testing.T) {
	rec := &recordingRecorder{}
	store := NewMemRevocationStore()
	p := newProvider(t, store, rec)
	ctx := context.Background()

	runID := uuid.New()
	id, err := p.MintRunIdentity(ctx, runID, "alice@example.com", "bob@example.com", testAudience)
	if err != nil {
		t.Fatalf("MintRunIdentity: %v", err)
	}
	wantSPIFFE := "spiffe://" + DefaultTrustDomain + "/agent-run/" + runID.String()
	if id.SPIFFEID != wantSPIFFE {
		t.Fatalf("SPIFFEID = %q, want %q", id.SPIFFEID, wantSPIFFE)
	}
	if id.JTI == "" {
		t.Fatal("JTI empty")
	}
	if !id.Expiry.After(time.Now().Add(50 * time.Minute)) {
		t.Fatalf("expiry %v not ~1h out", id.Expiry)
	}

	claims, err := p.Verify(ctx, id.Token, testAudience)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.Sub != "alice@example.com" {
		t.Fatalf("Sub = %q", claims.Sub)
	}
	if claims.Sponsor != "bob@example.com" {
		t.Fatalf("Sponsor = %q", claims.Sponsor)
	}
	if claims.SPIFFEID != wantSPIFFE {
		t.Fatalf("claims SPIFFEID = %q, want %q", claims.SPIFFEID, wantSPIFFE)
	}
	if claims.RunID != runID {
		t.Fatalf("RunID = %v, want %v", claims.RunID, runID)
	}
	if claims.JTI != id.JTI {
		t.Fatalf("JTI = %q, want %q", claims.JTI, id.JTI)
	}
	if claims.Audience != testAudience {
		t.Fatalf("Audience = %q", claims.Audience)
	}

	// One identity.mint success event with system actor type.
	evs := rec.snapshot()
	if len(evs) != 1 {
		t.Fatalf("got %d audit events, want 1", len(evs))
	}
	if evs[0].Action != "identity.mint" || evs[0].Outcome != "success" || evs[0].ActorType != types.ActorSystem {
		t.Fatalf("unexpected audit event: %+v", evs[0])
	}
	if evs[0].Actor != wantSPIFFE {
		t.Fatalf("audit actor = %q, want %q", evs[0].Actor, wantSPIFFE)
	}
}

func TestSponsorDefaultsToSub(t *testing.T) {
	p := newProvider(t, NewMemRevocationStore(), &recordingRecorder{})
	ctx := context.Background()
	id, err := p.MintRunIdentity(ctx, uuid.New(), "alice@example.com", "", testAudience)
	if err != nil {
		t.Fatalf("MintRunIdentity: %v", err)
	}
	claims, err := p.Verify(ctx, id.Token, testAudience)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.Sponsor != "alice@example.com" {
		t.Fatalf("Sponsor = %q, want alice@example.com", claims.Sponsor)
	}
}

func TestMintInputValidation(t *testing.T) {
	p := newProvider(t, NewMemRevocationStore(), &recordingRecorder{})
	ctx := context.Background()
	if _, err := p.MintRunIdentity(ctx, uuid.New(), "", "", testAudience); err == nil {
		t.Fatal("expected error for empty humanSub")
	}
	if _, err := p.MintRunIdentity(ctx, uuid.New(), "alice", "", ""); err == nil {
		t.Fatal("expected error for empty audience")
	}
}

func TestVerifyFailures(t *testing.T) {
	ctx := context.Background()

	t.Run("expired token", func(t *testing.T) {
		p := newProvider(t, NewMemRevocationStore(), &recordingRecorder{})
		// Mint as if it were 2h ago so exp is in the past (beyond leeway).
		p.now = func() time.Time { return time.Now().Add(-2 * time.Hour) }
		id, err := p.MintRunIdentity(ctx, uuid.New(), "alice", "", testAudience)
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		p.now = time.Now
		if _, err := p.Verify(ctx, id.Token, testAudience); !errors.Is(err, jwt.ErrExpired) {
			t.Fatalf("Verify err = %v, want ErrExpired", err)
		}
	})

	t.Run("wrong audience", func(t *testing.T) {
		p := newProvider(t, NewMemRevocationStore(), &recordingRecorder{})
		id, err := p.MintRunIdentity(ctx, uuid.New(), "alice", "", testAudience)
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		if _, err := p.Verify(ctx, id.Token, "some-other-aud"); !errors.Is(err, jwt.ErrInvalidAudience) {
			t.Fatalf("Verify err = %v, want ErrInvalidAudience", err)
		}
	})

	t.Run("empty expected audience rejected", func(t *testing.T) {
		p := newProvider(t, NewMemRevocationStore(), &recordingRecorder{})
		id, _ := p.MintRunIdentity(ctx, uuid.New(), "alice", "", testAudience)
		if _, err := p.Verify(ctx, id.Token, ""); err == nil {
			t.Fatal("expected error for empty expected audience")
		}
	})

	t.Run("wrong signing key", func(t *testing.T) {
		store := NewMemRevocationStore()
		p1 := newProvider(t, store, &recordingRecorder{})
		p2 := newProvider(t, store, &recordingRecorder{}) // different key
		id, err := p1.MintRunIdentity(ctx, uuid.New(), "alice", "", testAudience)
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		if _, err := p2.Verify(ctx, id.Token, testAudience); err == nil {
			t.Fatal("expected signature verification failure with foreign key")
		}
	})

	t.Run("garbage token", func(t *testing.T) {
		p := newProvider(t, NewMemRevocationStore(), &recordingRecorder{})
		if _, err := p.Verify(ctx, "not-a-jwt", testAudience); err == nil {
			t.Fatal("expected parse failure")
		}
	})
}

func TestVerifyRevokedJTI(t *testing.T) {
	ctx := context.Background()
	store := NewMemRevocationStore()
	p := newProvider(t, store, &recordingRecorder{})

	runID := uuid.New()
	id, err := p.MintRunIdentity(ctx, runID, "alice", "", testAudience)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// Valid before revocation.
	if _, err := p.Verify(ctx, id.Token, testAudience); err != nil {
		t.Fatalf("pre-revoke Verify: %v", err)
	}
	if err := store.RevokeJTI(ctx, id.JTI, runID); err != nil {
		t.Fatalf("RevokeJTI: %v", err)
	}
	if _, err := p.Verify(ctx, id.Token, testAudience); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("post-revoke Verify err = %v, want revoked", err)
	}
}

func TestRevokeRunCascade(t *testing.T) {
	ctx := context.Background()
	store := NewMemRevocationStore()
	rec := &recordingRecorder{}
	p := newProvider(t, store, rec)

	runID := uuid.New()
	id1, _ := p.MintRunIdentity(ctx, runID, "alice", "", testAudience)
	id2, _ := p.MintRunIdentity(ctx, runID, "alice", "", testAudience)

	if err := p.RevokeRun(ctx, runID); err != nil {
		t.Fatalf("RevokeRun: %v", err)
	}
	// Both tokens for the run must now fail closed.
	for i, tok := range []string{id1.Token, id2.Token} {
		if _, err := p.Verify(ctx, tok, testAudience); err == nil {
			t.Fatalf("token %d still valid after RevokeRun", i)
		}
	}
	// identity.revoke event emitted with system actor.
	var found bool
	for _, ev := range rec.snapshot() {
		if ev.Action == "identity.revoke" && ev.Outcome == "success" && ev.ActorType == types.ActorSystem {
			found = true
		}
	}
	if !found {
		t.Fatal("missing identity.revoke success audit event")
	}
}

func TestVerifyFailsClosedOnStoreError(t *testing.T) {
	ctx := context.Background()
	// Mint with a good store, verify with an erroring store: must fail closed.
	good := NewMemRevocationStore()
	p := newProvider(t, good, &recordingRecorder{})
	id, err := p.MintRunIdentity(ctx, uuid.New(), "alice", "", testAudience)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	p.revocations = erroringStore{}
	if _, err := p.Verify(ctx, id.Token, testAudience); err == nil {
		t.Fatal("expected fail-closed error when revocation store errors")
	} else if !strings.Contains(err.Error(), "failing closed") {
		t.Fatalf("err = %v, want failing-closed message", err)
	}
}

func TestCheckGrantsCloudSTSRefusal(t *testing.T) {
	p := newProvider(t, NewMemRevocationStore(), &recordingRecorder{})

	cases := []struct {
		name    string
		grants  []types.GrantSpec
		wantErr bool
	}{
		{"empty", nil, false},
		{"github only", []types.GrantSpec{{Kind: types.GrantGitHubToken}}, false},
		{"api key only", []types.GrantSpec{{Kind: types.GrantAPIKey}}, false},
		{"cloud_sts alone", []types.GrantSpec{{Kind: types.GrantCloudSTS}}, true},
		{"cloud_sts mixed", []types.GrantSpec{{Kind: types.GrantGitHubToken}, {Kind: types.GrantCloudSTS}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := p.CheckGrants(tc.grants)
			if tc.wantErr {
				if !errors.Is(err, ErrRequiresSPIRE) {
					t.Fatalf("err = %v, want ErrRequiresSPIRE", err)
				}
			} else if err != nil {
				t.Fatalf("unexpected err = %v", err)
			}
		})
	}
}

func TestVerifyRejectsForeignTrustDomain(t *testing.T) {
	ctx := context.Background()
	store := NewMemRevocationStore()
	rec := &recordingRecorder{}
	// Provider A (wardyn.local) mints; provider B (other.example) shares the
	// signing key but a different trust domain — the act SPIFFE ID must be
	// rejected as a foreign trust domain.
	pA := newProvider(t, store, rec)
	pB, err := New(pA.signKey, "other.example", store, rec)
	if err != nil {
		t.Fatalf("New pB: %v", err)
	}
	id, err := pA.MintRunIdentity(ctx, uuid.New(), "alice", "", testAudience)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := pB.Verify(ctx, id.Token, testAudience); err == nil {
		t.Fatal("expected foreign trust domain rejection")
	}
}

// TestAuditWriteFailureIsBestEffort proves the identity mint/revoke audit write
// is observability-only: a failing recorder logs loudly but must NOT fail the op.
func TestAuditWriteFailureIsBestEffort(t *testing.T) {
	ctx := context.Background()
	p, err := New(nil, "", NewMemRevocationStore(), erroringRecorder{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := p.MintRunIdentity(ctx, uuid.New(), "alice@example.com", "", testAudience); err != nil {
		t.Fatalf("mint must succeed despite audit write failure: %v", err)
	}
	if err := p.RevokeRun(ctx, uuid.New()); err != nil {
		t.Fatalf("revoke must succeed despite audit write failure: %v", err)
	}
}
