// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"errors"
	"time"
)

// ErrStartCutoff is the answer for a start that reached the runner link's writer at or after its
// start cutoff (WithStartCutoff): nothing was sent, so the runner never saw the start.
var ErrStartCutoff = errors.New("runner: the start's cutoff passed before it was sent")

type startCutoffKey struct{}

// WithStartCutoff sets the time at or after which a start made under ctx must not leave this process. A
// substrate whose starts cross a link (a registered runner) checks it at the link's writer.
func WithStartCutoff(ctx context.Context, by time.Time) context.Context {
	return context.WithValue(ctx, startCutoffKey{}, by)
}

// StartCutoff returns the cutoff WithStartCutoff set.
func StartCutoff(ctx context.Context) (time.Time, bool) {
	by, ok := ctx.Value(startCutoffKey{}).(time.Time)
	return by, ok && !by.IsZero()
}
