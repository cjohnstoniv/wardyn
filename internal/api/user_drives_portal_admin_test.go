// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// portalGrantStore is driveCRUDStore with the additive allocation write, which answers addErr when it is set.
type portalGrantStore struct {
	*driveCRUDStore
	addErr error
	added  []types.UserDriveGrant
	// readsAfterWrite counts the drive reads attempted once a portal row was committed.
	readsAfterWrite int
}

func (s *portalGrantStore) AddUserDriveGrant(_ context.Context, g types.UserDriveGrant, _ bool) (types.UserDriveGrant, error) {
	if s.addErr != nil {
		return types.UserDriveGrant{}, s.addErr
	}
	s.added = append(s.added, g)
	return g, nil
}

// GetUserDrive answers like driveCRUDStore's until a portal row has been written; after that it fails, as a database
// that dropped between the commit and the answer would.
func (s *portalGrantStore) GetUserDrive(ctx context.Context, id uuid.UUID) (types.UserDrive, error) {
	if len(s.added) > 0 {
		s.readsAfterWrite++
		return types.UserDrive{}, errors.New("database went away after the commit")
	}
	return s.driveCRUDStore.GetUserDrive(ctx, id)
}

func (s *portalGrantStore) ReplaceUserDriveGrant(context.Context, types.UserDriveGrant, bool) (types.UserDriveGrant, []uuid.UUID, error) {
	return types.UserDriveGrant{}, nil, store.ErrDrivePortalAdditiveOnly
}

func portalAdminFixture(t *testing.T) (*Server, *portalGrantStore, types.UserDrive) {
	t.Helper()
	st := &portalGrantStore{driveCRUDStore: newDriveCRUDStore()}
	st.site = teamDrivesSite(true)
	d, _ := teamDrive()
	st.drives[d.ID] = d
	srv, _ := driveAdminServer(st.driveCRUDStore, nil)
	srv.cfg.Store = st
	return srv, st, d
}

func portalAllocBody(drive types.UserDrive, portal string, extra string) string {
	return `{"subject_type":"portal","subject":"` + portal + `","drive_id":"` + drive.ID.String() + `"` + extra + `}`
}

