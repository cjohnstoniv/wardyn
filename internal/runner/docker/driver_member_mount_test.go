// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// The BIND-TIME half of the member-safe mount gate
// (docs/design/member-role-desktop.md §c, matrix row 3). These tests exist
// because the create-run check alone is not the guarantee: a symlink that was
// benign when the workspace was onboarded can be repointed before the run, so
// the resolved-real-path within-root assertion has to run HERE, as the last
// thing before the bind reaches ContainerCreate.
//
// Row 3 asks for the ORDERING to be tested, not the race (which cannot be run
// deterministically). That is exactly what "no agent container exists after the
// refusal" asserts: a check that ran after the append — or that ran only at
// create-run — would have let CreateSandbox build the container.

// memberSandboxRoot makes a real root with a project dir under it, resolved so
// the assertions are about the gate rather than about a symlinked tempdir.
func memberSandboxRoot(t *testing.T) (root, project string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve tempdir: %v", err)
	}
	root = filepath.Join(base, "projects")
	project = filepath.Join(root, "app")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return root, project
}

// createMemberSandbox runs CreateSandbox for a run carrying memberRoots and
// reports whether the agent container was actually created.
func createMemberSandbox(t *testing.T, mounts []runner.Mount, memberRoots []string) (created bool, err error) {
	t.Helper()
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newTestDriver(f)

	spec := testSpec()
	spec.Mounts = mounts
	spec.MemberMountRoots = memberRoots
	_, err = d.CreateSandbox(context.Background(), spec)
	return f.containers[agentContainerName(spec.RunID)] != nil, err
}

// TestCreateSandbox_MemberMountWithinRootApplied is the positive control: a
// member run whose source really is inside its roots still boots.
func TestCreateSandbox_MemberMountWithinRootApplied(t *testing.T) {
	root, project := memberSandboxRoot(t)
	created, err := createMemberSandbox(t,
		[]runner.Mount{{Source: project, Target: "/home/agent/work", ReadOnly: true, MemberAuthored: true}},
		[]string{root})
	if err != nil {
		t.Fatalf("member mount inside its root failed: %v", err)
	}
	if !created {
		t.Fatal("agent container was not created for an allowed member mount")
	}
}

// TestCreateSandbox_MemberMountRepointedBeforeBind is matrix row 3. The source
// passed every check at authoring time; between then and now it was replaced by
// a symlink out of the roots. CreateSandbox must fail closed AND must not have
// created the container — i.e. the check ran before the append, at the bind
// site, on the RESOLVED path.
func TestCreateSandbox_MemberMountRepointedBeforeBind(t *testing.T) {
	root, project := memberSandboxRoot(t)
	outside := filepath.Join(filepath.Dir(root), "elsewhere")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// The repoint: same path string the run was created with, different target.
	if err := os.Remove(project); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Symlink(outside, project); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}

	created, err := createMemberSandbox(t,
		[]runner.Mount{{Source: project, Target: "/home/agent/work", ReadOnly: true, MemberAuthored: true}},
		[]string{root})
	if err == nil {
		t.Fatal("a member mount repointed OUT of its roots was bound; the within-root check must re-resolve at bind time")
	}
	if !strings.Contains(err.Error(), "denied member workspace mount") {
		t.Errorf("error = %v, want the driver's member-mount refusal", err)
	}
	if created {
		t.Error("the agent container was created despite the refusal — the member check must run BEFORE the mount is appended and the container built")
	}
}

// TestCreateSandbox_MemberMountNoRootsFailsClosed pins the "member run, roots
// removed" case: memberMountRoots resolves to an EMPTY-but-non-nil slice so the
// run still reaches the driver as a member run, and every bind is refused —
// rather than degrading to the operator path because the config went away.
func TestCreateSandbox_MemberMountNoRootsFailsClosed(t *testing.T) {
	_, project := memberSandboxRoot(t)
	created, err := createMemberSandbox(t,
		[]runner.Mount{{Source: project, Target: "/home/agent/work", ReadOnly: true, MemberAuthored: true}},
		[]string{})
	if err == nil {
		t.Fatal("a member run with an empty (non-nil) root list bound its mount; it must fail closed")
	}
	if created {
		t.Error("the agent container was created despite the refusal")
	}
}

// TestCreateSandbox_SystemMountOnMemberRunApplied is the other half of "the
// gate is additive": a MEMBER run's spec also carries binds WARDYN authored —
// the operator's staged ~/.claude subscription creds and the Bedrock ~/.aws dir
// — whose sources are outside every member root by construction. Gating those
// on the roots too refused them at ContainerCreate, so on a subscription or
// Bedrock deployment NO run against a member-owned workspace could start, an
// admin's own record/verify session included. Only the member-authored bind is
// re-checked.
func TestCreateSandbox_SystemMountOnMemberRunApplied(t *testing.T) {
	root, project := memberSandboxRoot(t)
	created, err := createMemberSandbox(t, []runner.Mount{
		{Source: "/var/lib/wardyn/claude-creds", Target: "/home/agent/.claude", ReadOnly: true},
		{Source: "/home/operator/.aws", Target: "/home/agent/.aws", ReadOnly: true},
		{Source: project, Target: "/home/agent/work", ReadOnly: true, MemberAuthored: true},
	}, []string{root})
	if err != nil {
		t.Fatalf("member run with operator-authored credential mounts failed: %v — the roots bound the MEMBER's binds, not Wardyn's own", err)
	}
	if !created {
		t.Fatal("agent container was not created for a member run carrying the blessed credential mounts")
	}
}

// TestCreateSandbox_OperatorMountUnaffectedByMemberGate is matrix row 7: an
// operator run (nil MemberMountRoots) binds a legitimate source that is OUTSIDE
// every member root, exactly as it does today. The member gate must be purely
// additive — it never narrows an operator mount.
func TestCreateSandbox_OperatorMountUnaffectedByMemberGate(t *testing.T) {
	created, err := createMemberSandbox(t,
		[]runner.Mount{{Source: "/home/maintainer/repo", Target: "/home/agent/work", ReadOnly: false}},
		nil)
	if err != nil {
		t.Fatalf("operator mount outside every member root failed: %v — nil MemberMountRoots must take exactly today's path", err)
	}
	if !created {
		t.Fatal("agent container was not created for an operator mount")
	}
}
