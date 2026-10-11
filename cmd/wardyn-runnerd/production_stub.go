// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !docker

package main

import (
	"context"
	"errors"

	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
)

func productionRunnerHooks(context.Context, runneridentity.Identity, string, serveOptions) (runnerRuntime, error) {
	return runnerRuntime{}, errors.New("runner serve requires a build with the docker tag")
}