// Only a super admin allocates a drive to a portal, the allocation is additive, and the answer carries the object
// name the folder will have.
func TestPortalDriveAllocation_SuperAdminOnly(t *testing.T) {
	srv, st, d := portalAdminFixture(t)
	portal := uuid.New()
	body := portalAllocBody(d, portal.String(), `,"additional":true`)

	w := driveGrantAs(t, srv, driveSecurityAdminCtx(), body)
	if w.Code != http.StatusForbidden || errorReason(w) != string(authz.ReasonAdminSurface) || len(st.added) != 0 {
		t.Fatalf("security admin = %d %s (%d rows), want 403 admin_surface and no row", w.Code, w.Body, len(st.added))
	}
	w = driveGrantAs(t, srv, driveSuperAdminCtx(), portalAllocBody(d, portal.String(), ""))
	if w.Code != http.StatusBadRequest || len(st.added) != 0 {
		t.Fatalf("super admin without additional = %d %s, want 400 and no row", w.Code, w.Body)
	}
	w = driveGrantAs(t, srv, driveSuperAdminCtx(), body)
	if w.Code != http.StatusCreated {
		t.Fatalf("super admin = %d %s, want 201", w.Code, w.Body)
	}
	var out struct {
		ObjectName string `json:"object_name"`
		Subject    string `json:"subject"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if want := types.DrivePortalObjectName(d, types.DrivePortalHome(d, portal)); out.ObjectName != want || out.Subject != portal.String() {
		t.Errorf("answer = %+v, want object_name %q for portal %s", out, want, portal)
	}
	if len(st.added) != 1 || st.added[0].SubjectType != types.DriveSubjectPortal {
		t.Errorf("rows = %+v, want one portal row", st.added)
	}
}

// A committed allocation is answered and audited with no further database read: the object name is a function of the
// portal and the drive's id, so a store that fails every read after the commit still gets its 201 and its
// drive.grant.write row.
func TestPortalDriveAllocation_CommittedGrantNeedsNoSecondRead(t *testing.T) {
	srv, st, d := portalAdminFixture(t)
	audit := &recRecorder{}
	srv.cfg.Audit = audit
	portal := uuid.New()
	w := driveGrantAs(t, srv, driveSuperAdminCtx(), portalAllocBody(d, portal.String(), `,"additional":true`))
	if w.Code != http.StatusCreated {
		t.Fatalf("allocation with a failing post-commit read = %d %s, want 201", w.Code, w.Body)
	}
	if st.readsAfterWrite != 0 {
		t.Errorf("%d drive reads after the commit, want 0", st.readsAfterWrite)
	}
	var out struct {
		ObjectName string `json:"object_name"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if want := types.DrivePortalObjectName(d, types.DrivePortalHome(d, portal)); out.ObjectName != want {
		t.Errorf("object_name = %q, want %q", out.ObjectName, want)
	}
	var writes int
	for _, ev := range audit.events {
		if ev.Action == "drive.grant.write" {
			writes++
		}
	}
	if writes != 1 {
		t.Errorf("drive.grant.write rows = %d, want 1 (events %+v)", writes, audit.events)
	}
}

// Each refusal of a portal allocation is its own status and reason, writes no row, and a subject that is not a
// canonical lower-case uuid is refused at the shape check.
func TestPortalDriveAllocation_TeamDriveAndIssuerPortalOnly(t *testing.T) {
	srv, st, d := portalAdminFixture(t)
	super := driveSuperAdminCtx()
	for _, c := range []struct {
		name   string
		err    error
		status int
		reason string
	}{
		{"not a team drive", store.ErrDrivePortalNotTeam, http.StatusBadRequest, reasonUserDriveGrantInvalid},
		{"a host path drive", store.ErrDrivePortalBackend, http.StatusUnprocessableEntity, reasonDrivePortalBackend},
		{"a drive with a second place", store.ErrDrivePortalSecondPlace, http.StatusUnprocessableEntity, reasonDrivePortalSecondPlace},
		{"a console portal", store.ErrDrivePortalNotIssuer, http.StatusUnprocessableEntity, reasonDrivePortalNotIssuerPortal},
		{"a revoked or unknown portal", store.ErrDrivePortalNotLive, http.StatusUnprocessableEntity, reasonDrivePortalNotLive},
	} {
		st.addErr = c.err
		w := driveGrantAs(t, srv, super, portalAllocBody(d, uuid.NewString(), `,"additional":true`))
		if w.Code != c.status || errorReason(w) != c.reason {
			t.Errorf("%s = %d %s, want %d %s", c.name, w.Code, w.Body, c.status, c.reason)
		}
	}
	st.addErr = nil
	for name, subject := range map[string]string{
		"an upper-case id": "ABCDEFAB-0000-0000-0000-000000000000", "a name": "platform", "an empty subject": "",
	} {
		if w := driveGrantAs(t, srv, super, portalAllocBody(d, subject, `,"additional":true`)); w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d %s, want 400", name, w.Code, w.Body)
		}
	}
	if len(st.added) != 0 {
		t.Errorf("refused allocations wrote %+v", st.added)
	}
}

// A drive edit the store refuses because a portal holds the drive is a 422 with its own reason.
func TestPortalDriveAllocation_DriveEditRefusedWhileHeld(t *testing.T) {
	for _, c := range []struct {
		err    error
		reason string
	}{{store.ErrDrivePortalBackend, reasonDrivePortalBackend}, {store.ErrDrivePortalSecondPlace, reasonDrivePortalSecondPlace}} {
		srv, st, d := portalAdminFixture(t)
		st.upsertErr = c.err
		w := driveCall(t, srv.handleUpdateUserDrive, http.MethodPut, "/api/v1/drives/"+d.ID.String(),
			`{"name":"Team Data","backend":"docker_volume","team":true}`, map[string]string{"id": d.ID.String()})
		if w.Code != http.StatusUnprocessableEntity || errorReason(w) != c.reason {
			t.Errorf("edit refused with %v = %d %s, want 422 %s", c.err, w.Code, w.Body, c.reason)
		}
	}
}

// An edit that would unmark a drive a portal holds as a team drive is a 409 with the re-home reason and writes nothing.
func TestPortalDriveAllocation_UnmarkTeamRefusedWhileHeld(t *testing.T) {
	srv, st, d := portalAdminFixture(t)
	st.upsertErr = store.ErrDrivePortalNotTeam
	w := driveCall(t, srv.handleUpdateUserDrive, http.MethodPut, "/api/v1/drives/"+d.ID.String(),
		`{"name":"Team Data","backend":"docker_volume","team":false}`, map[string]string{"id": d.ID.String()})
	if w.Code != http.StatusConflict || errorReason(w) != reasonUserDriveRehomeInvalid {
		t.Errorf("unmark refused = %d %s, want 409 %s", w.Code, w.Body, reasonUserDriveRehomeInvalid)
	}
}

