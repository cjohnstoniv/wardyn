// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

func diskTarget(id string, word types.StorageEnforcement, keeps, drives bool) poolTarget {
	return poolTarget{id: id, executor: id, facts: runner.TargetFacts{
		EphemeralDiskEnforcement: word, KeepsEnded: keeps, UserDrives: drives}}
}

// The pool says the WEAKEST word of its targets, unknown when any target's is, and claims keep and drive
// mounts only when every target does.
func TestPoolMainDriveWeakest(t *testing.T) {
	const grace = 30 * time.Minute
	fs, ev, none := types.StorageEnforcementFilesystem, types.StorageEnforcementEviction, types.StorageEnforcementNone
	offline := diskTarget("off", fs, true, true)
	offline.offline = true
	remote, self := types.RunnerPoolRemoteProvided, types.RunnerPoolSelfHosted
	keep := client.MainDriveKeep{Supported: true, GraceSec: 1800}
	for _, tc := range []struct {
		name    string
		targets []poolTarget
		hosting types.RunnerPoolHosting
		grace   time.Duration
		want    client.MainDriveFacts
	}{
		{"filesystem and eviction say eviction", []poolTarget{diskTarget("a", fs, true, true), diskTarget("b", ev, true, true)}, remote, grace,
			client.MainDriveFacts{Enforcement: client.MainDriveEviction, Basis: types.RunEvidenceSubstrate, Coverage: client.MainDriveCoverageScratch, Keep: keep, DriveMounts: true}},
		{"filesystem alone covers all", []poolTarget{diskTarget("a", fs, true, true)}, remote, grace,
			client.MainDriveFacts{Enforcement: client.MainDriveFilesystem, Basis: types.RunEvidenceSubstrate, Coverage: client.MainDriveCoverageAll, Keep: keep, DriveMounts: true}},
		{"one target that caps nothing says none, with no coverage", []poolTarget{diskTarget("a", fs, true, true), diskTarget("b", none, true, true)}, remote, grace,
			client.MainDriveFacts{Enforcement: client.MainDriveNone, Basis: types.RunEvidenceSubstrate, Keep: keep, DriveMounts: true}},
		{"a scheduling request or an outside cap claims nothing", []poolTarget{diskTarget("a", types.StorageEnforcementRequest, true, true)}, remote, grace,
			client.MainDriveFacts{Enforcement: client.MainDriveNone, Basis: types.RunEvidenceSubstrate, Keep: keep, DriveMounts: true}},
		{"an offline target makes it unknown", []poolTarget{diskTarget("a", fs, true, true), offline}, remote, grace,
			client.MainDriveFacts{Enforcement: client.MainDriveUnknown, Basis: types.RunEvidenceSubstrate, DriveMounts: false}},
		{"a target that never reported a word is unknown, not none", []poolTarget{diskTarget("a", fs, true, true), diskTarget("b", "", true, true)}, remote, grace,
			client.MainDriveFacts{Enforcement: client.MainDriveUnknown, Basis: types.RunEvidenceSubstrate, Keep: keep, DriveMounts: true}},
		{"no target at all is unknown", nil, remote, grace,
			client.MainDriveFacts{Enforcement: client.MainDriveUnknown, Basis: types.RunEvidenceSubstrate}},
		{"a self-hosted runner's report is runner_asserted and mounts no drive", []poolTarget{diskTarget("r", fs, true, true)}, self, grace,
			client.MainDriveFacts{Enforcement: client.MainDriveFilesystem, Basis: types.RunEvidenceRunnerAsserted, Coverage: client.MainDriveCoverageAll, Keep: keep}},
		{"grace zero keeps nothing", []poolTarget{diskTarget("a", fs, true, true)}, remote, 0,
			client.MainDriveFacts{Enforcement: client.MainDriveFilesystem, Basis: types.RunEvidenceSubstrate, Coverage: client.MainDriveCoverageAll, DriveMounts: true}},
		{"one target that cannot keep a stopped sandbox (kubernetes) keeps nothing", []poolTarget{diskTarget("d", fs, true, true), diskTarget("k", ev, false, true)}, remote, grace,
			client.MainDriveFacts{Enforcement: client.MainDriveEviction, Basis: types.RunEvidenceSubstrate, Coverage: client.MainDriveCoverageScratch, DriveMounts: true}},
		{"one target that cannot mount a drive mounts none", []poolTarget{diskTarget("a", fs, true, true), diskTarget("b", fs, true, false)}, remote, grace,
			client.MainDriveFacts{Enforcement: client.MainDriveFilesystem, Basis: types.RunEvidenceSubstrate, Coverage: client.MainDriveCoverageAll, Keep: keep}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := poolMainDrive(tc.targets, tc.hosting, tc.grace); !reflect.DeepEqual(*got, tc.want) {
				t.Errorf("got %+v, want %+v", *got, tc.want)
			}
		})
	}
}

// loneDriver is a runner that is no orchestrator: its own capabilities are its executor's facts.
type loneDriver struct {
	*fakeRunner
	disk types.StorageEnforcement
}

func (d loneDriver) Capabilities(ctx context.Context) (runner.Capabilities, error) {
	caps, err := d.fakeRunner.Capabilities(ctx)
	caps.EphemeralDiskEnforcement = d.disk
	return caps, err
}

type endingLoneDriver struct{ loneDriver }

func (endingLoneDriver) EndSandbox(context.Context, string) error { return nil }

// The lone-driver fallback reports the driver's own disk word, whether it keeps an ended sandbox and
// whether it mounts drives, as an orchestrator's executor does.
func TestLoneDriverExecutorFactsCarryMainDriveFacts(t *testing.T) {
	base := loneDriver{fakeRunner: &fakeRunner{}, disk: types.StorageEnforcementFilesystem}
	for name, tc := range map[string]struct {
		r    runner.Runner
		keep bool
	}{"keeps an ended sandbox": {endingLoneDriver{base}, true}, "cannot keep one": {base, false}} {
		s := &Server{cfg: Config{Runner: tc.r}}
		facts, ok := s.executorFacts(context.Background(), "fake")
		if !ok || facts.EphemeralDiskEnforcement != types.StorageEnforcementFilesystem || facts.KeepsEnded != tc.keep || !facts.UserDrives {
			t.Errorf("%s: facts %+v ok=%v", name, facts, ok)
		}
	}
}
