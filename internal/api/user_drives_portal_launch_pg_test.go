// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// portalLaunchEnv is the queue fixture with team drives switched on, one issuer portal, one person of that
// portal with a bearer, and a pool with room for several runs. Its team drives are written through the store, so
// each test launches through the real route with the portal's own bearer.
type portalLaunchEnv struct {
	*queueEnv
	portal    types.Delegate
	principal string
	bearer    string
	rp        types.RunnerPool
}

func newPortalLaunchEnv(t *testing.T, poolMax int) *portalLaunchEnv {
	t.Helper()
	e := &portalLaunchEnv{queueEnv: newQueueEnv(t)}
	ctx := context.Background()
	if _, err := e.pg.ActivatePortalIssuers(ctx, "test", nil); err != nil {
		t.Fatal(err)
	}
	site := types.SiteConfig{Runners: &types.RunnerSettings{Enabled: true}, WorkspaceProviders: &types.WorkspaceProviders{
		Storage: &types.StorageProviders{UserDrive: &types.UserDriveProvider{TeamDrives: true}}}}
	if _, err := e.pg.PutSiteConfig(ctx, site); err != nil {
		t.Fatal(err)
	}
	e.srv.cfg.RunnerTarget = "docker"
	f := e.issuerPortal(t, nil)
	e.portal = f.portal
	e.principal, e.bearer = e.portalBearer(t, f.portal, "alice")
	e.rp = e.cappedPool("Portal drives", poolMax)
	return e
}

// teamDrive stores a team drive called name and, when allocate, allocates it to the portal.
func (e *portalLaunchEnv) teamDrive(name string, allocate bool) types.UserDrive {
	e.t.Helper()
	ctx := context.Background()
	d, err := e.pg.UpsertUserDrive(ctx, types.UserDrive{Name: name, Backend: types.DriveBackendDockerVolume,
		HomeTemplate: types.HomeTemplateHash, Writable: true, Reclaim: types.DriveReclaimRetain, Team: true, CreatedBy: "admin@example.com"}, false)
	if err != nil {
		e.t.Fatalf("store team drive %q: %v", name, err)
	}
	if allocate {
		e.allocate(d)
	}
	return d
}

func (e *portalLaunchEnv) allocate(d types.UserDrive) types.UserDriveGrant {
	e.t.Helper()
	g, err := e.pg.AddUserDriveGrant(context.Background(), types.UserDriveGrant{SubjectType: types.DriveSubjectPortal,
		Subject: e.portal.ID.String(), DriveID: d.ID, Enabled: true, CreatedBy: "admin@example.com"}, true)
	if err != nil {
		e.t.Fatalf("allocate %q to the portal: %v", d.Name, err)
	}
	return g
}

// launch posts a Background run into the pool through the portal's bearer, mounting drives.
func (e *portalLaunchEnv) launch(drives ...types.UserDrive) (queuedLaunchAnswer, *httptest.ResponseRecorder) {
	e.t.Helper()
	var list []string
	for _, d := range drives {
		list = append(list, fmt.Sprintf(`{"drive_id":%q}`, d.ID))
	}
	return e.launchAttach(list...)
}

// launchAttach is launch with each drives[] entry given as its JSON, so a request can say read_only.
func (e *portalLaunchEnv) launchAttach(attachments ...string) (queuedLaunchAnswer, *httptest.ResponseRecorder) {
	e.t.Helper()
	return e.launchAs(e.bearer, attachments...)
}

// launchAs is launchAttach through another portal person's bearer.
func (e *portalLaunchEnv) launchAs(bearer string, attachments ...string) (queuedLaunchAnswer, *httptest.ResponseRecorder) {
	e.t.Helper()
	body := strings.Replace(endBody("background", `"runner_pool_id":"`+e.rp.ID.String()+`"`),
		`"no_repositories_or_drives":true`, `"drives":[`+strings.Join(attachments, ",")+`]`, 1)
	w := do(e.t, e.srv, http.MethodPost, "/api/v1/runs", bearer, body)
	var out queuedLaunchAnswer
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return out, w
}

