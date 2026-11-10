// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"bytes"
	"context"
	"log/slog"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/system"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// These tests run on hosted CI only: the package builds with -tags docker, which the local go shim
// refuses.

// rootExecs returns the execs the driver ran as a user it named, in order.
func rootExecs(f *fakeDocker) []fakeExec {
	var out []fakeExec
	for _, e := range f.execLog {
		if e.user != "" {
			out = append(out, e)
		}
	}
	return out
}

// requireSafeRootExecs fails on a root exec that names its binary by bare name, or that does not pin
// a PATH of root-owned system directories: an image can put a directory the agent writes first on its
// own PATH, and the exec would then run the agent's file as uid 0.
func requireSafeRootExecs(t *testing.T, f *fakeDocker, wantAtLeast int) {
	t.Helper()
	execs := rootExecs(f)
	if len(execs) < wantAtLeast {
		t.Fatalf("root execs = %+v, want at least %d (recording setup)", execs, wantAtLeast)
	}
	for _, e := range execs {
		if len(e.cmd) == 0 || !path.IsAbs(e.cmd[0]) {
			t.Errorf("root exec %q resolves its binary through the image's PATH", e.cmd)
		}
		if !slices.Contains(e.env, "PATH=/usr/sbin:/usr/bin:/sbin:/bin") {
			t.Errorf("root exec %q env = %q, want the fixed system PATH", e.cmd, e.env)
		}
	}
}

// Recording setup runs mkdir and chmod as uid 0 in the agent: at create and again at every revive.
// Neither may take its binary from the image's PATH.
func TestRecordingSetup_RootExecsUseAbsolutePathsAndAFixedPATH(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev", InternalNetwork: "wardyn-internal", Record: true})
	ctx := context.Background()
	sb, err := d.CreateSandbox(ctx, testSpec())
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	requireSafeRootExecs(t, f, 2)
	atCreate := len(rootExecs(f))

	if err := d.EndSandbox(ctx, sb.Ref); err != nil {
		t.Fatalf("EndSandbox: %v", err)
	}
	if err := d.ReplaceProxy(ctx, sb.Ref, renderedTestConfig(t, "fresh")); err != nil {
		t.Fatalf("ReplaceProxy: %v", err)
	}
	if err := d.StartSandbox(ctx, sb.Ref); err != nil {
		t.Fatalf("StartSandbox: %v", err)
	}
	requireSafeRootExecs(t, f, atCreate+2)
}

// A kept end leaves the container (and so its writable layer and volumes) in place for the revive:
// only the final teardown asks for the anonymous volumes to go.
func TestEndSandbox_KeepsTheAgentAndItsVolumes(t *testing.T) {
	f := newFakeDocker()
	ref, err := keptEnd(t, f, testSpec())
	if err != nil {
		t.Fatalf("EndSandbox: %v", err)
	}
	if c := f.containers[ref]; c.removed {
		t.Errorf("agent after a kept end = %+v, want it kept", c)
	}
}

// The agent's removal takes the container's anonymous volumes (an image VOLUME) with it: they hold
// whatever the agent wrote and sit outside the disk cap. Named volumes (drives) are not anonymous,
// so the option leaves them.
func TestTeardown_RemovesTheAgentsAnonymousVolumes(t *testing.T) {
	for name, end := range map[string]func(*Driver, string) error{
		"kill": func(d *Driver, ref string) error { return d.KillSandbox(context.Background(), ref) },
		"stop": func(d *Driver, ref string) error { return d.StopSandbox(context.Background(), ref) },
	} {
		f := newFakeDocker()
		f.images["busybox:latest"] = true
		d := newTestDriver(f)
		sb, err := d.CreateSandbox(context.Background(), testSpec())
		if err != nil {
			t.Fatalf("%s: CreateSandbox: %v", name, err)
		}
		if err := end(d, sb.Ref); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if c := f.containers[sb.Ref]; !c.removed || !c.removedVolumes {
			t.Errorf("%s: agent removal removed=%v removedVolumes=%v, want both", name, c.removed, c.removedVolumes)
		}
	}
}

// A create that fails after the agent exists rolls the agent back, anonymous volumes included.
func TestCreateSandbox_RollbackRemovesTheAgentsAnonymousVolumes(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	spec := testSpec()
	f.failStartContainer = agentContainerName(spec.RunID)
	d := newTestDriver(f)
	if _, err := d.CreateSandbox(context.Background(), spec); err == nil {
		t.Fatal("CreateSandbox succeeded, want the agent start to fail")
	}
	c := f.containers[agentContainerName(spec.RunID)]
	if c == nil || !c.removed || !c.removedVolumes {
		t.Errorf("rolled-back agent = %+v, want removed with its anonymous volumes", c)
	}
}

