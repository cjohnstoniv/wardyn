// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// dockerVolumeDrive is a resolved MANAGED user drive, in the shape the control
// plane hands the driver (types.DriveMount — never a runner.Mount).
func dockerVolumeDrive() *types.DriveMount {
	return &types.DriveMount{
		Backend:     types.DriveBackendDockerVolume,
		ObjectName:  "wardyn-drive-alice",
		HomeName:    "alice",
		Target:      runner.DriveTarget,
		ReadOnly:    true,
		Enforcement: types.StorageEnforcementNone,
	}
}

// createWithDrive runs CreateSandbox for a spec carrying drive, with the given
// deployment host-root ceiling on the driver's config, and returns the fake
// daemon (so a test can assert volume state) plus the agent's applied mounts.
func createWithDrive(t *testing.T, drive *types.DriveMount, hostRoots []string) (*fakeDocker, []mount.Mount, error) {
	t.Helper()
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev", UserDriveHostRoots: hostRoots})

	spec := testSpec()
	spec.Drive = drive
	_, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		return f, nil, err
	}
	agent := f.containers[agentContainerName(spec.RunID)]
	if agent == nil {
		t.Fatalf("agent container not created")
	}
	return f, agent.host.Mounts, nil
}

// findMount returns the applied mount at target, or nil.
func findMount(mounts []mount.Mount, target string) *mount.Mount {
	for i := range mounts {
		if mounts[i].Target == target {
			return &mounts[i]
		}
	}
	return nil
}

// TestDriveVolume_CreatedWithLabelsAndNoOptions is the whole managed-drive
// contract in one place: the volume is created by NAME on the local driver,
// carries the drive/home labels an operator reclaims by, carries NO run label
// (teardown must never reap a member's persistent storage), and — the security
// half — carries NO DriverOpts, which is what keeps a share credential from
// ever reaching `docker volume inspect`.
func TestDriveVolume_CreatedWithLabelsAndNoOptions(t *testing.T) {
	f, mounts, err := createWithDrive(t, dockerVolumeDrive(), nil)
	if err != nil {
		t.Fatalf("CreateSandbox with a docker_volume drive: %v", err)
	}

	opts, ok := f.volumes["wardyn-drive-alice"]
	if !ok {
		t.Fatalf("drive volume was not created; volumes=%v", f.volumes)
	}
	if opts.Driver != "local" {
		t.Errorf("volume Driver = %q, want %q", opts.Driver, "local")
	}
	if len(opts.DriverOpts) != 0 {
		t.Errorf("volume DriverOpts = %v, want none — driver options are how a CIFS/NFS password leaks into `docker volume inspect`", opts.DriverOpts)
	}
	if opts.Labels[labelDrive] != "wardyn-drive-alice" {
		t.Errorf("label %s = %q, want the drive's object name", labelDrive, opts.Labels[labelDrive])
	}
	if opts.Labels[labelDriveHome] != "alice" {
		t.Errorf("label %s = %q, want %q", labelDriveHome, opts.Labels[labelDriveHome], "alice")
	}
	if _, has := opts.Labels[labelRun]; has {
		t.Errorf("drive volume carries a %s label (%v) — teardown reaps by that label and would delete the member's storage", labelRun, opts.Labels)
	}

	m := findMount(mounts, runner.DriveTarget)
	if m == nil {
		t.Fatalf("drive not mounted at %s; mounts=%+v", runner.DriveTarget, mounts)
	}
	if m.Type != mount.TypeVolume {
		t.Errorf("drive mount Type = %q, want %q (a managed drive has no host path)", m.Type, mount.TypeVolume)
	}
	if m.Source != "wardyn-drive-alice" {
		t.Errorf("drive mount Source = %q, want the volume name", m.Source)
	}
	if !m.ReadOnly {
		t.Errorf("drive mount ReadOnly = false, want true (the resolved allocation was read-only)")
	}
}

// TestDriveVolume_WritableDriveMountsWritable: ReadOnly comes from the resolved
// mount, so an allocation an admin made writable is bound writable.
func TestDriveVolume_WritableDriveMountsWritable(t *testing.T) {
	drive := dockerVolumeDrive()
	drive.ReadOnly = false
	_, mounts, err := createWithDrive(t, drive, nil)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	m := findMount(mounts, runner.DriveTarget)
	if m == nil {
		t.Fatalf("drive not mounted; mounts=%+v", mounts)
	}
	if m.ReadOnly {
		t.Errorf("drive mount ReadOnly = true, want false (the allocation is writable)")
	}
}

