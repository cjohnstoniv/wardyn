// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// ─── the seam harness ─────────────────────────────────────────────────────────

// driveRunServer builds the create-path server seedRequestDrive runs inside:
// the drive store double, an audit recorder (so the door's authz.denied row can
// be asserted, and its ABSENCE on every 422 arm), and the deployment's runner
// target.
func driveRunServer(st *driveStore, runnerTarget string) (*Server, *recRecorder) {
	audit := &recRecorder{}
	return New(Config{Store: st, Audit: audit, RunnerTarget: runnerTarget}), audit
}

// driveShareServer is driveRunServer for the host_path arm: the same seam with
// the boot-parsed env ceiling (WARDYN_USER_DRIVE_HOST_ROOTS) wired, which is
// the deployment fact driveShareIsBindable re-checks and the row cannot carry.
// Nil roots is the deployment that has un-set the variable since the drive was
// authored.
func driveShareServer(st *driveStore, roots []string) (*Server, *recRecorder) {
	audit := &recRecorder{}
	return New(Config{Store: st, Audit: audit, RunnerTarget: "docker", UserDriveHostRoots: roots}), audit
}

// driveRunRequest is a create-run request carrying the drive flag. readOnly nil
// leaves the allocation's own posture; non-nil is the NARROW-ONLY request field.
func driveRunRequest(enabled bool, readOnly *bool) createRunRequest {
	return createRunRequest{Agent: "claude-code", Task: "t", Drive: &client.DriveSelection{Enabled: enabled, ReadOnly: readOnly}}
}

// driveSeed runs seedRequestDrive as a signed-in MEMBER and hands back
// everything an assertion needs.
func driveSeed(t *testing.T, srv *Server, req createRunRequest, ceiling governanceCeiling,
	ctx context.Context) (*types.DriveMount, bool, *httptest.ResponseRecorder) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	mount, ok := srv.seedRequestDrive(w, r, req, ceiling)
	return mount, ok, w
}

// deniedCeiling is an ASSIGNED profile whose door is shut.
func deniedCeiling() governanceCeiling {
	return governanceCeiling{
		Profile: &types.GovernanceProfile{Name: "contractors"},
		Limits:  types.GovernanceLimits{DenyUserDrive: true},
	}
}

// ─── the matrix ───────────────────────────────────────────────────────────────

// TestSeedRequestDriveNoFlagIsANoOp pins the shape every run on every
// deployment takes: no drive asked for, nothing resolved, NO STORE READ. The
// store double panics on any read it does not implement, so a resolver call
// that leaked into this path would fail loudly rather than silently costing a
// query on every create.
func TestSeedRequestDriveNoFlagIsANoOp(t *testing.T) {
	srv, rec := driveRunServer(&driveStore{err: context.DeadlineExceeded}, "docker")
	for _, req := range []createRunRequest{
		{Agent: "claude-code", Task: "t"},     // no Drive at all
		driveRunRequest(false, nil),           // Drive present, enabled false
		driveRunRequest(false, boolPtr(true)), // …even with read_only set
	} {
		mount, ok, w := driveSeed(t, srv, req, deniedCeiling(), driveMemberCtx([]string{"eng"}, false))
		if !ok || mount != nil {
			t.Fatalf("mount = %+v, ok = %v; want no mount and no refusal — a failing store proves nothing was read", mount, ok)
		}
		if w.Code != http.StatusOK {
			t.Errorf("code = %d, want nothing written", w.Code)
		}
	}
	if len(rec.events) != 0 {
		t.Errorf("audit = %v, want nothing — the door does not apply to a run that asked for no drive", driveAuditActions(rec))
	}
}

// TestSeedRequestDriveDoorIs403WithAudit pins the ONE arm that is an
// authorization event: the profile refuses the door, so it answers 403 AND
// writes authz.denied at target runs.drive with reason governance_profile — the
// existing closed enum, no new value.
func TestSeedRequestDriveDoorIs403WithAudit(t *testing.T) {
	d := driveFixture(nil)
	st := &driveStore{drive: d, grant: grantFixture(d.ID, nil), tier: types.CapabilitySubjectUser}
	srv, rec := driveRunServer(st, "docker")

	mount, ok, w := driveSeed(t, srv, driveRunRequest(true, nil), deniedCeiling(), driveMemberCtx([]string{"eng"}, false))
	if ok || mount != nil {
		t.Fatalf("mount = %+v, ok = %v; want the door to stop the run", mount, ok)
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403: %s", w.Code, w.Body.String())
	}
	// The mock round's frozen member copy, byte-exact — and NO BACKTICKS. The
	// canon module (ui/src/app/lib/user-drives-copy.ts) carries none anywhere:
	// §7's header note makes a backticked substring in the doc a MONO SPAN the
	// console applies at display time, never characters on the wire. This
	// refusal used to ship "Launch without `drive`." and members read the
	// backticks as punctuation.
	const want = "mounting a user drive is not allowed by your governance profile \"contractors\". Launch without drive."
	if got := refusalBody(t, w); got != want {
		t.Errorf("body  = %s\nwant BYTE-EXACT: %s", got, want)
	}
	if strings.Contains(refusalBody(t, w), "`") {
		t.Errorf("the refusal ships a literal backtick: %s", refusalBody(t, w))
	}
	if r := auditReasons(t, srv, "authz.denied"); !slices.Contains(r, "governance_profile") {
		t.Errorf("authz.denied reasons = %v, want a governance_profile row", r)
	}
	var target string
	for _, ev := range rec.events {
		if ev.Action == "authz.denied" {
			target = ev.Target
		}
	}
	if target != "runs.drive" {
		t.Errorf("authz.denied target = %q, want runs.drive", target)
	}
}

