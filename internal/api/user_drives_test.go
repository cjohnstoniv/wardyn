// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ─── the CRUD store double ────────────────────────────────────────────────────

// driveCRUDStore is an in-memory user_drives + user_drive_grants pair with the
// two behaviours the HANDLERS branch on and nothing else: UNIQUE(name) answers
// ErrConflict, and ON DELETE RESTRICT answers ErrConflict while a grant points
// at the drive. Both are enforced by the schema in production, so a double that
// did not model them would let a handler's 409 arms go untested — which is
// exactly the arm that keeps a delete from silently un-allocating people.
type driveCRUDStore struct {
	// noGovernanceStore rather than a bare store.Store, for the reason
	// driveStore embeds it too: the two RESOLVER reads (ResolveUserDrive,
	// HasGroupTierDriveGrants) already have one "this deployment has adopted
	// neither" answer, and a double that re-typed them here would be a second
	// copy of it free to drift. The CRUD handlers under test do not resolve —
	// the preview one does.
	noGovernanceStore
	drives map[uuid.UUID]types.UserDrive
	grants map[uuid.UUID]types.UserDriveGrant
	// listErr fails the two list reads, for GET /drives' 500 arms and for the
	// grant-count lookup that must NOT turn into a 500. The grant-delete audit
	// row is deliberately NOT among them any more: it is written from the
	// DELETE's own RETURNING row, and TestDeleteUserDriveGrantAuditsTheReclaimIntent
	// sets this to prove the scan is gone.
	listErr error
}

func newDriveCRUDStore() *driveCRUDStore {
	return &driveCRUDStore{drives: map[uuid.UUID]types.UserDrive{}, grants: map[uuid.UUID]types.UserDriveGrant{}}
}

func (s *driveCRUDStore) UpsertUserDrive(_ context.Context, d types.UserDrive) (types.UserDrive, error) {
	for id, existing := range s.drives {
		if id != d.ID && strings.EqualFold(existing.Name, d.Name) {
			return types.UserDrive{}, store.ErrConflict
		}
	}
	s.drives[d.ID] = d
	return d, nil
}

func (s *driveCRUDStore) GetUserDrive(_ context.Context, id uuid.UUID) (types.UserDrive, error) {
	d, ok := s.drives[id]
	if !ok {
		return types.UserDrive{}, store.ErrNotFound
	}
	return d, nil
}

func (s *driveCRUDStore) DeleteUserDrive(_ context.Context, id uuid.UUID) error {
	if _, ok := s.drives[id]; !ok {
		return store.ErrNotFound
	}
	for _, g := range s.grants {
		if g.DriveID == id {
			return store.ErrConflict // ON DELETE RESTRICT
		}
	}
	delete(s.drives, id)
	return nil
}

func (s *driveCRUDStore) ListUserDrives(context.Context) ([]types.UserDriveListItem, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	var out []types.UserDriveListItem
	for _, d := range s.drives {
		n := 0
		for _, g := range s.grants {
			if g.DriveID == d.ID {
				n++
			}
		}
		out = append(out, types.UserDriveListItem{UserDrive: d, GrantCount: n})
	}
	return out, nil
}

func (s *driveCRUDStore) UpsertUserDriveGrant(_ context.Context, g types.UserDriveGrant) (types.UserDriveGrant, error) {
	if _, ok := s.drives[g.DriveID]; !ok {
		return types.UserDriveGrant{}, store.ErrNotFound // the FK
	}
	for id, existing := range s.grants {
		if existing.SubjectType == g.SubjectType && existing.Subject == g.Subject {
			g.ID = id // the EXISTING row's id, never the candidate's
			s.grants[id] = g
			return g, nil
		}
	}
	s.grants[g.ID] = g
	return g, nil
}

func (s *driveCRUDStore) DeleteUserDriveGrant(_ context.Context, id uuid.UUID) (types.UserDriveGrant, error) {
	g, ok := s.grants[id]
	if !ok {
		return types.UserDriveGrant{}, store.ErrNotFound
	}
	delete(s.grants, id)
	return g, nil // the RETURNING clause: the row that was actually removed
}

func (s *driveCRUDStore) ListUserDriveGrants(context.Context) ([]types.UserDriveGrant, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	out := make([]types.UserDriveGrant, 0, len(s.grants))
	for _, g := range s.grants {
		out = append(out, g)
	}
	return out, nil
}

