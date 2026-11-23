// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The guard runs before the client is touched, for every machine kind, so a nil client proves no side effect.
func TestCreateSandboxRefusesExtraContainers(t *testing.T) {
	for _, kind := range []types.MachineKind{"", "linux_vm", "macos_vm"} {
		t.Run(string(kind), func(t *testing.T) {
			d := &Driver{}
			_, err := d.CreateSandbox(context.Background(), runner.SandboxSpec{
				RunID:       uuid.New(),
				Image:       "img",
				MachineKind: kind,
				Containers:  []runner.ExtraContainer{{Name: "db", Image: "db@sha256:abc"}},
			})
			if !errors.Is(err, runner.ErrExtraContainersUnsupported) {
				t.Fatalf("err = %v, want ErrExtraContainersUnsupported", err)
			}
		})
	}
}
