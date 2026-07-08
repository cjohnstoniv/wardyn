// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// identity declares the per-run workload-identity contract (Claims, RunIdentity,
// Provider). There is no concrete logic in this package; the real providers live
// in internal/identity/embedded and (later) spire. These tests therefore assert
// the documented Provider CONTRACT and INVARIANTS using a hand-rolled fake
// provider (no real crypto / SPIRE):
//
//   - Name() surfaces the trust boundary ("embedded" vs "spire");
//   - Verify of a revoked or expired token FAILS CLOSED (security invariant);
//   - Confinement gating: an embedded-class provider must REFUSE to mint an
//     identity whose grants include types.GrantCloudSTS (cloud STS HARD-REQUIRES
//     spire), per the package-level INVARIANT comment.

// ---- fake provider (no real SPIRE / crypto) ------------------------------

var (
	errRevoked      = errors.New("token revoked")
	errExpired      = errors.New("token expired")
	errBadAudience  = errors.New("audience mismatch")
	errCloudSTSDeny = errors.New("embedded provider must not mint cloud_sts identities; requires spire")
)

// fakeProvider models an embedded-class provider just enough to exercise the
// contract: it mints opaque tokens, tracks revocation per run, honors a TTL, and
// enforces confinement gating when grants are supplied.
type fakeProvider struct {
	name    string
	now     time.Time
	ttl     time.Duration
	grants  []types.GrantKind // grants the run is eligible for (drives gating)
	minted  map[string]*Claims
	revoked map[uuid.UUID]bool
	seq     int
}

func newFakeProvider() *fakeProvider {
	return &fakeProvider{
		name:    "embedded",
		now:     time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC),
		ttl:     time.Hour,
		minted:  map[string]*Claims{},
		revoked: map[uuid.UUID]bool{},
	}
}

func (p *fakeProvider) Name() string { return p.name }

func (p *fakeProvider) MintRunIdentity(_ context.Context, runID uuid.UUID, humanSub, sponsor, audience string) (RunIdentity, error) {
	// Confinement gating: an embedded-class provider must refuse cloud_sts.
	if p.name == "embedded" {
		for _, g := range p.grants {
			if g == types.GrantCloudSTS {
				return RunIdentity{}, errCloudSTSDeny
			}
		}
	}
	if sponsor == "" {
		sponsor = humanSub // Sponsor defaults to Sub.
	}
	p.seq++
	tok := fmt.Sprintf("tok-%d", p.seq)
	jti := fmt.Sprintf("jti-%d", p.seq)
	spiffe := "spiffe://wardyn.test/agent-run/" + runID.String()
	exp := p.now.Add(p.ttl)
	p.minted[tok] = &Claims{
		SPIFFEID: spiffe,
		RunID:    runID,
		Sub:      humanSub,
		Sponsor:  sponsor,
		JTI:      jti,
		Audience: audience,
		IssuedAt: p.now,
		Expiry:   exp,
	}
	return RunIdentity{SPIFFEID: spiffe, Token: tok, JTI: jti, Expiry: exp}, nil
}

func (p *fakeProvider) Verify(_ context.Context, token, expectedAudience string) (*Claims, error) {
	c, ok := p.minted[token]
	if !ok {
		return nil, errors.New("unknown token")
	}
	if p.revoked[c.RunID] {
		return nil, errRevoked // fail closed
	}
	if !p.now.Before(c.Expiry) {
		return nil, errExpired // fail closed
	}
	if c.Audience != expectedAudience {
		return nil, errBadAudience
	}
	return c, nil
}

func (p *fakeProvider) RevokeRun(_ context.Context, runID uuid.UUID) error {
	p.revoked[runID] = true
	return nil
}

// Compile-time assertion that the fake satisfies the contract under test.
var _ Provider = (*fakeProvider)(nil)

// TestProviderNameSurfacesTrustBoundary checks Name() reports the provider class
// so the UI/audit can show whether identities are embedded or spire-backed.
func TestProviderNameSurfacesTrustBoundary(t *testing.T) {
	emb := newFakeProvider()
	if got := emb.Name(); got != "embedded" {
		t.Errorf("Name() = %q, want %q", got, "embedded")
	}

	spire := newFakeProvider()
	spire.name = "spire"
	if got := spire.Name(); got != "spire" {
		t.Errorf("Name() = %q, want %q", got, "spire")
	}
}

// TestMintThenVerifyRoundTrip is the happy path: a freshly minted token verifies
// against its audience and returns claims that carry the delegation chain
// (Sub = human principal, RunID = the run).
func TestMintThenVerifyRoundTrip(t *testing.T) {
	p := newFakeProvider()
	ctx := context.Background()
	runID := uuid.New()

	id, err := p.MintRunIdentity(ctx, runID, "alice", "alice", "broker")
	if err != nil {
		t.Fatalf("MintRunIdentity: %v", err)
	}
	if id.Token == "" || id.JTI == "" {
		t.Fatalf("minted identity missing token/jti: %+v", id)
	}

	claims, err := p.Verify(ctx, id.Token, "broker")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.RunID != runID {
		t.Errorf("claims.RunID = %v, want %v", claims.RunID, runID)
	}
	if claims.Sub != "alice" {
		t.Errorf("claims.Sub = %q, want %q", claims.Sub, "alice")
	}
	if claims.JTI != id.JTI {
		t.Errorf("claims.JTI = %q, want %q (mint/verify JTI mismatch)", claims.JTI, id.JTI)
	}
}

