// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"reflect"
	"testing"
)

// TestCeilingResolvesOnTheAnswerableIdentityOnly is F349.
//
// resolveEffectiveCeiling's stale arm carries the rule as a comment: "The
// unusable half must not be MATCHED against. Passing a truncated list would
// still let a surviving group's row win, which is not wrong on its own — but it
// makes the refusal below depend on which groups happened to fit, so the same
// human with the same claims could be refused or served depending on
// alphabetical luck. Resolve on the answerable identity only." That is a
// statement about the ARGUMENTS of one store call, and nothing asserted it.
//
// The reviewer's counterfactual: thread the truncated `groups` through
// ceilingWithUnusableGroups into ResolveGovernanceProfile — inverting the
// invariant outright — and all of ./internal/api stayed green (47.2s, ok). The
// api-layer doubles took their subject arguments as `_`, so the call was
// unobservable from any test in the package; the property could not be seen,
// let alone pinned.
//
// The DRIVE twin was already covered (TestResolveUserDriveReadsTheCallersOwnSubjects
// asserts the unusable-groups read passes no groups, via driveStore.sawGroups).
// This is the governance twin, on the same double, now that it records too.
func TestCeilingResolvesOnTheAnswerableIdentityOnly(t *testing.T) {
	// Two claims, because the resolver matches EITHER the sub or the email and
	// the order is load-bearing elsewhere: a test with one subject cannot tell
	// "the caller's own list" from "a list".
	users := []string{"sub-drive-bob", "bob@corp.example"}

	t.Run("the unusable-groups resolve asks on users alone", func(t *testing.T) {
		st := &driveStore{}
		srv := driveServer(st)
		if _, err := srv.ceilingWithUnusableGroups(context.Background(), users, governanceCeiling{}); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if len(st.sawGovUsers) == 0 {
			t.Fatal("the governance profile was never resolved")
		}
		if !reflect.DeepEqual(st.sawGovUsers[0], users) {
			t.Errorf("the ceiling was resolved for %v, want the caller's own subjects %v — in that order, "+
				"because the resolver's tie-break IS this slice's order", st.sawGovUsers[0], users)
		}
		if st.sawGovGroups[0] != nil {
			t.Errorf("the unusable-groups resolve was MATCHED against groups %v. A truncated snapshot is "+
				"sorted and cut at the cookie cap, so passing it lets whichever groups happened to FIT decide "+
				"the ceiling: the same human with the same claims is refused or served by alphabetical luck. "+
				"Resolve on the answerable identity only, and let the refusal cover the rest", st.sawGovGroups[0])
		}
	})

	// THE CONTROL, and it is what stops the assertion above from being satisfied
	// by a resolver that never passes groups at all — which would silently
	// delete the whole group tier rather than fail closed on it.
	t.Run("a complete snapshot passes its groups through", func(t *testing.T) {
		groups := []string{"eng", "sre"}
		st := &driveStore{}
		srv := driveServer(st)
		ctx := driveMemberCtx(groups, false)
		if _, err := srv.effectiveCeiling(ctx); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if len(st.sawGovGroups) == 0 {
			t.Fatal("the governance profile was never resolved")
		}
		if !reflect.DeepEqual(st.sawGovGroups[0], groups) {
			t.Errorf("an ANSWERABLE snapshot resolved with groups %v, want %v — the group tier only exists "+
				"if the ordinary path matches against it", st.sawGovGroups[0], groups)
		}
	})

	// …and the truncated snapshot really does take the unusable arm through the
	// PUBLIC entrance, not just when ceilingWithUnusableGroups is called
	// directly. Asserted separately because the first subtest calls the arm by
	// hand: a resolveEffectiveCeiling that stopped routing to it would leave
	// that subtest green while the invariant was gone from every request.
	t.Run("a truncated snapshot reaches the unusable arm from effectiveCeiling", func(t *testing.T) {
		groups := []string{"eng", "sre"}
		st := &driveStore{}
		srv := driveServer(st)
		// Present, non-nil and INCOMPLETE — the PF-26 shape.
		ctx := driveMemberCtx(groups, true)
		if _, err := srv.effectiveCeiling(ctx); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if len(st.sawGovGroups) == 0 {
			t.Fatal("the governance profile was never resolved")
		}
		if st.sawGovGroups[0] != nil {
			t.Errorf("a request whose snapshot is truncated resolved with groups %v — the truncation bit is "+
				"what routes to the unusable arm, and a resolver that ignores it is matching against a list "+
				"it knows is incomplete", st.sawGovGroups[0])
		}
	})
}
