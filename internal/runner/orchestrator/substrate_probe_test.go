// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package orchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// probingSubstrate is a fakeSubstrate that can prove its own reachability.
type probingSubstrate struct {
	*fakeSubstrate
	state runner.SubstrateState
}

func (p probingSubstrate) ProbeSubstrate(context.Context) runner.SubstrateState { return p.state }

// ProbeSubstrate asks the first substrate that can probe, and takes its word.
func TestOrchestrator_ProbeSubstrate_DelegatesToTheFirstProber(t *testing.T) {
	plain := &fakeSubstrate{name: "plain", classes: []types.ConfinementClass{types.CC1}}
	probing := probingSubstrate{&fakeSubstrate{name: "probing", classes: []types.ConfinementClass{types.CC1}}, runner.SubstrateForbidden}
	if got := New(plain, probing).ProbeSubstrate(context.Background()); got != runner.SubstrateForbidden {
		t.Fatalf("ProbeSubstrate = %q, want the probing substrate's forbidden", got)
	}
}

// deadSubstrate cannot report its classes: its daemon is not answering.
type deadSubstrate struct{ *fakeSubstrate }

func (deadSubstrate) Classes(context.Context) (substrate.ClassSupport, error) {
	return substrate.ClassSupport{}, errors.New("daemon down")
}

// With no probing substrate the answer is whether Capabilities can be read.
func TestOrchestrator_ProbeSubstrate_FallsBackToCapabilities(t *testing.T) {
	ok := New(&fakeSubstrate{name: "plain", classes: []types.ConfinementClass{types.CC1}})
	if got := ok.ProbeSubstrate(context.Background()); got != runner.SubstrateOK {
		t.Fatalf("readable capabilities: ProbeSubstrate = %q, want ok", got)
	}
	dead := New(deadSubstrate{&fakeSubstrate{name: "dead"}})
	if got := dead.ProbeSubstrate(context.Background()); got != runner.SubstrateUnreachable {
		t.Fatalf("capabilities error: ProbeSubstrate = %q, want unreachable", got)
	}
}

// lifecycleSubstrate is a fakeSubstrate that answers the drive lifecycle check.
type lifecycleSubstrate struct {
	*fakeSubstrate
	err error
}

func (l lifecycleSubstrate) CanManageDrives(context.Context) error { return l.err }

// CanManageDrives asks the first substrate that implements the check and takes
// its word, whatever a later one would say; none at all is unavailable.
func TestOrchestrator_CanManageDrives_DelegatesToTheFirstChecker(t *testing.T) {
	plain := &fakeSubstrate{name: "plain", classes: []types.ConfinementClass{types.CC1}}
	denied := lifecycleSubstrate{&fakeSubstrate{name: "first", classes: []types.ConfinementClass{types.CC1}}, runner.ErrDriveLifecycleUnavailable}
	allowed := lifecycleSubstrate{&fakeSubstrate{name: "second", classes: []types.ConfinementClass{types.CC1}}, nil}
	if err := New(plain, denied, allowed).CanManageDrives(context.Background()); !errors.Is(err, runner.ErrDriveLifecycleUnavailable) {
		t.Fatalf("CanManageDrives = %v, want the first checker's denial", err)
	}
	if err := New(plain, allowed, denied).CanManageDrives(context.Background()); err != nil {
		t.Fatalf("CanManageDrives = %v, want the first checker's yes", err)
	}
	if err := New(plain).CanManageDrives(context.Background()); !errors.Is(err, runner.ErrDriveLifecycleUnavailable) {
		t.Fatalf("no checker: CanManageDrives = %v, want ErrDriveLifecycleUnavailable", err)
	}
}