// ─── the handler harness ──────────────────────────────────────────────────────

// driveAdminServer builds the SUPER-admin server the /drives handlers run
// behind, with an audit recorder so every write's row can be asserted.
// hostRoots is the boot-parsed env ceiling; nil is the DEFAULT deployment,
// which authors no host_path drive at all.
func driveAdminServer(st *driveCRUDStore, hostRoots []string) (*Server, *recRecorder) {
	audit := &recRecorder{}
	return New(Config{
		Store: st, Audit: audit, RunnerTarget: "docker",
		UserDriveHostRoots: hostRoots, LocalMode: true,
	}), audit
}

// driveCall invokes one /drives handler directly, with chi's URL params
// populated the way the router would. Direct rather than through the router
// because the AUTHORIZATION boundary is TestAuthzMatrix's job (all seven routes
// are classified classAdmin there) and this file's job is the behaviour behind
// it.
func driveCall(t *testing.T, h http.HandlerFunc, method, path, body string, params map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	rctx := chi.NewRouteContext()
	for k, v := range params {
		rctx.URLParams.Add(k, v)
	}
	w := httptest.NewRecorder()
	h(w, r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx)))
	return w
}

// driveAuditActions lists the audit actions recorded so far, in order.
func driveAuditActions(rec *recRecorder) []string {
	var out []string
	for _, ev := range rec.events {
		out = append(out, ev.Action)
	}
	return out
}

// driveAuditData returns the LAST recorded event of one action, decoded.
func driveAuditData(t *testing.T, rec *recRecorder, action string) map[string]any {
	t.Helper()
	for i := len(rec.events) - 1; i >= 0; i-- {
		if rec.events[i].Action != action {
			continue
		}
		var m map[string]any
		if len(rec.events[i].Data) > 0 {
			if err := json.Unmarshal(rec.events[i].Data, &m); err != nil {
				t.Fatalf("unmarshal %s data: %v", action, err)
			}
		}
		return m
	}
	t.Fatalf("no %s audit event; got %v", action, driveAuditActions(rec))
	return nil
}

const driveCreateBody = `{"name":"Corp NAS","backend":"docker_volume","size_mib":10240}`

// ─── writes ───────────────────────────────────────────────────────────────────

// TestCreateUserDriveWritesAndAudits pins the happy path AND the audit row's
// field set — the row is the only durable record that an admin authorized
// binding a particular host tree into other people's sandboxes, so its contents
// are part of the contract, not a debugging aid.
func TestCreateUserDriveWritesAndAudits(t *testing.T) {
	st := newDriveCRUDStore()
	srv, rec := driveAdminServer(st, nil)

	w := driveCall(t, srv.handleCreateUserDrive, http.MethodPost, "/api/v1/drives", driveCreateBody, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201: %s", w.Code, w.Body.String())
	}
	var saved types.UserDrive
	if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if saved.ID == uuid.Nil || saved.Name != "Corp NAS" {
		t.Errorf("saved = %+v, want a server-minted id and the posted name", saved)
	}
	// Defaults filled by types.ValidateUserDrive, not by the client: a drive
	// that omitted them must not land with an empty template or reclaim.
	if saved.HomeTemplate != types.HomeTemplateHash || saved.Reclaim != types.DriveReclaimRetain {
		t.Errorf("defaults = %q/%q, want hash/retain", saved.HomeTemplate, saved.Reclaim)
	}
	if saved.Writable {
		t.Error("writable = true, want the read-only product default")
	}
	d := driveAuditData(t, rec, "drive.write")
	for _, k := range []string{"name", "backend", "host_root", "storage_class", "home_template", "size_mib", "writable", "reclaim"} {
		if _, ok := d[k]; !ok {
			t.Errorf("drive.write payload is missing %q: %v", k, d)
		}
	}
}

