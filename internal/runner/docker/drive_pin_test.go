// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestCreateSandbox_RefusesADriveVolumeReplacedWhileStarting: a run judges its drive's volume and the agent
// mounts it by name later. A volume reclaimed and made again under that name for another person in between is
// refused, and no agent container is made over it.
func TestCreateSandbox_RefusesADriveVolumeReplacedWhileStarting(t *testing.T) {
	f := newFakeDocker()
	drive := dockerVolumeDrive()
	swapped := false
	f.onCreate = func(name string) {
		if swapped || strings.HasPrefix(name, "wardyn-proxy-") {
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, ok := f.volumes[drive.ObjectName]; !ok {
			return
		}
		swapped = true
		f.volumes[drive.ObjectName] = client.VolumeCreateOptions{Name: drive.ObjectName, Driver: driveVolumeDriver, Labels: map[string]string{
			labelManaged: "true", labelDrive: uuid.NewString(), labelDriveHome: drive.HomeName,
			labelDriveSubject: types.DriveSubjectHash("bob@example.com"),
		}}
	}
	_, err := createWithDrives(t, f, testSpec().RunID, *drive)
	if !errors.Is(err, errDriveVolumeReplaced) {
		t.Fatalf("CreateSandbox = %v, want %v", err, errDriveVolumeReplaced)
	}
	if !swapped {
		t.Fatal("the volume was never replaced; the test proves nothing")
	}
	if c := f.containers[agentContainerName(testSpec().RunID)]; c != nil {
		t.Errorf("an agent container was made over the replaced volume: %+v", c.host.Mounts)
	}
	if c := f.containers[drivePinName(testSpec().RunID)]; c == nil || !c.removed {
		t.Errorf("the refused run's hold = %+v, want it made and removed", c)
	}
}

// TestCreateSandbox_HoldsDriveVolumesUntilTeardown: a run that mounts a managed drive volume holds it with a
// container of its own, never started, with no network, mounting every such volume read-only and NoCopy, so
// the daemon refuses to remove one until the run's teardown, which removes the hold.
func TestCreateSandbox_HoldsDriveVolumesUntilTeardown(t *testing.T) {
	f := newFakeDocker()
	run := testSpec().RunID
	if _, err := createWithDrives(t, f, run, *dockerVolumeDrive(), secondVolumeDrive()); err != nil {
		t.Fatal(err)
	}
	pin := f.containers[drivePinName(run)]
	if pin == nil || pin.removed || slices.Contains(f.startedNames, drivePinName(run)) {
		t.Fatalf("hold = %+v, want one made and kept, never started", pin)
	}
	if !pin.cfg.NetworkDisabled || pin.host.NetworkMode != "none" {
		t.Errorf("hold network = disabled %v, mode %q; want no network at all", pin.cfg.NetworkDisabled, pin.host.NetworkMode)
	}
	if pin.cfg.Labels[labelRun] != run.String() || pin.cfg.Labels[labelComponent] != componentDrivePin {
		t.Errorf("hold labels = %v, want the run's drive-pin labels", pin.cfg.Labels)
	}
	var held []string
	for _, m := range pin.host.Mounts {
		if !m.ReadOnly || m.VolumeOptions == nil || !m.VolumeOptions.NoCopy {
			t.Errorf("hold mount %+v, want read-only and NoCopy", m)
		}
		held = append(held, m.Source)
	}
	if want := []string{dockerVolumeDrive().ObjectName, secondVolumeDrive().ObjectName}; !slices.Equal(held, want) {
		t.Errorf("hold mounts %v, want %v", held, want)
	}
	if err := newTestDriver(f).teardown(context.Background(), agentContainerName(run)); err != nil {
		t.Fatal(err)
	}
	if !pin.removed {
		t.Error("teardown left the run's hold on its drive volumes")
	}
}

// TestSweepOrphanedSandboxes_ReapsAHoldWithNoAgent: a teardown cut short after the proxy went, or a rollback
// that could not remove it, leaves only the run's hold, which keeps its drive volumes in use. The orphan sweep
// keys a teardown on it, and leaves the hold of a run that is still live.
func TestSweepOrphanedSandboxes_ReapsAHoldWithNoAgent(t *testing.T) {
	gone, live := uuid.New(), uuid.New()
	f := newFakeDocker()
	d := newTestDriver(f)
	old := time.Now().Add(-time.Hour).Unix()
	for _, run := range []uuid.UUID{gone, live} {
		labels := d.labels(run, componentDrivePin, nil)
		f.containers[drivePinName(run)] = &createdContainer{name: drivePinName(run), cfg: &container.Config{Labels: labels}}
		f.listItems = append(f.listItems, container.Summary{ID: drivePinName(run), Created: old, Labels: labels})
	}
	swept, err := d.SweepOrphanedSandboxes(context.Background(), 10*time.Minute, func(id uuid.UUID) bool { return id == gone })
	if err != nil || swept != 1 {
		t.Fatalf("SweepOrphanedSandboxes = %d, %v; want 1, the ended run's hold", swept, err)
	}
	if !f.containers[drivePinName(gone)].removed {
		t.Error("the ended run's hold survived the sweep; its drive volumes stay in use")
	}
	if f.containers[drivePinName(live)].removed {
		t.Error("the sweep removed a live run's hold")
	}
}

// TestDriveSeed_LeavesAContainerThatIsNotThisRunsSeed: a container under a run's drive seed name that the run
// did not make is never removed, by the seed's create or by the run's teardown; the create is refused.
func TestDriveSeed_LeavesAContainerThatIsNotThisRunsSeed(t *testing.T) {
	run := testSpec().RunID
	foreign := func() *createdContainer {
		return &createdContainer{name: driveSeedName(run), cfg: &container.Config{Labels: map[string]string{"app": "someone-else"}}}
	}
	t.Run("create", func(t *testing.T) {
		f := newFakeDocker()
		c := foreign()
		f.containers[driveSeedName(run)] = c
		if _, err := createWithDrives(t, f, run, secondVolumeDrive()); !errors.Is(err, errRunContainerNotOurs) {
			t.Fatalf("CreateSandbox = %v, want %v", err, errRunContainerNotOurs)
		}
		if c.removed {
			t.Error("the seed's create removed a container that is not this run's")
		}
	})
	t.Run("teardown", func(t *testing.T) {
		f := newFakeDocker()
		seedSandbox(f, run)
		c := foreign()
		f.containers[driveSeedName(run)] = c
		if err := newTestDriver(f).teardown(context.Background(), agentContainerName(run)); err != nil {
			t.Fatal(err)
		}
		if c.removed {
			t.Error("teardown removed a container under the seed's name that is not this run's")
		}
	})
}

// TestDrivePin_LiveHoldRefusesAReclaim: on a real daemon a run's hold, created and never started, makes a
// reclaim answer that the volume is in use, and the reclaim goes through once the hold is gone. Skipped unless
// WARDYN_TEST_DOCKER=1.
func TestDrivePin_LiveHoldRefusesAReclaim(t *testing.T) {
	l := newDriveLive(t)
	drive := l.drive(runner.DriveTarget, false, true)
	vol, err := ensureDriveVolume(l.ctx, l.d.cli, drive)
	if err != nil {
		t.Fatal(err)
	}
	run := uuid.New()
	if err := l.d.pinDriveVolumes(l.ctx, run, []judgedVolume{{drive: drive, vol: vol}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.d.dropDrivePin(run) })
	if _, err := l.d.ReclaimDrive(l.ctx, *drive); !errors.Is(err, runner.ErrDriveInUse) {
		t.Fatalf("ReclaimDrive under the hold = %v, want %v", err, runner.ErrDriveInUse)
	}
	l.d.dropDrivePin(run)
	if out, err := l.d.ReclaimDrive(l.ctx, *drive); err != nil || out != runner.DriveReclaimDeleted {
		t.Fatalf("ReclaimDrive once the hold is gone = %q, %v; want %q", out, err, runner.DriveReclaimDeleted)
	}
}
