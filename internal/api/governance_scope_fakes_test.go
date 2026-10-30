// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"slices"
	"sync"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// matrixUnitAdminSub is the authz matrix's unit admin: a member holding one grant, on a unit the
// matrix store keeps no row for, so every route answers past requireUnitScope on its own merits.
const matrixUnitAdminSub = "sub-unit-admin"

// unitScopeDoorSentence is requireUnitScope's refusal, which a caller with a grant never reads.
const unitScopeDoorSentence = "this needs a unit admin or a security admin"

// fakeUnitAdminStore is store.UnitAdminStore over a list of grants and the parent links a test gives
// it (unit to parent), for the tests that need no Postgres.
type fakeUnitAdminStore struct {
	mu      sync.Mutex
	grants  []types.GovernanceUnitAdmin
	parents map[uuid.UUID]uuid.UUID
}

var _ store.UnitAdminStore = (*fakeUnitAdminStore)(nil)

func newMatrixUnitAdmins() *fakeUnitAdminStore {
	return &fakeUnitAdminStore{grants: []types.GovernanceUnitAdmin{{
		ID: uuid.New(), UnitID: uuid.New(), SubjectType: types.CapabilitySubjectUser, Subject: matrixUnitAdminSub, GrantedBy: "sub-admin",
	}}}
}

func (f *fakeUnitAdminStore) names(g types.GovernanceUnitAdmin, users, groups []string) bool {
	if g.SubjectType == types.CapabilitySubjectGroup {
		return slices.Contains(groups, g.Subject)
	}
	return slices.Contains(users, g.Subject)
}

func (f *fakeUnitAdminStore) ListUnitAdmins(context.Context) ([]types.GovernanceUnitAdmin, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.grants), nil
}

func (f *fakeUnitAdminStore) ListUnitAdminOf(_ context.Context, users, groups []string) ([]types.GovernanceUnitAdmin, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []types.GovernanceUnitAdmin
	for _, g := range f.grants {
		if f.names(g, users, groups) {
			out = append(out, g)
		}
	}
	return out, nil
}

func (f *fakeUnitAdminStore) UnitScope(_ context.Context, unit uuid.UUID, users, groups []string, strict bool) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cur, ok := unit, true
	if strict {
		cur, ok = f.parents[unit]
	}
	for n := 0; ok && n < store.MaxUnitDepthCap; n++ {
		if slices.ContainsFunc(f.grants, func(g types.GovernanceUnitAdmin) bool { return g.UnitID == cur && f.names(g, users, groups) }) {
			return true, nil
		}
		cur, ok = f.parents[cur]
	}
	return false, nil
}

func (f *fakeUnitAdminStore) GetUnitAdmin(_ context.Context, unit, id uuid.UUID) (types.GovernanceUnitAdmin, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, g := range f.grants {
		if g.ID == id && g.UnitID == unit {
			return g, nil
		}
	}
	return types.GovernanceUnitAdmin{}, store.ErrNotFound
}
