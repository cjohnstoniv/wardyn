// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The dispatch half of a USER DRIVE (migration 0054): dispatchRun consumes the
// already-resolved types.DriveMount seedRequestDrive put on dispatchParams, and
// does exactly three things with it — puts it on the SandboxSpec for the
// driver, announces it to the sandbox as WARDYN_USER_DRIVE, and audits
// run.drive.mount. Nothing here resolves, derives or re-checks anything: the
// resolution happened at create, and the mount happens in the driver.

// dispatchDrive is a resolved drive in the shape the control plane hands
// dispatch — a SHARE drive, whose enforcement is the NAS's own quota.
func dispatchDrive(readOnly bool) *types.DriveMount {
	return &types.DriveMount{
		Backend:     types.DriveBackendHostPath,
		ObjectName:  "/srv/wardyn-drives/alice",
		HomeName:    "alice",
		Target:      runner.DriveTarget,
		ReadOnly:    readOnly,
		SizeMiB:     10240,
		Enforcement: types.StorageEnforcementExternal,
	}
}

// runDriveDispatch dispatches one run carrying drive (nil for none) and returns
// the SandboxSpec the runner received plus the audit trail.
func runDriveDispatch(t *testing.T, drive *types.DriveMount) (runner.SandboxSpec, []types.AuditEvent) {
	t.Helper()
	return runDriveDispatchWith(t, &fakeRunner{}, drive)
}

// runDriveDispatchWith is the same, on a caller-supplied runner — so a test can
// make CreateSandbox fail.
func runDriveDispatchWith(t *testing.T, fr *fakeRunner, drive *types.DriveMount) (runner.SandboxSpec, []types.AuditEvent) {
	t.Helper()
	srv, _, audit, run := dispatchTeardownFixture(t, fr, types.RunPending)
	run.Task = "" // no agent exec / completion watcher: this is about composition
	srv.dispatchRun(context.Background(), run, dispatchParams{
		RunToken: "run-token", Image: "wardyn/claude-code:latest",
		Drive: drive,
	})
	return fr.lastSpec, audit.events
}

// TestDispatch_DriveReachesSpecEnvAndAudit is the whole dispatch contract in
// one pass: the drive rides SandboxSpec.Drive (never spec.Mounts — a drive is
// not a workspace mount and must not become one), the sandbox is told where it
// is and in which mode, and the attachment is on the audit feed with the five
// fields an operator reclaiming months later needs.
func TestDispatch_DriveReachesSpecEnvAndAudit(t *testing.T) {
	drive := dispatchDrive(true)
	spec, events := runDriveDispatch(t, drive)

	if spec.Drive == nil {
		t.Fatal("SandboxSpec.Drive is nil — the runner was handed no drive to mount")
	}
	if spec.Drive.ObjectName != drive.ObjectName || spec.Drive.Target != runner.DriveTarget {
		t.Errorf("SandboxSpec.Drive = %+v, want the resolved drive verbatim", *spec.Drive)
	}
	for _, m := range spec.Mounts {
		if m.Target == runner.DriveTarget {
			t.Errorf("the drive leaked into SandboxSpec.Mounts as %+v — it must ride its own field, "+
				"or the composer clamp and the workspace-source allow-list would each need a drive exemption", m)
		}
	}

	if got, want := spec.Env["WARDYN_USER_DRIVE"], "/home/agent/drive:ro"; got != want {
		t.Errorf("WARDYN_USER_DRIVE = %q, want %q", got, want)
	}

	ev := findAudit(events, spec.RunID, "run.drive.mount", "success")
	if ev == nil {
		t.Fatalf("dispatch recorded no run.drive.mount; events=%s", auditDump(events, spec.RunID))
	}
	if ev.ActorType != types.ActorSystem {
		t.Errorf("run.drive.mount actor = %q, want %q — a member ticked a checkbox, dispatch resolved it into an object",
			ev.ActorType, types.ActorSystem)
	}
	// THE ROW'S ONE RENDERED DETAIL. The console's Audit tab draws a row from
	// time, actor, action and Target and reads nothing out of Data, so a Target
	// set to the run id (which the event already carries) leaves the object name
	// on no screen at all — and "which storage did this run mount" is the whole
	// question this action exists to answer.
	if ev.Target != drive.ObjectName {
		t.Errorf("run.drive.mount target = %q, want the storage object %q — the run id is already on the event, and the Audit tab renders nothing from data",
			ev.Target, drive.ObjectName)
	}
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatalf("run.drive.mount payload is not an object: %v (%s)", err, ev.Data)
	}
	want := map[string]any{
		"backend":     "host_path",
		"drive":       "alice",
		"enforcement": "external",
		"mode":        "ro",
		"object":      "/srv/wardyn-drives/alice",
	}
	for k, v := range want {
		if data[k] != v {
			t.Errorf("run.drive.mount payload[%q] = %v, want %v (full payload: %s)", k, data[k], v, ev.Data)
		}
	}
	for k := range data {
		if _, ok := want[k]; !ok {
			t.Errorf("run.drive.mount payload carries an undocumented field %q — docs/AUDIT-ACTIONS.md lists five", k)
		}
	}
}

