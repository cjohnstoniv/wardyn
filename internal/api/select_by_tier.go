// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// selectByTier is the ONE stale-snapshot rule for the two precedence-selected
// answers, the governance ceiling and the user drive (authz design K3). Both
// pick a single winning row by tier, user > group > user_type > all, in one SQL
// ranking (internal/store's subjectPrecedence), and both meet the same problem
// when the caller's group snapshot is missing or truncated: the group tier
// cannot be evaluated, so a lower-tier winner may be one a group row would have
// outranked. ceilingWithUnusableGroups and driveWithUnusableGroups are the
// rule's two entrances, one per target; the refusal is decided and recorded
// only here.
//
// resolveUngrouped is the precedence-select made WITHOUT the unusable groups;
// hasGroupTier reads whether any group-tier row exists in that answer's table;
// stale is the entrance's own groups_snapshot_stale refusal, built there so each
// target stays written at its door.
// The answer is trustworthy in exactly two shapes, and the scoping is the whole
// point (a blanket 403 would lock every pre-0.6 cookie out of every deployment,
// including the ones that never adopted group rows):
//
//   - A USER-TIER row won. An explicitly named principal's answer is fully
//     determined whatever their groups are, INCLUDING a paused drive grant,
//     which is an answer and not an absence. The tier is the resolver's own
//     rather than re-derived, since a user-tier and an all-tier match are
//     otherwise indistinguishable, including when both name the same row.
//   - No group-tier row exists at all. Nothing an unknown group could have
//     matched, so nothing the snapshot could be hiding.
//
// A user_type-tier winner is NOT trustworthy: the type is stamped, not a
// snapshot, so it is matched — but its tier sits below group, so a group row
// could have outranked it, the same as an all-tier winner. Otherwise the
// refusal is the honest answer: with the group tier unreadable, an `all`-tier
// drive grant would win by default and could hand this person a WRITABLE drive
// where their group's row says read-only — a widening decided by alphabetical
// luck.
//
// Fail closed throughout: an error from either read is returned (the caller
// 500s), never read as "no row". hasGroupTier stays a SEPARATE read, made only
// after a non-user-tier answer, because the case that most needs it is the one
// where the resolver matched nothing, and a zero-row result carries no columns
// to piggyback the answer on.
//
// It returns the resolver's own answer unchanged when trustworthy, including
// its store.ErrNotFound, so each entrance keeps its own absent-row doctrine
// (the deployment ceiling, no drive).
func selectByTier[T any](ctx context.Context, s *Server, stale authz.Decision,
	resolveUngrouped func() (T, types.CapabilitySubjectType, error),
	hasGroupTier func(context.Context) (bool, error),
) (T, types.CapabilitySubjectType, error) {
	var zero T
	row, tier, err := resolveUngrouped()
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return zero, "", err
	}
	if err == nil && tier == types.CapabilitySubjectUser {
		return row, tier, nil
	}
	has, herr := hasGroupTier(ctx)
	if herr != nil {
		return zero, "", herr
	}
	if has {
		// Recorded here, the one place the refusal is DECIDED: the write
		// helpers (writeCeilingError, writeCeilingErrorPrefixed,
		// ceilingErrorStatus, writeDriveError) are free functions with no
		// server and no context, and auditing there would mean one emit per
		// seam. Once per request for the ceiling (effectiveCeiling memoizes);
		// the drive resolver is asked once. The target keeps the two decisions
		// apart on the stream (governance.ceiling, runs.drive): a deployment
		// can have group-tier rows in either table without the other.
		//
		// Not for a display read (isDisplayRead): GET /me and the admin preview
		// would otherwise write a denial per console poll for nobody refused.
		if !isDisplayRead(ctx) {
			s.recordRefusal(ctx, nil, stale)
		}
		return zero, "", errGroupsSnapshotStale
	}
	return row, tier, err
}
