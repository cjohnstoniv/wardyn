// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// An organization's claimed runner holds a session through the production Hub, its pending action binds
// to the run's creator and applies before READY, and once revoked it is refused.
func TestPG_OrgRunnerHoldsASession(t *testing.T) {
	f := newAdmissionFixtureOf(t, types.RunnerOwnerOrg)
	if err := f.srv.runnerEndpoint(f.rn.ID).sub.KillSandbox(f.ctx, f.run.SandboxRef); !errors.Is(err, runner.ErrPendingOnRunner) {
		t.Fatalf("offline kill on the org runner = %v, want queued (ErrPendingOnRunner)", err)
	}
	page, _, err := f.st.ListPendingRunnerActions(f.ctx, f.rn.ID)
	if err != nil || len(page) != 1 || page[0].Owner != "org-run-creator" {
		t.Fatalf("queued kill = %+v, %v, want bound to the run's creator", page, err)
	}
	s := f.connect(t, nil, f.liveRow())
	if !f.waitReady(s) {
		t.Fatalf("the org runner never reached READY: %v", s.result())
	}
	s.mu.Lock()
	applied := s.applied
	s.mu.Unlock()
	if len(applied) != 1 || applied[0] != page[0].ID {
		t.Fatalf("the org runner applied %v, want the queued kill", applied)
	}
	s.cancel()
	<-s.done
	if _, err := f.st.RevokeRunner(f.ctx, f.rn.ID, time.Now(), "sec-admin", ""); err != nil {
		t.Fatal(err)
	}
	again := f.connect(t, nil, f.liveRow())
	if f.waitReady(again) || !errors.Is(again.result(), runnerwire.ErrRevoked) {
		t.Fatalf("a revoked org runner reconnected: %v", again.result())
	}
}

// An org runner names no person, so its standing is its own claim and revocation; a person's runner
// still reads its owner's identity (TestSCIMSuspendRefusesTheRunnerUntilReactivated).
func TestRunnerOwnerInactiveOrgRunner(t *testing.T) {
	s := &Server{}
	now := time.Now()
	for name, c := range map[string]struct {
		rn   types.Runner
		want bool
	}{
		"claimed":       {types.Runner{OwnerKind: types.RunnerOwnerOrg, State: types.RunnerClaimed}, false},
		"unclaimed":     {types.Runner{OwnerKind: types.RunnerOwnerOrg, State: types.RunnerUnclaimed}, true},
		"revoked":       {types.Runner{OwnerKind: types.RunnerOwnerOrg, State: types.RunnerRevoked, RevokedAt: &now}, true},
		"revoked stamp": {types.Runner{OwnerKind: types.RunnerOwnerOrg, State: types.RunnerClaimed, RevokedAt: &now}, true},
		"named owner":   {types.Runner{OwnerKind: types.RunnerOwnerOrg, State: types.RunnerClaimed, Owner: "bob"}, true},
	} {
		if got, err := s.runnerOwnerInactive(t.Context(), c.rn); err != nil || got != c.want {
			t.Errorf("%s org runner inactive = %v, %v, want %v", name, got, err, c.want)
		}
	}
}