// TestUserDriveWriteRefusals walks every way a drive write is refused, and each
// case names the counterfactual — what would be true if the check were absent.
func TestUserDriveWriteRefusals(t *testing.T) {
	realRoot := t.TempDir()
	inside := filepath.Join(realRoot, "homes")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	outside := t.TempDir()

	for _, tc := range []struct {
		name string
		// runnerTarget defaults to docker; the k8s cases set it so the
		// backend-vs-runner arm does not fire first and mask what they test.
		runnerTarget string
		roots        []string
		body         string
		want         int
		msg          string
	}{
		{
			// Without this the row stores fine and every member's run 422s
			// three days later, on a deployment that can never mount it.
			name: "a k8s backend on a docker deployment is a 400",
			body: `{"name":"cluster","backend":"k8s_pvc","home_template":"sub"}`,
			want: http.StatusBadRequest, msg: `cannot be mounted by this deployment's runner (docker)`,
		},
		{
			// THE CEILING. Unset env means no host tree may be authored at all
			// — without it an admin widens the deployment's reach from inside
			// the product, which is the one thing the env ceiling exists to
			// prevent.
			name: "a host_path drive with NO roots configured is a 422",
			body: `{"name":"share","backend":"host_path","home_template":"email_local","host_root":"` + inside + `"}`,
			want: http.StatusUnprocessableEntity, msg: "WARDYN_USER_DRIVE_HOST_ROOTS",
		},
		{
			name:  "a host_path drive OUTSIDE the roots is a 422",
			roots: []string{realRoot},
			body:  `{"name":"share","backend":"host_path","home_template":"email_local","host_root":"` + outside + `"}`,
			want:  http.StatusUnprocessableEntity, msg: "is not inside WARDYN_USER_DRIVE_HOST_ROOTS",
		},
		{
			// The bind-mount deny-list, applied at AUTHORING rather than left to
			// the driver: /etc is inside no legitimate share.
			name:  "a host_path drive under a denied prefix is a 422",
			roots: []string{"/"},
			body:  `{"name":"share","backend":"host_path","home_template":"email_local","host_root":"/etc/homes"}`,
			want:  http.StatusUnprocessableEntity, msg: "denied host path",
		},
		{
			// A share's directories are named by whoever owns the share, so a
			// hash would name a directory that does not exist and Wardyn does
			// not create one.
			name:  "a share templated on a hash is a 400",
			roots: []string{realRoot},
			body:  `{"name":"share","backend":"host_path","home_template":"hash","host_root":"` + inside + `"}`,
			want:  http.StatusBadRequest, msg: "is not allowed on a share backend",
		},
		{
			// The k8s size rule reaches the API surface too: a claim requesting
			// zero bytes is rejected by the apiserver, so this deployment refuses
			// it at authoring instead of at every member's bind.
			name: "a k8s_pvc drive with no size is a 400", runnerTarget: "k8s",
			body: `{"name":"cluster","backend":"k8s_pvc"}`,
			want: http.StatusBadRequest, msg: "it is the volume request",
		},
		{
			name: "an unknown field is a 400 (strict decode)",
			body: `{"name":"x","backend":"docker_volume","quota_mib":5}`,
			want: http.StatusBadRequest, msg: "unknown field",
		},
		{
			name: "a blank name is a 400",
			body: `{"name":"   ","backend":"docker_volume"}`,
			want: http.StatusBadRequest, msg: "name: required",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, rec := driveAdminServer(newDriveCRUDStore(), tc.roots)
			if tc.runnerTarget != "" {
				srv.cfg.RunnerTarget = tc.runnerTarget
			}
			w := driveCall(t, srv.handleCreateUserDrive, http.MethodPost, "/api/v1/drives", tc.body, nil)
			if w.Code != tc.want {
				t.Fatalf("code = %d, want %d: %s", w.Code, tc.want, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.msg) {
				t.Errorf("body = %s, want it to name %q", w.Body.String(), tc.msg)
			}
			if len(rec.events) != 0 {
				t.Errorf("audit = %v, want nothing recorded for a refused write", driveAuditActions(rec))
			}
		})
	}
}

// TestHostPathDriveInsideRootsIsAccepted is the positive control for the four
// refusals above: with the ceiling configured and the root inside it, the write
// lands. Without this the refusals could all be passing because host_path is
// broken outright.
func TestHostPathDriveInsideRootsIsAccepted(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "homes")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	srv, _ := driveAdminServer(newDriveCRUDStore(), []string{root})
	body := `{"name":"share","backend":"host_path","home_template":"email_local","host_root":"` + inside + `"}`
	if w := driveCall(t, srv.handleCreateUserDrive, http.MethodPost, "/api/v1/drives", body, nil); w.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201: %s", w.Code, w.Body.String())
	}
}