// TestDispatch_WritableDriveAnnouncesRW: the mode the sandbox is told is the
// mode the mount actually has, in both directions.
func TestDispatch_WritableDriveAnnouncesRW(t *testing.T) {
	spec, events := runDriveDispatch(t, dispatchDrive(false))
	if got, want := spec.Env["WARDYN_USER_DRIVE"], "/home/agent/drive:rw"; got != want {
		t.Errorf("WARDYN_USER_DRIVE = %q, want %q", got, want)
	}
	ev := findAudit(events, spec.RunID, "run.drive.mount", "success")
	if ev == nil {
		t.Fatalf("no run.drive.mount; events=%s", auditDump(events, spec.RunID))
	}
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if data["mode"] != "rw" {
		t.Errorf("run.drive.mount mode = %v, want rw", data["mode"])
	}
}

// TestDispatch_NoDriveIsSilent: the overwhelmingly common run — no drive
// requested, or a deployment that has allocated none — must add no env var and
// write no audit row. An audit action that fires on every run is noise the
// operator learns to skip past.
func TestDispatch_NoDriveIsSilent(t *testing.T) {
	spec, events := runDriveDispatch(t, nil)
	if spec.Drive != nil {
		t.Errorf("SandboxSpec.Drive = %+v, want nil", *spec.Drive)
	}
	if v, ok := spec.Env["WARDYN_USER_DRIVE"]; ok {
		t.Errorf("WARDYN_USER_DRIVE = %q on a run with no drive, want absent", v)
	}
	if ev := findAudit(events, spec.RunID, "run.drive.mount", "success"); ev != nil {
		t.Errorf("a run with no drive audited run.drive.mount: %s", ev.Data)
	}
}

// TestApplyUserDriveEnv is the unit-level pin on the announcement itself: the
// value is the TARGET and the MODE and nothing else. A drive's object name,
// host path and human name are admin-facing — the member's own request carried
// a flag, never a path, and the sandbox env must not hand back what the request
// was not allowed to name.
func TestApplyUserDriveEnv(t *testing.T) {
	env := map[string]string{}
	applyUserDriveEnv(env, nil)
	if _, ok := env["WARDYN_USER_DRIVE"]; ok {
		t.Errorf("no drive must add nothing, got env = %v", env)
	}

	drive := dispatchDrive(true)
	applyUserDriveEnv(env, drive)
	if got, want := env["WARDYN_USER_DRIVE"], runner.DriveTarget+":ro"; got != want {
		t.Fatalf("WARDYN_USER_DRIVE = %q, want %q", got, want)
	}
	for _, leak := range []string{drive.ObjectName, drive.HomeName} {
		if strings.Contains(env["WARDYN_USER_DRIVE"], leak) {
			t.Errorf("WARDYN_USER_DRIVE = %q leaks the admin-facing %q", env["WARDYN_USER_DRIVE"], leak)
		}
	}
}

// TestDispatch_DriveIsNotAuditedWhenTheSandboxFails pins the ORDER the emit
// site's comment argues for, which nothing else held: run.drive.mount is
// written AFTER CreateSandbox returns, never beside the spec that carries the
// drive.
//
// The driver has the last word on whether a drive is actually bound — it re-runs
// the host-root ceiling and the bind deny-list on the symlink-resolved real
// path as the last thing before the container is created, and a share
// re-pointed since the drive row was written is refused THERE. Emitted earlier,
// the feed carried a `success` row for a mount that the very next event
// contradicted, and an operator reading back "which run mounted whose storage"
// months later would have believed the row rather than the run.
//
// Two assertions, both load-bearing: no mount row at all, and the run.create
// failure that says why.
func TestDispatch_DriveIsNotAuditedWhenTheSandboxFails(t *testing.T) {
	fr := &fakeRunner{createErr: errors.New("denied user drive mount: outside every configured root")}
	spec, events := runDriveDispatchWith(t, fr, dispatchDrive(false))

	if spec.Drive == nil {
		t.Fatal("the drive never reached SandboxSpec — this test would then pass for the wrong reason")
	}
	if ev := findAudit(events, spec.RunID, "run.drive.mount", "success"); ev != nil {
		t.Errorf("a run whose CreateSandbox FAILED audited run.drive.mount: %s\n"+
			"The row must be emitted after CreateSandbox returns — the driver can still refuse the drive, and a success row here is a claim the next event contradicts.", ev.Data)
	}
	if ev := findAudit(events, spec.RunID, "run.create", "failure"); ev == nil {
		t.Errorf("no run.create failure row; events=%s", auditDump(events, spec.RunID))
	}
}
