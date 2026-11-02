// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runneragent

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// reclaimingFake is the fake substrate with a disk drive reclaim that answers outcome or err.
type reclaimingFake struct {
	*runnertest.Fake
	asked   []types.DriveMount
	outcome runner.DriveReclaimOutcome
	err     error
}

func (f *reclaimingFake) ReclaimDrive(_ context.Context, m types.DriveMount) (runner.DriveReclaimOutcome, error) {
	f.asked = append(f.asked, m)
	return f.outcome, f.err
}

func TestDispatcherReclaimsOnlyADiskDrive(t *testing.T) {
	a, b := runnerwire.Loopback()
	defer b.Close()
	p := runnerwire.NewPeer(a, runnerwire.PeerConfig{})
	defer p.Close()
	sub := &reclaimingFake{Fake: runnertest.NewFake(), outcome: runner.DriveReclaimDeleted}
	d, err := newTestDispatcher(Config{RunnerID: uuid.NewString(), Substrate: sub,
		BeforeCreate: func(context.Context, runner.SandboxSpec) error { return nil },
		Created:      func(context.Context, runner.SandboxSpec, string) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	call := func(m types.DriveMount) (any, error) {
		data, _ := runnerwire.Marshal(m)
		return d.Call(context.Background(), p, 2, runnerwire.MethodReclaimDrive, data)
	}
	disk := types.DriveMount{Backend: types.DriveBackendMacDisk, ObjectName: "wardyn-drive-x"}
	if res, err := call(disk); err != nil || res != (runnerwire.ReclaimDriveResult{Outcome: runner.DriveReclaimDeleted}) {
		t.Fatalf("a disk drive: %v %v", res, err)
	}
	sub.outcome, sub.err = "", runner.ErrDriveInUse
	if res, err := call(disk); err != nil || res != (runnerwire.ReclaimDriveResult{InUse: true}) {
		t.Fatalf("a disk drive in a run: %v %v", res, err)
	}
	for _, backend := range []types.DriveBackend{types.DriveBackendDockerVolume, types.DriveBackendHostPath, types.DriveBackendK8sPVC, ""} {
		if _, err := call(types.DriveMount{Backend: backend, ObjectName: "wardyn-drive-x"}); err == nil {
			t.Errorf("%q was reclaimed over the wire", backend)
		}
	}
	if len(sub.asked) != 2 {
		t.Fatalf("the substrate was asked %d times, want 2", len(sub.asked))
	}
}