// A read-only workspace bind asks the engine for a recursively read-only mount, as a drive does,
// so a host submount under the folder cannot be written from the sandbox.
func TestAgentMounts_ReadOnlyWorkspaceBindIsRecursivelyReadOnly(t *testing.T) {
	d := newTestDriver(newFakeDocker())
	src := "/srv/data"
	for _, tc := range []struct {
		name     string
		readOnly bool
		rro      bool
		wantRRO  bool
		wantWarn bool
	}{
		{"read-only, runtime supports rro", true, true, true, false},
		{"read-only, runtime lacks rro", true, false, false, true},
		{"writable, runtime supports rro", false, true, false, false},
		{"writable, runtime lacks rro", false, false, false, false},
	} {
		var logs bytes.Buffer
		old := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
		spec := testSpec()
		spec.Mounts = []runner.Mount{{Source: src, Target: "/home/agent/work", ReadOnly: tc.readOnly}}
		got, err := d.agentMounts(context.Background(), spec, tc.rro)
		slog.SetDefault(old)
		if err != nil {
			t.Fatalf("%s: agentMounts: %v", tc.name, err)
		}
		i := slices.IndexFunc(got, func(m mount.Mount) bool { return m.Target == "/home/agent/work" })
		if i < 0 {
			t.Fatalf("%s: workspace mount missing from %+v", tc.name, got)
		}
		m := got[i]
		if rro := m.BindOptions != nil && m.BindOptions.ReadOnlyForceRecursive; rro != tc.wantRRO {
			t.Errorf("%s: ReadOnlyForceRecursive = %v, want %v", tc.name, rro, tc.wantRRO)
		}
		warned := strings.Contains(logs.String(), "workspace mount") && strings.Contains(logs.String(), src)
		if warned != tc.wantWarn {
			t.Errorf("%s: warned = %v, want %v (log %q)", tc.name, warned, tc.wantWarn, logs.String())
		}
	}
}

// The containers the driver makes from the run's image or the workspace's, and removes itself (the
// drive seed, the architecture probe), take their anonymous volumes with them. The seed also mounts
// the drive's named volume: the option removes anonymous volumes only, so the drive keeps its data.
func TestSeedAndProbeRemovalTakeTheirAnonymousVolumes(t *testing.T) {
	f := newFakeDocker()
	f.imageUsers = map[string]string{"busybox:latest": "1000"}
	run := testSpec().RunID
	if _, err := createWithDrives(t, f, run, teamVolumeDrive(false)); err != nil {
		t.Fatal(err)
	}
	if seed := f.containers[driveSeedName(run)]; seed == nil || !seed.removed || !seed.removedVolumes {
		t.Errorf("seed = %+v, want removed with its anonymous volumes", seed)
	}

	const ref = "mirror.example/agent:1"
	d, af := archDriver("aarch64", map[string]string{ref: "amd64"})
	_ = d.ensureImage(context.Background(), ref, "", nil)
	p, _ := probes(af)
	if len(p) != 1 || !p[0].removed || !p[0].removedVolumes {
		t.Errorf("probes = %+v, want one removed with its anonymous volumes", p)
	}
}

// quotaDriver is a fake whose engine enforces a per-container disk limit (overlay2 on xfs), with the
// test image declaring a VOLUME at /work/data.
func quotaDriver(info system.Info) (*Driver, *fakeDocker) {
	f := newFakeDocker()
	f.info = info
	f.info.Driver = "overlay2"
	f.info.DriverStatus = [][2]string{{"Backing Filesystem", "xfs"}}
	f.images["busybox:latest"] = true
	f.imageVolumes = map[string][]string{"busybox:latest": {"/work/data"}}
	return newTestDriver(f), f
}

// A disk limit bounds the container's writable layer, not a VOLUME's anonymous volume. A limit
// policy wrote (hard) refuses an image whose VOLUME no Wardyn mount covers, before anything starts;
// a limit nobody wrote (soft) starts and warns; no VOLUME, or a covered one, is not in the way.
func TestCreateSandbox_ImageVolumeAgainstTheDiskLimit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		soft    bool
		volumes []string
		mounts  []runner.Mount
		refused bool
		warned  bool
	}{
		{"hard limit, uncovered VOLUME", false, []string{"/work/data"}, nil, true, false},
		{"soft limit, uncovered VOLUME", true, []string{"/work/data"}, nil, false, true},
		{"hard limit, no VOLUME", false, nil, nil, false, false},
		{"hard limit, VOLUME covered by a mount", false, []string{"/work/data"},
			[]runner.Mount{{Source: "/srv/data", Target: "/work/data", ReadOnly: true}}, false, false},
	} {
		d, f := quotaDriver(system.Info{})
		f.imageVolumes = map[string][]string{"busybox:latest": tc.volumes}
		spec := testSpec()
		spec.Resources.DiskMiB, spec.Resources.DiskMiBSoft, spec.Mounts = 1024, tc.soft, tc.mounts

		var logs bytes.Buffer
		old := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
		_, err := d.CreateSandbox(context.Background(), spec)
		slog.SetDefault(old)

		if tc.refused {
			if err == nil || !strings.Contains(err.Error(), "/work/data") || !strings.Contains(err.Error(), "disk limit") {
				t.Errorf("%s: CreateSandbox = %v, want a refusal naming /work/data and the disk limit", tc.name, err)
			}
			if c := f.containers[agentContainerName(spec.RunID)]; c != nil && !c.removed {
				t.Errorf("%s: an agent was left behind: %+v", tc.name, c)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: CreateSandbox: %v", tc.name, err)
		}
		if warned := strings.Contains(logs.String(), "not counted in the disk limit"); warned != tc.warned {
			t.Errorf("%s: warned = %v, want %v (log %q)", tc.name, warned, tc.warned, logs.String())
		}
	}
}

// The exec-less create (a krun microVM) defers the container, so the check cannot wait for it: a
// hard limit over an uncovered VOLUME is refused at CreateSandbox, with no agent created.
func TestCreateSandbox_ExecLessRefusesAnImageVolumeUnderAHardLimit(t *testing.T) {
	d, f := quotaDriver(infoWithRuntimes("krun"))
	spec := testSpec()
	spec.ConfinementClass = types.CC3
	spec.Resources.DiskMiB = 1024
	if _, err := d.CreateSandbox(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "/work/data") {
		t.Fatalf("CreateSandbox = %v, want a refusal naming /work/data", err)
	}
	if c := f.containers[agentContainerName(spec.RunID)]; c != nil {
		t.Errorf("an agent exists after the refusal: %+v", c)
	}
}