// TestEnsureDriveVolume_IdempotentByName: the SECOND run for the same person
// must not create anything. Idempotence is the contract (a drive is created on
// first use and lived in thereafter), not merely the end state.
func TestEnsureDriveVolume_IdempotentByName(t *testing.T) {
	f := newFakeDocker()
	drive := dockerVolumeDrive()

	if err := ensureDriveVolume(context.Background(), f, drive); err != nil {
		t.Fatalf("first ensureDriveVolume: %v", err)
	}
	if f.volumeCreates != 1 {
		t.Fatalf("first call made %d VolumeCreate calls, want 1", f.volumeCreates)
	}
	if err := ensureDriveVolume(context.Background(), f, drive); err != nil {
		t.Fatalf("second ensureDriveVolume: %v", err)
	}
	if f.volumeCreates != 1 {
		t.Errorf("second call made another VolumeCreate (total %d) — an existing drive must be inspected, not re-created", f.volumeCreates)
	}
}

// TestEnsureDriveVolume_AdoptsOnlyWardynsOwnShape is the credential guardrail's
// second half. The first half is "Wardyn never CREATES a volume with driver
// options"; this is "Wardyn never MOUNTS one it did not create". The two are
// separable, and only asserting the first left the gap: `docker volume create
// --opt type=cifs --opt o=…,password=… wardyn-drive-alice` is a supported
// operator gesture, and an inspect-hit arm that returned nil on any name match
// would bind that share — password and all — into whichever member resolves to
// that object name.
//
// A matching volume (local driver, no options) is still REUSED, which is the
// ordinary second-run path and must not become a refusal.
func TestEnsureDriveVolume_AdoptsOnlyWardynsOwnShape(t *testing.T) {
	seed := func(opts client.VolumeCreateOptions) *fakeDocker {
		f := newFakeDocker()
		f.volumes["wardyn-drive-alice"] = opts
		return f
	}

	t.Run("foreign-driver", func(t *testing.T) {
		f := seed(client.VolumeCreateOptions{Name: "wardyn-drive-alice", Driver: "some-csi-plugin"})
		err := ensureDriveVolume(context.Background(), f, dockerVolumeDrive())
		if err == nil {
			t.Fatal("a same-named volume on a FOREIGN driver must be refused, not adopted as a managed drive")
		}
		if !strings.Contains(err.Error(), "some-csi-plugin") {
			t.Errorf("the refusal should name the driver it found, got: %v", err)
		}
	})

	t.Run("options-bearing", func(t *testing.T) {
		// The dangerous one: the local driver, but carrying share options. The
		// password is deliberately NOT asserted on — the point is that Wardyn
		// refuses before it can ever mount it.
		f := seed(client.VolumeCreateOptions{
			Name:       "wardyn-drive-alice",
			Driver:     "local",
			DriverOpts: map[string]string{"type": "cifs", "device": "//nas.corp/share", "o": "username=svc,password=hunter2"},
		})
		if err := ensureDriveVolume(context.Background(), f, dockerVolumeDrive()); err == nil {
			t.Fatal("a same-named volume carrying DRIVER OPTIONS must be refused — adopting it would mount an operator's share on a credential Wardyn never chose")
		}
	})

	t.Run("wardyn-shaped-is-reused", func(t *testing.T) {
		f := seed(client.VolumeCreateOptions{
			Name:   "wardyn-drive-alice",
			Driver: "local",
			Labels: map[string]string{labelManaged: "true", labelDrive: "wardyn-drive-alice", labelDriveHome: "alice"},
		})
		if err := ensureDriveVolume(context.Background(), f, dockerVolumeDrive()); err != nil {
			t.Fatalf("a Wardyn-shaped volume must be REUSED, not refused: %v", err)
		}
		if f.volumeCreates != 0 {
			t.Errorf("reuse made %d VolumeCreate calls, want 0", f.volumeCreates)
		}
	})
}

// TestCreateSandbox_HostPathDriveAllocatesNothing: only docker_volume allocates.
// A host_path drive is a bind of a tree the operator already mounted, and Wardyn
// never mkdirs on a share — asserted through the real dispatch path rather than
// against a backend guard inside ensureDriveVolume, which its single caller
// (driveMount's docker_volume arm) makes unreachable.
func TestCreateSandbox_HostPathDriveAllocatesNothing(t *testing.T) {
	root, home := driveHostRoot(t)
	f, mounts, err := createWithDrive(t, hostPathDrive(home), []string{root})
	if err != nil {
		t.Fatalf("CreateSandbox with an in-root host_path drive: %v", err)
	}
	if f.volumeCreates != 0 {
		t.Errorf("a host_path drive made %d VolumeCreate calls, want 0", f.volumeCreates)
	}
	// (the bind's source/mode are asserted by TestCreateSandbox_HostPathDriveApplied)
	if m := findMount(mounts, runner.DriveTarget); m != nil && m.Type != mount.TypeBind {
		t.Errorf("host_path drive mount Type = %q, want %q — a share is bound, never allocated", m.Type, mount.TypeBind)
	}
}

