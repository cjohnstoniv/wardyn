// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"errors"
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
		// THE RESERVED TARGET, arriving from a STORED POLICY ROW. validatePolicySpec
		// refuses it at authoring — but only since the reservation existed, and a
		// row written before that is exactly what this defense-in-depth re-check
		// is for. The driver's half used to be runner.ValidateMount, whose
		// ValidateTarget does not carry the reservation, so the bind landed INSIDE
		// the member's drive: nesting over a rw bind makes runc mkdir the
		// intermediate directories in the share, and whichever mount lands second
		// shadows the other.
		{"reserved-drive-target", runner.Mount{Source: "/home/u/repo", Target: runner.DriveTarget}},
		{"reserved-drive-target-nested", runner.Mount{Source: "/home/u/repo", Target: runner.DriveTarget + "/shared"}},
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
//
// HostRoot is that derivation run backwards, so every row below carries the
// root the resolver would have carried: driveMountFor copies UserDrive.HostRoot
// onto the mount, and the driver bounds the bind to THAT root rather than to
// the union of the deployment's ceiling. A test that wants a different root (a
// drive whose home was linked into another drive's tree) sets it explicitly.
func hostPathDrive(objectName string) *types.DriveMount {
	return &types.DriveMount{
		Backend:     types.DriveBackendHostPath,
		ObjectName:  objectName,
		HostRoot:    filepath.Dir(objectName),
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
	// AND read-only RECURSIVELY, or not at all. A bind's `ro` only reaches
	// submounts from Linux 5.12; below that the kernel silently leaves every
	// submount under the source WRITABLE — and a share's per-person home is
	// exactly where a submount turns up (an autofs home, a second export mounted
	// under the first). ReadOnlyForceRecursive makes the daemon refuse rather
	// than hand back that half-honoured mount.
	if m.BindOptions == nil || !m.BindOptions.ReadOnlyForceRecursive {
		t.Errorf("drive mount BindOptions = %+v, want ReadOnlyForceRecursive on a read-only share bind — "+
			"without it an old kernel binds the submounts read-WRITE and says nothing", m.BindOptions)
	}
}

// TestCreateSandbox_WritableHostPathDriveIsNotForcedRecursive is the other half
// of the row above: the flag is a claim about a READ-ONLY mount, and a writable
// allocation has none to make. Setting it unconditionally would ask the daemon
// to prove a property this bind does not have.
func TestCreateSandbox_WritableHostPathDriveIsNotForcedRecursive(t *testing.T) {
	root, home := driveHostRoot(t)
	drive := hostPathDrive(home)
	drive.ReadOnly = false
	_, mounts, err := createWithDrive(t, drive, []string{root})
	if err != nil {
		t.Fatalf("CreateSandbox with a writable host_path drive: %v", err)
	}
	m := findMount(mounts, runner.DriveTarget)
	if m == nil {
		t.Fatalf("drive not mounted; mounts=%+v", mounts)
	}
	if m.ReadOnly {
		t.Error("drive mount ReadOnly = true, want false (the allocation is writable)")
	}
	if m.BindOptions != nil && m.BindOptions.ReadOnlyForceRecursive {
		t.Error("a WRITABLE bind carries ReadOnlyForceRecursive — the flag is a read-only claim")
	}
}

// TestCreateSandbox_HostPathDriveHomeMustResolveToThisPrincipal is the SIBLING
// SYMLINK. Everything the composed source check asserts is satisfied by a home
// replaced host-side with a link to the home NEXT TO IT: it is inside the
// deployment's roots, inside this drive's own root, not a root itself, and it
// traverses no denied prefix and no dotfile. And it binds bob's directory into
// alice's sandbox — read-write whenever her allocation is writable.
//
// The assertion the driver adds is on the resolved directory's NAME, and the
// second sub-test is what keeps it from being a whole-path rule: a share may
// arrange its homes below the root (`<root>/alice -> <root>/2024/alice`) and
// that must still bind. What the name rule cannot catch — a link that LEAVES
// this drive's tree for another drive's — is
// TestCreateSandbox_HostPathDriveStaysInsideItsOwnDriveRoot's subject.
func TestCreateSandbox_HostPathDriveHomeMustResolveToThisPrincipal(t *testing.T) {
	t.Run("sibling-symlink-refused", func(t *testing.T) {
		root, _ := driveHostRoot(t)
		if err := os.MkdirAll(filepath.Join(root, "bob"), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		// alice's home, replaced host-side by a link to bob's.
		alice := filepath.Join(root, "alice-linked")
		if err := os.Symlink(filepath.Join(root, "bob"), alice); err != nil {
			t.Skipf("symlink unsupported here: %v", err)
		}
		drive := hostPathDrive(alice)
		drive.HomeName = "alice-linked"
		f, _, err := createWithDrive(t, drive, []string{root})
		if err == nil {
			t.Fatal("a home symlinked to a SIBLING home was bound — that is another person's directory in this sandbox")
		}
		if !strings.Contains(err.Error(), "denied user drive") {
			t.Errorf("error should identify the denied drive, got: %v", err)
		}
		if !strings.Contains(err.Error(), "alice-linked") {
			t.Errorf("the refusal should name the home it expected, got: %v", err)
		}
		if f.containers[agentContainerName(testSpec().RunID)] != nil {
			t.Error("agent container exists after the refusal — the check must precede ContainerCreate")
		}
	})

	// A home symlinked DEEPER INSIDE this drive's own root keeps its own name
	// and stays in this drive's tree — an ordinary share layout (homes filed
	// under a year, a department, a snapshot generation), and it must bind.
	t.Run("symlink-within-the-same-root-allowed", func(t *testing.T) {
		root, _ := driveHostRoot(t)
		if err := os.MkdirAll(filepath.Join(root, "2024", "alice"), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		linked := filepath.Join(root, "alice2")
		if err := os.Symlink(filepath.Join(root, "2024", "alice"), linked); err != nil {
			t.Skipf("symlink unsupported here: %v", err)
		}
		drive := hostPathDrive(linked)
		// The home the RESOLVER derived is what the resolved directory must be
		// named — "alice" here, since that is the directory the link lands on.
		drive.HomeName = "alice"
		_, mounts, err := createWithDrive(t, drive, []string{root})
		if err != nil {
			t.Fatalf("a home symlinked deeper inside its own drive's root was refused: %v", err)
		}
		if m := findMount(mounts, runner.DriveTarget); m == nil || m.Source != linked {
			t.Errorf("drive mount = %+v, want a bind of the resolver's own path %q", m, linked)
		}
	})
}

// TestCreateSandbox_HostPathDriveStaysInsideItsOwnDriveRoot is the two-drive
// deployment, and the hole the deployment ceiling alone could not close.
//
// WARDYN_USER_DRIVE_HOST_ROOTS is the OPERATOR's outer bound over every drive
// at once, so with two share drives — one rooted at /srv/a, one at /srv/b, both
// inside the ceiling — it cannot tell one drive's tree from the other's. A home
// under A replaced host-side by a link to the SAME-NAMED home under B satisfies
// every check the driver used to have: inside a configured root, not a root, no
// denied segment, and `filepath.Base` still says "alice". It bound drive B's
// directory — another person's whenever B names "alice" for somebody else.
//
// The mount now carries the drive's own host_root and the driver asserts the
// resolved path is a strict subdirectory of THAT root. The ceiling stays as the
// outer bound (an admin-authored row must not be able to name a tree the
// operator never allowed), so both are asserted, and the sub-tests below are
// each half of that pair plus the fail-closed arm for a mount that carries no
// root at all.
func TestCreateSandbox_HostPathDriveStaysInsideItsOwnDriveRoot(t *testing.T) {
	// Two roots, both configured — the deployment ceiling is satisfied for
	// either tree, which is exactly why it cannot be the whole check.
	twoRoots := func(t *testing.T) (rootA, rootB string) {
		t.Helper()
		base, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatalf("resolve tempdir: %v", err)
		}
		rootA, rootB = filepath.Join(base, "a"), filepath.Join(base, "b")
		for _, dir := range []string{filepath.Join(rootA, "alice"), filepath.Join(rootB, "alice")} {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
		}
		return rootA, rootB
	}

	t.Run("a home linked into ANOTHER drive's root is refused", func(t *testing.T) {
		rootA, rootB := twoRoots(t)
		// alice's home on drive A, replaced host-side by a link to the home of
		// the same name on drive B's root.
		linked := filepath.Join(rootA, "alice-linked")
		if err := os.Symlink(filepath.Join(rootB, "alice"), linked); err != nil {
			t.Skipf("symlink unsupported here: %v", err)
		}
		drive := hostPathDrive(linked)
		drive.HostRoot = rootA // the row an admin authored: drive A
		drive.DriveName = "nas"
		drive.HomeName = "alice"

		f, _, err := createWithDrive(t, drive, []string{rootA, rootB})
		if err == nil {
			t.Fatal("a home linked into the OTHER drive's root was bound — that is another drive's tree in this member's sandbox")
		}
		if !strings.Contains(err.Error(), "denied user drive") {
			t.Errorf("error should identify the denied drive, got: %v", err)
		}
		// NAMES THE DRIVE AND THE DIRECTORY, NEVER THE PATHS. Every driver
		// refusal becomes the run's failure_hint, which the run's CREATOR reads,
		// and both roots plus the bind source are the operator's filesystem
		// layout — the same disclosure driveAuditTarget masks off the audit row
		// and applyUserDriveEnv keeps out of the sandbox. The operator gets them
		// from the slog line UserDriveHomeWithinItsRoot writes beside this.
		for _, leak := range []string{rootA, rootB, linked, drive.ObjectName} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("refusal = %q leaks the host path %q to the run's creator", err, leak)
			}
		}
		for _, want := range []string{`drive "nas"`, `directory "alice"`} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal = %q, want it to name %s", err, want)
			}
		}
		if f.containers[agentContainerName(testSpec().RunID)] != nil {
			t.Error("agent container exists after the refusal — the check must precede ContainerCreate")
		}
	})

	// The control, and the reason the check is a strict-subdirectory rule rather
	// than an equality one: the ordinary drive on the same two-root deployment
	// still binds.
	t.Run("the ordinary home under its own root still binds", func(t *testing.T) {
		rootA, rootB := twoRoots(t)
		home := filepath.Join(rootA, "alice")
		_, mounts, err := createWithDrive(t, hostPathDrive(home), []string{rootA, rootB})
		if err != nil {
			t.Fatalf("an ordinary in-root home was refused: %v", err)
		}
		if m := findMount(mounts, runner.DriveTarget); m == nil || m.Source != home {
			t.Errorf("drive mount = %+v, want a bind of %q", m, home)
		}
	})

	// FAIL CLOSED on a mount that carries no host_root. "" means this DriveMount
	// was built by something that does not know the field — an older control
	// plane, a hand-written -spec for the standalone runner — and falling
	// through would be the pre-fix behaviour reappearing exactly where nobody
	// would look for it. The ceiling is deliberately satisfied here, so the row
	// is a statement about the drive's own root and nothing else.
	t.Run("a share mount with no host_root is refused", func(t *testing.T) {
		root, home := driveHostRoot(t)
		drive := hostPathDrive(home)
		drive.HostRoot = ""
		f, _, err := createWithDrive(t, drive, []string{root})
		if err == nil {
			t.Fatal("a share drive carrying no host_root was bound — an absent per-drive bound must refuse, never skip")
		}
		if !strings.Contains(err.Error(), "host_root") {
			t.Errorf("the refusal should name the missing field, got: %v", err)
		}
		if f.containers[agentContainerName(testSpec().RunID)] != nil {
			t.Error("agent container exists after the refusal")
		}
	})
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
		// The source IS the root: lexically fine, inside the ceiling, and it
		// would bind the WHOLE share — every other person's home — into this one
		// member's sandbox. Only a bug can produce it (a home that resolved to
		// "." or "", a symlink from a home back to its parent), which is exactly
		// what a last-thing-before-ContainerCreate check is for.
		{"source-is-the-root", root, []string{root}},
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