// TestSeedRequestDriveOperatorSkipsTheDoor pins the exemption named in
// denyMemberDrive: the DOOR does not apply to an operator, and RESOLUTION still
// runs for them. An operator whose drive resolves gets it — drives are
// per-principal, not per-tier.
func TestSeedRequestDriveOperatorSkipsTheDoor(t *testing.T) {
	d := driveFixture(nil)
	st := &driveStore{drive: d, grant: grantFixture(d.ID, nil), tier: types.CapabilitySubjectUser}
	srv, rec := driveRunServer(st, "docker")

	adminCtx := withOIDCGroups(operatorCtx("sub-drive-bob", "bob@corp.example", oidc.RoleAdmin), []string{"eng"})
	mount, ok, w := driveSeed(t, srv, driveRunRequest(true, nil), deniedCeiling(), adminCtx)
	if !ok {
		t.Fatalf("operator refused by the door: %d %s", w.Code, w.Body.String())
	}
	if mount == nil {
		t.Fatal("mount = nil — the door not applying must not also skip RESOLUTION")
	}
	if len(rec.events) != 0 {
		t.Errorf("audit = %v, want no denial for a caller the door does not bind", driveAuditActions(rec))
	}
}

// TestSeedRequestDrive422Matrix walks every refusal that is NOT an
// authorization event. Each one must answer 422 and write NO audit row: the
// caller is authorized and simply has nothing to mount, and filling the denial
// stream with those rows is how a real denial stops standing out.
//
// It is ALSO where the frozen member copy is pinned. A row carrying `want` is
// compared for EQUALITY, not containment: the console never rewords a server
// refusal, so these strings are where the mock round's frozen table actually
// ships, and a substring assertion would pass on a body that had grown an
// internal prefix in front of the sentence — exactly the drift a member reads
// as gibberish. A row carrying `msg` instead composes its tail at runtime (the
// backend/runner mismatch names both), so containment is all there is to assert.
func TestSeedRequestDrive422Matrix(t *testing.T) {
	// A caller with a sub and NO email claim, so email_local has nothing to
	// truncate. Only the home-derivation row needs it.
	noEmailCtx := withOIDCGroups(operatorCtx("sub-drive-bob", "", oidc.RoleMember), nil)
	for _, tc := range []struct {
		name         string
		store        *driveStore
		runnerTarget string
		req          createRunRequest
		// msg is the refusal's opening; want is the whole frozen sentence,
		// byte-for-byte. Exactly one of the two per row.
		msg  string
		want string
		// ctx overrides the ordinary member caller for the rows that need a
		// particular claim set.
		ctx context.Context
	}{
		{
			// Asking for storage and silently not getting it is how work is
			// lost, so an absent allocation refuses the run rather than
			// launching it driveless.
			name: "no grant resolves", store: &driveStore{}, runnerTarget: "docker",
			req:  driveRunRequest(true, nil),
			want: "drive: no user drive is allocated to you — ask an admin for an allocation",
		},
		{
			// A row that was valid when written and is not now: the deployment
			// re-pointed WARDYN_RUNNER. Re-checked rather than trusted, because
			// a stale row must not become a mount the driver has no path for.
			name: "the backend cannot be mounted on this runner",
			store: &driveStore{
				drive: driveFixture(func(d *types.UserDrive) {
					d.Backend, d.HomeTemplate = types.DriveBackendK8sPVC, types.HomeTemplateHash
				}),
				tier: types.CapabilitySubjectUser,
			},
			runnerTarget: "docker", req: driveRunRequest(true, nil),
			msg: "drive: this deployment cannot mount your drive",
		},
		{
			// A paused allocation IS an answer. The disabled row used to be
			// excluded from the resolution outright, so this member read "no
			// user drive is allocated to you" and went to their admin to ask
			// for the thing that admin had just turned off.
			name: "the allocation is paused", store: pausedDriveStore(nil),
			runnerTarget: "docker", req: driveRunRequest(true, nil),
			want: "drive: your allocation is paused by an admin",
		},
		{
			// WIDENING. Honouring the allocation silently would launch a run the
			// member believes is writable, and they find out when their work
			// fails to persist.
			name: "read_only:false against a read-only allocation",
			store: &driveStore{
				drive: driveFixture(nil), tier: types.CapabilitySubjectUser,
			},
			runnerTarget: "docker", req: driveRunRequest(true, boolPtr(false)),
			want: "drive: your allocation is read-only; read_only:false cannot widen it",
		},
		{
			// The HOME DERIVATION cannot answer: email_local with no email
			// claim. Byte-exact like the rest — this refusal used to carry the
			// derivation's own error in trailing brackets, which made it the one
			// whose shipped bytes were not the frozen sentence; the cause now
			// goes to the log, where the operator who has to act on it looks.
			name: "the home name cannot be derived",
			store: &driveStore{
				drive: driveFixture(func(d *types.UserDrive) {
					d.Backend, d.HomeTemplate, d.HostRoot = types.DriveBackendHostPath, types.HomeTemplateEmailLocal, "/srv/homes"
				}),
				tier: types.CapabilitySubjectUser,
			},
			runnerTarget: "docker", req: driveRunRequest(true, nil), ctx: noEmailCtx,
			want: "drive: your email_local cannot name a directory " +
				"(lowercase letters and digits, then . _ -, up to 63 characters) — ask an admin to set your directory name",
		},
		{
			// The SAME door on KUBERNETES, where the frozen sentence is not the
			// whole rule: it describes driveHomeSegmentRe (the Docker rule), while
			// a k8s home must satisfy driveHomeSegmentK8sRe, which also forbids
			// `_` and a trailing `-`/`.`. The motivating case is exactly this one
			// — an Entra `sub` is base64url and routinely carries `_` — and the
			// member was being told the character that refused them was allowed.
			//
			// The canon is frozen, so it is not reworded: the substrate's clause
			// is APPENDED after it (types.DriveHomeStricterRuleClause), which is
			// why this row is byte-exact on the canon sentence AND on the suffix.
			name: "the home name breaks the stricter Kubernetes rule",
			store: &driveStore{
				drive: driveFixture(func(d *types.UserDrive) {
					d.Backend, d.HomeTemplate, d.SizeMiB = types.DriveBackendK8sPVC, types.HomeTemplateSub, 10240
				}),
				tier: types.CapabilitySubjectUser,
			},
			runnerTarget: "k8s", req: driveRunRequest(true, nil),
			ctx: withOIDCGroups(operatorCtx("sub_drive_bob", "bob@corp.example", oidc.RoleMember), []string{"eng"}),
			want: "drive: your sub cannot name a directory " +
				"(lowercase letters and digits, then . _ -, up to 63 characters) — ask an admin to set your directory name " +
				"(on a Kubernetes deployment the rule is stricter: no _, and it may not end in - or .)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.store.drive != nil && tc.store.grant == nil {
				tc.store.grant = grantFixture(tc.store.drive.ID, nil)
			}
			ctx := tc.ctx
			if ctx == nil {
				ctx = driveMemberCtx([]string{"eng"}, false)
			}
			srv, rec := driveRunServer(tc.store, tc.runnerTarget)
			mount, ok, w := driveSeed(t, srv, tc.req, governanceCeiling{}, ctx)
			if ok || mount != nil {
				t.Fatalf("mount = %+v, ok = %v; want a refusal", mount, ok)
			}
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("code = %d, want 422: %s", w.Code, w.Body.String())
			}
			got := refusalBody(t, w)
			if tc.want != "" {
				if got != tc.want {
					t.Errorf("body  = %q\nwant BYTE-EXACT: %q", got, tc.want)
				}
			} else if !strings.HasPrefix(got, tc.msg) {
				t.Errorf("body = %q, want it to open %q", got, tc.msg)
			}
			// The sentinel's own name must never reach the member:
			// errDriveUnmountable exists for errors.Is, not for reading.
			if strings.Contains(w.Body.String(), "drive_unmountable") {
				t.Errorf("body = %q leaks the sentinel's name to the member", w.Body.String())
			}
			if len(rec.events) != 0 {
				t.Errorf("audit = %v, want NO audit — the caller is authorized and simply has nothing to mount", driveAuditActions(rec))
			}
		})
	}

	// The narrow direction is always honoured, and the un-narrowed writable
	// allocation is the positive control that proves the refusal above is about
	// widening rather than about writable drives being broken.
	t.Run("read_only:true NARROWS a writable allocation", func(t *testing.T) {
		d := driveFixture(func(d *types.UserDrive) { d.Writable = true })
		st := &driveStore{drive: d, grant: grantFixture(d.ID, nil), tier: types.CapabilitySubjectUser}
		srv, _ := driveRunServer(st, "docker")
		mount, ok, w := driveSeed(t, srv, driveRunRequest(true, boolPtr(true)), governanceCeiling{}, driveMemberCtx(nil, false))
		if !ok || mount == nil {
			t.Fatalf("narrowing refused: %d %s", w.Code, w.Body.String())
		}
		if !mount.ReadOnly {
			t.Error("read_only = false — a request may always narrow")
		}
		mount, ok, _ = driveSeed(t, srv, driveRunRequest(true, nil), governanceCeiling{}, driveMemberCtx(nil, false))
		if !ok || mount == nil || mount.ReadOnly {
			t.Errorf("mount = %+v, want the allocation's own writable posture when the request says nothing", mount)
		}
	})
}

