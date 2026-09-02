// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/mount"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// helper: run CreateSandbox with the given mounts and return the agent's applied
// bind mounts (excluding the recording mount, which is off in these tests) plus
// any error.
func createWithMounts(t *testing.T, mounts []runner.Mount) ([]mount.Mount, error) {
	t.Helper()
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newTestDriver(f)

	spec := testSpec()
	spec.Mounts = mounts
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		return nil, err
	}
	agent := f.containers[agentContainerName(spec.RunID)]
	if agent == nil {
		t.Fatalf("agent container not created (ref=%s)", sb.Ref)
	}
	return agent.host.Mounts, nil
}

// TestCreateSandbox_AllowedMountApplied: an allowed (absolute, non-dangerous
// source; allowed target prefix) mount is applied as a bind mount with the
// requested read-only flag.
func TestCreateSandbox_AllowedMountApplied(t *testing.T) {
	got, err := createWithMounts(t, []runner.Mount{
		{Source: "/home/maintainer/repo", Target: "/home/agent/work", ReadOnly: false},
	})
	if err != nil {
		t.Fatalf("CreateSandbox with allowed mount failed: %v", err)
	}
	var found *mount.Mount
	for i := range got {
		if got[i].Target == "/home/agent/work" {
			found = &got[i]
		}
	}
	if found == nil {
		t.Fatalf("allowed mount not applied; agent mounts = %+v", got)
	}
	if found.Type != mount.TypeBind {
		t.Errorf("mount Type = %q, want bind", found.Type)
	}
	if found.Source != "/home/maintainer/repo" {
		t.Errorf("mount Source = %q", found.Source)
	}
	if found.ReadOnly {
		t.Errorf("mount ReadOnly = true, want false (policy opted into RW)")
	}
}

// TestCreateSandbox_MountDefaultReadOnly: when the spec's mount ReadOnly is true
// (the resolved default for an omitted policy field), the bind is read-only.
func TestCreateSandbox_MountDefaultReadOnly(t *testing.T) {
	got, err := createWithMounts(t, []runner.Mount{
		{Source: "/srv/data", Target: "/work/data", ReadOnly: true},
	})
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	for _, m := range got {
		if m.Target == "/work/data" {
			if !m.ReadOnly {
				t.Errorf("default mount should be ReadOnly=true, got false")
			}
			return
		}
	}
	t.Fatalf("mount /work/data not applied; got %+v", got)
}

// ITEM 34(a): the recording-mount TARGET must never be chmod 0777'd for a
// host-bind RecordingMount (that would make the operator's HOST directory
// world-writable). CastDir is always prepared; a named-volume target still is.
func TestRecordingChmodDirs_NeverLoosensHostBindRoot(t *testing.T) {
	hostBind := recordingChmodDirs(Config{RecordingMount: "/host/recordings"})
	if slices.Contains(hostBind, RecordingMountTarget) {
		t.Errorf("host-bind RecordingMount: target %q must NOT be chmod 0777'd (host world-writable); got %v", RecordingMountTarget, hostBind)
	}
	if !slices.Contains(hostBind, defaultCastDir) {
		t.Errorf("CastDir must always be prepared; got %v", hostBind)
	}

	vol := recordingChmodDirs(Config{RecordingMount: "wardyn-rec-vol"})
	if !slices.Contains(vol, RecordingMountTarget) {
		t.Errorf("named-volume RecordingMount: target must be prepared (Docker-managed); got %v", vol)
	}

	none := recordingChmodDirs(Config{})
	if len(none) != 1 || none[0] != defaultCastDir {
		t.Errorf("no RecordingMount => only CastDir; got %v", none)
	}
}