// TestCreateSandbox_ReservedSpecMountIsRefusedBeforeTheDriveIsAllocated is the
// ORDER inside agentMounts, which the deny matrix above states nothing about.
//
// resolvePolicy hands dispatch a STORED spec verbatim — validatePolicySpec runs
// for inline specs only — so a workspace_mounts row written before the target
// was reserved reaches this driver and is refused here, which is fail-closed
// and correct. The question this pins is WHERE in the sequence. Spec mounts are
// validated before the drive is appended, so the refusal lands before
// ensureDriveVolume: a run that fails on a legacy policy row must not leave a
// persistent, member-owned volume behind, and nothing in this package removes
// one (there is no VolumeRemove call anywhere in it, and CreateSandbox's
// rollback deliberately skips drive volumes so a retry finds the member's
// files). Reordered, every failed run on such a deployment would allocate
// storage nobody ever reclaims.
//
// Asserted with AND without a drive on the same run, so the refusal is a
// property of the spec mount rather than of the pair.
func TestCreateSandbox_ReservedSpecMountIsRefusedBeforeTheDriveIsAllocated(t *testing.T) {
	src, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve tempdir: %v", err)
	}
	for _, withDrive := range []bool{true, false} {
		t.Run(map[bool]string{true: "with a drive", false: "without a drive"}[withDrive], func(t *testing.T) {
			f := newFakeDocker()
			f.images["busybox:latest"] = true
			d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev"})
			spec := testSpec()
			spec.Mounts = []runner.Mount{{Source: src, Target: runner.DriveTarget + "/shared"}}
			if withDrive {
				spec.Drive = dockerVolumeDrive()
			}
			_, err := d.CreateSandbox(context.Background(), spec)
			if err == nil || !strings.Contains(err.Error(), "reserved") {
				t.Fatalf("CreateSandbox = %v, want the reserved-target refusal", err)
			}
			if f.containers[agentContainerName(spec.RunID)] != nil {
				t.Error("agent container exists after the refusal")
			}
			if f.volumeCreates != 0 {
				t.Errorf("VolumeCreate calls = %d, want 0 — the refusal must precede the drive allocation, "+
					"or a failed run leaves a volume nothing in this package can remove", f.volumeCreates)
			}
		})
	}
}

