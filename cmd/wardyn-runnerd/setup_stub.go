// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !docker

package main

import (
	"context"
	"errors"
)

// prepareSetupDocker needs Docker, which this build has not: setup cannot tell the engine is usable.
func prepareSetupDocker(context.Context, string) (bool, error) {
	return false, errors.New("setup requires a build with the docker tag")
}