// rehomeFixture is the portal admin fixture on a static claim (Kubernetes target, which accepts both the hash and the sub
// home template) holding portals portal allocations and people group allocations of its team drive, named under scheme,
// with a PUT caller that sends the drive back with a new name and home template.
func rehomeFixture(t *testing.T, portals, people int, scheme types.DriveObjectScheme) (*portalGrantStore, *recRecorder, types.UserDrive, func(name string, template types.HomeTemplate, query string) *httptest.ResponseRecorder) {
	t.Helper()
	srv, st, d := portalAdminFixture(t)
	d.Backend, d.ObjectScheme = types.DriveBackendK8sPVCStatic, scheme
	st.drives[d.ID] = d
	srv.cfg.RunnerTarget = d.Backend.RunnerTarget()
	rec := &recRecorder{}
	srv.cfg.Audit = rec
	for range portals {
		g := types.UserDriveGrant{ID: uuid.New(), SubjectType: types.DriveSubjectPortal, Subject: uuid.NewString(), DriveID: d.ID, Enabled: true}
		st.grants[g.ID] = g
	}
	for i := range people {
		g := types.UserDriveGrant{ID: uuid.New(), SubjectType: types.CapabilitySubjectGroup, Subject: fmt.Sprintf("grp-%d", i), DriveID: d.ID, Enabled: true}
		st.grants[g.ID] = g
	}
	return st, rec, d, func(name string, template types.HomeTemplate, query string) *httptest.ResponseRecorder {
		return driveCall(t, srv.handleUpdateUserDrive, http.MethodPut, "/api/v1/drives/"+d.ID.String()+query,
			`{"name":"`+name+`","backend":"k8s_pvc_static","team":true,"home_template":"`+string(template)+`"}`, map[string]string{"id": d.ID.String()})
	}
}

// On a production (ID-named) static team drive a home template change names no storage for a group (the team folder is
// named by the drive id) and none for a portal (named by the portal and drive ids), so it is written with no
// confirmation and an audit row that claims no move, whatever mix of portals and groups holds the drive. A rename does
// the same under the ID scheme, since the name is not in the object name.
func TestPortalDriveRehome_NoStorageMovedIsAcknowledged(t *testing.T) {
	for _, c := range []struct {
		label          string
		portals, group int
	}{{"portals only", 2, 0}, {"portals and a group", 2, 1}, {"a group only", 0, 1}} {
		st, rec, d, put := rehomeFixture(t, c.portals, c.group, types.DriveObjectSchemeID)
		w := put("Team Data Renamed", types.HomeTemplateSub, "")
		if w.Code != http.StatusOK {
			t.Errorf("%s: a rename and template change = %d %s, want 200 with no confirmation", c.label, w.Code, w.Body)
			continue
		}
		if got := st.drives[d.ID]; got.Name != "Team Data Renamed" || got.HomeTemplate != types.HomeTemplateSub {
			t.Errorf("%s: stored drive = %+v, want the edit written", c.label, got)
		}
		data := driveAuditData(t, rec, "drive.write")
		if data["rehomed"] != false || data["rehomed_subjects"] != nil || data["rehomed_fields"] != nil {
			t.Errorf("%s: drive.write = %v, want rehomed false with no subjects or fields", c.label, data)
		}
	}
}