// setMembersCanWrite changes the allocation's write posture in place.
func (e *portalLaunchEnv) setMembersCanWrite(g types.UserDriveGrant, on bool) {
	e.t.Helper()
	e.exec(`UPDATE user_drive_grants SET members_can_write = $2 WHERE id = $1`, g.ID, on)
}

// mountRow is the run.drive.mount row of run for drive, waited for: the dispatch writes it once the sandbox exists.
func (e *portalLaunchEnv) mountRow(run, drive uuid.UUID) map[string]any {
	e.t.Helper()
	var row map[string]any
	e.eventually("the run.drive.mount row of run "+run.String(), func() bool {
		for _, ev := range e.h.audit.snapshot() {
			if ev.Action != "run.drive.mount" || ev.RunID == nil || *ev.RunID != run {
				continue
			}
			var data map[string]any
			if json.Unmarshal(ev.Data, &data) == nil && data["drive_id"] == drive.String() {
				row = data
				return true
			}
		}
		return false
	})
	return row
}

// dispatchedMount is the mount of drive in the sandbox spec the dispatch of run handed the runner, waited for: what the
// runner would mount, not the receipt written about it.
func (e *portalLaunchEnv) dispatchedMount(run, drive uuid.UUID) types.DriveMount {
	e.t.Helper()
	var got types.DriveMount
	e.eventually("the sandbox of run "+run.String()+" to be created with the drive", func() bool {
		e.runner.fakeRunner.mu.Lock()
		spec := e.runner.fakeRunner.lastSpec
		e.runner.fakeRunner.mu.Unlock()
		if spec.RunID != run {
			return false
		}
		for _, m := range spec.Drives {
			if m.DriveID == drive {
				got = m
				return true
			}
		}
		return false
	})
	return got
}

// The adopter's first test: a person of the portal launches with the portal's drive and gets one run that
// mounts it read-only (the allocation never said members_can_write), recorded as a portal drive run, with the
// mount row naming the person, the portal and the drive.
func TestPortalTeamDrive_PortalRunMountsReadOnly_PG(t *testing.T) {
	e := newPortalLaunchEnv(t, 5)
	d := e.teamDrive("Portal Data", true)
	out, w := e.launch(d)
	if w.Code != http.StatusCreated {
		t.Fatalf("launch with the portal's drive = %d %s, want 201", w.Code, w.Body)
	}
	run := e.run(out.ID)
	if !run.PortalDrives || run.CreatedVia == nil || *run.CreatedVia != e.portal.ID {
		t.Errorf("run = portal_drives %v via %v, want a portal drive run created through %s", run.PortalDrives, run.CreatedVia, e.portal.ID)
	}
	if m := e.dispatchedMount(run.ID, d.ID); !m.ReadOnly || !m.Team || m.ObjectName != types.DrivePortalObjectName(d, types.DrivePortalHome(d, e.portal.ID)) {
		t.Errorf("dispatched mount = %+v, want a read-only team mount of the portal's own folder", m)
	}
	row := e.mountRow(run.ID, d.ID)
	if row["mode"] != "ro" || row["portal"] != e.portal.ID.String() || row["mounted_by"] != e.principal || row["team"] != true {
		t.Errorf("mount row = %v, want ro, team, portal %s, mounted_by %s", row, e.portal.ID, e.principal)
	}
}