// TestSeedRequestDriveShareIsBindableAtMountTime walks the two facts a
// host_path mount depends on that the ROW CANNOT CARRY, and therefore has to be
// re-established at the moment of the mount rather than trusted from authoring
// time.
//
// Both were previously unchecked here, and both fail in the direction that
// loses work rather than the direction that refuses it:
//
//   - THE CEILING. handleUpsertUserDrive applied WARDYN_USER_DRIVE_HOST_ROOTS
//     when this row was written, but the variable is boot state and the share
//     is the host's — an operator narrowing the roots, or un-setting them, or
//     the share going away, all leave a stored drive this deployment no longer
//     allows binding into OTHER PEOPLE's sandboxes. Trusting the row would let
//     whoever authored one hold the ceiling open across every later boot.
//   - THE DIRECTORY. Wardyn never mkdir's on a share, and a bind mount of a
//     missing source is one of the few places Docker HELPFULLY CREATES IT: an
//     empty root-owned directory appears on the operator's share, the run
//     launches green, and the member's work goes somewhere no admin allocated.
func TestSeedRequestDriveShareIsBindableAtMountTime(t *testing.T) {
	// The share as the operator mounted it, with exactly one member's home in
	// it: <root>/bob exists, and this member's claim derives `bob`.
	newShare := func(t *testing.T) (root string, st *driveStore) {
		t.Helper()
		root = t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "bob"), 0o755); err != nil {
			t.Fatalf("mkdir home: %v", err)
		}
		d := driveFixture(func(d *types.UserDrive) {
			d.Backend, d.HomeTemplate, d.HostRoot = types.DriveBackendHostPath, types.HomeTemplateSub, root
			d.Writable = true
		})
		return root, &driveStore{drive: d, grant: grantFixture(d.ID, nil), tier: types.CapabilitySubjectUser}
	}
	// The claim `bob` names the directory that exists; every arm below uses it,
	// so the only thing that differs between them is the fact under test.
	shareCtx := func() context.Context {
		return withOIDCGroups(operatorCtx("bob", "bob@corp.example", oidc.RoleMember), nil)
	}

	t.Run("the home directory is there and the drive mounts", func(t *testing.T) {
		// The positive control: without it, every refusal below would also pass
		// on a seam that simply refused host_path drives outright.
		root, st := newShare(t)
		srv, rec := driveShareServer(st, []string{root})
		mount, ok, w := driveSeed(t, srv, driveRunRequest(true, nil), governanceCeiling{}, shareCtx())
		if !ok || mount == nil {
			t.Fatalf("a bindable share was refused: %d %s", w.Code, w.Body.String())
		}
		if mount.ObjectName != filepath.Join(root, "bob") {
			t.Errorf("object_name = %q, want this member's subdirectory of the share", mount.ObjectName)
		}
		if len(rec.events) != 0 {
			t.Errorf("audit = %v, want nothing for a mount that succeeded", driveAuditActions(rec))
		}
	})

	t.Run("a missing home directory is REFUSED_HOME_MISSING", func(t *testing.T) {
		root, st := newShare(t)
		// The same share, a member whose directory nobody created — the
		// offboarding-in-reverse case: allocated in Wardyn, absent on the NAS.
		srv, rec := driveShareServer(st, []string{root})
		ctx := withOIDCGroups(operatorCtx("carol", "carol@corp.example", oidc.RoleMember), nil)
		mount, ok, w := driveSeed(t, srv, driveRunRequest(true, nil), governanceCeiling{}, ctx)
		if ok || mount != nil {
			t.Fatalf("mount = %+v; want a refusal rather than a directory Docker would create", mount)
		}
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("code = %d, want 422: %s", w.Code, w.Body.String())
		}
		// The frozen §7.7 sentence, byte-exact, naming the HOME — never the
		// host path, which is the operator's filesystem layout and is not a
		// member's to read (GET /drives ships a boolean for the same reason).
		const want = "drive: directory carol does not exist on the share — ask an admin to create it"
		if got := refusalBody(t, w); got != want {
			t.Errorf("body  = %q\nwant BYTE-EXACT: %q", got, want)
		}
		if strings.Contains(w.Body.String(), root) {
			t.Errorf("body = %q leaks the operator's host path to a member", w.Body.String())
		}
		if len(rec.events) != 0 {
			t.Errorf("audit = %v, want NO audit — the caller is authorized and there is simply nothing to mount",
				driveAuditActions(rec))
		}
	})

	t.Run("a home that is a FILE is refused too", func(t *testing.T) {
		// Binding a regular file at the drive target is not a drive, and the
		// sentence the member needs is the same one.
		root, st := newShare(t)
		if err := os.WriteFile(filepath.Join(root, "dave"), []byte("not a directory"), 0o600); err != nil {
			t.Fatalf("write file: %v", err)
		}
		srv, _ := driveShareServer(st, []string{root})
		ctx := withOIDCGroups(operatorCtx("dave", "dave@corp.example", oidc.RoleMember), nil)
		_, ok, w := driveSeed(t, srv, driveRunRequest(true, nil), governanceCeiling{}, ctx)
		if ok || w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("code = %d, ok = %v; want 422", w.Code, ok)
		}
		if got := refusalBody(t, w); !strings.HasSuffix(got, "does not exist on the share — ask an admin to create it") {
			t.Errorf("body = %q, want the missing-directory sentence", got)
		}
	})

	t.Run("the env ceiling is UNSET since the drive was authored", func(t *testing.T) {
		// The row is still perfectly valid and the directory is still there.
		// What changed is the deployment, and a stale row must not survive it.
		root, st := newShare(t)
		srv, rec := driveShareServer(st, nil)
		mount, ok, w := driveSeed(t, srv, driveRunRequest(true, nil), governanceCeiling{}, shareCtx())
		if ok || mount != nil {
			t.Fatalf("mount = %+v; a deployment with no roots must mount no share", mount)
		}
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("code = %d, want 422: %s", w.Code, w.Body.String())
		}
		// REFUSED_BACKEND's shape, not a fifth refusal: from the member's side
		// "the roots moved" and "this deployment dispatches elsewhere" are one
		// fact.
		got := refusalBody(t, w)
		if !strings.HasPrefix(got, "drive: this deployment cannot mount your drive (") {
			t.Errorf("body = %q, want the REFUSED_BACKEND shape", got)
		}
		// AND THE DIAGNOSIS IS NOT IN IT. UserDriveHostRootCheck's error spells
		// the drive's host_root and the whole ceiling list, and this body goes to
		// a MEMBER — the same reader the missing-home arm, applyUserDriveEnv and
		// driveAuditTarget all keep the operator's filesystem layout from. The
		// roots go to slog; the member gets the drive's name and who to ask.
		for _, leak := range []string{"WARDYN_USER_DRIVE_HOST_ROOTS", root} {
			if strings.Contains(got, leak) {
				t.Errorf("body = %q discloses %q to the member", got, leak)
			}
		}
		if !strings.Contains(got, `drive "Corp NAS" is on a share this deployment does not allow — ask an admin`) {
			t.Errorf("body = %q, want it to name the drive and who fixes it", got)
		}
		if len(rec.events) != 0 {
			t.Errorf("audit = %v, want NO audit", driveAuditActions(rec))
		}
	})

	t.Run("the root MOVED OUT of the ceiling", func(t *testing.T) {
		// The variable is still set; it just no longer covers this row's root.
		root, st := newShare(t)
		allowed := t.TempDir()
		srv, _ := driveShareServer(st, []string{allowed})
		_, ok, w := driveSeed(t, srv, driveRunRequest(true, nil), governanceCeiling{}, shareCtx())
		if ok || w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("code = %d, ok = %v; want 422 for a root outside the roots", w.Code, ok)
		}
		got := refusalBody(t, w)
		if !strings.HasPrefix(got, "drive: this deployment cannot mount your drive (") {
			t.Errorf("body = %q, want the REFUSED_BACKEND shape", got)
		}
		// Neither the row's root nor the roots it is being measured against.
		for _, leak := range []string{root, allowed} {
			if strings.Contains(got, leak) {
				t.Errorf("body = %q discloses the host path %q to the member", got, leak)
			}
		}
	})

	t.Run("a MANAGED backend is not stat'd", func(t *testing.T) {
		// The check is host_path's alone: a docker_volume is created on first
		// use, and stat'ing a volume name as a path would refuse every one of
		// them on a deployment that sets no roots at all.
		d := driveFixture(nil)
		st := &driveStore{drive: d, grant: grantFixture(d.ID, nil), tier: types.CapabilitySubjectUser}
		srv, _ := driveShareServer(st, nil)
		mount, ok, w := driveSeed(t, srv, driveRunRequest(true, nil), governanceCeiling{}, shareCtx())
		if !ok || mount == nil {
			t.Fatalf("a managed drive was refused by the share check: %d %s", w.Code, w.Body.String())
		}
	})
}

