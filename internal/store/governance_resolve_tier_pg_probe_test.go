// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// F2-sso-to-ceiling PROBE 4 — destination: internal/store/governance_resolve_tier_pg_probe_test.go
//
// Package store_test, guarded by WARDYN_TEST_PG like governance_pg_test.go
// (reuses its seedGovernanceProfile / seedGovernanceAssignment / runsPGPool).
//
// INVARIANT UNDER TEST: the TIER value ResolveGovernanceProfile returns is the
// matched row's subject_type — the value internal/api's ceilingWithUnusableGroups
// (internal/api/governance.go) trusts to tell "serve" from "refuse" on a
// truncated snapshot. TestPG_ResolveGovernanceProfile discards the tier on every
// case (its resolveName helper in governance_pg_test.go), and internal/api's
// precedence test drives it through a FAKE store (capStore.govTier), so nothing
// today pins that the real SQL returns the right tier. It also pins the
// truncated-shape call (groups nil) end to end against real rows: user > group
// > all, and an all-tier answer on groups=nil is reported AS all (so the api
// layer refuses it).
//
// Run (needs Postgres; the DSN is whatever the coordinator's test-pg uses):
//
//	cd /home/cjohn/wt-v07-profiles && \
//	cp local/review-0.7/deep/F2-sso-to-ceiling/governance_resolve_tier_pg_probe_test.go internal/store/ && \
//	WARDYN_TEST_PG='postgres://<user>:<pass>@127.0.0.1:55432/<db>?sslmode=disable' \
//	nice -n 10 GOMAXPROCS=8 go test ./internal/store/ -run 'TestF2_' -count=1 -p 4 -v ; \
//	rm -f internal/store/governance_resolve_tier_pg_probe_test.go
package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestF2_ResolveGovernanceProfile_TierIsTheMatchedRow(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	uniq := uuid.NewString()

	pUser := seedGovernanceProfile(t, st, "aaa-f2-user-"+uniq)
	pGroup := seedGovernanceProfile(t, st, "mmm-f2-group-"+uniq)
	pAll := seedGovernanceProfile(t, st, "zzz-f2-all-"+uniq)

	sub := "f2sub-" + uniq
	email := "f2-" + uniq + "@example.com"
	group := "f2grp-" + uniq

	// Before any group row exists the gate must be false — otherwise every
	// stale snapshot on this substrate would 403 for rows this test never wrote.
	// (Other tests may have left group rows; only assert the TRUE direction
	// after our own insert.)
	seedGovernanceAssignment(t, st, types.GovernanceAssignment{SubjectType: types.CapabilitySubjectAll, ProfileID: pAll.ID})
	seedGovernanceAssignment(t, st, types.GovernanceAssignment{SubjectType: types.CapabilitySubjectGroup, Subject: group, ProfileID: pGroup.ID, Priority: 100})

	resolve := func(t *testing.T, users, groups []string) (string, types.CapabilitySubjectType) {
		t.Helper()
		p, tier, err := st.ResolveGovernanceProfile(ctx, users, groups)
		if errors.Is(err, store.ErrNotFound) {
			return "", ""
		}
		if err != nil {
			t.Fatalf("resolve(%v,%v): %v", users, groups, err)
		}
		return p.Name, tier
	}

	t.Run("complete snapshot: group row wins and is REPORTED as group", func(t *testing.T) {
		name, tier := resolve(t, []string{sub, email}, []string{group})
		if name != pGroup.Name || tier != types.CapabilitySubjectGroup {
			t.Errorf("= (%q, %q), want (%q, group)", name, tier, pGroup.Name)
		}
	})

	t.Run("truncated shape (groups nil): falls to the ALL row and is REPORTED as all", func(t *testing.T) {
		// This is the answer ceilingWithUnusableGroups must REFUSE when a group
		// row exists; if the tier came back "" or "user" the api would serve it.
		name, tier := resolve(t, []string{sub, email}, nil)
		if name != pAll.Name || tier != types.CapabilitySubjectAll {
			t.Errorf("= (%q, %q), want (%q, all)", name, tier, pAll.Name)
		}
		has, err := st.HasGroupTierAssignments(ctx)
		if err != nil || !has {
			t.Errorf("HasGroupTierAssignments = (%v, %v), want true — the refusal gate must see our group row", has, err)
		}
	})

	t.Run("truncated shape with an EMAIL-keyed user row: user wins over the priority-100 group row, REPORTED as user", func(t *testing.T) {
		seedGovernanceAssignment(t, st, types.GovernanceAssignment{SubjectType: types.CapabilitySubjectUser, Subject: email, ProfileID: pUser.ID})
		name, tier := resolve(t, []string{sub, email}, nil)
		if name != pUser.Name || tier != types.CapabilitySubjectUser {
			t.Errorf("= (%q, %q), want (%q, user)", name, tier, pUser.Name)
		}
		// And WITH the group present the user row still wins (tier never crossed by priority).
		name, tier = resolve(t, []string{sub, email}, []string{group})
		if name != pUser.Name || tier != types.CapabilitySubjectUser {
			t.Errorf("with groups: = (%q, %q), want (%q, user)", name, tier, pUser.Name)
		}
	})

	t.Run("subject case: an UNFOLDED user row never matches the folded sub the api sends", func(t *testing.T) {
		// capabilitySubjects lowercases the sub (internal/api/capabilities.go) and
		// validateGovernanceAssignment lowercases the subject
		// (internal/api/governance.go);
		// the store itself stores what it is given. If a row bypassed the api
		// boundary with an upper-case subject it must be INERT, never matched
		// by a folded caller — and the folded row must match. Two rows, one
		// resolve each way, so the store and the api cannot disagree on WHO a
		// user row names.
		sub2 := "f2sub2-" + uniq
		seedGovernanceAssignment(t, st, types.GovernanceAssignment{SubjectType: types.CapabilitySubjectUser, Subject: "F2SUB2-" + uniq, ProfileID: pUser.ID})
		if name, tier := resolve(t, []string{sub2}, nil); tier == types.CapabilitySubjectUser {
			t.Errorf("an upper-case user row matched the folded sub (%q, %q) — the store folds where the api does not", name, tier)
		}
		seedGovernanceAssignment(t, st, types.GovernanceAssignment{SubjectType: types.CapabilitySubjectUser, Subject: sub2, ProfileID: pUser.ID})
		if name, tier := resolve(t, []string{sub2}, nil); name != pUser.Name || tier != types.CapabilitySubjectUser {
			t.Errorf("folded sub = (%q, %q), want the lowercase user row", name, tier)
		}
	})
}
