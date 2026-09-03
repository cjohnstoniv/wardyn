// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// capabilities_unresolvable_test.go guards the FAIL-CLOSED half of capScan
// against its own optimisation.
//
// capUnresolvableGroupDeny answers "could a group DENY row I cannot see cover
// this value" for every caller whose group snapshot is unanswerable — which,
// because a NULL groups_truncated column reads as truncated by design, is every
// API token minted before 0.7, on every request. It used to answer by reading
// the WHOLE capability_grants table, once per value checked, so the cost of an
// authorization check scaled with the size of the grant table (68 ms at 20k
// rows against 0.35 ms for the indexed sibling) and a token holder could force
// that read per checked value.
//
// Making a fail-closed check faster risks the only thing worse than a slow
// deny: a deny that stops firing. So the equivalence is pinned directly, over a
// generated matrix, against a reference implementation of the exact scan the
// fix replaced — not merely "the new path is quicker".
package api

import (
	"context"
	"fmt"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// capUnresolvableGroupDenyFullScanReference is the PRE-FIX implementation,
// verbatim: the whole table, filtered in Go. It is the oracle, so it must not
// be "improved" — if this and the production path ever disagree, the
// production path is wrong.
func capUnresolvableGroupDenyFullScanReference(grants []types.CapabilityGrant, kind, value string) bool {
	for _, g := range grants {
		if g.SubjectType == types.CapabilitySubjectGroup && g.Effect == types.CapabilityDeny &&
			g.Capability == kind && capValueOverlaps(kind, g.Value, value) {
			return true
		}
	}
	return false
}

// capUnresolvableMatrixRows is deliberately adversarial about the ways a row
// can ALMOST match: the right value on the wrong kind, the right kind with the
// wrong effect, a user-tier row that looks identical, wildcards on both sides,
// and the egress-host suffix semantics that make overlap SYMMETRIC (a deny on
// "*.corp.example" covers a request for "api.corp.example", and a deny on
// "api.corp.example" must also be reported when the request is for the wider
// "*.corp.example" — capValueOverlaps runs the compare both ways for that kind
// alone).
func capUnresolvableMatrixRows() []types.CapabilityGrant {
	g := func(st types.CapabilitySubjectType, subj, kind, val string, eff types.CapabilityEffect) types.CapabilityGrant {
		return types.CapabilityGrant{SubjectType: st, Subject: subj, Capability: kind, Value: val, Effect: eff}
	}
	return []types.CapabilityGrant{
		// The rows that SHOULD make the refusal fire.
		g(types.CapabilitySubjectGroup, "walled", capSecret, "prod-db", types.CapabilityDeny),
		g(types.CapabilitySubjectGroup, "walled", capEgressHost, "*.corp.example", types.CapabilityDeny),
		g(types.CapabilitySubjectGroup, "contractors", capEgressHost, "pypi.org", types.CapabilityDeny),
		g(types.CapabilitySubjectGroup, "walled", capImage, capWildcard, types.CapabilityDeny),
		g(types.CapabilitySubjectGroup, "walled", capAgent, "claude-code", types.CapabilityDeny),
		// Near misses, each wrong in exactly one field.
		g(types.CapabilitySubjectGroup, "walled", capSecret, "prod-db", types.CapabilityAllow), // effect
		g(types.CapabilitySubjectUser, "alice", capSecret, "staging-db", types.CapabilityDeny), // subject_type
		g(types.CapabilitySubjectAll, "", capSecret, "shared-db", types.CapabilityDeny),        // subject_type
		g(types.CapabilitySubjectGroup, "walled", capWorkspace, "ws-1", types.CapabilityDeny),  // kind, for a secret query
		g(types.CapabilitySubjectGroup, "walled", capIntegration, "github", types.CapabilityDeny),
	}
}

// TestCapUnresolvableGroupDenyMatchesFullScan is the equivalence pin: for every
// (kind, value) in the matrix, the production path and the pre-fix full scan
// must return the SAME answer over the SAME rows.
//
// Counterfactual: narrow the store predicate wrongly — drop the effect clause,
// or match subject_type='user' — and the two disagree here, in the direction
// that matters (a deny that no longer fires, or one that fires on a row the old
// code ignored).
func TestCapUnresolvableGroupDenyMatchesFullScan(t *testing.T) {
	rows := capUnresolvableMatrixRows()
	values := []string{
		"prod-db", "staging-db", "shared-db", "unknown-secret", "",
		"api.corp.example", "corp.example", "*.corp.example", "evilcorp.example",
		"pypi.org", "files.pypi.org", capWildcard,
		"ghcr.io/x:1", "claude-code", "codex", "ws-1", "ws-2", "github", "gitlab",
	}
	kinds := []string{capSecret, capEgressHost, capImage, capWorkspace, capAgent, capIntegration}

	var agreed, fired int
	for _, kind := range kinds {
		for _, v := range values {
			kind, v := kind, v
			t.Run(fmt.Sprintf("%s/%s", kind, v), func(t *testing.T) {
				st := &capStore{grants: rows}
				srv := New(baseTestConfig(newHarness(t), st))
				got, err := srv.capUnresolvableGroupDeny(context.Background(), kind, v)
				if err != nil {
					t.Fatalf("capUnresolvableGroupDeny: %v", err)
				}
				want := capUnresolvableGroupDenyFullScanReference(rows, kind, v)
				if got != want {
					t.Fatalf("capUnresolvableGroupDeny(%q, %q) = %v, full-scan reference = %v — "+
						"the faster fail-closed path disagrees with the scan it replaced", kind, v, got, want)
				}
				// The whole table must not be read on this path any more.
				if st.fullTableReads != 0 {
					t.Errorf("issued %d whole-table reads; the targeted query is the point", st.fullTableReads)
				}
			})
			if capUnresolvableGroupDenyFullScanReference(rows, kind, v) {
				fired++
			}
			agreed++
		}
	}
	// A matrix where the refusal never fires would agree trivially.
	if fired == 0 {
		t.Fatalf("the reference never fired across %d cases; the matrix proves nothing", agreed)
	}
	t.Logf("%d cases, %d of them firing the refusal", agreed, fired)
}

// TestCapUnresolvableGroupDenyStillRefusesThroughCapScan drives the real entry
// point rather than the helper, because the helper agreeing with itself is not
// the property that protects anyone: what matters is that a caller with an
// unanswerable snapshot is still REFUSED, and that a caller with an answerable
// one is unaffected.
//
// Counterfactual: point capUnresolvableGroupDeny at a query that returns
// nothing and the first arm reports deny=false — the silent evaporation PF-26
// exists to prevent.
func TestCapUnresolvableGroupDenyStillRefusesThroughCapScan(t *testing.T) {
	rows := capUnresolvableMatrixRows()

	t.Run("stale snapshot: the unseen group deny still refuses", func(t *testing.T) {
		st := &capStore{grants: rows}
		srv := New(baseTestConfig(newHarness(t), st))
		// nil groups => capabilitySubjects reports stale (a pre-0.6 cookie or a
		// pre-0.7 API token, the population this path exists for).
		ctx := withHumanIdentity(context.Background(), "sub-alice", "alice@corp.example", "member", nil, false)
		deny, allow, err := srv.capScan(ctx, capSecret, "prod-db")
		if err != nil {
			t.Fatalf("capScan: %v", err)
		}
		if !deny {
			t.Fatal("deny = false for an unanswerable snapshot with a matching group deny row — the refusal evaporated")
		}
		if allow {
			t.Error("allow = true alongside the refusal")
		}
		if st.fullTableReads != 0 {
			t.Errorf("capScan issued %d whole-table reads", st.fullTableReads)
		}
	})

	t.Run("stale snapshot, no group deny of that kind: unaffected", func(t *testing.T) {
		st := &capStore{grants: rows}
		srv := New(baseTestConfig(newHarness(t), st))
		ctx := withHumanIdentity(context.Background(), "sub-alice", "alice@corp.example", "member", nil, false)
		// capModelProvider has no group deny row in the matrix, so the scoping
		// that keeps "an upgrade with no configuration changes nothing" must
		// hold: no refusal.
		deny, _, err := srv.capScan(ctx, capWorkspace, "ws-9")
		if err != nil {
			t.Fatalf("capScan: %v", err)
		}
		if deny {
			t.Fatal("deny = true with no group deny row covering ws-9 — the refusal is no longer SCOPED, " +
				"which denies every pre-0.7 token on every deployment")
		}
	})

	t.Run("answerable snapshot never asks the question", func(t *testing.T) {
		st := &capStore{grants: rows}
		srv := New(baseTestConfig(newHarness(t), st))
		ctx := withHumanIdentity(context.Background(), "sub-alice", "alice@corp.example", "member", []string{"eng"}, false)
		if _, _, err := srv.capScan(ctx, capSecret, "prod-db"); err != nil {
			t.Fatalf("capScan: %v", err)
		}
		if st.groupDenyReads != 0 {
			t.Errorf("groupDenyReads = %d for a complete snapshot; the extra read must stay on the stale path only", st.groupDenyReads)
		}
	})

	// A store error on this path must never read as permission.
	t.Run("a store error refuses rather than allowing", func(t *testing.T) {
		st := &capStore{grants: rows, err: fmt.Errorf("boom")}
		srv := New(baseTestConfig(newHarness(t), st))
		ctx := withHumanIdentity(context.Background(), "sub-alice", "alice@corp.example", "member", nil, false)
		if _, _, err := srv.capScan(ctx, capSecret, "prod-db"); err == nil {
			t.Fatal("capScan returned nil error when the store failed; an unresolvable question must not answer 'allowed'")
		}
	})
}

// TestCapUnresolvableGroupDenyReadCostIsPerValueNotPerTable is the availability
// half, stated as a test rather than a benchmark. The old path made ONE
// WHOLE-TABLE read per value checked, so a handler examining N caller-supplied
// values multiplied an unbounded read by N — reachable by anyone holding a
// pre-0.7 API token. The read is now a targeted one; the whole-table read must
// not appear on this path at any N.
func TestCapUnresolvableGroupDenyReadCostIsPerValueNotPerTable(t *testing.T) {
	st := &capStore{grants: capUnresolvableMatrixRows()}
	srv := New(baseTestConfig(newHarness(t), st))
	ctx := withHumanIdentity(context.Background(), "sub-alice", "alice@corp.example", "member", nil, false)

	const n = 200
	for i := 0; i < n; i++ {
		if _, _, err := srv.capScan(ctx, capEgressHost, fmt.Sprintf("host-%d.example", i)); err != nil {
			t.Fatalf("capScan: %v", err)
		}
	}
	if st.fullTableReads != 0 {
		t.Fatalf("%d values checked issued %d WHOLE-TABLE reads (want 0) — a pre-0.7 token holder can force one per value",
			n, st.fullTableReads)
	}
	if st.groupDenyReads != n {
		t.Errorf("groupDenyReads = %d, want %d (one targeted read per value)", st.groupDenyReads, n)
	}
}
