// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// countingCapStore counts the three capability reads narrowMemberInlinePolicy's
// loops can make, so the assertion below is a COUNT rather than a stopwatch —
// a wall-clock threshold on a shared CI box is a flake, while "how many round
// trips did one request make" is the actual defect and is exact.
type countingCapStore struct {
	store.Store
	forCalls, fullCalls, enfCalls atomic.Int64
	groupDenyCalls                atomic.Int64
	grants                        []types.CapabilityGrant
	enf                           map[string]bool
	workspaces                    []types.Workspace
}

// ListCapabilityGrantsFor FILTERS BY SUBJECT, as the real store's SQL does. That
// is load-bearing rather than fixture realism: on a stale snapshot
// capabilitySubjects yields groups=nil, so a GROUP-tier grant is invisible to
// this read — which is the entire reason the full-table
// unresolvable-group-deny fallback exists. A double that returned every grant
// regardless would surface the group deny through the ordinary loop and the
// stale arm below would pass without ever reaching the code it names.
func (c *countingCapStore) ListCapabilityGrantsFor(_ context.Context, users, groups []string) ([]types.CapabilityGrant, error) {
	c.forCalls.Add(1)
	var out []types.CapabilityGrant
	for _, g := range c.grants {
		switch g.SubjectType {
		case types.CapabilitySubjectAll:
			out = append(out, g)
		case types.CapabilitySubjectUser:
			if slices.Contains(users, g.Subject) {
				out = append(out, g)
			}
		case types.CapabilitySubjectGroup:
			if slices.Contains(groups, g.Subject) {
				out = append(out, g)
			}
		}
	}
	return out, nil
}
func (c *countingCapStore) ListCapabilityGrants(context.Context) ([]types.CapabilityGrant, error) {
	c.fullCalls.Add(1)
	return c.grants, nil
}

// ListGroupDenyGrants is the read capUnresolvableGroupDeny makes on the stale
// path. It replaced the whole-table ListCapabilityGrants above, which used to
// cost O(grant table) per checked value on the path EVERY pre-0.7 API token
// takes; the predicate here mirrors the SQL exactly (group + deny + this kind)
// so this double cannot be the reason the two agree.
//
// Counted, and counted into total(): the stale path's third read is still a
// read, and leaving it out would let a per-VALUE resolution of it slip past the
// flatness law below — the very growth that law exists to forbid.
func (c *countingCapStore) ListGroupDenyGrants(_ context.Context, capability string) ([]types.CapabilityGrant, error) {
	c.groupDenyCalls.Add(1)
	var out []types.CapabilityGrant
	for _, g := range c.grants {
		if g.SubjectType == types.CapabilitySubjectGroup && g.Effect == types.CapabilityDeny && g.Capability == capability {
			out = append(out, g)
		}
	}
	return out, nil
}
func (c *countingCapStore) GetCapabilityEnforcement(context.Context) (map[string]bool, error) {
	c.enfCalls.Add(1)
	return c.enf, nil
}
func (c *countingCapStore) ListWorkspaces(context.Context) ([]types.Workspace, error) {
	return c.workspaces, nil
}
func (c *countingCapStore) total() int64 {
	return c.forCalls.Load() + c.fullCalls.Load() + c.enfCalls.Load() + c.groupDenyCalls.Load()
}

