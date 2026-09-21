// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func probeDriveFixture() types.DriveMount {
	return types.DriveMount{
		Backend:    types.DriveBackendHostPath,
		ObjectName: "/srv/nas/homes/bob",
		Target:     runner.DriveTarget,
	}
}

// TestProbeDrive_Readable pins the pass case: the probe container exits 0 (the
// fake models "the agent uid could read and enter the directory") and ProbeDrive
// answers DriveProbeReadable.
func TestProbeDrive_Readable(t *testing.T) {
	f := newFakeDocker()
	f.images[defaultDriveProbeImage] = true
	d := newTestDriver(f)

	probe, err := d.ProbeDrive(context.Background(), probeDriveFixture())
	if err != nil {
		t.Fatalf("ProbeDrive: %v", err)
	}
	if probe.Result != runner.DriveProbeReadable {
		t.Errorf("result = %q, want %q", probe.Result, runner.DriveProbeReadable)
	}
}

// TestProbeDrive_Unreadable is the #165 regression at the driver level: a
// probe that ran AS THE AGENT'S OWN UID and got EACCES (modelled here as a
// nonzero exit, since the fake has no real `test` to run against a bind mount)
// answers DriveProbeUnreadable — the fact an inline root-run os.Stat could
// never establish.
func TestProbeDrive_Unreadable(t *testing.T) {
	f := newFakeDocker()
	f.images[defaultDriveProbeImage] = true
	f.probeExitCode = 1
	d := newTestDriver(f)

	probe, err := d.ProbeDrive(context.Background(), probeDriveFixture())
	if err != nil {
		t.Fatalf("ProbeDrive: %v", err)
	}
	if probe.Result != runner.DriveProbeUnreadable {
		t.Errorf("result = %q, want %q", probe.Result, runner.DriveProbeUnreadable)
	}
}

// TestProbeDrive_RunsAsTheAgentUID pins the property the whole interface
// exists for: the probe container runs as uid 1000, never as this daemon's own
// root process — the gap an inline os.Stat could not close.
func TestProbeDrive_RunsAsTheAgentUID(t *testing.T) {
	f := newFakeDocker()
	f.images[defaultDriveProbeImage] = true
	d := newTestDriver(f)

	if _, err := d.ProbeDrive(context.Background(), probeDriveFixture()); err != nil {
		t.Fatalf("ProbeDrive: %v", err)
	}
	var got *createdContainer
	for name, c := range f.containers {
		if len(name) > len("wardyn-drive-probe-") && name[:len("wardyn-drive-probe-")] == "wardyn-drive-probe-" {
			got = c
		}
	}
	if got == nil {
		t.Fatal("no drive-probe container was created")
	}
	if got.cfg.User != driveProbeUser {
		t.Errorf("probe container User = %q, want %q (the agent uid, never root)", got.cfg.User, driveProbeUser)
	}
	if !got.removed {
		t.Error("the throwaway probe container was not removed")
	}
}

// TestProbeDrive_ManagedBackendAnswersUnknown pins the scope: a docker_volume
// is Docker's own managed object, never chowned away from the default, so
// there is nothing on the host worth a container spin-up to inspect —
// DriveProbeUnknown is the honest answer, never a guessed pass.
func TestProbeDrive_ManagedBackendAnswersUnknown(t *testing.T) {
	f := newFakeDocker()
	d := newTestDriver(f)

	probe, err := d.ProbeDrive(context.Background(), types.DriveMount{
		Backend: types.DriveBackendDockerVolume, ObjectName: "wardyn-drive-bob", Target: runner.DriveTarget,
	})
	if err != nil {
		t.Fatalf("ProbeDrive: %v", err)
	}
	if probe.Result != runner.DriveProbeUnknown {
		t.Errorf("result = %q, want %q", probe.Result, runner.DriveProbeUnknown)
	}
	if len(f.containers) != 0 {
		t.Errorf("a docker_volume probe must not spin up a container, got %d", len(f.containers))
	}
}