// TestEnsureDriveVolume_FailsClosed: an unreadable daemon, a refused create and
// an empty object name each abort rather than proceed. The empty-name arm is
// the sharpest: VolumeCreate with no Name allocates an ANONYMOUS volume, which
// would be storage that is neither the member's drive nor reclaimable by name.
func TestEnsureDriveVolume_FailsClosed(t *testing.T) {
	t.Run("inspect-error", func(t *testing.T) {
		f := newFakeDocker()
		f.failVolumeInspect = true
		err := ensureDriveVolume(context.Background(), f, dockerVolumeDrive())
		if err == nil {
			t.Fatal("an unreadable daemon must fail closed, got nil")
		}
		if f.volumeCreates != 0 {
			t.Errorf("created a volume over an unreadable daemon (%d calls)", f.volumeCreates)
		}
	})
	t.Run("create-error", func(t *testing.T) {
		f := newFakeDocker()
		f.failVolumeCreate = true
		if err := ensureDriveVolume(context.Background(), f, dockerVolumeDrive()); err == nil {
			t.Fatal("a refused VolumeCreate must fail closed, got nil")
		}
	})
	t.Run("empty-object-name", func(t *testing.T) {
		f := newFakeDocker()
		drive := dockerVolumeDrive()
		drive.ObjectName = ""
		err := ensureDriveVolume(context.Background(), f, drive)
		if err == nil {
			t.Fatal("an empty object name must fail closed (it would create an anonymous volume), got nil")
		}
		if f.volumeCreates != 0 {
			t.Errorf("made %d VolumeCreate calls for an unnamed drive, want 0", f.volumeCreates)
		}
	})
}

// TestCreateSandbox_DriveVolumeCreateFailureAbortsSandbox: a drive that cannot
// be allocated fails the whole CreateSandbox closed. Coming up WITHOUT the
// drive would hand the member a sandbox whose /home/agent/drive is an ordinary
// container directory — writable, empty, and gone at teardown.
func TestCreateSandbox_DriveVolumeCreateFailureAbortsSandbox(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	f.failVolumeCreate = true
	d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev"})

	spec := testSpec()
	spec.Drive = dockerVolumeDrive()
	if _, err := d.CreateSandbox(context.Background(), spec); err == nil {
		t.Fatal("CreateSandbox must fail closed when the drive volume cannot be created, got nil")
	}
	if f.containers[agentContainerName(spec.RunID)] != nil {
		t.Error("agent container exists after a failed drive allocation — the refusal must precede ContainerCreate")
	}
}

// TestCreateSandbox_NoDriveTouchesNothing: the overwhelmingly common run (no
// drive) must be byte-identical to before this feature — no volume calls, no
// mount at the reserved target.
func TestCreateSandbox_NoDriveTouchesNothing(t *testing.T) {
	f, mounts, err := createWithDrive(t, nil, nil)
	if err != nil {
		t.Fatalf("CreateSandbox without a drive: %v", err)
	}
	if f.volumeCreates != 0 {
		t.Errorf("a run with no drive made %d VolumeCreate calls, want 0", f.volumeCreates)
	}
	if m := findMount(mounts, runner.DriveTarget); m != nil {
		t.Errorf("a run with no drive got a mount at %s: %+v", runner.DriveTarget, *m)
	}
}

// TestCreateSandbox_K8sBackedDriveIsAnError: a k8s backend on the Docker driver
// is refused, never silently skipped. types.ValidateUserDrive refuses the
// combination at the write boundary, so reaching here means a stored row
// outlived a change of runner target — and a writable drive that silently did
// not mount would let a session's work land in a directory that dies with the
// run.
func TestCreateSandbox_K8sBackedDriveIsAnError(t *testing.T) {
	for _, backend := range []types.DriveBackend{types.DriveBackendK8sPVC, types.DriveBackendK8sPVCStatic} {
		t.Run(string(backend), func(t *testing.T) {
			drive := dockerVolumeDrive()
			drive.Backend = backend
			f, _, err := createWithDrive(t, drive, nil)
			if err == nil {
				t.Fatalf("a %s drive on the docker runner must FAIL CLOSED, got nil", backend)
			}
			if !strings.Contains(err.Error(), string(backend)) {
				t.Errorf("the error should name the backend it cannot mount, got: %v", err)
			}
			if f.containers[agentContainerName(testSpec().RunID)] != nil {
				t.Error("agent container exists after the refusal")
			}
		})
	}
}
