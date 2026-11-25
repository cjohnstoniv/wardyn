// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package identitytest provides a reusable conformance suite for any
// identity.Provider implementation, so the blessed default (embedded) and the
// alternate (spire) are held to the identical security contract.
package identitytest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// NewProviderFunc returns a FRESH provider on each call, backed by rev as its kill-switch denylist.
type NewProviderFunc func(t *testing.T, rev identity.RevocationStore) identity.Provider

const conformanceAud = "wardyn-internal"

// jtiRevoker is the single-token revoke a revive retires a run's old token with (internal/api's
// jtiRevoker); RunConformance requires it of every provider.
type jtiRevoker interface {
	RevokeJTI(ctx context.Context, jti string, runID uuid.UUID) error
}

// grantChecker is the optional create-time grant check (internal/api's runs_create_confinement).
type grantChecker interface {
	CheckGrants([]types.GrantSpec) error
}

// RunConformance exercises the identity.Provider contract.
func RunConformance(t *testing.T, newProvider NewProviderFunc) {
	fresh := func(t *testing.T) identity.Provider { return newProvider(t, NewMemRevocationStore()) }

	t.Run("mint_verify_roundtrip_and_spiffe_shape", func(t *testing.T) { roundtripConformance(t, fresh(t)) })
	t.Run("operator_owned_is_carried", func(t *testing.T) { operatorOwnedConformance(t, fresh(t)) })
	t.Run("sponsor_defaults_to_sub", func(t *testing.T) { sponsorDefaultConformance(t, fresh(t)) })
	t.Run("mint_and_verify_refuse_empty_inputs", func(t *testing.T) { emptyInputsConformance(t, fresh(t)) })
	t.Run("wrong_audience_fails_closed", func(t *testing.T) { wrongAudienceConformance(t, fresh(t)) })
	t.Run("tampered_token_fails_closed", func(t *testing.T) { tamperedConformance(t, fresh(t)) })
	t.Run("spliced_token_fails_closed", func(t *testing.T) { splicedConformance(t, fresh(t)) })
	t.Run("revoke_run_is_kill_switch", func(t *testing.T) { revokeRunConformance(t, fresh(t)) })
	t.Run("revoke_run_spares_other_runs", func(t *testing.T) { revokeSparesConformance(t, fresh(t)) })
	t.Run("revocation_store_error_fails_closed", func(t *testing.T) {
		revocationErrorConformance(t, newProvider(t, failingRevocations{}))
	})
	t.Run("revoke_jti_retires_one_token", func(t *testing.T) { revokeJTIConformance(t, fresh(t)) })
	t.Run("jti_unique_per_mint", func(t *testing.T) { jtiUniqueConformance(t, fresh(t)) })
	// A run a workload launched is named by the workload, then the run, and stays a run of its own: its
	// token verifies to its run, and revoking it leaves another run of the same workload working.
	t.Run("workload_run_spiffe_shape_and_per_run_revocation", func(t *testing.T) {
		workloadRunConformance(t, fresh(t), conformanceAud)
	})
	t.Run("cloud_sts_refused", func(t *testing.T) { cloudSTSConformance(t, fresh(t)) })
	t.Run("workload_claim_is_the_minted_workload", func(t *testing.T) { workloadClaimConformance(t, fresh(t)) })
}

func mint(t *testing.T, p identity.Provider, runID uuid.UUID, sub, sponsor string, operatorOwned bool) identity.RunIdentity {
	t.Helper()
	ri, err := p.MintRunIdentity(context.Background(), runID, sub, sponsor, conformanceAud, operatorOwned)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return ri
}