// A queued run's drive is judged again when it starts: with the allocation removed while it waited the run ends
// with the 403 reason, with the portal revoked it ends queue_portal_revoked, and a run whose allocation stood
// starts.
func TestPortalTeamDrive_QueuedRunRecheckedAtStart_PG(t *testing.T) {
	t.Run("allocation removed", func(t *testing.T) {
		e := newPortalLaunchEnv(t, 2)
		kept, gone := e.teamDrive("Kept Data", true), e.teamDrive("Gone Data", false)
		grant := e.allocate(gone)
		first, second := e.occupy(e.rp), e.occupy(e.rp)
		keptRun, w1 := e.launch(kept)
		goneRun, w2 := e.launch(gone)
		for _, c := range []struct {
			w   *httptest.ResponseRecorder
			run queuedLaunchAnswer
		}{{w1, keptRun}, {w2, goneRun}} {
			if c.w.Code != http.StatusCreated || c.run.State != types.RunQueued {
				t.Fatalf("launch into a full pool = %d %s, want 201 QUEUED", c.w.Code, c.w.Body)
			}
		}
		if _, err := e.pg.DeleteUserDriveGrant(context.Background(), grant.ID); err != nil {
			t.Fatal(err)
		}
		e.free(first)
		e.free(second)
		e.waitStarted(keptRun.ID)
		got := e.waitFailed(goneRun.ID)
		if want := statusDetailQueuePrefix + string(authz.ReasonDriveNotAllocatedToPortal); got.StatusDetail != want {
			t.Errorf("a run whose allocation was removed while it waited: %s %q, want %q", got.State, got.StatusDetail, want)
		}
	})
	t.Run("portal revoked", func(t *testing.T) {
		e := newPortalLaunchEnv(t, 1)
		d := e.teamDrive("Portal Data", true)
		occupant := e.occupy(e.rp)
		queued, w := e.launch(d)
		if w.Code != http.StatusCreated || queued.State != types.RunQueued {
			t.Fatalf("launch into a full pool = %d %s, want 201 QUEUED", w.Code, w.Body)
		}
		if _, _, err := e.pg.RevokeDelegate(context.Background(), e.portal.ID, time.Now().UTC(), false); err != nil {
			t.Fatal(err)
		}
		e.free(occupant)
		got := e.waitFailed(queued.ID)
		if want := statusDetailQueuePrefix + string(types.RunEndQueuePortalRevoked); got.StatusDetail != want {
			t.Errorf("a run whose portal was revoked while it waited: %s %q, want %q", got.State, got.StatusDetail, want)
		}
	})
}

// A run already running keeps its drive when the allocation is removed and when the portal is revoked: it is not
// ended, no unmount is recorded, its mount row stands; the next create is refused (403, then 401). To stop the
// access an admin kills the run.
func TestPortalTeamDrive_RunningRunKeepsMount_PG(t *testing.T) {
	e := newPortalLaunchEnv(t, 5)
	d := e.teamDrive("Portal Data", false)
	grant := e.allocate(d)
	out, w := e.launch(d)
	if w.Code != http.StatusCreated {
		t.Fatalf("launch = %d %s, want 201", w.Code, w.Body)
	}
	e.mountRow(out.ID, d.ID)

	if _, err := e.pg.DeleteUserDriveGrant(context.Background(), grant.ID); err != nil {
		t.Fatal(err)
	}
	if _, w := e.launch(d); w.Code != http.StatusForbidden || errorReason(w) != string(authz.ReasonDriveNotAllocatedToPortal) {
		t.Errorf("a new launch after the removal = %d %s, want 403 drive_not_allocated_to_portal", w.Code, w.Body)
	}
	if _, _, err := e.pg.RevokeDelegate(context.Background(), e.portal.ID, time.Now().UTC(), false); err != nil {
		t.Fatal(err)
	}
	if _, w := e.launch(d); w.Code != http.StatusUnauthorized {
		t.Errorf("a new launch after the revoke = %d %s, want 401", w.Code, w.Body)
	}
	run := e.run(out.ID)
	if run.State.IsTerminal() || !run.PortalDrives {
		t.Errorf("the running run = %s portal_drives %v, want it still live and still a portal drive run", run.State, run.PortalDrives)
	}
	for _, ev := range e.h.audit.snapshot() {
		if ev.RunID != nil && *ev.RunID == out.ID && strings.HasPrefix(ev.Action, "run.drive.") && ev.Action != "run.drive.mount" {
			t.Errorf("an audit row %q for the running run: nothing unmounts it", ev.Action)
		}
	}
}

// Two of the portal's drives whose names make one folder name are refused together at the run, and nothing is
// created.
func TestPortalTeamDrive_SlugCollisionRefusedAtRun_PG(t *testing.T) {
	e := newPortalLaunchEnv(t, 5)
	a, b := e.teamDrive("Team Data", true), e.teamDrive("team data", true)
	_, w := e.launch(a, b)
	if w.Code != http.StatusUnprocessableEntity || errorReason(w) != string(authz.ReasonDriveTargetConflict) {
		t.Fatalf("two drives on one folder name = %d %s, want 422 drive_target_conflict", w.Code, w.Body)
	}
	if n := e.count(`SELECT count(*) FROM agent_runs WHERE created_via = $1`, e.portal.ID); n != 0 {
		t.Errorf("%d run(s) created by the refused launch, want none", n)
	}
}

