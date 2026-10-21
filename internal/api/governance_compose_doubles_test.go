// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// unitChainOf serves GetGovernanceUnitChain from a double's own profile list the way the one
// recursive statement does in Postgres: the profile first, then each base, stopping at three rows or
// at a row with no base. err is the double's own list error, so a double that fails its list read
// fails its chain read too.
func unitChainOf(profiles []types.GovernanceUnit, err error, id uuid.UUID) ([]types.GovernanceUnit, error) {
	if err != nil {
		return nil, err
	}
	byID := unitsByID(profiles)
	var chain []types.GovernanceUnit
	for cur := &id; cur != nil && len(chain) < maxUnitDepth; {
		row, ok := byID[*cur]
		if !ok {
			break
		}
		chain = append(chain, row)
		cur = row.ParentID
	}
	if len(chain) == 0 {
		return nil, store.ErrNotFound
	}
	return chain, nil
}

func (s *authzStore) GetGovernanceUnitChain(context.Context, uuid.UUID) ([]types.GovernanceUnit, error) {
	return nil, store.ErrNotFound
}

func (s *authzStore) WriteGovernanceUnit(_ context.Context, id uuid.UUID, build store.GovernanceUnitBuild) (types.GovernanceUnit, error) {
	p, err := build(nil, nil)
	p.ID = id
	return p, err
}

func (s *profileListStore) GetGovernanceUnitChain(_ context.Context, id uuid.UUID) ([]types.GovernanceUnit, error) {
	return unitChainOf(s.profiles, s.err, id)
}

func (s *reclampStore) GetGovernanceUnitChain(_ context.Context, id uuid.UUID) ([]types.GovernanceUnit, error) {
	return unitChainOf(s.profiles, nil, id)
}

func (s profileErrStore) GetGovernanceUnitChain(ctx context.Context, id uuid.UUID) ([]types.GovernanceUnit, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.reclampStore.GetGovernanceUnitChain(ctx, id)
}

func (s *profileStore) GetGovernanceUnitChain(_ context.Context, id uuid.UUID) ([]types.GovernanceUnit, error) {
	return unitChainOf(s.profiles, nil, id)
}

func (s *pvStore) GetGovernanceUnitChain(_ context.Context, id uuid.UUID) ([]types.GovernanceUnit, error) {
	return unitChainOf(s.profiles, nil, id)
}

func (s *reviveStore) GetGovernanceUnitChain(_ context.Context, id uuid.UUID) ([]types.GovernanceUnit, error) {
	return unitChainOf(s.profiles, nil, id)
}

func (s *sshMemStore) GetGovernanceUnitChain(_ context.Context, id uuid.UUID) ([]types.GovernanceUnit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return unitChainOf(s.profiles, s.profilesErr, id)
}

// resolvedOf is a hand-built standalone profile as the resolver hands it to a reader.
func resolvedOf(p *types.GovernanceUnit) *ResolvedUnit {
	return &ResolvedUnit{ID: p.ID, Name: p.Name, Contact: p.Contact, Ceiling: p.Ceiling, Limits: p.Limits}
}
