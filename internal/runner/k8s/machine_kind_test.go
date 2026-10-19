// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A Linux VM run that reaches the Kubernetes driver is refused before any object exists: a Pod
// must never start in a VM's place.
func TestCreateSandboxRefusesALinuxVM(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	before := len(cs.Actions()) // the driver's own start-up canary
	spec := runner.SandboxSpec{RunID: uuid.New(), Image: "img", MachineKind: types.MachineLinuxVM, ConfinementClass: types.CC1}
	if _, err := d.CreateSandbox(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "registered runner") {
		t.Fatalf("err = %v", err)
	}
	for _, a := range cs.Actions()[before:] {
		if a.GetVerb() == "create" {
			t.Fatalf("a refused VM created %s", a.GetResource().Resource)
		}
	}
}
