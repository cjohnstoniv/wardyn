// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// SetRunNotRecorded writes why the run has no session recording ("" when it is recorded), as its
// dispatch decided it.
func (s PG) SetRunNotRecorded(ctx context.Context, id uuid.UUID, v types.RunNotRecorded) error {
	return s.execRun(ctx, "set run not recorded", `UPDATE agent_runs SET not_recorded=$2 WHERE id=$1`, id, string(v))
}

// SetRunRecordingLevel writes the level the run is recorded at, as its dispatch resolved it.
func (s PG) SetRunRecordingLevel(ctx context.Context, id uuid.UUID, l types.RecordingLevel) error {
	return s.execRun(ctx, "set run recording level", `UPDATE agent_runs SET recording_level=$2 WHERE id=$1`, id, string(l))
}

// SetRunRecordingOff writes a dispatch's off decision in one statement: the level and its
// not_recorded policy_off land together or not at all.
func (s PG) SetRunRecordingOff(ctx context.Context, id uuid.UUID) error {
	return s.execRun(ctx, "set run recording off", `UPDATE agent_runs SET recording_level=$2, not_recorded=$3 WHERE id=$1`,
		id, string(types.RecordingOff), string(types.NotRecordedPolicyOff))
}

// ClearRunDeclined clears not_recorded when it is runner_declined, reporting whether it did: a cast
// the run's runner uploaded proves it recorded.
func (s PG) ClearRunDeclined(ctx context.Context, id uuid.UUID) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `UPDATE agent_runs SET not_recorded = '' WHERE id = $1 AND not_recorded = $2`,
		id, string(types.NotRecordedRunnerDeclined))
	return tag.RowsAffected() == 1, err
}
