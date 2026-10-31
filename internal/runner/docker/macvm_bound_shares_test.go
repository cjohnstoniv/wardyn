// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && unix

package docker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The macOS VM twins of driver_bound_mount_test.go: a share is a bind by another name, so on a runner
// only a member folder bound to it, inside the owner's --drive-root, is shared, a writable one only
// under a ceiling; drives keep their own rules; and wardynd's host never shares a runner-bound folder.

// macRunnerRig is a macOS VM rig whose driver serves thisRunner with driveRoots as its --drive-root.
func macRunnerRig(t *testing.T, driveRoots []string) *macRig {
	t.Helper()
	r := newMacRig(t)
	r.d.cfg.RunnerID, r.d.cfg.UserDriveHostRoots = thisRunner, driveRoots
	return r
}

func macMemberSpec(mounts []runner.Mount, memberRoot string, writable *runner.UserWritableCeiling) runner.SandboxSpec {
	spec := macSpec()
	spec.Mounts, spec.UserMountRoots, spec.UserWritable = mounts, []string{memberRoot}, writable
	return spec
}

func TestMacShares_RunnerRefusesNonMemberMount(t *testing.T) {
	memberRoot, driveRoot, project := boundMountRoot(t)
	r := macRunnerRig(t, []string{driveRoot})
	bound := runner.Mount{Source: project, Target: "/home/agent/work", ReadOnly: true, MemberAuthored: true, BoundRunner: thisRunner}
	shares, _, err := r.d.macShares(context.Background(), macMemberSpec([]runner.Mount{bound}, memberRoot, nil))
	if err != nil || len(shares) != 1 || shares[0].HostPath != project || !shares[0].ReadOnly {
		t.Fatalf("a member folder bound to this runner: shares %+v, %v; want it shared read-only", shares, err)
	}
	operator := runner.Mount{Source: project, Target: "/home/agent/work", ReadOnly: true}
	noRoots := macSpec()
	noRoots.Mounts = []runner.Mount{operator}
	for name, spec := range map[string]runner.SandboxSpec{
		"with member roots": macMemberSpec([]runner.Mount{operator}, memberRoot, nil),
		"with no roots":     noRoots,
	} {
		if shares, _, err := r.d.macShares(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "denied") {
			t.Errorf("%s: a non-member share on a runner = %+v, %v; want refused", name, shares, err)
		}
	}
}

func TestMacShares_RunnerRefusesMountBoundElsewhere(t *testing.T) {
	memberRoot, driveRoot, project := boundMountRoot(t)
	r := macRunnerRig(t, []string{driveRoot})
	elsewhere := runner.Mount{Source: project, Target: "/home/agent/work", ReadOnly: true, MemberAuthored: true,
		BoundRunner: "99999999-2222-3333-4444-555555555555"}
	if shares, _, err := r.d.macShares(context.Background(), macMemberSpec([]runner.Mount{elsewhere}, memberRoot, nil)); err == nil {
		t.Fatalf("a folder bound to another runner was shared here: %+v", shares)
	}
}

func TestMacShares_RunnerHoldsOwnRoots(t *testing.T) {
	memberRoot, driveRoot, project := boundMountRoot(t)
	outsideOwn := filepath.Join(memberRoot, "app")
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := macRunnerRig(t, tc.driveRoots)
			shares, _, err := r.d.macShares(context.Background(), macMemberSpec(at(tc.src), memberRoot, nil))
			if tc.refused != (err != nil) {
				t.Fatalf("macShares = %+v, %v; want refused=%v", shares, err, tc.refused)
			}
		})
	}
}

func TestMacShares_MemberWritableWithoutCeilingRefused(t *testing.T) {
	memberRoot, driveRoot, project := boundMountRoot(t)
	r := macRunnerRig(t, []string{driveRoot})
	writable := []runner.Mount{{Source: project, Target: "/home/agent/work", MemberAuthored: true, BoundRunner: thisRunner}}
	if shares, _, err := r.d.macShares(context.Background(), macMemberSpec(writable, memberRoot, nil)); err == nil || !strings.Contains(err.Error(), "writable") {
		t.Fatalf("a writable member share with no writable ceiling = %+v, %v; want the writable refusal", shares, err)
	}
	shares, _, err := r.d.macShares(context.Background(), macMemberSpec(writable, memberRoot, &runner.UserWritableCeiling{Roots: []string{memberRoot}}))
	if err != nil || len(shares) != 1 || shares[0].ReadOnly {
		t.Fatalf("a writable member share inside the ceiling: shares %+v, %v; want it shared writable", shares, err)
	}
}

// Drives never enter the share loop for mounts, so the non-member rule leaves them alone.
func TestMacShares_RunnerDrivesUnaffectedByG2(t *testing.T) {
	root, home := driveHostRoot(t)
	project := filepath.Join(root, "work", "app")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	r := macRunnerRig(t, []string{root})
	spec := macSpec()
	spec.Drives = []types.DriveMount{shareDriveAt(home, runner.DriveTarget, true)}
	shares, _, err := r.d.macShares(context.Background(), spec)
	if err != nil || len(shares) != 1 || shares[0].HostPath != home {
		t.Fatalf("a drive on a runner: shares %+v, %v; want the drive shared", shares, err)
	}
	spec = macMemberSpec([]runner.Mount{{Source: project, Target: "/home/agent/work", ReadOnly: true, MemberAuthored: true, BoundRunner: thisRunner}},
		filepath.Join(root, "work"), nil)
	spec.Drives = []types.DriveMount{shareDriveAt(home, runner.DriveTarget, true)}
	if shares, _, err = r.d.macShares(context.Background(), spec); err != nil || len(shares) != 2 {
		t.Fatalf("a drive beside a bound folder on a runner: shares %+v, %v; want both", shares, err)
	}
}

func TestMacShares_WardyndHostRefusesBoundMount(t *testing.T) {
	memberRoot, project := memberSandboxRoot(t)
	r := newMacRig(t) // no RunnerID: wardynd's host
	bound := []runner.Mount{{Source: project, Target: "/home/agent/work", ReadOnly: true, MemberAuthored: true, BoundRunner: thisRunner}}
	if shares, _, err := r.d.macShares(context.Background(), macMemberSpec(bound, memberRoot, nil)); err == nil {
		t.Fatalf("a runner-bound folder was shared on wardynd's host: %+v", shares)
	}
	// Guard: the same folder unbound is shared there as before.
	unbound := []runner.Mount{{Source: project, Target: "/home/agent/work", ReadOnly: true, MemberAuthored: true}}
	if shares, _, err := r.d.macShares(context.Background(), macMemberSpec(unbound, memberRoot, nil)); err != nil || len(shares) != 1 {
		t.Fatalf("an unbound member folder on wardynd's host: shares %+v, %v; want it shared", shares, err)
	}
}
