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
		ri, err := p.MintRunIdentity(ctx, runID, "alice@example.com", "sponsor@example.com", aud, false)
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
		if cl.RunID != runID || cl.Sub != "alice@example.com" || cl.SPIFFEID != ri.SPIFFEID || cl.OperatorOwned {
			t.Fatalf("claims mismatch: %+v", cl)
		}
	})

	t.Run("operator_owned_is_carried_signed", func(t *testing.T) {
		p := newProvider(t)
		ri, err := p.MintRunIdentity(ctx, uuid.New(), "admin-token", "admin-token", aud, true)
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		if cl, err := p.Verify(ctx, ri.Token, aud); err != nil || !cl.OperatorOwned {
			t.Fatalf("verify = (%+v, %v), want OperatorOwned carried from the mint", cl, err)
		}
	})

	t.Run("wrong_audience_fails_closed", func(t *testing.T) {
		p := newProvider(t)
		ri, err := p.MintRunIdentity(ctx, uuid.New(), "a", "a", aud, false)
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		if _, err := p.Verify(ctx, ri.Token, "wardyn-groundtruth"); err == nil {
			t.Fatal("verify with the wrong audience must fail closed")
		}
	})

	t.Run("tampered_token_fails_closed", func(t *testing.T) {
		p := newProvider(t)
		ri, err := p.MintRunIdentity(ctx, uuid.New(), "a", "a", aud, false)
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
		ri, err := p.MintRunIdentity(ctx, runID, "a", "a", aud, false)
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

	// A run a workload launched is named by the workload, then the run, and stays a run of its own: its
	// token verifies to its run, and revoking it leaves another run of the same workload working.
	t.Run("workload_run_spiffe_shape_and_per_run_revocation", func(t *testing.T) {
		workloadRunConformance(t, newProvider(t), aud)
	})
}

// workloadRunConformance is RunConformance's workload-run case, kept apart so neither function grows
// past the complexity limit.
func workloadRunConformance(t *testing.T, p identity.Provider, aud string) {
	t.Helper()
	ctx := context.Background()
	m, ok := p.(identity.WorkloadRunMinter)
	if !ok {
		t.Fatal("the provider cannot mint for a run a workload launched (identity.WorkloadRunMinter)")
	}
	w := identity.Workload{Owner: "service:ci-bot", Name: "nightly"}
	runID, other := uuid.New(), uuid.New()
	ri, err := m.MintWorkloadRunIdentity(ctx, runID, w, "service:ci-bot", "sponsor@example.com", aud, false)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if !strings.HasSuffix(ri.SPIFFEID, "/workload/service_3Aci-bot/nightly/run/"+runID.String()) {
		t.Fatalf("spiffe id %q is not spiffe://<td>/workload/<owner>/<name>/run/<run-id>", ri.SPIFFEID)
	}
	ro, err := m.MintWorkloadRunIdentity(ctx, other, w, "service:ci-bot", "sponsor@example.com", aud, false)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if cl, err := p.Verify(ctx, ri.Token, aud); err != nil || cl.RunID != runID || cl.SPIFFEID != ri.SPIFFEID {
		t.Fatalf("verify = (%+v, %v), want the run and its SPIFFE id", cl, err)
	}
	if err := p.RevokeRun(ctx, runID); err != nil {
		t.Fatalf("RevokeRun: %v", err)
	}
	if _, err := p.Verify(ctx, ri.Token, aud); err == nil {
		t.Fatal("verify after RevokeRun must fail closed")
	}
	if cl, err := p.Verify(ctx, ro.Token, aud); err != nil || cl.RunID != other {
		t.Fatalf("the workload's other run = (%+v, %v): a revoke is per run", cl, err)
	}
}