// TestSeedRequestDriveMountShape pins what the runner is handed: the reserved
// target as the SYMBOL (never a re-typed literal), the derived object name, and
// the enforcement vocabulary that says what the size actually means.
func TestSeedRequestDriveMountShape(t *testing.T) {
	d := driveFixture(nil)
	st := &driveStore{drive: d, grant: grantFixture(d.ID, nil), tier: types.CapabilitySubjectUser}
	srv, _ := driveRunServer(st, "docker")

	mount, ok, w := driveSeed(t, srv, driveRunRequest(true, nil), governanceCeiling{}, driveMemberCtx(nil, false))
	if !ok || mount == nil {
		t.Fatalf("seed refused: %d %s", w.Code, w.Body.String())
	}
	home, err := types.DriveHomeName(*d, "sub-drive-bob", "")
	if err != nil {
		t.Fatalf("DriveHomeName: %v", err)
	}
	want := types.DriveMount{
		DriveID: d.ID,
		Backend: types.DriveBackendDockerVolume, ObjectName: types.DriveObjectName(*d, home),
		DriveName: d.Name,
		HomeName:  home, SubjectHash: types.DriveSubjectHash("sub-drive-bob"),
		Target: runner.DriveTarget, ReadOnly: true, SizeMiB: 10240,
		Enforcement: types.StorageEnforcementNone,
		// StorageClass stays empty: a Docker volume has no such concept, and a
		// substrate that sees one on this backend is looking at a bad row.
		// HostRoot stays empty for the same reason: a managed volume has no host
		// tree, so there is nothing for the driver to bound it against — the
		// per-drive root check runs on the host_path arm only.
	}
	if *mount != want {
		t.Errorf("mount = %+v\nwant %+v", *mount, want)
	}
	// The PRINCIPAL's fingerprint, carried from the resolver rather than
	// re-derived here: an object name is per-HOME, so this is the only value
	// that can tell two principals apart once a template has folded them onto
	// one home, and the driver stamps it on the volume it allocates. A DIGEST,
	// so the label `docker volume inspect` echoes carries no claim.
	if mount.SubjectHash == "" || strings.Contains(mount.SubjectHash, "sub-drive-bob") {
		t.Errorf("subject hash = %q, want a digest of the claim the home came from", mount.SubjectHash)
	}
	// The DRIVE's id, not the grant's: labels and reclaim sweeps group by the
	// drive, and an object name is per-principal so it cannot answer that.
	if mount.DriveID != d.ID {
		t.Errorf("drive_id = %s, want the drive row's %s", mount.DriveID, d.ID)
	}
	// The honesty vocabulary is not decoration: a Docker named volume has NO
	// byte cap, and reporting anything but `none` would let a console render an
	// allocation as a limit.
	if mount.Enforcement != types.StorageEnforcementNone {
		t.Errorf("enforcement = %q for a docker volume, want none", mount.Enforcement)
	}
}

