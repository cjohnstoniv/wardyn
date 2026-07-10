// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !docker

package main

import (
	"errors"

	"github.com/cjohnstoniv/wardyn/internal/api"
)

// newEnvBuilder fails closed in the default build: devcontainer builds require
// the docker driver (`-tags docker`). This makes `-envbuild` on a non-docker
// build a boot-time error rather than a silent no-op.
func newEnvBuilder(string, string) (api.ImageBuilder, error) {
	return nil, errors.New("devcontainer builds not compiled in; rebuild wardynd with -tags docker")
}