// TestCapBatch_StoreReadsAreFlatInCallerInput is the pin for the growth law.
//
// narrowMemberInlinePolicy called capSeamAllowed once per allowed_domains entry,
// and capSeamAllowed makes two uncached Postgres round trips (three when the
// caller's group snapshot is unanswerable). spec.AllowedDomains is the REQUEST
// BODY's list and nothing on this path caps or de-duplicates it before the loop:
// validatePolicySpec's count caps have no allowed_domains arm AND run after
// boundMemberSpec, and composer.Clamp's intersection preserves duplicates of a
// permitted entry and is skipped entirely under a ceiling with allow_all_egress.
//
// MEASURED before the fix, against a real store.PG over loopback with an empty
// grants table: ~505µs per entry, linear. One member request carrying the most
// entries that fit under maxJSONBody (52,425 x "api.anthropic.com" = 1,048,572
// bytes) made 104,850 sequential round trips and spent 27.0s inside this one
// function — 157,275 and 45.3s on a stale snapshot. POST /runs/preflight is on
// the member router group and persists nothing, so it was repeatable for free
// against a pool whose default MaxConns is max(4, NumCPU).
//
// The assertion is CONSTANCY, not a budget: the count must not depend on
// len(AllowedDomains) at all. A per-value resolution of any kind fails it.
func TestCapBatch_StoreReadsAreFlatInCallerInput(t *testing.T) {
	run := func(t *testing.T, n int, stale bool) (int64, int) {
		t.Helper()
		cs := &countingCapStore{}
		h := newHarness(t)
		srv := New(baseTestConfig(h, cs))
		domains := make([]string, n)
		for i := range domains {
			domains[i] = "api.anthropic.com" // one legal entry, repeated: nothing dedupes it
		}
		spec := types.RunPolicySpec{AllowedDomains: domains}
		if _, _, err := srv.narrowMemberInlinePolicy(govMemberCtx([]string{"eng"}, stale), "", &spec); err != nil {
			t.Fatalf("n=%d stale=%v: %v", n, stale, err)
		}
		return cs.total(), len(spec.AllowedDomains)
	}

	for _, c := range []struct {
		name  string
		stale bool
		want  int64 // GrantsFor + Enforcement, plus the group-deny read when stale
	}{
		{"answerable snapshot", false, 2},
		{"stale snapshot", true, 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, n := range []int{1, 10, 1000, 20000} {
				got, kept := run(t, n, c.stale)
				if got != c.want {
					t.Errorf("n=%d: store reads = %d, want %d regardless of n — the cost of a member's request "+
						"must not be chosen by that member's request body", n, got, c.want)
				}
				// The narrowing itself still happens: a test where the loop
				// stopped running would pass the count assertion and prove
				// nothing.
				if kept != n {
					t.Errorf("n=%d: kept %d domains, want all %d (no grants, no enforcement => nothing is dropped)", n, kept, n)
				}
			}
		})
	}

	// Nothing to check performs NO reads at all — the batch is lazy, so an empty
	// spec keeps the behaviour capSeamAllowed's nil-store arm gives the ~30
	// store-less doubles in this package.
	t.Run("an empty spec reads nothing", func(t *testing.T) {
		cs := &countingCapStore{}
		h := newHarness(t)
		srv := New(baseTestConfig(h, cs))
		spec := types.RunPolicySpec{}
		if _, _, err := srv.narrowMemberInlinePolicy(govMemberCtx([]string{"eng"}, false), "", &spec); err != nil {
			t.Fatal(err)
		}
		if got := cs.total(); got != 0 {
			t.Errorf("empty spec made %d store reads, want 0", got)
		}
	})
}

// TestCapBatch_CPUIsFlatInCallerInput is the second half of the growth law, and
// the half 0.7 shipped OPEN: the store round trips were made flat and the CPU
// was not. capBatch.allowed walked the caller's WHOLE grant set for every value,
// `continue`-ing past every row of another kind, and narrowMemberInlinePolicy
// called it once per allowed_domains ENTRY — a list taken verbatim from the
// request body, which nothing on this path caps or de-duplicates. So one
// authenticated member's POST /runs/preflight bought O(len(AllowedDomains) x
// grants) grant comparisons inside a single handler: 1.44s at 200 grants, 7.20s
// at 1000, while the database work stayed at the flat 2 reads the sibling test
// pins. "No more round trips" is not "no more amplification".
//
// Counted, not timed: a wall-clock threshold on a shared box is a flake, and the
// count is the defect exactly.
func TestCapBatch_CPUIsFlatInCallerInput(t *testing.T) {
	// 300 grants the caller actually holds: 100 egress_host (the kind asked
	// about) and 200 of other kinds, which a per-value walk pays for and a
	// kind-indexed one never touches.
	var grants []types.CapabilityGrant
	for i := 0; i < 100; i++ {
		grants = append(grants, types.CapabilityGrant{
			ID: uuid.New(), Capability: capEgressHost, Effect: types.CapabilityAllow,
			SubjectType: types.CapabilitySubjectGroup, Subject: "eng", Value: "granted-" + strconv.Itoa(i) + ".example.com",
		})
	}
	for _, kind := range []string{capSecret, capWorkspace} {
		for i := 0; i < 100; i++ {
			grants = append(grants, types.CapabilityGrant{
				ID: uuid.New(), Capability: kind, Effect: types.CapabilityAllow,
				SubjectType: types.CapabilitySubjectGroup, Subject: "eng", Value: kind + "-" + strconv.Itoa(i),
			})
		}
	}

	run := func(t *testing.T, n int) int64 {
		t.Helper()
		h := newHarness(t)
		srv := New(baseTestConfig(h, &countingCapStore{grants: grants}))
		domains := make([]string, n)
		for i := range domains {
			domains[i] = "granted-7.example.com" // one legal entry, repeated: nothing dedupes it
		}
		spec := types.RunPolicySpec{AllowedDomains: domains}
		if _, _, err := srv.narrowMemberInlinePolicy(govMemberCtx([]string{"eng"}, false), "", &spec); err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if len(spec.AllowedDomains) != n {
			t.Fatalf("n=%d: kept %d domains, want all %d — a test whose loop stopped running proves nothing", n, len(spec.AllowedDomains), n)
		}
		return srv.capRowsScanned.Load()
	}

	// CONSTANCY in n, and bounded by the ONE kind asked about — not by every
	// grant the caller holds. 100 egress_host rows are scanned once, for the
	// single distinct host; 8 is slack for the seam's own other lookups.
	const want = 108
	for _, n := range []int{1, 10, 1000, 20000} {
		if got := run(t, n); got > want {
			t.Errorf("n=%d: compared %d grant rows, want <= %d regardless of n — a member's request body "+
				"must not choose how much CPU the control plane spends, any more than it chooses the round trips", n, got, want)
		}
	}

	// The counterweight: a genuinely NEW host is still resolved, so the memo
	// cannot be "answer everything from the first entry".
	t.Run("distinct hosts are each resolved", func(t *testing.T) {
		h := newHarness(t)
		srv := New(baseTestConfig(h, &countingCapStore{grants: grants}))
		spec := types.RunPolicySpec{AllowedDomains: []string{"granted-1.example.com", "granted-2.example.com", "granted-1.example.com"}}
		if _, _, err := srv.narrowMemberInlinePolicy(govMemberCtx([]string{"eng"}, false), "", &spec); err != nil {
			t.Fatal(err)
		}
		if len(spec.AllowedDomains) != 3 {
			t.Fatalf("kept %v, want all three entries (duplicates included — the memo must not de-duplicate the OUTPUT)", spec.AllowedDomains)
		}
		if got := srv.capRowsScanned.Load(); got < 100 {
			t.Errorf("compared %d rows for two distinct hosts — under one full kind scan means a host went unresolved", got)
		}
	})
}