// Under the legacy slug scheme the name is in a group's object name, so a rename moves the group's folder and not the
// portals': the refusal and the confirmed audit row count the group alone and name the name, never the template.
func TestPortalDriveRehome_MixedGrantsCountOnlyWhatMoves(t *testing.T) {
	st, rec, d, put := rehomeFixture(t, 2, 1, types.DriveObjectSchemeSlug)
	w := put("Team Data Renamed", types.HomeTemplateSub, "")
	if w.Code != http.StatusConflict || errorReason(w) != reasonUserDriveRehomeInvalid ||
		!strings.Contains(w.Body.String(), "allocated to 1 subject and this change re-homes it") ||
		strings.Contains(w.Body.String(), "home_template") {
		t.Fatalf("mixed edit = %d %s, want 409 naming 1 subject and the name only", w.Code, w.Body)
	}
	if st.drives[d.ID].Name != d.Name {
		t.Errorf("the refused edit wrote %q", st.drives[d.ID].Name)
	}
	if w = put("Team Data Renamed", types.HomeTemplateSub, "?confirm=rehome"); w.Code != http.StatusOK {
		t.Fatalf("confirmed mixed edit = %d %s, want 200", w.Code, w.Body)
	}
	data := driveAuditData(t, rec, "drive.write")
	if data["rehomed"] != true || data["rehomed_subjects"] != float64(1) ||
		!reflect.DeepEqual(data["rehomed_fields"], []any{`name "Team Data" → "Team Data Renamed"`}) {
		t.Errorf("drive.write = %v, want rehomed true for 1 subject through the name alone", data)
	}
}

// What moves, by allocation kind and drive: an ordinary drive's person or group allocation is named by all five identity
// fields; a team drive's group by the backend, a host path's root and, under the slug scheme, the name; a portal's by
// the backend alone.
func TestPortalDriveRehome_AllocationsThatMove(t *testing.T) {
	name, template := `name "a" → "b"`, `home_template "hash" → "sub"`
	root := `host_root "/a" → "/b"`
	backend := `backend "docker_volume" → "k8s_pvc_static"`
	item := func(team bool, b types.DriveBackend, people, portals int) types.UserDriveListItem {
		return types.UserDriveListItem{UserDrive: types.UserDrive{Team: team, Backend: b}, GrantCount: people + portals, PortalGrantCount: portals}
	}
	same := func(b types.DriveBackend) types.UserDrive { return types.UserDrive{Backend: b} }
	for _, c := range []struct {
		label       string
		before      types.UserDriveListItem
		after       types.UserDrive
		changes     []string
		wantMoved   int
		wantChanges []string
	}{
		{"team, portals only, template and name", item(true, types.DriveBackendK8sPVCStatic, 0, 2), same(types.DriveBackendK8sPVCStatic), []string{template, name}, 0, nil},
		{"team, a group and portals, template only", item(true, types.DriveBackendK8sPVCStatic, 1, 2), same(types.DriveBackendK8sPVCStatic), []string{template}, 0, nil},
		{"team, a group and portals, name (slug scheme)", item(true, types.DriveBackendK8sPVCStatic, 1, 2), same(types.DriveBackendK8sPVCStatic), []string{template, name}, 1, []string{name}},
		{"team, a group and portals, backend", item(true, types.DriveBackendDockerVolume, 1, 2), same(types.DriveBackendK8sPVCStatic), []string{backend}, 3, []string{backend}},
		{"team, portals only, backend", item(true, types.DriveBackendDockerVolume, 0, 2), same(types.DriveBackendK8sPVCStatic), []string{backend, template}, 2, []string{backend}},
		{"team on a docker volume, a group, host root", item(true, types.DriveBackendDockerVolume, 1, 0), same(types.DriveBackendDockerVolume), []string{root}, 0, nil},
		{"team on a host path, a group, host root", item(true, types.DriveBackendHostPath, 1, 0), same(types.DriveBackendHostPath), []string{root}, 1, []string{root}},
		{"team on a host path, a group, name (slug scheme)", item(true, types.DriveBackendHostPath, 1, 0), same(types.DriveBackendHostPath), []string{name}, 0, nil},
		{"team on a host path, a group, name and host root", item(true, types.DriveBackendHostPath, 1, 0), same(types.DriveBackendHostPath), []string{name, root}, 1, []string{root}},
		{"team on a host path with a second place, a group, name", func() types.UserDriveListItem {
			it := item(true, types.DriveBackendHostPath, 1, 0)
			it.AltBackend = types.DriveBackendHostPath
			return it
		}(), same(types.DriveBackendHostPath), []string{name}, 0, nil},
		{"team on a docker volume, a group, name (slug scheme)", item(true, types.DriveBackendDockerVolume, 1, 0), same(types.DriveBackendDockerVolume), []string{name}, 1, []string{name}},
		{"ordinary drive on a host path, people, name", item(false, types.DriveBackendHostPath, 2, 0), same(types.DriveBackendHostPath), []string{name}, 2, []string{name}},
		{"ordinary drive, people, template", item(false, types.DriveBackendDockerVolume, 2, 0), same(types.DriveBackendDockerVolume), []string{template}, 2, []string{template}},
	} {
		moved, changes := driveRehomedAllocations(c.before, c.after, c.changes)
		if moved != c.wantMoved || !reflect.DeepEqual(changes, c.wantChanges) {
			t.Errorf("%s: moved %d %v, want %d %v", c.label, moved, changes, c.wantMoved, c.wantChanges)
		}
	}
}

