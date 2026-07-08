// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !docker

package main

import (
	"errors"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// newDockerRunner returns an error in the default build: the docker driver is a
// build-tagged add-on (`-tags docker`). Run wardynd with -runner=none for the
// headless API-only control plane, or build with `-tags docker` to wire it.
func newDockerRunner(string, map[types.ConfinementClass]string) (runner.Runner, error) {
	return nil, errors.New("docker runner not compiled in; rebuild wardynd with -tags docker (or use -runner=none)")
}