// TestSeedRequestDriveMountCarriesTheDrivesOwnRoot is the API half of the
// per-drive containment fix: a SHARE mount carries the drive row's own
// host_root, because the driver bounds the bind to THAT tree and not merely to
// the union of WARDYN_USER_DRIVE_HOST_ROOTS — with two share drives inside one
// ceiling, the ceiling cannot tell one drive's tree from the other's.
//
// Copied from the resolved row, never re-derived: the driver must not join a
// root and a home a second time, and a mount whose root and object name came
// from two different derivations is the drift the whole carry-it-forward shape
// exists to prevent. The drive's NAME rides along for the audit row's Target.
func TestSeedRequestDriveMountCarriesTheDrivesOwnRoot(t *testing.T) {
	dir := t.TempDir()
	d := driveFixture(func(d *types.UserDrive) {
		d.Name, d.Backend, d.HomeTemplate, d.HostRoot = "Corp NAS", types.DriveBackendHostPath, types.HomeTemplateSub, dir
	})
	st := &driveStore{drive: d, grant: grantFixture(d.ID, nil), tier: types.CapabilitySubjectUser}
	srv, _ := driveRunServer(st, "docker")
	srv.cfg.UserDriveHostRoots = []string{dir}
	if err := os.MkdirAll(filepath.Join(dir, "sub-drive-bob"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	mount, ok, w := driveSeed(t, srv, driveRunRequest(true, nil), governanceCeiling{}, driveMemberCtx(nil, false))
	if !ok || mount == nil {
		t.Fatalf("seed refused: %d %s", w.Code, w.Body.String())
	}
	if mount.HostRoot != d.HostRoot {
		t.Errorf("host_root = %q, want the drive row's own %q — the deployment ceiling bounds every drive at once and "+
			"cannot say which tree THIS one was authored against", mount.HostRoot, d.HostRoot)
	}
	// And the object name really is inside it, so the driver's strict-subdir
	// assertion is a statement about a pair the resolver derived together.
	if filepath.Dir(mount.ObjectName) != mount.HostRoot {
		t.Errorf("object %q is not directly under host_root %q", mount.ObjectName, mount.HostRoot)
	}
	if mount.DriveName != d.Name {
		t.Errorf("drive_name = %q, want the drive's own %q (the audit row's Target is built from it)", mount.DriveName, d.Name)
	}
}

// TestSeedRequestDriveFailsClosedOnAStoreError pins the arm that separates this
// seam from /me: a store that cannot answer is a 500, never "you have no
// drive". Mounting nothing where an admin allocated something loses a member's
// work silently.
func TestSeedRequestDriveFailsClosedOnAStoreError(t *testing.T) {
	srv, _ := driveRunServer(&driveStore{err: context.DeadlineExceeded}, "docker")
	_, ok, w := driveSeed(t, srv, driveRunRequest(true, nil), governanceCeiling{}, driveMemberCtx([]string{"eng"}, false))
	if ok || w.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d, ok = %v; want a 500 rather than a quiet driveless launch", w.Code, ok)
	}
}

// TestSeedRequestDriveTruncatedGroupsIs403 pins the third shape: the group
// snapshot is unreadable AND a group-tier grant exists, so any answer would be
// a guess and the only wrong guess is the widening one.
func TestSeedRequestDriveTruncatedGroupsIs403(t *testing.T) {
	st := &driveStore{hasGroupTier: true, userTierOnly: true}
	srv, rec := driveRunServer(st, "docker")
	_, ok, w := driveSeed(t, srv, driveRunRequest(true, nil), governanceCeiling{}, driveMemberCtx([]string{"eng"}, true))
	if ok || w.Code != http.StatusForbidden {
		t.Fatalf("code = %d, ok = %v; want 403: %s", w.Code, ok, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "groups_snapshot_stale") {
		t.Errorf("body = %s, want the stale-snapshot refusal naming its remedy", w.Body.String())
	}
	// Not an authz.denied: this is "we cannot tell", not "you may not".
	if len(rec.events) != 0 {
		t.Errorf("audit = %v, want none", driveAuditActions(rec))
	}
}

// TestRevokingAGrantStopsTheNextRunMounting is the revoke pin. Resolution runs
// PER RUN — there is no cached answer and no per-run persistence to go stale —
// so an allocation removed between two creates stops the second one, and the
// refusal is the ordinary no-grant 422 rather than a silent driveless launch.
//
// It is scoped to the NEXT run deliberately, and that scope is the honest
// statement of the guarantee: dispatchRun is called inline from handleCreateRun
// a few statements after this seam, so there is no create-then-dispatch window
// for a revoke to land in, and no identity on the run row to re-resolve from if
// there were (see user_drives_run.go).
func TestRevokingAGrantStopsTheNextRunMounting(t *testing.T) {
	d := driveFixture(nil)
	st := &driveStore{drive: d, grant: grantFixture(d.ID, nil), tier: types.CapabilitySubjectUser}
	srv, _ := driveRunServer(st, "docker")

	if mount, ok, w := driveSeed(t, srv, driveRunRequest(true, nil), governanceCeiling{}, driveMemberCtx(nil, false)); !ok || mount == nil {
		t.Fatalf("first run refused: %d %s", w.Code, w.Body.String())
	}
	st.drive, st.grant = nil, nil // the admin deleted the allocation
	mount, ok, w := driveSeed(t, srv, driveRunRequest(true, nil), governanceCeiling{}, driveMemberCtx(nil, false))
	if ok || mount != nil {
		t.Fatalf("mount = %+v after the allocation was removed — resolution must not be cached", mount)
	}
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("code = %d, want the ordinary no-grant 422: %s", w.Code, w.Body.String())
	}
}

// ─── /me ──────────────────────────────────────────────────────────────────────

// meDriveBody drives GET /me through the real handler and returns the
// user_drive value (nil when the key is null) alongside the SIBLING door field.
// Both are read from one call because the pair is the contract: four states, two
// keys, and the console tells them apart by reading both.
func meDriveBody(t *testing.T, srv *Server, ctx context.Context) (map[string]any, string) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	srv.handleMe(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("/me = %d: %s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /me: %v", err)
	}
	if _, ok := body["user_drive"]; !ok {
		t.Fatal("/me has no user_drive key — nil-means-none needs the key PRESENT and null")
	}
	denied, ok := body["user_drive_denied_by_profile"]
	if !ok {
		t.Fatal("/me has no user_drive_denied_by_profile key — always present, so an older daemon's MISSING key is distinguishable from an open door")
	}
	name, _ := denied.(string)
	ud, _ := body["user_drive"].(map[string]any)
	return ud, name
}