// A queued portal run starts with the mode it was admitted with, never wider: each case waits in a full pool, the
// allocation changes, and the run that starts mounts what the table says. The mode is read from the mount in the sandbox spec
// the dispatch handed the runner, and from the run.drive.mount row written from it.
//
//	admitted      request        allocation at start   mounts
//	read-only     omitted        writable              ro  (never widened)
//	writable      omitted        writable              rw  (control)
//	writable      omitted        read-only             ro  (still narrows)
//	writable      read_only:true writable              ro  (explicit)
func TestPortalTeamDrive_QueuedRunKeepsAdmittedMode_PG(t *testing.T) {
	for _, c := range []struct {
		name       string
		admittedRW bool   // the allocation's members_can_write at the launch
		attach     string // "" is the drive's id alone, else the drives[] entry with %s for the id
		startRW    bool   // the allocation's members_can_write at the start
		want       string
	}{
		{"read-only becomes writable while queued", false, "", true, "ro"},
		{"writable stays writable (control)", true, "", true, "rw"},
		{"writable becomes read-only while queued", true, "", false, "ro"},
		{"an explicit read_only stays read-only", true, `{"drive_id":%q,"read_only":true}`, true, "ro"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newPortalLaunchEnv(t, 1)
			d := e.teamDrive("Portal Data", false)
			grant := e.allocate(d)
			e.setMembersCanWrite(grant, c.admittedRW)
			occupant := e.occupy(e.rp)
			entry := fmt.Sprintf(`{"drive_id":%q}`, d.ID)
			if c.attach != "" {
				entry = fmt.Sprintf(c.attach, d.ID)
			}
			queued, w := e.launchAttach(entry)
			if w.Code != http.StatusCreated || queued.State != types.RunQueued {
				t.Fatalf("launch into a full pool = %d %s, want 201 QUEUED", w.Code, w.Body)
			}
			var launch queuedLaunch
			var raw []byte
			if err := e.pool.QueryRow(context.Background(), `SELECT launch FROM run_queue WHERE run_id = $1`, queued.ID).Scan(&raw); err != nil || json.Unmarshal(raw, &launch) != nil {
				t.Fatalf("read the stored launch: %v", err)
			}
			if _, recorded := launch.DriveModes[d.ID.String()]; !recorded {
				t.Fatalf("the stored launch records no mode for the portal drive: %v", launch.DriveModes)
			}
			e.setMembersCanWrite(grant, c.startRW)
			e.free(occupant)
			e.waitStarted(queued.ID)
			if m := e.dispatchedMount(queued.ID, d.ID); m.ReadOnly != (c.want == "ro") {
				t.Errorf("dispatched mount read_only = %v, want mode %s", m.ReadOnly, c.want)
			}
			if row := e.mountRow(queued.ID, d.ID); row["mode"] != c.want {
				t.Errorf("mount row mode = %v, want %s", row["mode"], c.want)
			}
		})
	}
}

// A queued run that mounted a portal's drive starts only through the portal that created it: the run row's own
// creator is compared with the portal the stored authority names. A row whose creator differs from the authority's
// portal ends portal_run_scope with nothing started; a row that agrees starts.
func TestPortalTeamDrive_QueuedStartChecksTheCreatingPortal_PG(t *testing.T) {
	for _, c := range []struct {
		name     string
		mismatch bool
	}{{"the creator and the authority agree", false}, {"the creator is another portal", true}} {
		t.Run(c.name, func(t *testing.T) {
			e := newPortalLaunchEnv(t, 1)
			d := e.teamDrive("Portal Data", true)
			other := e.issuerPortal(t, nil).portal
			occupant := e.occupy(e.rp)
			queued, w := e.launch(d)
			if w.Code != http.StatusCreated || queued.State != types.RunQueued {
				t.Fatalf("launch into a full pool = %d %s, want 201 QUEUED", w.Code, w.Body)
			}
			if c.mismatch {
				e.exec(`UPDATE agent_runs SET created_via = $2 WHERE id = $1`, queued.ID, other.ID)
			}
			e.free(occupant)
			if !c.mismatch {
				e.waitStarted(queued.ID)
				return
			}
			got := e.waitFailed(queued.ID)
			if want := statusDetailQueuePrefix + queueEndPortalRunScope; got.StatusDetail != want {
				t.Errorf("a run whose creator is not the authority's portal = %s %q, want %q", got.State, got.StatusDetail, want)
			}
		})
	}
}