// TestCreateSandbox_DriveTargetIsPinnedToTheReservedPath is the DRIVER's side
// of the reserved-target rule, and the parity fix for it: this driver used to
// run runner.ValidateTarget alone, which asks only "is this a legal place for a
// mount" — and /home/agent, /home/agent/.claude and /work all are. The k8s
// driver has refused anything but runner.DriveTarget since D4
// (validateDriveMount, errDriveTargetInvalid), so one rule had two answers
// depending on the substrate.
//
// The rows are not interchangeable: /home/agent/.claude shadows the injected
// credential directory with a member-owned volume that SURVIVES the run,
// /home/agent shadows the whole home, /work the workspace. /usr/local is the
// fourth and the one ValidateTarget already caught — kept here so both checks
// are pinned in one place rather than in two tests that could drift.
//
// Nothing in the control plane produces any of them (driveMountFor copies the
// constant); the reachable inputs are a control-plane bug and the standalone
// runner's -spec JSON, which is exactly what a driver-side check is for. The
// OTHER half of the split — that the drive's own target passes ValidateTarget
// and is refused to AUTHORS — is walked by
// TestValidateAuthoredTargetReservesTheDriveTarget, in the package that owns
// both functions; restating it here would be a second, weaker copy.
func TestCreateSandbox_DriveTargetIsPinnedToTheReservedPath(t *testing.T) {
	for _, tgt := range []string{"/home/agent", "/home/agent/.claude", "/work", "/usr/local"} {
		t.Run(tgt, func(t *testing.T) {
			drive := dockerVolumeDrive()
			drive.Target = tgt
			f, mounts, err := createWithDrive(t, drive, nil)
			if err == nil {
				t.Fatalf("a drive addressed to %q was mounted (%+v); only %q may back a drive",
					tgt, findMount(mounts, tgt), runner.DriveTarget)
			}
			if !errors.Is(err, errDriveTargetInvalid) {
				t.Errorf("error = %v, want the errDriveTargetInvalid sentinel the k8s driver also raises", err)
			}
			// BEFORE any call to the daemon: a refused drive must not have left a
			// persistent volume behind, since nothing in the tree ever removes one.
			if f.volumeCreates != 0 {
				t.Errorf("VolumeCreate calls = %d, want 0 — the refusal must precede any daemon call", f.volumeCreates)
			}
			if f.containers[agentContainerName(testSpec().RunID)] != nil {
				t.Error("agent container exists after the refusal")
			}
		})
	}

	// The control: the reserved path itself still binds, so the pin above is a
	// refusal of everything else rather than of everything.
	t.Run(runner.DriveTarget, func(t *testing.T) {
		_, mounts, err := createWithDrive(t, dockerVolumeDrive(), nil)
		if err != nil {
			t.Fatalf("the reserved target was refused: %v", err)
		}
		if findMount(mounts, runner.DriveTarget) == nil {
			t.Errorf("no mount at %s; mounts=%+v", runner.DriveTarget, mounts)
		}
	})
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