// TestMeUserDrive pins the member's own view of the same resolution the run
// path takes: present, absent, denied-by-profile, and the identity-less
// operator.
func TestMeUserDrive(t *testing.T) {
	t.Run("no allocation is null, not an empty object", func(t *testing.T) {
		srv, _ := driveRunServer(&driveStore{}, "docker")
		ud, denied := meDriveBody(t, srv, driveMemberCtx(nil, false))
		if ud != nil {
			t.Errorf("user_drive = %v, want null", ud)
		}
		if denied != "" {
			t.Errorf("user_drive_denied_by_profile = %q, want empty — an unassigned member has no door", denied)
		}
	})

	t.Run("an allocation is reported with its honest enforcement", func(t *testing.T) {
		d := driveFixture(nil)
		st := &driveStore{drive: d, grant: grantFixture(d.ID, nil), tier: types.CapabilitySubjectUser}
		srv, _ := driveRunServer(st, "docker")
		ud, denied := meDriveBody(t, srv, driveMemberCtx(nil, false))
		if ud == nil {
			t.Fatal("user_drive = null for an allocated member")
		}
		if ud["name"] != d.Name || ud["writable"] != false {
			t.Errorf("user_drive = %v, want the drive's name and its read-only posture", ud)
		}
		if ud["enforcement"] != string(types.StorageEnforcementNone) {
			t.Errorf("enforcement = %v, want none for a docker volume", ud["enforcement"])
		}
		// The object means ONE thing — what is allocated — so the door is not
		// in it. A field here would be the drift the split exists to prevent.
		if _, ok := ud["denied_by_profile"]; ok {
			t.Error("user_drive carries a denied_by_profile field; the door is the SIBLING key")
		}
		if denied != "" {
			t.Errorf("user_drive_denied_by_profile = %q, want empty", denied)
		}
	})

	t.Run("a PAUSED allocation is reported as paused, not as absent", func(t *testing.T) {
		// The state that had no representation on the wire before: the drive
		// exists, an admin turned it off, and the console renders NR_PAUSED
		// where the checkbox would be. Reported as null instead, the member
		// would be told to ask for an allocation they already have.
		srv, _ := driveRunServer(pausedDriveStore(nil), "docker")
		ud, denied := meDriveBody(t, srv, driveMemberCtx(nil, false))
		if ud == nil {
			t.Fatal("user_drive = null for a paused allocation")
		}
		if ud["paused"] != true {
			t.Errorf("paused = %v, want true", ud["paused"])
		}
		// ABSENT, not empty: nothing mounts, so there is no directory to name.
		if _, ok := ud["home_name"]; ok {
			t.Errorf("home_name = %v is present; a paused drive derives no directory", ud["home_name"])
		}
		if ud["name"] != "Corp NAS" {
			t.Errorf("name = %v, want the paused drive's", ud["name"])
		}
		// A pause is not a door: the sibling key answers a different question
		// and this member's profile has not shut anything.
		if denied != "" {
			t.Errorf("user_drive_denied_by_profile = %q, want empty", denied)
		}
	})

	t.Run("a shut door is reported as denied, not as absent", func(t *testing.T) {
		// The whole reason the field exists: "you have none" and "you have one
		// you may not use" both render as no mount, and only ONE of them is
		// something the member should take to their admin.
		d := driveFixture(nil)
		cs := &capStore{
			drive: d, driveGrant: grantFixture(d.ID, nil), driveTier: types.CapabilitySubjectUser,
			govProfile: &types.GovernanceProfile{
				Name: "contractors", Limits: types.GovernanceLimits{DenyUserDrive: true},
			},
			govTier: types.CapabilitySubjectUser,
		}
		srv := New(Config{Store: cs, Audit: &recRecorder{}, RunnerTarget: "docker"})
		ud, denied := meDriveBody(t, srv, driveMemberCtx(nil, false))
		if ud == nil {
			t.Fatal("user_drive = null — a shut door does not un-allocate the drive")
		}
		if denied != "contractors" {
			t.Errorf("user_drive_denied_by_profile = %q, want the profile NAME (the member sentence quotes it)", denied)
		}
	})

	t.Run("DENIED WITH NO ALLOCATION is its own state", func(t *testing.T) {
		// THE STATE ONE KEY COULD NOT EXPRESS, and the reason the door is a
		// sibling: this member's answer is not "ask an admin for an allocation"
		// — an allocation would not help them until the profile changes.
		cs := &capStore{
			govProfile: &types.GovernanceProfile{
				Name: "contractors", Limits: types.GovernanceLimits{DenyUserDrive: true},
			},
			govTier: types.CapabilitySubjectUser,
		}
		srv := New(Config{Store: cs, Audit: &recRecorder{}, RunnerTarget: "docker"})
		ud, denied := meDriveBody(t, srv, driveMemberCtx(nil, false))
		if ud != nil {
			t.Errorf("user_drive = %v, want null", ud)
		}
		if denied != "contractors" {
			t.Errorf("user_drive_denied_by_profile = %q, want the profile name", denied)
		}
	})

	t.Run("an operator is never reported as denied", func(t *testing.T) {
		// The door keys on isOperator exactly as denyMemberDrive does, so a
		// profile that happens to carry DenyUserDrive never renders a closed
		// door for a caller it does not bind.
		cs := &capStore{
			govProfile: &types.GovernanceProfile{
				Name: "contractors", Limits: types.GovernanceLimits{DenyUserDrive: true},
			},
			govTier: types.CapabilitySubjectUser,
		}
		srv := New(Config{Store: cs, Audit: &recRecorder{}, RunnerTarget: "docker"})
		adminCtx := withOIDCGroups(operatorCtx("sub-drive-bob", "bob@corp.example", oidc.RoleAdmin), nil)
		if _, denied := meDriveBody(t, srv, adminCtx); denied != "" {
			t.Errorf("user_drive_denied_by_profile = %q for an operator, want empty", denied)
		}
	})

	t.Run("an identity-less operator has no drive", func(t *testing.T) {
		// Local mode and the admin token carry no per-human subject, so there is
		// no principal to name a home after — and an `all`-tier grant must not
		// hand every identity-less caller ONE shared directory.
		d := driveFixture(nil)
		st := &driveStore{drive: d, grant: grantFixture(d.ID, nil), tier: types.CapabilitySubjectAll}
		srv, _ := driveRunServer(st, "docker")
		if ud, _ := meDriveBody(t, srv, context.Background()); ud != nil {
			t.Errorf("user_drive = %v for a caller with no subjects, want null", ud)
		}
	})
}

