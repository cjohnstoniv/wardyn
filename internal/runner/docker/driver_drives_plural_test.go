// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// secondVolumeDrive is a managed drive beside the legacy one, at its derived path.
func secondVolumeDrive() types.DriveMount {
	d := *dockerVolumeDrive()
	d.DriveID, d.ObjectName, d.Target, d.ReadOnly = uuid.New(), "wardyn-drive-team-alice", runner.DrivesRoot+"/team", false
	return d
}

func createWithDrives(t *testing.T, f *fakeDocker, runID uuid.UUID, drives ...types.DriveMount) ([]mount.Mount, error) {
	t.Helper()
	f.images["busybox:latest"] = true
	d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev"})
	spec := testSpec()
	spec.RunID = runID
	spec.Drives = drives
	if _, err := d.CreateSandbox(context.Background(), spec); err != nil {
		return nil, err
	}
	return f.containers[agentContainerName(spec.RunID)].host.Mounts, nil
}

// TestCreateSandbox_MountsEveryDrive: two drives, each at its own path, each a
// volume of its own. The one below DrivesRoot is first seeded through the
// image's own drive directory by a container created and removed, never
// started, with no network and the run's labels; the legacy one needs no seed.
func TestCreateSandbox_MountsEveryDrive(t *testing.T) {
	f := newFakeDocker()
	var seeds []string
	f.onCreate = func(name string) {
		if strings.HasPrefix(name, driveSeedPrefix) {
			seeds = append(seeds, name)
		}
	}
	second := secondVolumeDrive()
	mounts, err := createWithDrives(t, f, testSpec().RunID, *dockerVolumeDrive(), second)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []types.DriveMount{*dockerVolumeDrive(), second} {
		m := findMount(mounts, d.Target)
		if m == nil || m.Type != mount.TypeVolume || m.Source != d.ObjectName || m.ReadOnly != d.ReadOnly {
			t.Errorf("drive at %s mounted as %+v, want volume %s ro=%v", d.Target, m, d.ObjectName, d.ReadOnly)
		}
	}
	if len(seeds) != 1 {
		t.Fatalf("seed containers created = %v, want one, for the drive below DrivesRoot only", seeds)
	}
	seed := f.containers[driveSeedName(testSpec().RunID)]
	if seed == nil || !seed.removed || seed.state != nil && seed.state.Running {
		t.Fatalf("seed container = %+v, want one created and removed, never started", seed)
	}
	if m := findMount(seed.host.Mounts, runner.DriveTarget); m == nil || m.Source != second.ObjectName || m.ReadOnly || len(seed.host.Mounts) != 1 {
		t.Errorf("seed mounts = %+v, want only %s writable at %s", seed.host.Mounts, second.ObjectName, runner.DriveTarget)
	}
	if !seed.cfg.NetworkDisabled || seed.host.NetworkMode != "none" {
		t.Errorf("seed network = disabled %v, mode %q; want no network at all", seed.cfg.NetworkDisabled, seed.host.NetworkMode)
	}
	if seed.cfg.Labels[labelRun] != testSpec().RunID.String() || seed.cfg.Labels[labelManaged] != "true" || seed.cfg.Labels[labelComponent] != componentDriveSeed {
		t.Errorf("seed labels = %v, want the run's managed labels", seed.cfg.Labels)
	}
}

// TestCreateSandbox_ConcurrentRunsSeedOneVolume: two runs attaching the same
// drive at once each seed under their own name, so neither refuses the other.
func TestCreateSandbox_ConcurrentRunsSeedOneVolume(t *testing.T) {
	f := newFakeDocker()
	var mu sync.Mutex
	seen := map[string]int{}
	f.onCreate = func(name string) {
		if strings.HasPrefix(name, driveSeedPrefix) {
			mu.Lock()
			seen[name]++
			mu.Unlock()
		}
	}
	f.images["busybox:latest"] = true
	d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev"})
	drive := secondVolumeDrive()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			spec := testSpec()
			spec.RunID = uuid.New()
			spec.Drives = []types.DriveMount{drive}
			_, errs[i] = d.CreateSandbox(context.Background(), spec)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("run %d: %v", i, err)
		}
	}
	if len(seen) != 2 {
		t.Errorf("seed names = %v, want one per run", seen)
	}
}

