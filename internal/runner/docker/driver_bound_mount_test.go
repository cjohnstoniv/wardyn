// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/mount"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A member's local_dir folder can be bound to their own runner (Mount.BoundRunner). On a runner
// (Config.RunnerID set) the driver binds only member folders bound to it, inside the member's roots
// AND the runner owner's own --drive-root (Config.UserDriveHostRoots in runner mode), and a writable
// one only under a writable ceiling. Everything outside the agent-mount loop (drives, the recording
// mount) is untouched by those rules. On wardynd's host the driver is as before, except that a
// folder bound to any runner never binds there.

const thisRunner = "11111111-2222-3333-4444-555555555555"

// boundMountRoot is a member root holding the runner owner's --drive-root with one project in it.
func boundMountRoot(t *testing.T) (memberRoot, driveRoot, project string) {
	t.Helper()
	memberRoot, _ = memberSandboxRoot(t)
	driveRoot = filepath.Join(memberRoot, "shared")
	project = filepath.Join(driveRoot, "app")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return memberRoot, driveRoot, project
}

// runnerDriver is the driver as wardyn-runnerd builds it: its own id and its --drive-root.
func runnerDriver(driveRoots []string) *Driver {
	return newWithClient(newFakeDocker(), Config{ProxyImage: "wardyn-proxy:dev", RunnerID: thisRunner, UserDriveHostRoots: driveRoots})
}

// memberSpec is a run carrying mounts under the member's roots and the given writable ceiling.
func memberSpec(mounts []runner.Mount, memberRoot string, writable *runner.UserWritableCeiling) runner.SandboxSpec {
	spec := testSpec()
	spec.Mounts, spec.UserMountRoots, spec.UserWritable = mounts, []string{memberRoot}, writable
	return spec
}

func TestAgentMounts_RunnerRefusesNonMemberMount(t *testing.T) {
	memberRoot, driveRoot, project := boundMountRoot(t)
	d := runnerDriver([]string{driveRoot})
	bound := runner.Mount{Source: project, Target: "/home/agent/work", ReadOnly: true, MemberAuthored: true, BoundRunner: thisRunner}
	got, err := d.agentMounts(context.Background(), memberSpec([]runner.Mount{bound}, memberRoot, nil), false)
	if err != nil {
		t.Fatalf("a member folder bound to this runner = %v, want bound", err)
	}
	assertMountSet(t, got, []mountSummary{{Target: "/home/agent/work", Source: project, ReadOnly: true}})

	// A mount no member authored never binds on a runner, whatever the org's member flags say: the
	// operator deny-list alone does not cover a person's ~/.ssh.
	operator := runner.Mount{Source: project, Target: "/home/agent/work", ReadOnly: true}
	noRoots := testSpec()
	noRoots.Mounts = []runner.Mount{operator}
	for name, spec := range map[string]runner.SandboxSpec{
		"with member roots": memberSpec([]runner.Mount{operator}, memberRoot, nil),
		"with no roots":     noRoots,
		"beside a bound one": memberSpec([]runner.Mount{bound, {Source: filepath.Join(memberRoot, "app"),
			Target: "/home/agent/ref", ReadOnly: true}}, memberRoot, nil),
	} {
		if got, err := d.agentMounts(context.Background(), spec, false); err == nil || !strings.Contains(err.Error(), "denied") {
			t.Errorf("%s: a non-member mount on a runner = %v (mounts %+v), want refused", name, err, got)
		}
	}
}

func TestAgentMounts_RunnerRefusesMountBoundElsewhere(t *testing.T) {
	memberRoot, driveRoot, project := boundMountRoot(t)
	d := runnerDriver([]string{driveRoot})
	elsewhere := runner.Mount{Source: project, Target: "/home/agent/work", ReadOnly: true, MemberAuthored: true,
		BoundRunner: "99999999-2222-3333-4444-555555555555"}
	if got, err := d.agentMounts(context.Background(), memberSpec([]runner.Mount{elsewhere}, memberRoot, nil), false); err == nil {
		t.Fatalf("a folder bound to another runner was bound here: %+v", got)
	}
}