// ─── create ↔ preflight parity ────────────────────────────────────────────────

// TestPreflightAnswersTheSameDriveRefusalAsCreate is the parity pin, and it is
// the whole reason seedRequestDrive is called from two places. Preflight is the
// wizard's Review dry run; a Review that previewed a green checklist for a
// launch that will 422 is the exact failure the preflight handler exists not to
// have.
//
// Both are driven through the REAL router as a signed-in member, so the
// assertion covers the call site and its ORDER, not just the helper.
func TestPreflightAnswersTheSameDriveRefusalAsCreate(t *testing.T) {
	const body = `{"agent":"claude-code","task":"t","drive":{"enabled":true}}`

	t.Run("no allocation: 422 on both, byte-identical", func(t *testing.T) {
		srv, _, _ := govEscapeFixture(t, &capStore{})
		session := govSession(t, "sub-drives", []string{"eng"}, false)

		create := doSSO(t, srv, http.MethodPost, "/api/v1/runs", session, body)
		preflight := doSSO(t, srv, http.MethodPost, "/api/v1/runs/preflight", session, body)
		if create.Code != http.StatusUnprocessableEntity || preflight.Code != http.StatusUnprocessableEntity {
			t.Fatalf("create = %d, preflight = %d; want 422 on both\ncreate: %s\npreflight: %s",
				create.Code, preflight.Code, create.Body.String(), preflight.Body.String())
		}
		if got, want := refusalBody(t, preflight), refusalBody(t, create); got != want {
			t.Errorf("preflight body = %q\ncreate body    = %q\nwant them identical", got, want)
		}
	})

	t.Run("the door: 403 on both", func(t *testing.T) {
		// And the counterfactual the placement guards: preflight runs
		// denyMemberRequest FIRST, so the ceiling it hands seedRequestDrive is
		// the SAME one create resolved — a preflight that passed a zero ceiling
		// would preview an open door for a run the door will refuse.
		srv, _, _ := govEscapeFixture(t, assignedStore(limitsProfile("contractors",
			types.GovernanceLimits{DenyUserDrive: true})))
		session := govSession(t, "sub-drives", []string{"eng"}, false)

		create := doSSO(t, srv, http.MethodPost, "/api/v1/runs", session, body)
		preflight := doSSO(t, srv, http.MethodPost, "/api/v1/runs/preflight", session, body)
		if create.Code != http.StatusForbidden || preflight.Code != http.StatusForbidden {
			t.Fatalf("create = %d, preflight = %d; want 403 on both\ncreate: %s\npreflight: %s",
				create.Code, preflight.Code, create.Body.String(), preflight.Body.String())
		}
		if got, want := refusalBody(t, preflight), refusalBody(t, create); got != want {
			t.Errorf("preflight body = %q\ncreate body    = %q\nwant them identical", got, want)
		}
	})

	t.Run("NO REGRESSION: a run with no drive flag is untouched", func(t *testing.T) {
		// The pin that matters on upgrade day: every run on every deployment
		// that has allocated nothing must behave byte-for-byte as it did before
		// this seam existed.
		srv, _, _ := govEscapeFixture(t, &capStore{})
		session := govSession(t, "sub-drives", []string{"eng"}, false)
		if w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", session,
			`{"agent":"claude-code","task":"t"}`); w.Code != http.StatusCreated {
			t.Fatalf("create = %d, want 201: %s", w.Code, w.Body.String())
		}
	})
}
