// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"errors"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

func validateRetirementState(r localRun) error {
	p := r.Retirement
	if p == nil {
		if r.Retired {
			return errors.New("retired run lacks settlement proof")
		}
		return nil
	}
	if p.ID == uuid.Nil || p.Spool.SpoolID == uuid.Nil || p.OutputEnd < 0 || p.Generation != r.Generation || p.ExecID != r.ExecID || p.ExitHash != exitDigest(r.Exit) || (r.Retired && r.Quarantine != nil) {
		return errors.New("retirement settlement identity mismatch")
	}
	if r.Containment == nil || r.Containment.ID == uuid.Nil || r.Containment.Generation != r.Generation || r.Containment.Operation != string(types.RunnerActionKill) || !r.Containment.Contained || r.ProxyStoppedAt == nil {
		return errors.New("retirement lacks settled terminal containment")
	}
	if (r.Generation == uuid.Nil && len(r.Executions) != 0) || (r.Exit != nil && !r.ExitAcked) || (r.MutationFailure != nil && !r.MutationFailure.Contained) {
		return errors.New("retirement hides unsettled execution evidence")
	}
	return nil
}
