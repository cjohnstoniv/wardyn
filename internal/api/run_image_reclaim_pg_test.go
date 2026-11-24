// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// imageKillRunner confirms a sandbox kill and records the images it is asked to remove.
type imageKillRunner struct {
	*killCountingRunner
	imu     sync.Mutex
	removed []string
}

func (r *imageKillRunner) ImageRemove(_ context.Context, ref string) error {
	r.imu.Lock()
	defer r.imu.Unlock()
	r.removed = append(r.removed, ref)
	return nil
}

// TestPG_KillRun_ReclaimsTheRunsOwnImage: a kill the runner confirms clears the run's sandbox ref, and the image
// tag built for that run alone is then removed, once.
func TestPG_KillRun_ReclaimsTheRunsOwnImage(t *testing.T) {
	f := newLifetimeFixture(t)
	run := f.run(t, types.ExperienceInteractive, nil, nil)
	tag := "wardyn-byoi/" + run.ID.String() + ":latest"
	if err := f.st.SetRunImage(context.Background(), run.ID, tag); err != nil {
		t.Fatalf("set the run's image: %v", err)
	}
	rr := &imageKillRunner{killCountingRunner: f.rn}
	f.srv.cfg.Runner = rr

	applied, _, err := f.srv.killRunCascade(context.Background(), f.get(t, run.ID), types.ActorHuman, "owner@example.com", nil)
	if err != nil || !applied {
		t.Fatalf("killRunCascade = %v, %v; want the kill applied", applied, err)
	}
	f.srv.WaitBackground()

	rr.imu.Lock()
	got := slices.Clone(rr.removed)
	rr.imu.Unlock()
	if !slices.Equal(got, []string{tag}) {
		t.Errorf("removed %v; want [%s] once", got, tag)
	}
	if after := f.get(t, run.ID); after.State != types.RunKilled || after.SandboxRef != "" {
		t.Errorf("run = %s with sandbox_ref %q; want KILLED with the ref cleared", after.State, after.SandboxRef)
	}
}