// TestSponsorDefaultsToSub verifies the documented default: when no explicit
// sponsor is given, the accountable owner is the human principal (Sub).
func TestSponsorDefaultsToSub(t *testing.T) {
	p := newFakeProvider()
	ctx := context.Background()

	id, err := p.MintRunIdentity(ctx, uuid.New(), "alice", "" /* sponsor */, "broker")
	if err != nil {
		t.Fatalf("MintRunIdentity: %v", err)
	}
	claims, err := p.Verify(ctx, id.Token, "broker")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.Sponsor != "alice" {
		t.Errorf("Sponsor = %q, want %q (should default to Sub)", claims.Sponsor, "alice")
	}
}

// TestVerifyRevokedFailsClosed regresses the security invariant that a revoked
// run's token must fail verification (kill-switch cascade). A fail-open here
// would let a revoked agent keep calling the broker.
func TestVerifyRevokedFailsClosed(t *testing.T) {
	p := newFakeProvider()
	ctx := context.Background()
	runID := uuid.New()

	id, err := p.MintRunIdentity(ctx, runID, "alice", "alice", "broker")
	if err != nil {
		t.Fatalf("MintRunIdentity: %v", err)
	}
	// Sanity: verifies before revocation.
	if _, err := p.Verify(ctx, id.Token, "broker"); err != nil {
		t.Fatalf("pre-revoke Verify: %v", err)
	}

	if err := p.RevokeRun(ctx, runID); err != nil {
		t.Fatalf("RevokeRun: %v", err)
	}
	if _, err := p.Verify(ctx, id.Token, "broker"); !errors.Is(err, errRevoked) {
		t.Errorf("post-revoke Verify err = %v, want revoked (must fail closed)", err)
	}
}

// TestVerifyExpiredFailsClosed regresses the invariant that an expired token
// must fail verification rather than being accepted.
func TestVerifyExpiredFailsClosed(t *testing.T) {
	p := newFakeProvider()
	ctx := context.Background()

	id, err := p.MintRunIdentity(ctx, uuid.New(), "alice", "alice", "broker")
	if err != nil {
		t.Fatalf("MintRunIdentity: %v", err)
	}
	// Advance the clock past expiry.
	p.now = p.now.Add(2 * time.Hour)

	if _, err := p.Verify(ctx, id.Token, "broker"); !errors.Is(err, errExpired) {
		t.Errorf("expired Verify err = %v, want expired (must fail closed)", err)
	}
}

// TestVerifyAudienceMismatch checks RFC 8707 audience discipline: a token minted
// for one audience must not verify for another.
func TestVerifyAudienceMismatch(t *testing.T) {
	p := newFakeProvider()
	ctx := context.Background()

	id, err := p.MintRunIdentity(ctx, uuid.New(), "alice", "alice", "broker")
	if err != nil {
		t.Fatalf("MintRunIdentity: %v", err)
	}
	if _, err := p.Verify(ctx, id.Token, "some-other-aud"); !errors.Is(err, errBadAudience) {
		t.Errorf("audience mismatch Verify err = %v, want audience mismatch", err)
	}
}

// TestEmbeddedRefusesCloudSTS regresses the package INVARIANT (Confinement
// gating): the embedded provider MUST refuse to mint identities whose grants
// include types.GrantCloudSTS — cloud STS federation hard-requires spire. A
// spire-class provider has no such restriction.
func TestEmbeddedRefusesCloudSTS(t *testing.T) {
	ctx := context.Background()

	t.Run("embedded refuses cloud_sts", func(t *testing.T) {
		p := newFakeProvider() // name == "embedded"
		p.grants = []types.GrantKind{types.GrantCloudSTS}
		if _, err := p.MintRunIdentity(ctx, uuid.New(), "alice", "alice", "broker"); !errors.Is(err, errCloudSTSDeny) {
			t.Errorf("embedded mint with cloud_sts grant err = %v, want refusal", err)
		}
	})

	t.Run("embedded allows non-cloud_sts grants", func(t *testing.T) {
		p := newFakeProvider()
		p.grants = []types.GrantKind{types.GrantGitHubToken, types.GrantAPIKey}
		if _, err := p.MintRunIdentity(ctx, uuid.New(), "alice", "alice", "broker"); err != nil {
			t.Errorf("embedded mint with safe grants err = %v, want success", err)
		}
	})

	t.Run("spire allows cloud_sts", func(t *testing.T) {
		p := newFakeProvider()
		p.name = "spire"
		p.grants = []types.GrantKind{types.GrantCloudSTS}
		if _, err := p.MintRunIdentity(ctx, uuid.New(), "alice", "alice", "broker"); err != nil {
			t.Errorf("spire mint with cloud_sts grant err = %v, want success", err)
		}
	})
}
