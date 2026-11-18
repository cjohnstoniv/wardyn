// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runneragent

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// finishMutation joins the mutation's safety follow-up to its answer. A teardown that is only still stopping
// (runner.ErrTeardownConfirming) whose follow-up also failed (a save, a late disposition, CheckLapses) goes to
// the organisation as internal, the answer it had before that code: it is not only still stopping.
func (d *Dispatcher) finishMutation(ctx context.Context, method, ref string, runID, generation uuid.UUID, cause error) error {
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	after := d.cfg.MutationFinished(finish, method, ref, runID, generation, cause)
	err := errors.Join(cause, after)
	if after != nil && runnerwire.ErrorFor(err).Code == runnerwire.CodeTeardownConfirming {
		return &runnerwire.Error{Code: runnerwire.CodeInternal, Message: err.Error()}
	}
	return err
}