func roundtripConformance(t *testing.T, p identity.Provider) {
	ctx := context.Background()
	runID := uuid.New()
	before := time.Now()
	ri := mint(t, p, runID, "alice@example.com", "sponsor@example.com", false)
	if ri.Token == "" || ri.SPIFFEID == "" {
		t.Fatal("mint returned empty token/spiffe id")
	}
	if !strings.HasSuffix(ri.SPIFFEID, "/agent-run/"+runID.String()) {
		t.Fatalf("spiffe id %q is not spiffe://<td>/agent-run/<run-id>", ri.SPIFFEID)
	}
	if !ri.Expiry.After(before) || ri.Expiry.After(time.Now().Add(identity.RunTokenTTL+time.Second)) {
		t.Fatalf("expiry %v is not in (now, now + RunTokenTTL]", ri.Expiry)
	}
	cl, err := p.Verify(ctx, ri.Token, conformanceAud)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if cl.RunID != runID || cl.Sub != "alice@example.com" || cl.Sponsor != "sponsor@example.com" ||
		cl.SPIFFEID != ri.SPIFFEID || cl.OperatorOwned || cl.JTI != ri.JTI || cl.Audience != conformanceAud {
		t.Fatalf("claims mismatch: %+v (minted %+v)", cl, ri)
	}
	if d := cl.Expiry.Sub(ri.Expiry); d > time.Second || d < -time.Second {
		t.Fatalf("claims expiry %v is not within 1s of the minted expiry %v", cl.Expiry, ri.Expiry)
	}
	if cl.IssuedAt.After(time.Now().Add(time.Second)) {
		t.Fatalf("issued-at %v is in the future", cl.IssuedAt)
	}
}

func operatorOwnedConformance(t *testing.T, p identity.Provider) {
	ri := mint(t, p, uuid.New(), "admin-token", "admin-token", true)
	if cl, err := p.Verify(context.Background(), ri.Token, conformanceAud); err != nil || !cl.OperatorOwned {
		t.Fatalf("verify = (%+v, %v), want OperatorOwned carried from the mint", cl, err)
	}
}

func sponsorDefaultConformance(t *testing.T, p identity.Provider) {
	ri := mint(t, p, uuid.New(), "alice@example.com", "", false)
	if cl, err := p.Verify(context.Background(), ri.Token, conformanceAud); err != nil || cl.Sponsor != "alice@example.com" {
		t.Fatalf("verify = (%+v, %v), want an empty sponsor to default to the subject", cl, err)
	}
}

func emptyInputsConformance(t *testing.T, p identity.Provider) {
	ctx := context.Background()
	if _, err := p.MintRunIdentity(ctx, uuid.New(), "", "s", conformanceAud, false); err == nil {
		t.Fatal("mint with an empty subject must be refused")
	}
	if _, err := p.MintRunIdentity(ctx, uuid.New(), "a", "s", "", false); err == nil {
		t.Fatal("mint with an empty audience must be refused")
	}
	ri := mint(t, p, uuid.New(), "a", "a", false)
	if _, err := p.Verify(ctx, ri.Token, ""); err == nil {
		t.Fatal("verify with an empty expected audience must be refused")
	}
}

func wrongAudienceConformance(t *testing.T, p identity.Provider) {
	ri := mint(t, p, uuid.New(), "a", "a", false)
	if _, err := p.Verify(context.Background(), ri.Token, "wardyn-groundtruth"); err == nil {
		t.Fatal("verify with the wrong audience must fail closed")
	}
}

func tamperedConformance(t *testing.T, p identity.Provider) {
	ri := mint(t, p, uuid.New(), "a", "a", false)
	if _, err := p.Verify(context.Background(), ri.Token+"tamper", conformanceAud); err == nil {
		t.Fatal("verify of a tampered token must fail closed")
	}
}

// splicedConformance puts token A's header and signature on token B's payload: both halves are well
// formed, so only the signature check can refuse it.
func splicedConformance(t *testing.T, p identity.Provider) {
	a := strings.Split(mint(t, p, uuid.New(), "a", "a", false).Token, ".")
	b := strings.Split(mint(t, p, uuid.New(), "b", "b", true).Token, ".")
	if len(a) != 3 || len(b) != 3 {
		t.Fatalf("tokens are not compact JWS: %d and %d parts", len(a), len(b))
	}
	if _, err := p.Verify(context.Background(), a[0]+"."+b[1]+"."+a[2], conformanceAud); err == nil {
		t.Fatal("verify of a spliced token must fail closed")
	}
}

