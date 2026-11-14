// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ErrPolicyStale is returned by UpdatePolicy when the policy was saved after the
// caller read it. It does not wrap ErrConflict, so a stale save is never
// reported as a name collision.
var ErrPolicyStale = errors.New("store: the policy changed after it was read")

// UpdatePolicy replaces a policy's name and spec only while its updated_at still
// equals seen, the updated_at the caller read. A save from an older copy would
// otherwise put back whatever a later save removed, an allowed host among them.
// Every write moves updated_at forward by at least a microsecond, even when the
// database clock has stepped back, so a copy read before a write never matches
// again. A guard miss on a row that exists returns ErrPolicyStale, no row
// returns ErrNotFound, and a rename onto a taken name returns ErrConflict, as
// CreatePolicy does. The caller validates the spec before calling.
func (s PG) UpdatePolicy(ctx context.Context, id uuid.UUID, seen time.Time, name string, spec types.RunPolicySpec) (types.RunPolicy, error) {
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return types.RunPolicy{}, fmt.Errorf("store: marshal policy spec: %w", err)
	}
	const q = `
		UPDATE run_policies
		SET name=$1, spec=$2, updated_at=GREATEST(now(), updated_at + interval '1 microsecond')
		WHERE id=$3 AND updated_at=$4
		RETURNING id, name, created_at, updated_at, spec`
	out, err := scanPolicy(s.Pool.QueryRow(ctx, q, name, specJSON, id, seen))
	switch {
	case errors.Is(err, ErrNotFound):
		// A guard miss and a missing row look the same to the UPDATE.
		if _, gerr := s.GetPolicy(ctx, id); gerr != nil {
			return types.RunPolicy{}, gerr
		}
		return types.RunPolicy{}, ErrPolicyStale
	case isUniqueViolation(err):
		return types.RunPolicy{}, ErrConflict
	}
	return out, err
}
