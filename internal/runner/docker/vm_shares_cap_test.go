// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// vmShareMounts is n operator folders, one source at n targets.
func vmShareMounts(t *testing.T, n int) []runner.Mount {
	t.Helper()
	src := t.TempDir()
	ms := make([]runner.Mount, n)
	for i := range ms {
		ms[i] = runner.Mount{Source: src, Target: "/workspace/s" + strconv.Itoa(i)}
	}
	return ms
}

// A VM with more shares than teardown removes is refused before anything is started, so no
// virtiofsd outlives its run.
func TestVMRefusesMoreSharesThanItsTeardownRemoves(t *testing.T) {
	r := newVMRig(t)
	spec := vmSpec()
	spec.Mounts = vmShareMounts(t, maxVMShares+1)
	_, err := r.d.CreateSandbox(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "cannot take more than 64 folders and drives; this run has 65") {
		t.Fatalf("CreateSandbox with %d shares = %v, want the share cap refusal", maxVMShares+1, err)
	}
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	if len(r.f.containers) != 0 || len(r.f.networks) != 0 {
		t.Fatalf("a refused VM created %d containers, %d networks", len(r.f.containers), len(r.f.networks))
	}
}

// At the cap a VM is created up to its network relay, whose start fails: the rollback removes every
// share's virtiofsd.
func TestVMRollbackRemovesEveryShareAtTheCap(t *testing.T) {
	r := newVMRig(t)
	r.f.failStartContainer = "wardyn-vmnet-"
	spec := vmSpec()
	spec.Mounts = vmShareMounts(t, maxVMShares)
	if _, err := r.d.CreateSandbox(context.Background(), spec); err == nil {
		t.Fatal("CreateSandbox succeeded past a failed network relay start")
	}
	for n := range maxVMShares {
		if !r.container(t, vmfsName(spec.RunID, n)).removed {
			t.Fatalf("share %d's virtiofsd outlived the rollback", n)
		}
	}
}