func revokeRunConformance(t *testing.T, p identity.Provider) {
	ctx := context.Background()
	runID := uuid.New()
	ri := mint(t, p, runID, "a", "a", false)
	if _, err := p.Verify(ctx, ri.Token, conformanceAud); err != nil {
		t.Fatalf("pre-revoke verify should succeed: %v", err)
	}
	if err := p.RevokeRun(ctx, runID); err != nil {
		t.Fatalf("RevokeRun: %v", err)
	}
	_, err := p.Verify(ctx, ri.Token, conformanceAud)
	var rev *identity.RevokedTokenError
	if !errors.As(err, &rev) || rev.Claims == nil || rev.Claims.RunID != runID {
		t.Fatalf("verify after RevokeRun = %v, want a RevokedTokenError carrying run %s", err, runID)
	}
}

func revokeSparesConformance(t *testing.T, p identity.Provider) {
	ctx := context.Background()
	runID, other := uuid.New(), uuid.New()
	mint(t, p, runID, "a", "a", false)
	ro := mint(t, p, other, "b", "b", false)
	if err := p.RevokeRun(ctx, runID); err != nil {
		t.Fatalf("RevokeRun: %v", err)
	}
	if cl, err := p.Verify(ctx, ro.Token, conformanceAud); err != nil || cl.RunID != other {
		t.Fatalf("an unrelated run = (%+v, %v): a revoke is per run", cl, err)
	}
}

// A provider may fail closed at the mint (one that checks the denylist before it mints) or at Verify; it
// never yields a token that verifies.
func revocationErrorConformance(t *testing.T, p identity.Provider) {
	ri, err := p.MintRunIdentity(context.Background(), uuid.New(), "a", "a", conformanceAud, false)
	if err != nil {
		if ri.Token != "" {
			t.Fatal("a mint that failed closed still returned a token")
		}
		return
	}
	_, err = p.Verify(context.Background(), ri.Token, conformanceAud)
	var rev *identity.RevokedTokenError
	if err == nil || errors.As(err, &rev) {
		t.Fatalf("verify with a failing revocation store = %v, want an ordinary error (fail closed)", err)
	}
}

func revokeJTIConformance(t *testing.T, p identity.Provider) {
	ctx := context.Background()
	r, ok := p.(jtiRevoker)
	if !ok {
		t.Fatal("the provider cannot retire one token (RevokeJTI), so a revive would leave old tokens live")
	}
	runID := uuid.New()
	a, b := mint(t, p, runID, "a", "a", false), mint(t, p, runID, "a", "a", false)
	if err := r.RevokeJTI(ctx, a.JTI, runID); err != nil {
		t.Fatalf("RevokeJTI: %v", err)
	}
	var rev *identity.RevokedTokenError
	if _, err := p.Verify(ctx, a.Token, conformanceAud); !errors.As(err, &rev) {
		t.Fatalf("the retired token = %v, want a RevokedTokenError", err)
	}
	if cl, err := p.Verify(ctx, b.Token, conformanceAud); err != nil || cl.RunID != runID {
		t.Fatalf("the run's other token = (%+v, %v): a jti revoke is not a run revoke", cl, err)
	}
}

func jtiUniqueConformance(t *testing.T, p identity.Provider) {
	runID := uuid.New()
	a, b := mint(t, p, runID, "a", "a", false), mint(t, p, runID, "a", "a", false)
	if a.JTI == "" || a.JTI == b.JTI || a.Token == b.Token {
		t.Fatalf("two mints for one run gave jti %q / %q: each token needs its own", a.JTI, b.JTI)
	}
}

func cloudSTSConformance(t *testing.T, p identity.Provider) {
	c, ok := p.(grantChecker)
	if !ok {
		t.Skip("the provider has no create-time grant check")
	}
	if err := c.CheckGrants([]types.GrantSpec{{Kind: types.GrantCloudSTS}}); err == nil {
		t.Fatal("a cloud_sts grant must be refused: cloud STS federation is not built")
	}
}