// TestCapBatch_AnswersMatchCapSeamAllowed is the correctness half: the batch is
// a different SHAPE of the same rule, so it must agree with the per-value
// resolver on every case that distinguishes them — not merely be faster.
//
// The stale/deny arm is the one that matters most and the one I got wrong first
// while writing this: capScan runs its unresolvable-group-deny check even when a
// grant already ALLOWED the value, because an unanswerable snapshot may be
// hiding a group deny and a deny beats an allow. Gating that check on "nothing
// allowed it yet" is a widening, and only this table catches it.
func TestCapBatch_AnswersMatchCapSeamAllowed(t *testing.T) {
	const host = "corp.example"
	userAllow := types.CapabilityGrant{
		ID: uuid.New(), Capability: capEgressHost, Value: host,
		Effect: types.CapabilityAllow, SubjectType: types.CapabilitySubjectUser, Subject: "sub-gov-bob",
	}
	groupDeny := types.CapabilityGrant{
		ID: uuid.New(), Capability: capEgressHost, Value: host,
		Effect: types.CapabilityDeny, SubjectType: types.CapabilitySubjectGroup, Subject: "eng",
	}

	// groups==nil models the snapshot that is MISSING rather than truncated, and
	// the distinction is what makes the last two arms real: capabilitySubjects
	// reports stale for either, but with a nil snapshot the caller's group grants
	// are invisible to ListCapabilityGrantsFor — so a group deny can only be
	// found by the full-table fallback. Passing []string{"eng"} with
	// truncated=true would surface the same deny through the ordinary loop and
	// the fallback would never run.
	for _, c := range []struct {
		name      string
		grants    []types.CapabilityGrant
		enf       map[string]bool
		groups    []string
		truncated bool
		wantOK    bool
	}{
		{"no grants, not enforced => allowed", nil, nil, []string{"eng"}, false, true},
		{"no grants, ENFORCED => refused", nil, map[string]bool{capEgressHost: true}, []string{"eng"}, false, false},
		{"allow grant, enforced => allowed", []types.CapabilityGrant{userAllow}, map[string]bool{capEgressHost: true}, []string{"eng"}, false, true},
		{"deny beats allow", []types.CapabilityGrant{userAllow, groupDeny}, nil, []string{"eng"}, false, false},
		// THE arm that catches gating the fallback on "nothing allowed it yet":
		// a user grant ALLOWS the value, the snapshot is missing, and a group
		// deny of this kind exists that only the full-table read can see.
		{"missing snapshot + group deny beats an explicit allow", []types.CapabilityGrant{userAllow, groupDeny}, nil, nil, false, false},
		{"missing snapshot, no group deny => unchanged", []types.CapabilityGrant{userAllow}, nil, nil, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			cs := &countingCapStore{grants: c.grants, enf: c.enf}
			h := newHarness(t)
			srv := New(baseTestConfig(h, cs))
			ctx := govMemberCtx(c.groups, c.truncated)

			viaSeam, err := srv.capSeamAllowed(ctx, capEgressHost, host)
			if err != nil {
				t.Fatalf("capSeamAllowed: %v", err)
			}
			viaBatch, err := srv.newCapBatch(ctx).allowed(ctx, capEgressHost, host)
			if err != nil {
				t.Fatalf("capBatch.allowed: %v", err)
			}
			if viaSeam != c.wantOK {
				t.Errorf("capSeamAllowed = %v, want %v — the fixture stopped exercising this case", viaSeam, c.wantOK)
			}
			if viaBatch != viaSeam {
				t.Errorf("capBatch.allowed = %v but capSeamAllowed = %v — the batch is a different ANSWER, not just a different shape", viaBatch, viaSeam)
			}
		})
	}
}