// A legacy slug-scheme team drive on one host-path place is the root and the team home whatever it is called, so
// renaming it moves no group's folder: it is written with no confirmation and an audit row that claims no move. The same
// rename of the same legacy drive on a Docker volume, where the name is in the volume's name, still moves the group's
// folder, and the refusal and the confirmed audit row count it.
func TestTeamDriveRehome_LegacyRenameMovesOnlyANamedObject(t *testing.T) {
	root := t.TempDir()
	for _, c := range []struct {
		label, backend string
		rehomes        bool
	}{{"host path", "host_path", false}, {"docker volume", "docker_volume", true}} {
		srv, st, d := portalAdminFixture(t)
		d.Backend, d.ObjectScheme = types.DriveBackend(c.backend), types.DriveObjectSchemeSlug
		if c.backend == "host_path" {
			d.HostRoot = root
		}
		st.drives[d.ID] = d
		srv.cfg.UserDriveHostRoots = []string{root}
		rec := &recRecorder{}
		srv.cfg.Audit = rec
		for i := range 2 {
			g := types.UserDriveGrant{ID: uuid.New(), SubjectType: types.CapabilitySubjectGroup, Subject: fmt.Sprintf("grp-%d", i), DriveID: d.ID, Enabled: true}
			st.grants[g.ID] = g
		}
		hostRoot := ""
		if c.backend == "host_path" {
			hostRoot = `,"host_root":"` + root + `"`
		}
		put := func(query string) *httptest.ResponseRecorder {
			return driveCall(t, srv.handleUpdateUserDrive, http.MethodPut, "/api/v1/drives/"+d.ID.String()+query,
				`{"name":"Team Data Renamed","backend":"`+c.backend+`","team":true`+hostRoot+`}`, map[string]string{"id": d.ID.String()})
		}
		if !c.rehomes {
			if w := put(""); w.Code != http.StatusOK {
				t.Errorf("%s: rename = %d %s, want 200 with no confirmation", c.label, w.Code, w.Body)
				continue
			}
			data := driveAuditData(t, rec, "drive.write")
			if data["rehomed"] != false || data["rehomed_subjects"] != nil || data["rehomed_fields"] != nil {
				t.Errorf("%s: drive.write = %v, want rehomed false with no subjects or fields", c.label, data)
			}
			continue
		}
		if w := put(""); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "allocated to 2 subjects") {
			t.Errorf("%s: rename = %d %s, want 409 naming 2 subjects", c.label, w.Code, w.Body)
			continue
		}
		if w := put("?confirm=rehome"); w.Code != http.StatusOK {
			t.Errorf("%s: confirmed rename = %d %s, want 200", c.label, w.Code, w.Body)
			continue
		}
		data := driveAuditData(t, rec, "drive.write")
		if data["rehomed"] != true || data["rehomed_subjects"] != float64(2) {
			t.Errorf("%s: drive.write = %v, want rehomed true for 2 subjects", c.label, data)
		}
	}
}

// An allocation the store refuses under the drive's lock because the drive's team setting changed after the API judged
// it is a 409 naming that change, not the home-name or re-home message of the other conflict, and it writes nothing.
func TestDriveAllocation_TeamSettingChangedUnderLockIs409(t *testing.T) {
	srv, st, d := portalAdminFixture(t)
	st.addErr = fmt.Errorf("%w: subject_type: a team drive is allocated to groups only", store.ErrDriveGrantTeamRule)
	w := driveGrantAs(t, srv, driveSuperAdminCtx(),
		`{"subject_type":"group","subject":"platform","drive_id":"`+d.ID.String()+`","additional":true}`)
	if w.Code != http.StatusConflict || errorReason(w) != reasonUserDriveGrantConflict ||
		!strings.Contains(w.Body.String(), "team setting changed") || len(st.added) != 0 {
		t.Fatalf("allocation refused under the lock = %d %s (%d rows), want 409 %s naming the team setting", w.Code, w.Body, len(st.added), reasonUserDriveGrantConflict)
	}
}