// A portal never reaches what only another portal held: the second portal is refused the first's drive, removing the
// first's allocation changes nothing for the second, and each portal then mounts its own folder, never the other's.
// The first portal, allocated again, returns to its own earlier folder. A revoked second portal cannot launch while the
// first still can.
func TestPortalTeamDrive_OtherPortalNeverReachesAfterRemovalOrRevoke_PG(t *testing.T) {
	e := newPortalLaunchEnv(t, 8)
	d := e.teamDrive("Shared Data", false)
	first := e.allocate(d)
	second := e.issuerPortal(t, nil).portal
	_, secondBearer := e.portalBearer(t, second, "alice")
	denied := func(who, bearer string) {
		t.Helper()
		if _, w := e.launchAs(bearer, fmt.Sprintf(`{"drive_id":%q}`, d.ID)); w.Code != http.StatusForbidden || errorReason(w) != string(authz.ReasonDriveNotAllocatedToPortal) {
			t.Errorf("%s asking for the drive = %d %s, want 403 drive_not_allocated_to_portal", who, w.Code, w.Body)
		}
	}
	denied("the second portal before any allocation of its own", secondBearer)

	if _, err := e.pg.DeleteUserDriveGrant(context.Background(), first.ID); err != nil {
		t.Fatal(err)
	}
	denied("the second portal after the first's allocation was removed", secondBearer)
	denied("the first portal after its allocation was removed", e.bearer)

	if _, err := e.pg.AddUserDriveGrant(context.Background(), types.UserDriveGrant{SubjectType: types.DriveSubjectPortal,
		Subject: second.ID.String(), DriveID: d.ID, Enabled: true, CreatedBy: "admin@example.com"}, true); err != nil {
		t.Fatalf("allocate the drive to the second portal: %v", err)
	}
	firstFolder := types.DrivePortalObjectName(d, types.DrivePortalHome(d, e.portal.ID))
	secondFolder := types.DrivePortalObjectName(d, types.DrivePortalHome(d, second.ID))
	if firstFolder == secondFolder {
		t.Fatalf("the two portals name one folder %q", firstFolder)
	}
	out, w := e.launchAs(secondBearer, fmt.Sprintf(`{"drive_id":%q}`, d.ID))
	if w.Code != http.StatusCreated {
		t.Fatalf("the second portal's launch = %d %s, want 201", w.Code, w.Body)
	}
	if m := e.dispatchedMount(out.ID, d.ID); m.ObjectName != secondFolder {
		t.Errorf("the second portal mounted %q, want its own folder %q", m.ObjectName, secondFolder)
	}
	denied("the first portal while only the second is allocated", e.bearer)

	e.allocate(d)
	again, w := e.launch(d)
	if w.Code != http.StatusCreated {
		t.Fatalf("the first portal allocated again = %d %s, want 201", w.Code, w.Body)
	}
	if m := e.dispatchedMount(again.ID, d.ID); m.ObjectName != firstFolder {
		t.Errorf("the first portal mounted %q after a new allocation, want its earlier folder %q", m.ObjectName, firstFolder)
	}

	if _, _, err := e.pg.RevokeDelegate(context.Background(), second.ID, time.Now().UTC(), false); err != nil {
		t.Fatal(err)
	}
	if _, w := e.launchAs(secondBearer, fmt.Sprintf(`{"drive_id":%q}`, d.ID)); w.Code != http.StatusUnauthorized {
		t.Errorf("the revoked second portal's launch = %d %s, want 401", w.Code, w.Body)
	}
	if _, w := e.launch(d); w.Code != http.StatusCreated {
		t.Errorf("the first portal after the second was revoked = %d %s, want 201", w.Code, w.Body)
	}
}