// TestProbeDrive_RealDocker is the #165 regression against an ACTUAL daemon,
// skipped unless WARDYN_TEST_DOCKER=1: an inline daemon-side os.Stat (root)
// happily reads a 0700 root-owned directory, and that false "yes" is the whole
// bug. ProbeDrive must refuse it, because it runs the check AS THE AGENT'S OWN
// UID — the one thing this test's daemon-root process cannot fake its way
// around.
func TestProbeDrive_RealDocker(t *testing.T) {
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("set WARDYN_TEST_DOCKER=1 to run the real-Docker drive probe test")
	}
	d := newNetworkTestDriver(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := d.ensureImage(ctx, "busybox:latest", nil); err != nil {
		t.Fatalf("pull busybox: %v", err)
	}

	root := t.TempDir()
	// Both directories are made by a plain (unmodified) busybox container,
	// which runs as root by default — so a bind-mounted mkdir lands root-owned
	// on the HOST, exactly the shape a misconfigured NAS export takes.
	blocked := makeRootOwnedHostDir(ctx, t, d, root, "blocked", "0700")
	open := makeRootOwnedHostDir(ctx, t, d, root, "open", "0755")

	t.Run("a 0700 root-owned directory is refused", func(t *testing.T) {
		probe, err := d.ProbeDrive(ctx, types.DriveMount{
			Backend: types.DriveBackendHostPath, ObjectName: blocked, Target: runner.DriveTarget})
		if err != nil {
			t.Fatalf("ProbeDrive: %v", err)
		}
		if probe.Result != runner.DriveProbeUnreadable {
			t.Errorf("result = %q, want %q for a 0700 root-owned directory (the exact false-pass #165 closes)",
				probe.Result, runner.DriveProbeUnreadable)
		}
	})

	t.Run("a world-readable directory is readable", func(t *testing.T) {
		probe, err := d.ProbeDrive(ctx, types.DriveMount{
			Backend: types.DriveBackendHostPath, ObjectName: open, Target: runner.DriveTarget})
		if err != nil {
			t.Fatalf("ProbeDrive: %v", err)
		}
		if probe.Result != runner.DriveProbeReadable {
			t.Errorf("result = %q, want %q", probe.Result, runner.DriveProbeReadable)
		}
	})
}

// makeRootOwnedHostDir creates hostRoot/name at mode via a one-shot ROOT
// container bind-mounting hostRoot — the real-daemon fixture helper for
// TestProbeDrive_RealDocker.
func makeRootOwnedHostDir(ctx context.Context, t *testing.T, d *Driver, hostRoot, name, mode string) string {
	t.Helper()
	created, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: "wardyn-test-mkdir-" + uuid.New().String(),
		Config: &container.Config{
			Image: "busybox:latest",
			Cmd:   []string{"mkdir", "-m", mode, "/host/" + name},
		},
		HostConfig: &container.HostConfig{
			Mounts: []mount.Mount{{Type: mount.TypeBind, Source: hostRoot, Target: "/host"}},
		},
	})
	if err != nil {
		t.Fatalf("create mkdir fixture: %v", err)
	}
	t.Cleanup(func() {
		_, _ = d.cli.ContainerRemove(context.Background(), created.ID, client.ContainerRemoveOptions{Force: true})
	})
	if _, err := d.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("start mkdir fixture: %v", err)
	}
	wait := d.cli.ContainerWait(ctx, created.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case res := <-wait.Result:
		if res.StatusCode != 0 {
			t.Fatalf("mkdir fixture exited %d", res.StatusCode)
		}
	case werr := <-wait.Error:
		t.Fatalf("wait mkdir fixture: %v", werr)
	case <-ctx.Done():
		t.Fatalf("wait mkdir fixture: %v", ctx.Err())
	}
	return filepath.Join(hostRoot, name)
}
