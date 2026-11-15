// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"errors"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
)

func TestNewRunner_RefusesWarmPools(t *testing.T) {
	t.Setenv("WARDYN_K8S_NAMESPACE", "")
	t.Setenv("WARDYN_K8S_WARM_POOLS", "[]")
	applied := false
	_, err := NewRunner(substrate.Deps{}, func(*Config) { applied = true })
	if !errors.Is(err, errWarmPoolsRunner) {
		t.Fatalf("NewRunner = %v, want errWarmPoolsRunner", err)
	}
	if applied {
		t.Error("the runner's own fields were applied before the refusal")
	}
}
