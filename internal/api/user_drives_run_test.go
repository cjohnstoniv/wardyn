// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	// The mock round's frozen member copy, byte-exact.
	const want = "mounting a user drive is not allowed by your governance profile \"contractors\". Launch without `drive`."
	if got := refusalBody(t, w); got != want {
		t.Errorf("body  = %s\nwant BYTE-EXACT: %s", got, want)
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
func TestSeedRequestDrive422Matrix(t *testing.T) {
	writableDrive := func() *types.UserDrive {
		return driveFixture(func(d *types.UserDrive) { d.Writable = true })
	}
	for _, tc := range []struct {
		name         string
		store        *driveStore
		runnerTarget string
		req          createRunRequest
		msg          string
	}{
		{
			// Asking for storage and silently not getting it is how work is
			// lost, so an absent allocation refuses the run rather than
			// launching it driveless.
			name: "no grant resolves", store: &driveStore{}, runnerTarget: "docker",
			req: driveRunRequest(true, nil),
			msg: "drive: no user drive is allocated to you",
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
			// WIDENING. Honouring the allocation silently would launch a run the
			// member believes is writable, and they find out when their work
			// fails to persist.
			name: "read_only:false against a read-only allocation",
			store: &driveStore{
				drive: driveFixture(nil), tier: types.CapabilitySubjectUser,
			},
			runnerTarget: "docker", req: driveRunRequest(true, boolPtr(false)),
			msg: "drive: your allocation is read-only",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.store.drive != nil && tc.store.grant == nil {
				tc.store.grant = grantFixture(tc.store.drive.ID, nil)
			}
			srv, rec := driveRunServer(tc.store, tc.runnerTarget)
			mount, ok, w := driveSeed(t, srv, tc.req, governanceCeiling{}, driveMemberCtx([]string{"eng"}, false))
			if ok || mount != nil {
				t.Fatalf("mount = %+v, ok = %v; want a refusal", mount, ok)
			}
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("code = %d, want 422: %s", w.Code, w.Body.String())
			}
			if got := refusalBody(t, w); !strings.HasPrefix(got, tc.msg) {
				t.Errorf("body = %q, want it to open %q", got, tc.msg)
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
		d := writableDrive()
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
		Backend: types.DriveBackendDockerVolume, ObjectName: types.DriveObjectName(*d, home),
		HomeName: home, Target: runner.DriveTarget, ReadOnly: true, SizeMiB: 10240,
		Enforcement: types.StorageEnforcementNone,
	}
	if *mount != want {
		t.Errorf("mount = %+v\nwant %+v", *mount, want)
	}
	// The honesty vocabulary is not decoration: a Docker named volume has NO
	// byte cap, and reporting anything but `none` would let a console render an
	// allocation as a limit.
	if mount.Enforcement != types.StorageEnforcementNone {
		t.Errorf("enforcement = %q for a docker volume, want none", mount.Enforcement)
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

// ─── the frozen refusal copy ──────────────────────────────────────────────────

// TestDriveRefusalsAreTheFrozenMemberCopy compares every member-facing refusal
// this seam can raise against the mock round's frozen table for EQUALITY, not
// containment. The console never rewords a server refusal, so these strings are
// where that copy actually ships — a substring assertion would pass on a body
// that had grown an internal prefix in front of the sentence, which is exactly
// the drift a member reads as gibberish.
func TestDriveRefusalsAreTheFrozenMemberCopy(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store *driveStore
		req   createRunRequest
		want  string
	}{
		{
			name: "REFUSED_NO_GRANT", store: &driveStore{}, req: driveRunRequest(true, nil),
			want: "drive: no user drive is allocated to you — ask an admin for an allocation",
		},
		{
			name: "REFUSED_WRITABLE",
			store: &driveStore{
				drive: driveFixture(nil), tier: types.CapabilitySubjectUser,
			},
			req:  driveRunRequest(true, boolPtr(false)),
			want: "drive: your allocation is read-only; `read_only:false` cannot widen it",
		},
		{
			// REFUSED_HOME_INVALID. The sentinel's own name must not reach the
			// member: errDriveUnmountable exists for errors.Is, not for reading.
			name: "REFUSED_HOME_INVALID",
			store: &driveStore{
				drive: driveFixture(func(d *types.UserDrive) {
					d.Backend, d.HomeTemplate, d.HostRoot = types.DriveBackendHostPath, types.HomeTemplateEmailLocal, "/srv/homes"
				}),
				tier: types.CapabilitySubjectUser,
			},
			req: driveRunRequest(true, nil),
			want: "drive: your email_local cannot name a directory " +
				"(lowercase letters and digits, then `. _ -`, up to 63 characters) — ask an admin to set your directory name",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.store.drive != nil && tc.store.grant == nil {
				tc.store.grant = grantFixture(tc.store.drive.ID, nil)
			}
			srv, _ := driveRunServer(tc.store, "docker")
			// A caller with a sub and NO email claim, so email_local has nothing
			// to truncate; harmless for the other two cases.
			ctx := withOIDCGroups(operatorCtx("sub-drive-bob", "", oidc.RoleMember), nil)
			_, ok, w := driveSeed(t, srv, tc.req, governanceCeiling{}, ctx)
			if ok {
				t.Fatalf("expected a refusal, got a mount")
			}
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("code = %d, want 422: %s", w.Code, w.Body.String())
			}
			got := refusalBody(t, w)
			if tc.name == "REFUSED_HOME_INVALID" {
				// The wrapped cause rides in brackets for the log; the frozen
				// sentence is what opens the body.
				if !strings.HasPrefix(got, tc.want) {
					t.Fatalf("body  = %q\nwant it to OPEN with the frozen sentence: %q", got, tc.want)
				}
				if strings.Contains(got, "drive_unmountable") {
					t.Errorf("body = %q leaks the sentinel's name to the member", got)
				}
				return
			}
			if got != tc.want {
				t.Errorf("body  = %q\nwant BYTE-EXACT: %q", got, tc.want)
			}
		})
	}
}
