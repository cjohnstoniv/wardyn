// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
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
		HostRoot:    "/srv/wardyn-drives",
		DriveName:   "nas",
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
	// The ceiling argument the policy lane made mandatory: dispatchRun grew it
	// when the governance ceiling stopped being two optional dispatchParams
	// fields and became a parameter every door must pass. The zero ceiling is
	// what every other dispatch test uses — this test is about drive
	// composition, not governance.
	srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}), dispatchParams{
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
	// THE ROW'S ONE RENDERED DETAIL, and the one field whose reader is not the
	// operator. The console's Audit tab draws a row from time, actor, action and
	// Target and reads nothing out of Data, and auditScope lets a run's CREATOR
	// read their own run's rows — so for a SHARE the target used to hand the
	// member `/srv/wardyn-drives/alice`, the operator's filesystem layout, on
	// their own run page. Two sites in this same tree refuse to disclose exactly
	// that to exactly that reader (driveShareIsBindable names the home, never the
	// resolved path; applyUserDriveEnv carries the target and the mode and
	// nothing else), so the target names the DRIVE and the DIRECTORY instead.
	if ev.Target != "nas/alice" {
		t.Errorf("run.drive.mount target = %q, want \"nas/alice\" — a share's absolute host path is the operator's layout, "+
			"and the member reads this row", ev.Target)
	}
	if strings.Contains(ev.Target, drive.HostRoot) {
		t.Errorf("run.drive.mount target = %q leaks the share's host root %q to the member", ev.Target, drive.HostRoot)
	}
	// …AND THE PAYLOAD IS NOT A SIDE DOOR. Masking the Target while `object`
	// still spelled the absolute path moved the operator's layout one field over
	// inside the SAME row: GET /audit?run_id= hands the creator the whole event,
	// data included, so `object` carries the masked name too. The operator reads
	// the root from GET /drives, which is operator-only and already shows it.
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatalf("run.drive.mount payload is not an object: %v (%s)", err, ev.Data)
	}
	want := map[string]any{
		"backend":     "host_path",
		"drive":       "alice",
		"enforcement": "external",
		"mode":        "ro",
		"object":      "nas/alice",
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
	// The whole row, not one field of it: nothing a member can fetch may spell
	// the share's root.
	if strings.Contains(string(ev.Data), drive.HostRoot) {
		t.Errorf("run.drive.mount payload = %s leaks the share's host root %q to the member", ev.Data, drive.HostRoot)
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
//
// Walked over EVERY backend, and over every admin-facing field a DriveMount
// carries, because the value is built by formatting and the tempting change is
// always the same one: adding the object to the string so an agent can "see
// where it is". A managed backend's object name is Wardyn's own and looks
// harmless; a share's IS the operator's absolute host path, and the four
// backends share one line of code.
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

	for _, backend := range types.DriveBackends {
		d := &types.DriveMount{
			DriveID: uuid.New(), Backend: backend, ObjectName: "object-for-" + string(backend),
			StorageClass: "fast-block", DriveName: "nas", HomeName: "alice-home",
			SubjectHash: "0123456789abcdef0123", Target: runner.DriveTarget,
			ReadOnly: backend == types.DriveBackendHostPath, SizeMiB: 10,
			Enforcement: types.EnforcementFor(backend),
		}
		// A pre-existing key, so "adds exactly one" is a statement about this
		// function rather than about the map it was handed.
		got := map[string]string{"KEEP": "1"}
		applyUserDriveEnv(got, d)
		if len(got) != 2 {
			t.Errorf("%s: env = %v, want exactly one drive key beside KEEP", backend, got)
			continue
		}
		mode := "rw"
		if d.ReadOnly {
			mode = "ro"
		}
		value := got["WARDYN_USER_DRIVE"]
		if want := runner.DriveTarget + ":" + mode; value != want {
			t.Errorf("%s: WARDYN_USER_DRIVE = %q, want %q", backend, value, want)
		}
		for _, leak := range []string{d.ObjectName, d.HomeName, d.DriveName, d.DriveID.String(), d.StorageClass, d.SubjectHash} {
			if strings.Contains(value, leak) {
				t.Errorf("%s: WARDYN_USER_DRIVE = %q carries the admin-facing %q", backend, value, leak)
			}
		}
	}

	// And through the whole composition, which is the only place the claim
	// "nothing else in the sandbox names the drive" can actually be made: the
	// composed env is assembled from several sources, and the credential half
	// (SecretEnv) is a second map with its own writers.
	spec, _ := runDriveDispatch(t, dispatchDrive(false))
	share := dispatchDrive(false)
	keys := 0
	for k, v := range spec.Env {
		if strings.Contains(strings.ToUpper(k), "DRIVE") {
			keys++
		}
		if strings.Contains(v, share.HostRoot) {
			t.Errorf("composed env %s = %q carries the share's host root", k, v)
		}
	}
	if keys != 1 {
		t.Errorf("composed env has %d drive-related keys, want exactly WARDYN_USER_DRIVE: %v", keys, spec.Env)
	}
	for k, v := range spec.SecretEnv {
		if strings.Contains(strings.ToUpper(k), "DRIVE") || strings.Contains(v, share.HostRoot) {
			t.Errorf("secret env carries the drive (%s)", k)
		}
	}
}

// TestDispatch_TheMemberCannotReadTheShareHostPathFromTheirOwnRun closes the
// loop the masking exists for. TestDispatch_DriveReachesSpecEnvAndAudit pins
// the row dispatch WRITES; this pins the row a member READS, through the
// handler and the scope gate that let them.
//
// The two are separable, and that gap is where the disclosure lived: auditScope
// hands a run's CREATOR every row of their own run, whole — Target and Data
// both — so masking the Target while `object` still spelled the absolute path
// moved the operator's filesystem layout one field over inside the same
// response. The event here is the REAL one dispatch emitted rather than a
// hand-written fixture, so the handler and the emitter cannot drift apart
// without this failing.
func TestDispatch_TheMemberCannotReadTheShareHostPathFromTheirOwnRun(t *testing.T) {
	const memberSub = "sub-drive-member"
	drive := dispatchDrive(true)
	spec, events := runDriveDispatch(t, drive)
	ev := findAudit(events, spec.RunID, "run.drive.mount", "success")
	if ev == nil {
		t.Fatalf("dispatch recorded no run.drive.mount; events=%s", auditDump(events, spec.RunID))
	}

	h := newHarness(t)
	st := &auditScopeStore{runs: map[uuid.UUID]types.AgentRun{spec.RunID: {ID: spec.RunID, CreatedBy: memberSub}}}
	st.auditByRun = map[uuid.UUID][]types.AuditEvent{spec.RunID: {*ev}}
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	srv := New(cfg)

	member := ssoSession(t, memberSub, "alice@corp.example", oidc.RoleMember)
	w := doSSO(t, srv, http.MethodGet, "/api/v1/audit?run_id="+spec.RunID.String(), member, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /audit as the run's creator = %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// The row really is there — otherwise every assertion below passes on an
	// empty page.
	if !strings.Contains(body, "run.drive.mount") {
		t.Fatalf("the member's own run's mount row is missing: %s", body)
	}
	if !strings.Contains(body, "nas/alice") {
		t.Errorf("the row does not name the drive and the directory: %s", body)
	}
	for _, leak := range []string{drive.HostRoot, drive.ObjectName} {
		if strings.Contains(body, leak) {
			t.Errorf("the member read back %q from their own run's audit trail: %s", leak, body)
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

// TestDispatch_ManagedDriveAuditsTheObjectName is the other half of the target
// ruling, and the reason it is a ruling rather than a blanket mask: a MANAGED
// object's name is Wardyn's own (`wardyn-drive-<home>` on Docker,
// `wardyn-drive-<slug>-<home>` on Kubernetes). It says which volume or claim
// this run was handed, it is the exact string an operator's reclaim command
// takes, and it discloses nothing about the host — so it goes on the row
// verbatim, and only a share's absolute host path is masked.
func TestDispatch_ManagedDriveAuditsTheObjectName(t *testing.T) {
	drive := dispatchDrive(true)
	drive.Backend, drive.ObjectName, drive.HostRoot = types.DriveBackendDockerVolume, "wardyn-drive-alice", ""
	drive.Enforcement = types.StorageEnforcementNone
	spec, events := runDriveDispatch(t, drive)

	ev := findAudit(events, spec.RunID, "run.drive.mount", "success")
	if ev == nil {
		t.Fatalf("dispatch recorded no run.drive.mount; events=%s", auditDump(events, spec.RunID))
	}
	if ev.Target != "wardyn-drive-alice" {
		t.Errorf("run.drive.mount target = %q, want the managed object's own name — it names no host path, so there is "+
			"nothing to mask and the operator's reclaim command reads it straight off the row", ev.Target)
	}
	// And the payload's `object` is UNCHANGED on this arm — the masking is the
	// share's alone, so a managed row still hands the reclaim command the exact
	// volume name.
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatalf("run.drive.mount payload is not an object: %v (%s)", err, ev.Data)
	}
	if data["object"] != "wardyn-drive-alice" {
		t.Errorf("run.drive.mount payload[object] = %v, want the managed object verbatim (full payload: %s)", data["object"], ev.Data)
	}
}