// TestUserDriveNameConflictIs409 pins UNIQUE(name) as a caller-fixable 409
// rather than a raw driver error, and TestUpdateUserDriveRenames pins that the
// SAME name on the SAME id is an update, not a conflict — renaming has to work,
// because ON DELETE RESTRICT makes delete-and-recreate impossible for an
// allocated drive.
func TestUserDriveNameConflictIs409(t *testing.T) {
	st := newDriveCRUDStore()
	srv, _ := driveAdminServer(st, nil)
	if w := driveCall(t, srv.handleCreateUserDrive, http.MethodPost, "/api/v1/drives", driveCreateBody, nil); w.Code != http.StatusCreated {
		t.Fatalf("first create = %d: %s", w.Code, w.Body.String())
	}
	w := driveCall(t, srv.handleCreateUserDrive, http.MethodPost, "/api/v1/drives", driveCreateBody, nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("second create = %d, want 409: %s", w.Code, w.Body.String())
	}
}

func TestUpdateUserDriveRenames(t *testing.T) {
	st := newDriveCRUDStore()
	srv, _ := driveAdminServer(st, nil)
	id := uuid.New()
	w := driveCall(t, srv.handleUpdateUserDrive, http.MethodPut, "/api/v1/drives/"+id.String(),
		driveCreateBody, map[string]string{"id": id.String()})
	if w.Code != http.StatusOK {
		t.Fatalf("PUT of an absent id = %d, want 200 (PUT means put): %s", w.Code, w.Body.String())
	}
	w = driveCall(t, srv.handleUpdateUserDrive, http.MethodPut, "/api/v1/drives/"+id.String(),
		`{"name":"Renamed","backend":"docker_volume"}`, map[string]string{"id": id.String()})
	if w.Code != http.StatusOK {
		t.Fatalf("rename = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := st.drives[id].Name; got != "Renamed" {
		t.Errorf("stored name = %q, want the rename to have landed", got)
	}
}

// TestDeleteAllocatedUserDriveIs409 is the RESTRICT arm and the count in its
// message. Cascading instead would silently un-allocate every person this drive
// named a directory for, with nothing in the log saying so.
func TestDeleteAllocatedUserDriveIs409(t *testing.T) {
	st := newDriveCRUDStore()
	srv, rec := driveAdminServer(st, nil)
	d := *driveFixture(nil)
	st.drives[d.ID] = d
	st.grants[uuid.New()] = types.UserDriveGrant{
		ID: uuid.New(), SubjectType: types.CapabilitySubjectUser, Subject: "bob", DriveID: d.ID, Enabled: true,
	}

	w := driveCall(t, srv.handleDeleteUserDrive, http.MethodDelete, "/api/v1/drives/"+d.ID.String(),
		"", map[string]string{"id": d.ID.String()})
	if w.Code != http.StatusConflict {
		t.Fatalf("delete = %d, want 409: %s", w.Code, w.Body.String())
	}
	// NO COUNT on the wire (the mock round's ruling): the console's own
	// pre-fill names the number it is looking at, and a second, later number
	// from here would contradict it on exactly the race path this body serves.
	if body := w.Body.String(); !strings.Contains(body, "remove its allocations first") || strings.Contains(body, "1 subject") {
		t.Errorf("body = %s, want the frozen count-free refusal", body)
	}
	if len(rec.events) != 0 {
		t.Errorf("audit = %v, want nothing recorded for a refused delete", driveAuditActions(rec))
	}

	// And no store read stands between the FK's refusal and the answer: a
	// broken list read must not turn a correct 409 into a 500.
	st.listErr = context.DeadlineExceeded
	w = driveCall(t, srv.handleDeleteUserDrive, http.MethodDelete, "/api/v1/drives/"+d.ID.String(),
		"", map[string]string{"id": d.ID.String()})
	if w.Code != http.StatusConflict {
		t.Fatalf("delete with an unreadable count = %d, want 409 anyway: %s", w.Code, w.Body.String())
	}
}

func TestDeleteUnallocatedUserDriveSucceeds(t *testing.T) {
	st := newDriveCRUDStore()
	srv, rec := driveAdminServer(st, nil)
	d := *driveFixture(nil)
	st.drives[d.ID] = d
	w := driveCall(t, srv.handleDeleteUserDrive, http.MethodDelete, "/api/v1/drives/"+d.ID.String(),
		"", map[string]string{"id": d.ID.String()})
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204: %s", w.Code, w.Body.String())
	}
	driveAuditData(t, rec, "drive.delete")
}

