// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Measurer is a substrate that can say how much it has: CPU, memory and disk for runs. An amount it
// cannot measure is 0, never a guess. The server proposes a remote-hosted runner's limits from it once,
// and shows it beside the limits as what the runner reports. On an error the amounts are whatever it
// could still measure, and the error says which read it could not make.
type Measurer interface {
	Measure(ctx context.Context) (types.RunnerAmounts, error)
}

// ExecutorMeasurer measures one of a deployment's configured executors by name.
type ExecutorMeasurer interface {
	MeasureExecutor(ctx context.Context, executor string) (types.RunnerAmounts, error)
}
