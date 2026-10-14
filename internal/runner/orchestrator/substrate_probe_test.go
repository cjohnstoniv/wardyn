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

// lifecycleSubstrate is a fakeSubstrate that answers the drive lifecycle check
// but cannot reclaim.
type lifecycleSubstrate struct {
	*fakeSubstrate
	err error
}

func (l lifecycleSubstrate) CanManageDrives(context.Context) error { return l.err }

// reclaimerSubstrate is a fakeSubstrate that can reclaim but not check.
type reclaimerSubstrate struct{ *fakeSubstrate }

func (reclaimerSubstrate) ReclaimDrive(context.Context, types.DriveMount) (runner.DriveReclaimOutcome, error) {
	return runner.DriveReclaimDeleted, nil
}

// fullDriveSubstrate can both reclaim and check, and records which one answered.
type fullDriveSubstrate struct {
	*fakeSubstrate
	err       error
	reclaimed *string
}

func (f fullDriveSubstrate) CanManageDrives(context.Context) error { return f.err }
func (f fullDriveSubstrate) ReclaimDrive(context.Context, types.DriveMount) (runner.DriveReclaimOutcome, error) {
	*f.reclaimed = f.name
	return runner.DriveReclaimDeleted, nil
}

// CanManageDrives asks the substrate ReclaimDrive would use: the first local
// that can do BOTH. A substrate with only one of the two does not count, and
// none at all is unavailable.
func TestOrchestrator_CanManageDrives_AgreesWithReclaimDrive(t *testing.T) {
	cc1 := []types.ConfinementClass{types.CC1}
	var reclaimed string
	checkerOnly := lifecycleSubstrate{&fakeSubstrate{name: "checker-only", classes: cc1}, nil}
	reclaimerOnly := reclaimerSubstrate{&fakeSubstrate{name: "reclaimer-only", classes: cc1}}
	full := fullDriveSubstrate{&fakeSubstrate{name: "full", classes: cc1}, nil, &reclaimed}
	fullDenied := fullDriveSubstrate{&fakeSubstrate{name: "full-denied", classes: cc1}, runner.ErrDriveLifecycleUnavailable, &reclaimed}
	ctx := context.Background()

	o := New(checkerOnly, full)
	if err := o.CanManageDrives(ctx); err != nil {
		t.Fatalf("checker-only before a full substrate: CanManageDrives = %v, want the full substrate's yes", err)
	}
	if _, err := o.ReclaimDrive(ctx, types.DriveMount{}); err != nil || reclaimed != "full" {
		t.Fatalf("ReclaimDrive went to %q (err %v), want the same full substrate", reclaimed, err)
	}
	if err := New(checkerOnly, fullDenied, full).CanManageDrives(ctx); !errors.Is(err, runner.ErrDriveLifecycleUnavailable) {
		t.Fatalf("CanManageDrives = %v, want the first full substrate's denial", err)
	}
	for name, o := range map[string]*Orchestrator{
		"checker without reclaimer": New(checkerOnly),
		"reclaimer without checker": New(reclaimerOnly),
		"reclaimer then checker":    New(reclaimerOnly, checkerOnly),
		"no drive substrate":        New(&fakeSubstrate{name: "plain", classes: cc1}),
	} {
		if err := o.CanManageDrives(ctx); !errors.Is(err, runner.ErrDriveLifecycleUnavailable) {
			t.Errorf("%s: CanManageDrives = %v, want ErrDriveLifecycleUnavailable", name, err)
		}
	}
}
