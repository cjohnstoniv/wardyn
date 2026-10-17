// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !docker

package main

import (
	"context"
	"errors"

	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func productionRunnerHooks(context.Context, runneridentity.Identity, string, serveOptions) (runnerRuntime, error) {
	return runnerRuntime{}, errors.New("runner serve requires a build with the docker tag")
}

// dockerArch needs Docker, which this build has not: no architecture is reported.
func dockerArch(context.Context) string { return "" }

// hostMeasure needs Docker, which this build has not: nothing is measured, so every total is asked for.
func hostMeasure(context.Context) (types.RunnerAmounts, error) {
	return types.RunnerAmounts{}, errors.New("measuring this computer requires a build with the docker tag")
}