func TestAgentMounts_RunnerHoldsOwnRoots(t *testing.T) {
	memberRoot, driveRoot, project := boundMountRoot(t)
	outsideOwn := filepath.Join(memberRoot, "app") // in the member's roots, not the owner's
	link := filepath.Join(driveRoot, "link")
	if err := os.Symlink(outsideOwn, link); err != nil {
		t.Fatal(err)
	}
	at := func(src string) []runner.Mount {
		return []runner.Mount{{Source: src, Target: "/home/agent/work", ReadOnly: true, MemberAuthored: true, BoundRunner: thisRunner}}
	}
	for _, tc := range []struct {
		name       string
		driveRoots []string
		src        string
		refused    bool
	}{
		{"inside the owner's --drive-root", []string{driveRoot}, project, false},
		{"a runner with no --drive-root", nil, project, true},
		{"in the member's roots, outside the owner's", []string{driveRoot}, outsideOwn, true},
		{"a link out of the owner's roots", []string{driveRoot}, link, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := runnerDriver(tc.driveRoots).agentMounts(context.Background(), memberSpec(at(tc.src), memberRoot, nil), false)
			if tc.refused != (err != nil) {
				t.Fatalf("agentMounts = %+v, %v; want refused=%v", got, err, tc.refused)
			}
		})
	}
}

func TestAgentMounts_MemberWritableWithoutCeilingRefused(t *testing.T) {
	memberRoot, driveRoot, project := boundMountRoot(t)
	d := runnerDriver([]string{driveRoot})
	writable := []runner.Mount{{Source: project, Target: "/home/agent/work", MemberAuthored: true, BoundRunner: thisRunner}}
	if got, err := d.agentMounts(context.Background(), memberSpec(writable, memberRoot, nil), false); err == nil || !strings.Contains(err.Error(), "writable") {
		t.Fatalf("a writable member folder with no writable ceiling = %+v, %v; want the writable refusal", got, err)
	}
	got, err := d.agentMounts(context.Background(), memberSpec(writable, memberRoot, &runner.UserWritableCeiling{Roots: []string{memberRoot}}), false)
	if err != nil {
		t.Fatalf("a writable member folder inside the ceiling = %v, want bound", err)
	}
	assertMountSet(t, got, []mountSummary{{Target: "/home/agent/work", Source: project, ReadOnly: false}})
}

// Guard: on wardynd's host (no RunnerID) operator mounts and unbound member folders bind as before,
// and the deployment's drive roots (a different list there) never hold a member folder.
func TestAgentMounts_WardyndHostUnchanged(t *testing.T) {
	memberRoot, project := memberSandboxRoot(t)
	shares, _ := driveHostRoot(t)
	d := newWithClient(newFakeDocker(), Config{ProxyImage: "wardyn-proxy:dev", UserDriveHostRoots: []string{shares}})
	spec := memberSpec([]runner.Mount{
		{Source: project, Target: "/home/agent/work", ReadOnly: true, MemberAuthored: true},
		{Source: memberRoot, Target: "/home/agent/all", MemberAuthored: true},
		{Source: "/home/operator/.aws", Target: "/home/agent/.aws", ReadOnly: true},
	}, memberRoot, &runner.UserWritableCeiling{Roots: []string{memberRoot}})
	got, err := d.agentMounts(context.Background(), spec, false)
	if err != nil {
		t.Fatalf("wardynd host: %v", err)
	}
	assertMountSet(t, got, []mountSummary{
		{Target: "/home/agent/work", Source: project, ReadOnly: true},
		{Target: "/home/agent/all", Source: memberRoot, ReadOnly: false},
		{Target: "/home/agent/.aws", Source: "/home/operator/.aws", ReadOnly: true},
	})
	operator := testSpec()
	operator.Mounts = []runner.Mount{{Source: "/home/maintainer/repo", Target: "/home/agent/work"}}
	if _, err := d.agentMounts(context.Background(), operator, false); err != nil {
		t.Fatalf("wardynd host, operator run: %v", err)
	}
}

// A folder bound to a runner is a path on that runner; the same string on wardynd's host is some
// other folder, or nothing, so it never binds here.
func TestAgentMounts_WardyndHostRefusesBoundMount(t *testing.T) {
	memberRoot, project := memberSandboxRoot(t)
	d := newTestDriver(newFakeDocker())
	bound := []runner.Mount{{Source: project, Target: "/home/agent/work", ReadOnly: true, MemberAuthored: true, BoundRunner: thisRunner}}
	if got, err := d.agentMounts(context.Background(), memberSpec(bound, memberRoot, nil), false); err == nil {
		t.Fatalf("a runner-bound folder was bound on wardynd's host: %+v", got)
	}
}

