// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package identitytest provides a reusable conformance suite for any
// identity.Provider implementation, so the blessed default (embedded) and a
// future alternate (SPIRE) are held to the identical security contract.
package identitytest

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
)

// RunConformance exercises the identity.Provider contract. newProvider must
// return a FRESH provider on each call.
func RunConformance(t *testing.T, newProvider func(t *testing.T) identity.Provider) {
	ctx := context.Background()
	const aud = "wardyn-internal"

	t.Run("mint_verify_roundtrip_and_spiffe_shape", func(t *testing.T) {
		p := newProvider(t)
		runID := uuid.New()
		ri, err := p.MintRunIdentity(ctx, runID, "alice@example.com", "sponsor@example.com", aud)
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		if ri.Token == "" || ri.SPIFFEID == "" {
			t.Fatal("mint returned empty token/spiffe id")
		}
		if !strings.HasSuffix(ri.SPIFFEID, "/agent-run/"+runID.String()) {
			t.Fatalf("spiffe id %q is not spiffe://<td>/agent-run/<run-id>", ri.SPIFFEID)
		}
		cl, err := p.Verify(ctx, ri.Token, aud)
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		if cl.RunID != runID || cl.Sub != "alice@example.com" || cl.SPIFFEID != ri.SPIFFEID {
			t.Fatalf("claims mismatch: %+v", cl)
		}
	})

	t.Run("wrong_audience_fails_closed", func(t *testing.T) {
		p := newProvider(t)
		ri, err := p.MintRunIdentity(ctx, uuid.New(), "a", "a", aud)
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		if _, err := p.Verify(ctx, ri.Token, "wardyn-groundtruth"); err == nil {
			t.Fatal("verify with the wrong audience must fail closed")
		}
	})

	t.Run("tampered_token_fails_closed", func(t *testing.T) {
		p := newProvider(t)
		ri, err := p.MintRunIdentity(ctx, uuid.New(), "a", "a", aud)
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		if _, err := p.Verify(ctx, ri.Token+"tamper", aud); err == nil {
			t.Fatal("verify of a tampered token must fail closed")
		}
	})

	t.Run("revoke_run_is_kill_switch", func(t *testing.T) {
		p := newProvider(t)
		runID := uuid.New()
		ri, err := p.MintRunIdentity(ctx, runID, "a", "a", aud)
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		if _, err := p.Verify(ctx, ri.Token, aud); err != nil {
			t.Fatalf("pre-revoke verify should succeed: %v", err)
		}
		if err := p.RevokeRun(ctx, runID); err != nil {
			t.Fatalf("RevokeRun: %v", err)
		}
		if _, err := p.Verify(ctx, ri.Token, aud); err == nil {
			t.Fatal("verify after RevokeRun must fail closed (kill-switch cascade)")
		}
	})
}
