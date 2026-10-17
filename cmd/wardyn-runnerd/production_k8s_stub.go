// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && !k8s

package main

import (
	"context"
	"errors"

	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
)

func k8sRunnerHooks(context.Context, runneridentity.Identity, string, serveOptions) (runnerRuntime, error) {
	return runnerRuntime{}, errors.New("--substrate=k8s requires a build with the k8s tag")
}
