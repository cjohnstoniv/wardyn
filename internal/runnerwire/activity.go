// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// CurrentActivity carries a newly admitted live decision, never a spool row or
// a runner timestamp. The org decides whether its receipt updates idle activity.
type CurrentActivity struct {
	RunID uuid.UUID `json:"run_id"`
}

// CurrentActivity sends an ephemeral signal on this connection only. Loss is
// conservative: it is never retained for replay, acknowledged or retried here.
func (p *Peer) CurrentActivity(ctx context.Context, runID uuid.UUID) error {
	if p.cfg.Org || runID == uuid.Nil {
		return fmt.Errorf("%w: activity requires a runner and run", ErrBadFrame)
	}
	payload, err := Marshal(CurrentActivity{RunID: runID})
	if err != nil {
		return err
	}
	return p.send(ctx, Frame{Type: TypeCurrentActivity, Payload: payload})
}
