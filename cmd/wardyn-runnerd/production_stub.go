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

// productionRunnerHooks serves only --substrate folders, which needs no Docker.
func productionRunnerHooks(ctx context.Context, identity runneridentity.Identity, stateDir string, opts serveOptions) (runnerRuntime, error) {
	if opts.substrate == "folders" {
		return foldersRunnerHooks(ctx, identity, stateDir, opts)
	}
	return runnerRuntime{}, errors.New("runner serve requires a build with the docker tag")
}

// dockerArch needs Docker, which this build has not: no architecture is reported.
func dockerArch(context.Context) string { return "" }

// hostMeasure needs Docker, which this build has not: nothing is measured, so every total is asked for.
func hostMeasure(context.Context) (types.RunnerAmounts, error) {
	return types.RunnerAmounts{}, errors.New("measuring this computer requires a build with the docker tag")
}