// Drives never enter the agent-mount loop, so the non-member rule leaves them alone.
func TestAgentMounts_RunnerDrivesUnaffectedByG2(t *testing.T) {
	root, home := driveHostRoot(t)
	project := filepath.Join(root, "work", "app")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	d := runnerDriver([]string{root})
	spec := testSpec()
	spec.Drives = []types.DriveMount{*hostPathDrive(home)}
	got, err := d.agentMounts(context.Background(), spec, false)
	if err != nil {
		t.Fatalf("a drive on a runner: %v", err)
	}
	assertMountSet(t, got, []mountSummary{{Target: runner.DriveTarget, Source: home, ReadOnly: true}})

	spec = memberSpec([]runner.Mount{{Source: project, Target: "/home/agent/work", ReadOnly: true, MemberAuthored: true, BoundRunner: thisRunner}},
		filepath.Join(root, "work"), nil)
	spec.Drives = []types.DriveMount{*hostPathDrive(home)}
	if got, err = d.agentMounts(context.Background(), spec, false); err != nil {
		t.Fatalf("a drive beside a bound folder on a runner: %v", err)
	}
	assertMountSet(t, got, []mountSummary{
		{Target: runner.DriveTarget, Source: home, ReadOnly: true},
		{Target: "/home/agent/work", Source: project, ReadOnly: true},
	})
}

// The recording mount is the runner's own, appended before the loop, so the non-member rule never
// reaches it.
func TestAgentMounts_RunnerRecordingMountUnaffected(t *testing.T) {
	memberRoot, driveRoot, project := boundMountRoot(t)
	rec := filepath.Join(filepath.Dir(memberRoot), "recordings")
	if err := os.MkdirAll(rec, 0o755); err != nil {
		t.Fatal(err)
	}
	d := newWithClient(newFakeDocker(), Config{ProxyImage: "wardyn-proxy:dev", RunnerID: thisRunner,
		UserDriveHostRoots: []string{driveRoot}, RecordingMount: rec})
	for name, spec := range map[string]runner.SandboxSpec{
		"alone": testSpec(),
		"beside a bound folder": memberSpec([]runner.Mount{{Source: project, Target: "/home/agent/work", ReadOnly: true,
			MemberAuthored: true, BoundRunner: thisRunner}}, memberRoot, nil),
	} {
		got, err := d.agentMounts(context.Background(), spec, false)
		if err != nil {
			t.Fatalf("%s: the recording mount on a runner: %v", name, err)
		}
		var found bool
		for _, m := range got {
			found = found || m.Target == RecordingMountTarget && m.Source == rec && m.Type == mount.TypeBind
		}
		if !found {
			t.Errorf("%s: mounts %+v carry no recording bind of %s", name, got, rec)
		}
	}
}

// A Linux VM's folders are the container's binds (vm.go reuses agentMounts), so on a runner only a
// member folder bound to it is shared, and the shares are exactly those folders.
func TestVMShares_BoundFolderOnly(t *testing.T) {
	memberRoot, driveRoot, project := boundMountRoot(t)
	bound := runner.Mount{Source: project, Target: "/home/agent/work", ReadOnly: true, MemberAuthored: true, BoundRunner: thisRunner}
	for name, tc := range map[string]struct {
		mounts  []runner.Mount
		refused bool
	}{
		"a folder bound here":              {mounts: []runner.Mount{bound}},
		"a non-member folder":              {mounts: []runner.Mount{{Source: project, Target: "/home/agent/work", ReadOnly: true}}, refused: true},
		"a folder bound to another":        {mounts: []runner.Mount{{Source: project, Target: "/home/agent/work", ReadOnly: true, MemberAuthored: true, BoundRunner: "99999999-2222-3333-4444-555555555555"}}, refused: true},
		"outside the owner's --drive-root": {mounts: []runner.Mount{{Source: filepath.Join(memberRoot, "app"), Target: "/home/agent/work", ReadOnly: true, MemberAuthored: true, BoundRunner: thisRunner}}, refused: true},
	} {
		t.Run(name, func(t *testing.T) {
			r := newVMRig(t)
			r.d.cfg.RunnerID, r.d.cfg.UserDriveHostRoots = thisRunner, []string{driveRoot}
			r.d.cfg.DecisionSink, r.d.cfg.Evidence = newFakeSink(), &memEvidence{m: map[uuid.UUID][]byte{}}
			spec := vmSpec()
			spec.Mounts, spec.UserMountRoots = tc.mounts, []string{memberRoot}
			_, err := r.d.CreateSandbox(context.Background(), spec)
			if tc.refused {
				if err == nil {
					t.Fatal("accepted")
				}
				if len(r.f.containers) != 0 || len(r.f.networks) != 0 {
					t.Fatalf("a refused VM created %d containers, %d networks", len(r.f.containers), len(r.f.networks))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(r.mounts) != 1 || r.mounts[0].Target != "/home/agent/work" || !r.mounts[0].ReadOnly {
				t.Fatalf("guest mounts %+v, want the one bound folder, read-only", r.mounts)
			}
		})
	}
}
