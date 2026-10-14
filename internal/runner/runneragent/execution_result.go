// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runneragent

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

func (d *Dispatcher) executionResult(ctx context.Context, data json.RawMessage) (runnerwire.ExecutionResult, error) {
	var args runnerwire.ExecutionResultArgs
	if err := runnerwire.Unmarshal(data, &args); err != nil {
		return runnerwire.ExecutionResult{}, err
	}
	ref, err := d.local(args.Ref)
	if err != nil {
		return runnerwire.ExecutionResult{}, err
	}
	if args.Generation == uuid.Nil {
		return runnerwire.ExecutionResult{}, runnerwire.Refuse("execution result requires the issued generation")
	}
	return d.cfg.ExecutionResult(ctx, ref, args.Generation)
}