// ─── grants ───────────────────────────────────────────────────────────────────

// TestUserDriveGrantDefaultsEnabled is THE regression this whole *bool exists
// for. UpsertUserDriveGrant writes Enabled verbatim, so a plain bool would make
// every allocation written by a client that omits the field arrive PAUSED — an
// admin allocates a drive, sees it listed, and the member gets nothing, with no
// error anywhere to explain it.
func TestUserDriveGrantDefaultsEnabled(t *testing.T) {
	st := newDriveCRUDStore()
	srv, rec := driveAdminServer(st, nil)
	d := *driveFixture(nil)
	st.drives[d.ID] = d

	w := driveCall(t, srv.handleUpsertUserDriveGrant, http.MethodPost, "/api/v1/drives/grants",
		`{"subject_type":"user","subject":"Sub-Bob","drive_id":"`+d.ID.String()+`"}`, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201: %s", w.Code, w.Body.String())
	}
	var saved types.UserDriveGrant
	if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !saved.Enabled {
		t.Fatal("enabled = false for a grant that never mentioned it — a new allocation must be ON, or every scripted allocation lands paused")
	}
	// Subject hygiene is the SAME normalization the resolver's SQL matches on:
	// a subject lowercased one way here and another way there never matches.
	if saved.Subject != "sub-bob" {
		t.Errorf("subject = %q, want it lowercased like every other subject", saved.Subject)
	}
	if d := driveAuditData(t, rec, "drive.grant.write"); d["enabled"] != true {
		t.Errorf("drive.grant.write enabled = %v, want true", d["enabled"])
	}

	// An EXPLICIT false still pauses: the default may not swallow an admin's
	// deliberate "off".
	w = driveCall(t, srv.handleUpsertUserDriveGrant, http.MethodPost, "/api/v1/drives/grants",
		`{"subject_type":"user","subject":"sub-bob","drive_id":"`+d.ID.String()+`","enabled":false}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("repoint = %d, want 200 (the natural key already had a row): %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if saved.Enabled {
		t.Error("enabled = true after an explicit false — the default must not out-vote the admin")
	}
}

// TestUserDriveGrantRefusals pins the two shape rules that protect the
// isolation a per-user subdirectory buys.
func TestUserDriveGrantRefusals(t *testing.T) {
	st := newDriveCRUDStore()
	srv, _ := driveAdminServer(st, nil)
	d := *driveFixture(nil)
	st.drives[d.ID] = d

	// A home override on a GROUP row would hand every member of that group the
	// SAME directory — the opposite of what the per-user subdirectory is for.
	w := driveCall(t, srv.handleUpsertUserDriveGrant, http.MethodPost, "/api/v1/drives/grants",
		`{"subject_type":"group","subject":"eng","drive_id":"`+d.ID.String()+`","home_override":"shared"}`, nil)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "home_override") {
		t.Errorf("group home_override = %d %s, want 400 naming home_override", w.Code, w.Body.String())
	}

	// An unknown drive_id is the FK refusing: a 404 naming the drive, never a
	// silent no-op that leaves an admin thinking they allocated something.
	w = driveCall(t, srv.handleUpsertUserDriveGrant, http.MethodPost, "/api/v1/drives/grants",
		`{"subject_type":"user","subject":"bob","drive_id":"`+uuid.New().String()+`"}`, nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown drive_id = %d, want 404: %s", w.Code, w.Body.String())
	}
}

// TestDeleteUserDriveGrantAuditsTheReclaimIntent pins the one field that makes
// the offboarding half of the log useful: removing an allocation deletes no
// data, so the row has to say what the operator was told to do about the
// directory it was the last pointer to.
func TestDeleteUserDriveGrantAuditsTheReclaimIntent(t *testing.T) {
	st := newDriveCRUDStore()
	srv, rec := driveAdminServer(st, nil)
	d := *driveFixture(func(d *types.UserDrive) { d.Reclaim = types.DriveReclaimDelete })
	st.drives[d.ID] = d
	g := types.UserDriveGrant{
		ID: uuid.New(), SubjectType: types.CapabilitySubjectUser, Subject: "bob", DriveID: d.ID, Enabled: true,
	}
	st.grants[g.ID] = g

	w := driveCall(t, srv.handleDeleteUserDriveGrant, http.MethodDelete, "/api/v1/drives/grants/"+g.ID.String(),
		"", map[string]string{"id": g.ID.String()})
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204: %s", w.Code, w.Body.String())
	}
	data := driveAuditData(t, rec, "drive.grant.delete")
	if data["reclaim"] != string(types.DriveReclaimDelete) || data["subject"] != "bob" {
		t.Errorf("drive.grant.delete data = %v, want the subject and the drive's reclaim intent", data)
	}

	// AND the row is written from the DELETE's own RETURNING row, not from a
	// scan taken beside it: with every list read failing, the audit row is still
	// complete. The scan it replaced loaded EVERY allocation in the deployment
	// to describe one, and could describe a row a concurrent write had changed.
	st.grants[g.ID] = g
	st.listErr = errors.New("pg: connection refused")
	w = driveCall(t, srv.handleDeleteUserDriveGrant, http.MethodDelete, "/api/v1/drives/grants/"+g.ID.String(),
		"", map[string]string{"id": g.ID.String()})
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete with the list read failing = %d, want 204: %s", w.Code, w.Body.String())
	}
	if data := driveAuditData(t, rec, "drive.grant.delete"); data["subject"] != "bob" ||
		data["reclaim"] != string(types.DriveReclaimDelete) {
		t.Errorf("drive.grant.delete data = %v, want the deleted row's own fields", data)
	}
	st.listErr = nil

	w = driveCall(t, srv.handleDeleteUserDriveGrant, http.MethodDelete, "/api/v1/drives/grants/"+g.ID.String(),
		"", map[string]string{"id": g.ID.String()})
	if w.Code != http.StatusNotFound {
		t.Errorf("second delete = %d, want 404", w.Code)
	}
}

// ─── the read ─────────────────────────────────────────────────────────────────

// TestGetUserDrivesIsTheWholePicture pins the one-read contract AND the two
// deployment scalars the console cannot derive: without host_roots_configured
// the backend picker offers an option whose every save 422s, and without
// runner_target it offers the two backends this deployment cannot mount.
func TestGetUserDrivesIsTheWholePicture(t *testing.T) {
	st := newDriveCRUDStore()
	d := *driveFixture(nil)
	st.drives[d.ID] = d
	st.grants[uuid.New()] = types.UserDriveGrant{
		ID: uuid.New(), SubjectType: types.CapabilitySubjectAll, DriveID: d.ID, Enabled: true,
	}

	srv, _ := driveAdminServer(st, nil)
	w := driveCall(t, srv.handleGetUserDrives, http.MethodGet, "/api/v1/drives", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get = %d: %s", w.Code, w.Body.String())
	}
	var got userDrivesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Drives) != 1 || got.Drives[0].GrantCount != 1 {
		t.Errorf("drives = %+v, want one drive carrying its grant count", got.Drives)
	}
	if len(got.Grants) != 1 {
		t.Errorf("grants = %+v, want the allocations in the SAME read", got.Grants)
	}
	if got.HostRootsConfigured {
		t.Error("host_roots_configured = true with no roots set")
	}
	if got.RunnerTarget != "docker" {
		t.Errorf("runner_target = %q, want docker", got.RunnerTarget)
	}

	srv, _ = driveAdminServer(st, []string{"/srv/homes"})
	w = driveCall(t, srv.handleGetUserDrives, http.MethodGet, "/api/v1/drives", "", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.HostRootsConfigured {
		t.Error("host_roots_configured = false with roots set")
	}
}

// TestPreviewUserDriveRoundTrip pins D1's preview handler on the route D2
// registers it at: the admin's "who gets what" answer comes from THE resolver,
// and the OBJECT NAME it returns is the string the offboarding runbook copies
// rather than computing a hash by hand.
func TestPreviewUserDriveRoundTrip(t *testing.T) {
	d := driveFixture(nil)
	st := &driveStore{drive: d, grant: grantFixture(d.ID, nil), tier: types.CapabilitySubjectUser}
	srv := New(Config{Store: st, Audit: &recRecorder{}, RunnerTarget: "docker"})

	w := driveCall(t, srv.handlePreviewUserDrive, http.MethodPost, "/api/v1/drives/preview",
		`{"user_subjects":["sub-drive-bob","bob@corp.example"],"groups":["eng"]}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("preview = %d: %s", w.Code, w.Body.String())
	}
	var got userDrivePreviewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.DriveName != d.Name || got.MatchedTier != types.CapabilitySubjectUser {
		t.Errorf("preview = %+v, want the drive and the tier that won", got)
	}
	// The SAME derivation the runner will use, not a second copy of it.
	wantHome, err := types.DriveHomeName(*d, "sub-drive-bob", "")
	if err != nil {
		t.Fatalf("DriveHomeName: %v", err)
	}
	if got.HomeName != wantHome || got.ObjectName != types.DriveObjectName(*d, wantHome) {
		t.Errorf("home/object = %q/%q, want %q/%q", got.HomeName, got.ObjectName, wantHome, types.DriveObjectName(*d, wantHome))
	}

	// No match is the EMPTY OBJECT, not a 404: "nobody is allocated this" is an
	// answer an admin came for.
	srv = New(Config{Store: &driveStore{}, Audit: &recRecorder{}, RunnerTarget: "docker"})
	w = driveCall(t, srv.handlePreviewUserDrive, http.MethodPost, "/api/v1/drives/preview", `{"user_subjects":["nobody"]}`, nil)
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "{}" {
		t.Errorf("no-match preview = %d %s, want 200 {}", w.Code, w.Body.String())
	}
}

