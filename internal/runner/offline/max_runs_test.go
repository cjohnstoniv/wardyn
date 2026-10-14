// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// The runner refuses a create past the runs at once the control plane sent with it, counting the runs
// it still holds itself; a run that has exited no longer counts, and a create that sends no limit is not
// held to one.
func TestBeforeCreateRefusesPastTheRunnersMaxRuns(t *testing.T) {
	m, _, _, org := managerFixture(t)
	ctx := context.Background()
	first := createLocal(t, m, org)
	createLocal(t, m, org)
	create := func(maxRuns int) error {
		id := uuid.New()
		if err := m.Lease(ctx, runnerwire.Lease{RunID: id, TokenRenewedAt: org, EndsAt: org.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		return m.BeforeCreate(ctx, runner.SandboxSpec{RunID: id, RunnerMaxRuns: maxRuns})
	}
	err := create(2)
	var refused *runnerwire.Error
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "limit of runs at once (2)") {
		t.Fatalf("a third create on a runner allowed 2 = %v, want the runner's refusal", err)
	}
	if err := create(0); err != nil {
		t.Fatalf("a create with no limit sent = %v, want admitted", err)
	}
	m.mu.Lock()
	r := m.state.Runs[first]
	r.Exit = &runnerwire.AgentExit{Code: 0}
	m.state.Runs[first] = r
	m.mu.Unlock()
	if err := create(3); err != nil {
		t.Fatalf("a create once a run exited = %v, want admitted (two live runs under 3)", err)
	}
}
