// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The governance-changes methods of the authz matrix's store double. Same honest-empty-state posture
// as the governance block it sits beside: no change exists, so every /governance/changes route the
// matrix walks answers as a deployment that never held one would, and the only thing under test on
// those rows is the authorization boundary. The transactions themselves are against a real Postgres
// (governance_changes_pg_test.go).

func (s *authzStore) ProposeGovernanceChange(context.Context, types.GovernanceChange, time.Duration) (types.GovernanceChange, []store.ExpiredGovernanceChange, error) {
	return types.GovernanceChange{}, nil, errors.New("authzStore: no governance changes")
}

func (s *authzStore) ListGovernanceChanges(context.Context, string) ([]types.GovernanceChange, error) {
	return nil, nil
}

func (s *authzStore) GetGovernanceChange(context.Context, uuid.UUID) (types.GovernanceChange, error) {
	return types.GovernanceChange{}, store.ErrNotFound
}

func (s *authzStore) DecideGovernanceChange(context.Context, uuid.UUID, store.GovernanceDecision, store.GovernanceDecideFunc) (types.GovernanceChange, error) {
	return types.GovernanceChange{}, store.ErrNotFound
}

func (s *authzStore) DryRunGovernance(context.Context, func(store.Querier) error) error {
	return errors.New("authzStore: no database")
}