// ITEM 34(b): a host-path RecordingMount source is run through the same deny-list
// as workspace binds and FAILS CLOSED; a named volume is not a host path and is
// not deny-listed; a valid host path is applied as a bind.
func TestCreateSandbox_RecordingMountHostBindValidated(t *testing.T) {
	for _, src := range []string{"/", "/etc", "/proc", "/var/run/docker.sock", "/var/lib/docker"} {
		f := newFakeDocker()
		f.images["busybox:latest"] = true
		d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev", RecordingMount: src})
		if _, err := d.CreateSandbox(context.Background(), testSpec()); err == nil {
			t.Errorf("denied RecordingMount source %q must FAIL CLOSED, got nil", src)
		} else if !strings.Contains(err.Error(), "denied recording mount") {
			t.Errorf("RecordingMount %q: error should identify the denied recording mount, got %v", src, err)
		}
	}

	// Valid host-path RecordingMount: applied as a bind at RecordingMountTarget.
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev", RecordingMount: "/srv/wardyn/recordings"})
	if _, err := d.CreateSandbox(context.Background(), testSpec()); err != nil {
		t.Fatalf("valid host-bind RecordingMount must succeed, got %v", err)
	}
	agent := f.containers[agentContainerName(testSpec().RunID)]
	var found bool
	for _, m := range agent.host.Mounts {
		if m.Target == RecordingMountTarget && m.Type == mount.TypeBind && m.Source == "/srv/wardyn/recordings" {
			found = true
		}
	}
	if !found {
		t.Errorf("valid host-bind RecordingMount not applied as bind; mounts=%+v", agent.host.Mounts)
	}

	// Named-volume RecordingMount: not a host path -> not deny-listed, succeeds.
	f2 := newFakeDocker()
	f2.images["busybox:latest"] = true
	d2 := newWithClient(f2, Config{ProxyImage: "wardyn-proxy:dev", RecordingMount: "wardyn-rec-vol"})
	if _, err := d2.CreateSandbox(context.Background(), testSpec()); err != nil {
		t.Fatalf("named-volume RecordingMount must not be deny-listed, got %v", err)
	}
}

// TestCreateSandbox_DeniedMountsRejected runs the full deny-list matrix: each
// dangerous Source (and a bad Target) must FAIL the CreateSandbox closed (the
// driver defense-in-depth re-check), so a bad mount can never reach Docker even
// if it somehow got past policy validation.
func TestCreateSandbox_DeniedMountsRejected(t *testing.T) {
	cases := []struct {
		name string
		m    runner.Mount
	}{
		{"root", runner.Mount{Source: "/", Target: "/home/agent/x"}},
		{"proc", runner.Mount{Source: "/proc", Target: "/home/agent/x"}},
		{"proc-sub", runner.Mount{Source: "/proc/sys", Target: "/home/agent/x"}},
		{"sys", runner.Mount{Source: "/sys", Target: "/home/agent/x"}},
		{"sys-cgroup", runner.Mount{Source: "/sys/fs/cgroup", Target: "/work/x"}},
		{"dev", runner.Mount{Source: "/dev", Target: "/home/agent/x"}},
		{"dev-mem", runner.Mount{Source: "/dev/mem", Target: "/work/x"}},
		{"run", runner.Mount{Source: "/run", Target: "/home/agent/x"}},
		{"var-run", runner.Mount{Source: "/var/run", Target: "/home/agent/x"}},
		{"var-lib-docker", runner.Mount{Source: "/var/lib/docker", Target: "/work/x"}},
		{"docker-sock-varrun", runner.Mount{Source: "/var/run/docker.sock", Target: "/work/x"}},
		{"docker-sock-anywhere", runner.Mount{Source: "/home/u/docker.sock", Target: "/work/x"}},
		{"etc", runner.Mount{Source: "/etc", Target: "/home/agent/x"}},
		{"etc-sub", runner.Mount{Source: "/etc/shadow", Target: "/work/x"}},
		{"boot", runner.Mount{Source: "/boot", Target: "/home/agent/x"}},
		{"root-home", runner.Mount{Source: "/root", Target: "/home/agent/x"}},
		{"root-home-sub", runner.Mount{Source: "/root/.ssh", Target: "/work/x"}},
		{"relative-source", runner.Mount{Source: "relative/path", Target: "/work/x"}},
		{"traversal-source", runner.Mount{Source: "/home/u/../../etc", Target: "/work/x"}},
		{"uncleaned-trailing-slash", runner.Mount{Source: "/home/u/repo/", Target: "/work/x"}},
		{"empty-source", runner.Mount{Source: "", Target: "/work/x"}},
		{"bad-target-prefix", runner.Mount{Source: "/home/u/repo", Target: "/etc"}},
		{"bad-target-usr", runner.Mount{Source: "/home/u/repo", Target: "/usr/local"}},
		{"relative-target", runner.Mount{Source: "/home/u/repo", Target: "work"}},
		{"empty-target", runner.Mount{Source: "/home/u/repo", Target: ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := createWithMounts(t, []runner.Mount{tc.m})
			if err == nil {
				t.Fatalf("CreateSandbox with denied mount %+v should FAIL CLOSED, got nil error", tc.m)
			}
			if !strings.Contains(err.Error(), "denied workspace mount") {
				t.Errorf("error should identify the denied mount, got: %v", err)
			}
		})
	}
}

