// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// profileChainOf serves GetGovernanceProfileChain from a double's own profile list the way the one
// recursive statement does in Postgres: the profile first, then each base, stopping at three rows or
// at a row with no base. err is the double's own list error, so a double that fails its list read
// fails its chain read too.
func profileChainOf(profiles []types.GovernanceProfile, err error, id uuid.UUID) ([]types.GovernanceProfile, error) {
	if err != nil {
		return nil, err
	}
	byID := profilesByID(profiles)
	var chain []types.GovernanceProfile
	for cur := &id; cur != nil && len(chain) < maxProfileDepth; {
		row, ok := byID[*cur]
		if !ok {
			break
		}
		chain = append(chain, row)
		cur = row.BaseProfileID
	}
	if len(chain) == 0 {
		return nil, store.ErrNotFound
	}
	return chain, nil
}

func (s *authzStore) GetGovernanceProfileChain(context.Context, uuid.UUID) ([]types.GovernanceProfile, error) {
	return nil, store.ErrNotFound
}

func (s *authzStore) WriteGovernanceProfile(_ context.Context, id uuid.UUID, build store.GovernanceProfileBuild) (types.GovernanceProfile, error) {
	p, err := build(nil)
	p.ID = id
	return p, err
}

func (s *profileListStore) GetGovernanceProfileChain(_ context.Context, id uuid.UUID) ([]types.GovernanceProfile, error) {
	return profileChainOf(s.profiles, s.err, id)
}

func (s *reclampStore) GetGovernanceProfileChain(_ context.Context, id uuid.UUID) ([]types.GovernanceProfile, error) {
	return profileChainOf(s.profiles, nil, id)
}

func (s profileErrStore) GetGovernanceProfileChain(ctx context.Context, id uuid.UUID) ([]types.GovernanceProfile, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.reclampStore.GetGovernanceProfileChain(ctx, id)
}

func (s *profileStore) GetGovernanceProfileChain(_ context.Context, id uuid.UUID) ([]types.GovernanceProfile, error) {
	return profileChainOf(s.profiles, nil, id)
}

func (s *pvStore) GetGovernanceProfileChain(_ context.Context, id uuid.UUID) ([]types.GovernanceProfile, error) {
	return profileChainOf(s.profiles, nil, id)
}

func (s *reviveStore) GetGovernanceProfileChain(_ context.Context, id uuid.UUID) ([]types.GovernanceProfile, error) {
	return profileChainOf(s.profiles, nil, id)
}

func (s *sshMemStore) GetGovernanceProfileChain(_ context.Context, id uuid.UUID) ([]types.GovernanceProfile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return profileChainOf(s.profiles, s.profilesErr, id)
}

// resolvedOf is a hand-built standalone profile as the resolver hands it to a reader.
func resolvedOf(p *types.GovernanceProfile) *ResolvedProfile {
	return &ResolvedProfile{ID: p.ID, Name: p.Name, Contact: p.Contact, Ceiling: p.Ceiling, Limits: p.Limits}
}
