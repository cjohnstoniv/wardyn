// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

var _ store.ApprovalsOnUnitsPager = (*approvalService)(nil)

// ListPendingApprovalsOnUnits is the unit-scoped read of GET /approvals?decidable_by=me; the api handler
// type-asserts for it on s.cfg.Approvals. Pure delegation, promoted from the embedded store.PG.
func (s *approvalService) ListPendingApprovalsOnUnits(ctx context.Context, units []uuid.UUID) ([]types.ApprovalRequest, error) {
	return s.st.ListPendingApprovalsOnUnits(ctx, units)
}