// ─── USER DRIVES (host_path): the same deny matrix, one object up ─────────────

// driveHostRoot makes a real, symlink-resolved root with this person's home
// directory under it — the shape a host_path drive actually has (the OPERATOR
// mounted the share at the root; Wardyn binds one subdirectory of it).
func driveHostRoot(t *testing.T) (root, home string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve tempdir: %v", err)
	}
	root = filepath.Join(base, "shares")
	home = filepath.Join(root, "alice")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return root, home
}

// hostPathDrive is a resolved SHARE drive whose ObjectName is the per-person
// subdirectory the resolver already derived (<host_root>/<home>).
func hostPathDrive(objectName string) *types.DriveMount {
	return &types.DriveMount{
		Backend:     types.DriveBackendHostPath,
		ObjectName:  objectName,
		HomeName:    "alice",
		Target:      runner.DriveTarget,
		ReadOnly:    true,
		Enforcement: types.StorageEnforcementExternal,
	}
}

// TestCreateSandbox_HostPathDriveApplied is the happy path: a real subdirectory
// of a configured root is bound at the reserved target, read-only.
func TestCreateSandbox_HostPathDriveApplied(t *testing.T) {
	root, home := driveHostRoot(t)
	_, mounts, err := createWithDrive(t, hostPathDrive(home), []string{root})
	if err != nil {
		t.Fatalf("CreateSandbox with an in-root host_path drive: %v", err)
	}
	m := findMount(mounts, runner.DriveTarget)
	if m == nil {
		t.Fatalf("drive not mounted at %s; mounts=%+v", runner.DriveTarget, mounts)
	}
	if m.Type != mount.TypeBind {
		t.Errorf("host_path drive mount Type = %q, want %q", m.Type, mount.TypeBind)
	}
	if m.Source != home {
		t.Errorf("drive mount Source = %q, want %q (only the person's subdir is ever bound, never the root)", m.Source, home)
	}
	if !m.ReadOnly {
		t.Error("drive mount ReadOnly = false, want true")
	}
}

// TestCreateSandbox_DeniedDriveMountsRejected extends the workspace deny matrix
// above to the ONE bind the driver synthesizes itself
// (runner.Mount.DriveAuthored). Each row FAILS CreateSandbox closed, so no
// agent container ever exists:
//
//   - the deny-list rows prove the drive runs runner.ValidateMount verbatim —
//     it is not a privileged path that skips the matrix because an admin
//     authored it;
//   - the roots rows prove the deployment's WARDYN_USER_DRIVE_HOST_ROOTS
//     ceiling is re-asserted HERE, on the symlink-RESOLVED real path, as the
//     last thing before ContainerCreate — a share directory replaced by a
//     symlink out of the root after the drive row was written is caught at bind
//     time, not merely at authoring time;
//   - the unset-roots row proves the fail-closed default: a daemon whose
//     wardynd was never told where shares are mounted refuses every host_path
//     drive rather than binding one on trust.
func TestCreateSandbox_DeniedDriveMountsRejected(t *testing.T) {
	root, home := driveHostRoot(t)

	// A symlink INSIDE the root pointing at a directory outside it: lexically
	// in-root, really not.
	outside := filepath.Join(filepath.Dir(root), "elsewhere")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	escaping := filepath.Join(root, "escape")
	if err := os.Symlink(outside, escaping); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	cases := []struct {
		name  string
		src   string
		roots []string
	}{
		{"under-etc", "/etc/wardyn-drives/alice", []string{"/etc/wardyn-drives"}},
		{"socket-basename", filepath.Join(root, "docker.sock"), []string{root}},
		{"containerd-socket-basename", filepath.Join(root, "containerd.sock"), []string{root}},
		{"symlink-escapes-root", escaping, []string{root}},
		{"outside-the-roots", home, []string{filepath.Join(filepath.Dir(root), "other-share")}},
		{"no-roots-configured", home, nil},
		{"traversal-source", root + "/../alice", []string{root}},
		{"relative-source", "shares/alice", []string{root}},
		{"missing-directory", filepath.Join(root, "no-such-home"), []string{root}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, _, err := createWithDrive(t, hostPathDrive(tc.src), tc.roots)
			if err == nil {
				t.Fatalf("CreateSandbox with denied drive source %q should FAIL CLOSED, got nil error", tc.src)
			}
			if !strings.Contains(err.Error(), "denied user drive") {
				t.Errorf("error should identify the denied drive, got: %v", err)
			}
			if f.containers[agentContainerName(testSpec().RunID)] != nil {
				t.Error("agent container exists after the refusal — the check must precede ContainerCreate")
			}
		})
	}
}

