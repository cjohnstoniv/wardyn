// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runneragent

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

func (d *Dispatcher) finishMutation(ctx context.Context, method, ref string, runID, generation uuid.UUID, cause error) error {
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	return errors.Join(cause, d.cfg.MutationFinished(finish, method, ref, runID, generation, cause))
}
