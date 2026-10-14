// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// MemberDriveMount is the mount a person's own drive gets: a managed object
// named for the drive and its owner, as the control plane derives it at
// dispatch and again at delete.
func MemberDriveMount(t *testing.T, backend types.DriveBackend, owner string, sizeMiB int) types.DriveMount {
	t.Helper()
	d := types.UserDrive{ID: uuid.New(), Name: "Member drive", Backend: backend, SizeMiB: sizeMiB,
		HomeTemplate: types.HomeTemplateHash, ObjectScheme: types.DriveObjectSchemeID, Writable: true, OwnerSubject: owner}
	home, err := types.DriveHomeName(d, owner, "")
	if err != nil {
		t.Fatal(err)
	}
	return types.DriveMount{
		DriveID: d.ID, Backend: backend, ObjectName: types.DriveObjectName(d, home), DriveName: d.Name,
		HomeName: home, SubjectHash: types.DriveSubjectHash(owner), Target: runner.DrivesRoot + "/member-drive",
		SizeMiB: sizeMiB, Enforcement: types.EnforcementFor(backend),
	}
}

// RunMemberDriveLifecycle is the member-drive row. The substrate must say it
// can delete what it creates; a file run A writes is there for run B; while a
// sandbox holds the drive (kept after its end where the substrate keeps one,
// else running) ReclaimDrive refuses with ErrDriveInUse and the object stays;
// once nothing holds it ReclaimDrive deletes the object, and a second call
// finds it already absent. objectExists reads the substrate from outside the
// runner.
func RunMemberDriveLifecycle(t *testing.T, r runner.Runner, opts Options, mount types.DriveMount, objectExists func(t *testing.T) bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout())
	defer cancel()
	rc, ok := r.(runner.DriveReclaimer)
	lc, lok := r.(runner.DriveLifecycleChecker)
	if !ok || !lok {
		t.Fatalf("runner %q cannot reclaim or check drives", r.Name())
	}
	if err := lc.CanManageDrives(ctx); err != nil {
		t.Fatalf("CanManageDrives = %v, want nil: no member drive may be created here", err)
	}
	caps, err := r.Capabilities(ctx)
	if err != nil || !caps.UserDrives || len(caps.ConfinementClasses) == 0 {
		t.Fatalf("Capabilities = %+v %v, want UserDrives and a confinement class", caps, err)
	}
	withDrive := func(spec *runner.SandboxSpec) {
		spec.Image = opts.image()
		if opts.AgentUserImage != "" {
			spec.Image = opts.AgentUserImage
		}
		spec.Drives = []types.DriveMount{mount}
	}
	token := "wardyn-member-drive-" + uuid.NewString()

	a := createStrongestSandboxWith(t, ctx, r, caps, opts, "MemberDrive run A", withDrive)
	if got := ExecKeyValues(t, ctx, r, a.Ref, userDriveProbeScriptAt(mount.Target, token)); got["w_drive"] != "WROTE" {
		t.Fatalf("run A could not write its member drive: %v", got)
	}
	if err := r.StopSandbox(ctx, a.Ref); err != nil {
		t.Fatalf("StopSandbox(run A): %v", err)
	}

	b := createStrongestSandboxWith(t, ctx, r, caps, opts, "MemberDrive run B", withDrive)
	if got := ExecKeyValues(t, ctx, r, b.Ref, fmt.Sprintf("echo r_drive=$(cat %s/%s)\n", mount.Target, userDriveProbeFile)); got["r_drive"] != token {
		t.Fatalf("run B read %q from the member drive, want run A's %q", got["r_drive"], token)
	}
	if e, ok := r.(runner.SandboxEnder); ok {
		if err := e.EndSandbox(ctx, b.Ref); err != nil && !errors.Is(err, runner.ErrEndUnsupported) {
			t.Fatalf("EndSandbox(run B): %v", err)
		}
	}
	if _, err := rc.ReclaimDrive(ctx, mount); !errors.Is(err, runner.ErrDriveInUse) {
		t.Fatalf("ReclaimDrive while run B holds the drive = %v, want ErrDriveInUse", err)
	}
	if !objectExists(t) {
		t.Fatal("a refused delete removed the drive's object")
	}

	if err := r.StopSandbox(ctx, b.Ref); err != nil {
		t.Fatalf("StopSandbox(run B): %v", err)
	}
	if out, err := rc.ReclaimDrive(ctx, mount); err != nil || out != runner.DriveReclaimDeleted {
		t.Fatalf("ReclaimDrive with nothing holding it = %q %v, want deleted", out, err)
	}
	if objectExists(t) {
		t.Fatal("the drive's object is still there after a delete")
	}
	if out, err := rc.ReclaimDrive(ctx, mount); err != nil || out != runner.DriveReclaimAlreadyAbsent {
		t.Errorf("a second ReclaimDrive = %q %v, want already_absent", out, err)
	}
}