// TestCreateSandbox_DriveSeedFailureRefuses: a drive that could not be seeded
// refuses the sandbox rather than starting it with an unwritable drive.
func TestCreateSandbox_DriveSeedFailureRefuses(t *testing.T) {
	f := newFakeDocker()
	f.failCreateContainer = driveSeedPrefix
	if _, err := createWithDrives(t, f, testSpec().RunID, secondVolumeDrive()); err == nil || !strings.Contains(err.Error(), "seed user drive volume") {
		t.Fatalf("CreateSandbox = %v, want the seed refusal", err)
	}
}

// TestCreateSandbox_DrivesAtOneOrForeignTargetRefused: two drives at one path
// would hide one; a managed drive under the shared root, or below its own
// folder, is not a path the server derives.
func TestCreateSandbox_DrivesAtOneOrForeignTargetRefused(t *testing.T) {
	twin := secondVolumeDrive()
	twin.Target = runner.DriveTarget
	cases := map[string][]types.DriveMount{
		"two at one target":      {*dockerVolumeDrive(), twin},
		"managed under shared":   {withTarget(secondVolumeDrive(), runner.SharedRoot+"/team")},
		"below its own folder":   {withTarget(secondVolumeDrive(), runner.DrivesRoot+"/team/x")},
		"the drives root itself": {withTarget(secondVolumeDrive(), runner.DrivesRoot)},
	}
	for name, drives := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := createWithDrives(t, newFakeDocker(), testSpec().RunID, drives...)
			if err == nil || !strings.Contains(err.Error(), errDriveTargetInvalid.Error()) {
				t.Fatalf("CreateSandbox = %v, want %v", err, errDriveTargetInvalid)
			}
		})
	}
}

// TestTeardown_RemovesALeftDriveSeed: a seed a crash left behind holds its
// drive's volume; the run's teardown removes it by its run-derived name.
func TestTeardown_RemovesALeftDriveSeed(t *testing.T) {
	f := newFakeDocker()
	id := uuid.New()
	f.containers[driveSeedName(id)] = &createdContainer{name: driveSeedName(id)}
	if err := newTestDriver(f).teardown(context.Background(), agentContainerName(id)); err != nil {
		t.Fatal(err)
	}
	if c := f.containers[driveSeedName(id)]; c != nil && !c.removed {
		t.Errorf("teardown left the drive seed %s", driveSeedName(id))
	}
}

// TestSweepOrphanedSandboxes_RemovesLeftoverDriveSeeds: a seed a crash left
// behind (older than minAge, carrying the seed's managed labels) is removed by
// the orphan sweep, whatever its run's state; a young seed, which a create may
// still be using, is left alone, and so is everything else.
func TestSweepOrphanedSandboxes_RemovesLeftoverDriveSeeds(t *testing.T) {
	f := newFakeDocker()
	oldRun, youngRun, liveRun := uuid.New(), uuid.New(), uuid.New()
	seedLabels := func(id uuid.UUID) map[string]string { return wardynLabels(id, componentDriveSeed, nil) }
	for _, id := range []uuid.UUID{oldRun, youngRun} {
		f.containers[driveSeedName(id)] = &createdContainer{name: driveSeedName(id)}
	}
	seedSandbox(f, liveRun)
	old := time.Now().Add(-time.Hour).Unix()
	f.listItems = []container.Summary{
		{ID: driveSeedName(oldRun), Created: old, Labels: seedLabels(oldRun)},
		{ID: driveSeedName(youngRun), Created: time.Now().Unix(), Labels: seedLabels(youngRun)},
		{ID: agentContainerName(liveRun), Created: old, Labels: wardynLabels(liveRun, componentAgent, nil)},
	}
	swept, err := newTestDriver(f).SweepOrphanedSandboxes(context.Background(), 10*time.Minute, func(uuid.UUID) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if swept != 1 || !f.containers[driveSeedName(oldRun)].removed {
		t.Errorf("swept %d; the aged seed removed = %v, want it removed", swept, f.containers[driveSeedName(oldRun)].removed)
	}
	if f.containers[driveSeedName(youngRun)].removed || f.containers[agentContainerName(liveRun)].removed {
		t.Error("the sweep removed a young seed or a live run's agent")
	}
}

func withTarget(d types.DriveMount, target string) types.DriveMount {
	d.Target = target
	return d
}