// ─── the reserved target ──────────────────────────────────────────────────────

// TestDriveTargetIsReservedFromAuthoring pins the refusal at BOTH authoring
// seams the reserved path can be named from. Without it a policy mount or a
// workspace source lands on the same in-container path as the member's own
// drive, and one of the two silently disappears inside a running sandbox —
// which is not a failure anybody can act on.
func TestDriveTargetIsReservedFromAuthoring(t *testing.T) {
	for _, target := range []string{runner.DriveTarget, runner.DriveTarget + "/shared"} {
		t.Run("policy workspace_mounts "+target, func(t *testing.T) {
			err := validatePolicySpec(types.RunPolicySpec{
				MinConfinementClass: types.CC1,
				WorkspaceMounts:     []types.WorkspaceMount{{Source: "/srv/data", Target: target}},
			})
			if err == nil || !strings.Contains(err.Error(), "reserved") {
				t.Fatalf("err = %v, want a reserved-target refusal", err)
			}
		})
		t.Run("policy workspace_repos "+target, func(t *testing.T) {
			err := validatePolicySpec(types.RunPolicySpec{
				MinConfinementClass: types.CC1,
				WorkspaceRepos:      []types.WorkspaceRepo{{Repo: "octocat/hello", Target: target}},
			})
			if err == nil || !strings.Contains(err.Error(), "reserved") {
				t.Fatalf("err = %v, want a reserved-target refusal", err)
			}
		})
		t.Run("workspace source "+target, func(t *testing.T) {
			msg := validateWorkspaceSource(types.WorkspaceSource{
				Type: types.WorkspaceSourceTypeEphemeral, Target: target,
			})
			if !strings.Contains(msg, "reserved") {
				t.Fatalf("msg = %q, want a reserved-target refusal", msg)
			}
		})
	}
	// The refusal is FROZEN COPY, so pin the bytes and not just the word:
	// DRIVE_MEMBER.REFUSED_TARGET_RESERVED (ui/src/app/lib/user-drives-copy.ts,
	// docs/design/user-drives-prompt.md §7.7) is what the console renders, and a
	// Contains("reserved") check above would pass on any rewording of it. The
	// [0] is the MOUNT'S POSITION — validatePolicySpec prefixes every mount
	// error with its index — so the canon spells the first mount's.
	const canonReservedTarget = "workspace_mounts[0]: target /home/agent/drive is reserved for the user drive"
	if err := validatePolicySpec(types.RunPolicySpec{
		MinConfinementClass: types.CC1,
		WorkspaceMounts:     []types.WorkspaceMount{{Source: "/srv/data", Target: runner.DriveTarget}},
	}); err == nil || err.Error() != canonReservedTarget {
		t.Errorf("err = %v, want the frozen canon %q", err, canonReservedTarget)
	}

	// The positive control: a NEIGHBOURING path under the same allowed prefix
	// is untouched, so the refusal is the reserved subtree and not /home/agent.
	if msg := validateWorkspaceSource(types.WorkspaceSource{
		Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/drives-report",
	}); msg != "" {
		t.Errorf("neighbouring target refused: %q — the reservation must be the subtree, not a prefix match", msg)
	}
}
