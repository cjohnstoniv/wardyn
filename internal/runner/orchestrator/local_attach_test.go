// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package orchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// attachingSubstrate is a fakeSubstrate that can attach locally.
type attachingSubstrate struct {
	*fakeSubstrate
	attached []string
}

func (a *attachingSubstrate) LocalAttach(_ context.Context, ref string, _ runner.LocalAttachRequest) (runner.LocalSession, error) {
	a.rec(&a.attached, ref)
	return nil, nil
}

// A local attach reaches a substrate that serves one; any other answers ErrLocalAttachUnsupported.
func TestOrchestrator_LocalAttach(t *testing.T) {
	ctx := context.Background()
	with := &attachingSubstrate{fakeSubstrate: &fakeSubstrate{name: "docker", classes: []types.ConfinementClass{types.CC1}}}
	if _, err := New(with).LocalAttach(ctx, "wardyn-agent-x", runner.LocalAttachRequest{}); err != nil || len(with.attached) != 1 {
		t.Fatalf("LocalAttach: %v, attached %v", err, with.attached)
	}
	without := New(&fakeSubstrate{name: "docker", classes: []types.ConfinementClass{types.CC1}})
	if _, err := without.LocalAttach(ctx, "wardyn-agent-y", runner.LocalAttachRequest{}); !errors.Is(err, runner.ErrLocalAttachUnsupported) {
		t.Fatalf("LocalAttach on a substrate that cannot = %v, want ErrLocalAttachUnsupported", err)
	}
}