// TestCreateSandbox_DriveTargetIsValidatedNotAuthored pins the ValidateTarget /
// ValidateAuthoredTarget split from the driver's side: the drive's own mount
// must pass (runner.DriveTarget is a legal place to put something), while a
// drive whose target was corrupted to somewhere illegal is still refused.
// Running ValidateAuthoredTarget here instead would make the drive fail its own
// validation, since that function exists to reserve this exact path from
// everybody else.
func TestCreateSandbox_DriveTargetIsValidatedNotAuthored(t *testing.T) {
	if err := runner.ValidateTarget(runner.DriveTarget); err != nil {
		t.Fatalf("runner.ValidateTarget(%q) must pass — the drive mounts there: %v", runner.DriveTarget, err)
	}
	if err := runner.ValidateAuthoredTarget(runner.DriveTarget); err == nil {
		t.Fatalf("runner.ValidateAuthoredTarget(%q) must refuse — the target is reserved from authors", runner.DriveTarget)
	}

	drive := dockerVolumeDrive()
	drive.Target = "/usr/local"
	if _, _, err := createWithDrive(t, drive, nil); err == nil {
		t.Fatal("a drive targeting a system path must FAIL CLOSED, got nil")
	}
}

// TestDriveMount_HostPathCeilingIsUnconditional is the pin the old
// driveHostMount stamp test used to be, moved onto the function that now does
// the conversion (driveMount, its single caller).
//
// The stamp test asserted that a share drive reached ContainerCreate carrying
// runner.Mount.DriveAuthored, because the roots ceiling was written `if
// m.DriveAuthored` — fail-OPEN by shape, since the flag's only false state is a
// refactor that stops setting it. The ceiling is now unconditional, so the
// property worth pinning is the ceiling itself: the SAME call that binds an
// in-root share refuses when the deployment named no roots, with nothing in
// between that could turn it off. The happy-path half also carries the old
// test's other assertions — source, target and mode arrive verbatim from the
// resolver, never re-derived here.
func TestDriveMount_HostPathCeilingIsUnconditional(t *testing.T) {
	root, home := driveHostRoot(t)
	drive := hostPathDrive(home)

	d := newWithClient(newFakeDocker(), Config{ProxyImage: "wardyn-proxy:dev", UserDriveHostRoots: []string{root}})
	got, err := d.driveMount(context.Background(), drive)
	if err != nil {
		t.Fatalf("driveMount for an in-root share: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("driveMount returned %d mounts, want exactly 1: %+v", len(got), got)
	}
	if got[0].Type != mount.TypeBind || got[0].Source != drive.ObjectName || got[0].Target != drive.Target || got[0].ReadOnly != drive.ReadOnly {
		t.Errorf("driveMount(%+v) = %+v, want a bind carrying the resolver's object name, target and mode verbatim", *drive, got[0])
	}

	// The SAME drive, on a deployment that configured no roots: refused. No
	// flag, provenance stamp or backend detail sits between the two calls.
	unset := newWithClient(newFakeDocker(), Config{ProxyImage: "wardyn-proxy:dev"})
	if _, err := unset.driveMount(context.Background(), drive); err == nil {
		t.Error("driveMount bound a share on a deployment with no WARDYN_USER_DRIVE_HOST_ROOTS — the ceiling must run unconditionally")
	} else if !strings.Contains(err.Error(), "denied user drive") {
		t.Errorf("the refusal should identify the denied drive, got: %v", err)
	}
}
