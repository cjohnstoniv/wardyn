// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
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
	// listErrOnce makes listErr a TRANSIENT blip on ListUserDrives: it fails
	// once and the store is healthy again.
	//
	// WHY A ONE-SHOT EXISTS AT ALL. The drive-write path takes TWO
	// ListUserDrives reads — driveHostRootNesting's, then driveRehomeGuard's —
	// and each answers 500 on its own. A permanently failing list therefore
	// cannot tell them apart: the nesting gate's 500 arm could be deleted
	// outright and "a store failure is a 500, never a pass" stayed green on the
	// rehome guard's identical answer. Failing exactly the FIRST read leaves the
	// second healthy, so the 500 can only have come from the gate under test.
	listErrOnce bool
	// upsertErr replaces the write's answer, so a caller can be shown the
	// SPECIFIC conflict sentinels UpsertUserDrive returns (R1 F177 / F284).
	// Without it only UNIQUE(name) was reachable from an api-side test, which is
	// exactly why the other two 409s shipped the name-taken sentence.
	upsertErr error
}

func newDriveCRUDStore() *driveCRUDStore {
	return &driveCRUDStore{drives: map[uuid.UUID]types.UserDrive{}, grants: map[uuid.UUID]types.UserDriveGrant{}}
}

func (s *driveCRUDStore) UpsertUserDrive(_ context.Context, d types.UserDrive) (types.UserDrive, error) {
	if s.upsertErr != nil {
		return types.UserDrive{}, s.upsertErr
	}
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
		err := s.listErr
		if s.listErrOnce {
			s.listErr = nil // the blip is consumed; the next read succeeds
		}
		return nil, err
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

func (s *driveCRUDStore) UpsertUserDriveGrant(_ context.Context, g types.UserDriveGrant, homeOverrideStated bool) (types.UserDriveGrant, error) {
	if _, ok := s.drives[g.DriveID]; !ok {
		return types.UserDriveGrant{}, store.ErrNotFound // the FK
	}
	// The SILENT RE-HOME guard, mirrored from the store's own DO UPDATE ...
	// WHERE: a write that never mentioned home_override may not clear a pinned
	// one. Mirrored rather than skipped because this double is what every
	// handler-level drive test writes through, and a double that cannot refuse
	// is a double that hides the refusal.
	if !homeOverrideStated {
		for _, existing := range s.grants {
			if existing.SubjectType == g.SubjectType && existing.Subject == g.Subject && existing.HomeOverride != "" {
				return types.UserDriveGrant{}, store.ErrConflict
			}
		}
	}
	// The one uniqueness rule the natural key does not carry, mirrored from the
	// store's own NOT EXISTS guard: a directory name on a drive is ONE
	// person's.
	for _, existing := range s.grants {
		if g.HomeOverride != "" && existing.DriveID == g.DriveID && existing.HomeOverride == g.HomeOverride &&
			!(existing.SubjectType == g.SubjectType && existing.Subject == g.Subject) {
			return types.UserDriveGrant{}, store.ErrConflict
		}
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
	// SORTED, because a Go map's iteration order is randomised per run and the
	// real read is ORDERED (tier, then priority, then subject). A double that
	// returned map order made every paging assertion depend on the run — it
	// passed alone and failed in the suite — and, worse, would have let a
	// handler that dropped the ordering look correct here.
	sort.Slice(out, func(i, j int) bool {
		if a, b := driveTierRank(out[i].SubjectType), driveTierRank(out[j].SubjectType); a != b {
			return a < b
		}
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority // DESC, as the SQL has it
		}
		return out[i].Subject < out[j].Subject
	})
	return out, nil
}

// driveTierRank mirrors userDriveTierOrder: user > group > all, most specific
// first.
func driveTierRank(t types.CapabilitySubjectType) int {
	switch t {
	case types.CapabilitySubjectUser:
		return 0
	case types.CapabilitySubjectGroup:
		return 1
	default:
		return 2
	}
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

// driveAuditEvent returns the LAST recorded event of one action, whole — the
// row's TARGET is part of the record (it names which drive was authorized) and
// a decoded payload cannot carry it.
func driveAuditEvent(t *testing.T, rec *recRecorder, action string) types.AuditEvent {
	t.Helper()
	for i := len(rec.events) - 1; i >= 0; i-- {
		if rec.events[i].Action == action {
			return rec.events[i]
		}
	}
	t.Fatalf("no %s audit event; got %v", action, driveAuditActions(rec))
	return types.AuditEvent{}
}

// driveAuditData returns the LAST recorded event of one action, decoded.
func driveAuditData(t *testing.T, rec *recRecorder, action string) map[string]any {
	t.Helper()
	ev := driveAuditEvent(t, rec, action)
	var m map[string]any
	if len(ev.Data) > 0 {
		if err := json.Unmarshal(ev.Data, &m); err != nil {
			t.Fatalf("unmarshal %s data: %v", action, err)
		}
	}
	return m
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
	// BY VALUE, not by key presence: every field is compared, and the map is
	// compared WHOLE so a tenth key cannot appear unnoticed either. The
	// host_path half of this contract — where host_root is non-empty and
	// therefore falsifiable — is TestUserDriveWriteAuditIsTheWholeRow.
	if got, want := driveAuditData(t, rec, "drive.write"), map[string]any{
		"name":          "Corp NAS",
		"backend":       "docker_volume",
		"host_root":     "",
		"storage_class": "",
		"home_template": "hash",
		"size_mib":      float64(10240),
		"writable":      false,
		"reclaim":       "retain",
		// The NINTH key, and the one that is not a column: it separates an edit
		// that moved every allocated member's storage from a cosmetic one, which
		// this single action otherwise covers indistinguishably. False on a
		// create — nothing was allocated to re-home.
		"rehomed": false,
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("drive.write data = %#v,\nwant EXACTLY %#v", got, want)
	}
	if ev := driveAuditEvent(t, rec, "drive.write"); ev.Target != saved.ID.String() {
		t.Errorf("drive.write target = %q, want the saved row's id %q", ev.Target, saved.ID)
	}
}

// TestUserDriveWriteAuditIsTheWholeRow is the drive.write payload asserted BY
// VALUE on a host_path drive — the shape where the row actually carries a claim.
//
// WHAT KEY-PRESENCE COULD NOT SEE. The assertion above this one used to check
// only that eight keys EXISTED, over a docker_volume fixture whose host_root is
// "" even on a pass. So `"host_root": "REDACTED"`, a hard-coded `"backend"`, an
// always-true `"writable"` and a target of uuid.Nil all survived it — and
// host_root is the single most audit-worthy field on the row, since it is the
// host tree this write authorized binding into other people's sandboxes. A
// redacted or invented one makes the row a record of something that did not
// happen, which is worse than no row.
//
// BOTH MODES, because ONE row cannot falsify a constant: the writable case
// catches a hard-coded false, the read-only case catches a hard-coded true, and
// read-only is the product default — so only the pair pins the field at all.
func TestUserDriveWriteAuditIsTheWholeRow(t *testing.T) {
	realRoot := t.TempDir()
	inside := filepath.Join(realRoot, "homes")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, writable := range []bool{true, false} {
		t.Run(fmt.Sprintf("writable=%v", writable), func(t *testing.T) {
			st := newDriveCRUDStore()
			srv, rec := driveAdminServer(st, []string{realRoot})
			body := fmt.Sprintf(
				`{"name":"Corp NAS","backend":"host_path","home_template":"email_local","host_root":%q,"writable":%v,"reclaim":"delete"}`,
				inside, writable)
			w := driveCall(t, srv.handleCreateUserDrive, http.MethodPost, "/api/v1/drives", body, nil)
			if w.Code != http.StatusCreated {
				t.Fatalf("create = %d, want 201: %s", w.Code, w.Body.String())
			}
			var saved types.UserDrive
			if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil {
				t.Fatalf("decode: %v", err)
			}
			ev := driveAuditEvent(t, rec, "drive.write")
			// THE TARGET names the row this payload describes, and nothing in
			// the payload carries the id. uuid.Nil — or another drive's id —
			// files the authorization under a drive that does not exist.
			if ev.Target != saved.ID.String() {
				t.Errorf("drive.write target = %q, want the saved row's id %q", ev.Target, saved.ID)
			}
			if saved.ID == uuid.Nil {
				t.Fatal("the create returned a nil id — the target assertion above would be vacuous")
			}
			var got map[string]any
			if err := json.Unmarshal(ev.Data, &got); err != nil {
				t.Fatalf("unmarshal drive.write data: %v", err)
			}
			want := map[string]any{
				"name":          "Corp NAS",
				"backend":       "host_path",
				"host_root":     inside,
				"storage_class": "",
				"home_template": "email_local",
				"size_mib":      float64(0),
				"writable":      writable,
				"reclaim":       "delete",
				// See the create-path assertion: `rehomed` marks the edits that
				// re-pointed an allocated member's storage. False here — this
				// drive has no allocations.
				"rehomed": false,
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("drive.write data = %#v,\nwant EXACTLY %#v", got, want)
			}
		})
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
			//
			// It names the DENIED PREFIX, not the mount-source phrasing the
			// generic bind refusal uses: an admin authoring a drive typed a
			// host_root, and "mount source" is vocabulary from a layer they are
			// not looking at. The prefix in parentheses is the actionable half
			// — WHICH tree bit — so it is pinned too, which is strictly more
			// than the single substring this row used to assert.
			name:  "a host_path drive under a denied prefix is a 422",
			roots: []string{"/"},
			body:  `{"name":"share","backend":"host_path","home_template":"email_local","host_root":"/etc/homes"}`,
			want:  http.StatusUnprocessableEntity, msg: "is under a denied prefix (/etc)",
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
			// THE MIRROR IMAGE, and the security half of the pair. A MANAGED
			// object is named by the HOME alone, so two people whose addresses
			// share the part before the "@" are allocated ONE volume — invisible
			// from this surface, since both allocations preview a perfectly
			// well-formed object name, and invisible to the driver too, since the
			// object carries this same drive's id.
			name: "a managed drive templated on the email local part is a 400",
			body: `{"name":"vol","backend":"docker_volume","home_template":"email_local"}`,
			want: http.StatusBadRequest, msg: "is not allowed on a managed backend",
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

// TestNestedHostRootDrivesAreRefused is the gate that has to read the other
// ROWS, and the hole it closes belongs to a MEMBER rather than to an admin's
// typo.
//
// Drive A is rooted at the share's mount point and gives alice a WRITABLE home
// under it. Drive B is then rooted INSIDE that home. Every check above passes —
// B's root is in the ceiling, exists, is not a credential directory — and B's
// whole tree is now a directory alice can rewrite from inside a run, so she can
// point B's members wherever she likes. The reverse direction is the same fact
// authored in the other order.
//
// Equal roots stay legal: that is "one share, two allocations with different
// home templates", and neither drive's members can move a root that is not
// inside anybody's home.
func TestNestedHostRootDrivesAreRefused(t *testing.T) {
	root := t.TempDir()
	shares := filepath.Join(root, "shares")
	nested := filepath.Join(shares, "alice", "team")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	sibling := filepath.Join(root, "other-shares")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := func(name, hostRoot string) string {
		return `{"name":"` + name + `","backend":"host_path","home_template":"sub","host_root":"` + hostRoot + `"}`
	}
	// The FIRST drive, which every case below is authored against.
	seed := func(t *testing.T) (*driveCRUDStore, *Server) {
		t.Helper()
		st := newDriveCRUDStore()
		srv, _ := driveAdminServer(st, []string{root})
		if w := driveCall(t, srv.handleCreateUserDrive, http.MethodPost, "/api/v1/drives", body("Shares", shares), nil); w.Code != http.StatusCreated {
			t.Fatalf("seed create = %d: %s", w.Code, w.Body.String())
		}
		return st, srv
	}

	t.Run("a descendant root is refused, naming the other drive", func(t *testing.T) {
		_, srv := seed(t)
		w := driveCall(t, srv.handleCreateUserDrive, http.MethodPost, "/api/v1/drives", body("Team", nested), nil)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("code = %d, want 422: %s", w.Code, w.Body.String())
		}
		for _, want := range []string{"Shares", shares, "separate trees"} {
			if !strings.Contains(w.Body.String(), want) {
				t.Errorf("body = %s, want it to name %q", w.Body.String(), want)
			}
		}
	})

	t.Run("an ancestor root is refused too", func(t *testing.T) {
		// Authored in the other order: the new drive CONTAINS the stored one, so
		// this drive's members would be the ones doing the redirecting.
		_, srv := seed(t)
		w := driveCall(t, srv.handleCreateUserDrive, http.MethodPost, "/api/v1/drives", body("Everything", root), nil)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("code = %d, want 422: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "Shares") {
			t.Errorf("body = %s, want it to name the drive it collides with", w.Body.String())
		}
	})

	t.Run("a sibling root is fine", func(t *testing.T) {
		_, srv := seed(t)
		if w := driveCall(t, srv.handleCreateUserDrive, http.MethodPost, "/api/v1/drives", body("Other", sibling), nil); w.Code != http.StatusCreated {
			t.Fatalf("a sibling tree was refused = %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("the SAME root is fine", func(t *testing.T) {
		// Two drives on one share with different home templates is the ordinary
		// shape, and nesting is a containment question rather than a naming one.
		_, srv := seed(t)
		if w := driveCall(t, srv.handleCreateUserDrive, http.MethodPost, "/api/v1/drives", body("Shares by email", shares), nil); w.Code != http.StatusCreated {
			t.Fatalf("a second drive on the SAME root was refused = %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("a drive is not its own ancestor", func(t *testing.T) {
		// A PUT that re-saves a row unchanged must not start refusing itself.
		st, srv := seed(t)
		var id uuid.UUID
		for k := range st.drives {
			id = k
		}
		w := driveCall(t, srv.handleUpdateUserDrive, http.MethodPut, "/api/v1/drives/"+id.String(),
			body("Shares", shares), map[string]string{"id": id.String()})
		if w.Code != http.StatusOK {
			t.Fatalf("re-saving a drive over itself = %d, want 200: %s", w.Code, w.Body.String())
		}
	})

	t.Run("a MANAGED drive is unaffected", func(t *testing.T) {
		// The gate is host_path-only: a managed drive has no host tree, so there
		// is nothing for it to nest inside.
		_, srv := seed(t)
		if w := driveCall(t, srv.handleCreateUserDrive, http.MethodPost, "/api/v1/drives", driveCreateBody, nil); w.Code != http.StatusCreated {
			t.Fatalf("a managed drive was caught by the nesting gate = %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("a store failure is a 500, never a pass", func(t *testing.T) {
		// A list that failed cannot say the tree is clear, and treating it as
		// clear is how the check silently stops biting on exactly the deployment
		// whose database is unhappy.
		//
		// ONE BLIP, not a permanently broken store, and that IS the assertion
		// (driveCRUDStore.listErrOnce carries the argument): the write path
		// takes two ListUserDrives reads and a store that failed both answered
		// 500 whichever one you deleted. Failing exactly the first leaves the
		// rehome guard's read healthy, so this 500 can only be the nesting
		// gate's own.
		st, srv := seed(t)
		st.listErr, st.listErrOnce = errors.New("boom"), true
		w := driveCall(t, srv.handleCreateUserDrive, http.MethodPost, "/api/v1/drives", body("Team", nested), nil)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("code = %d, want 500: %s", w.Code, w.Body.String())
		}
		if st.listErr != nil {
			t.Fatal("the blip was never consumed — the nesting gate did not read the store at all, so this subtest proved nothing")
		}
	})
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

// TestUpdateAllocatedUserDriveGuardsTheRehome is the asymmetry this surface had
// backwards. DELETING an allocated drive already answered 409; the far more
// consequential IN-PLACE REWRITE answered 200 and moved every allocated
// member's storage with the grant rows untouched — a rename re-points a volume
// and a claim name, a host-root correction binds a different tree, a template
// change derives a different home. Nothing is deleted, so nothing surfaces: the
// old object is retained under a name nothing in the product points at, and the
// same `drive.write` row covered a cosmetic edit and an identity-affecting one.
func TestUpdateAllocatedUserDriveGuardsTheRehome(t *testing.T) {
	// allocated seeds one drive with one grant and returns a PUT caller for it.
	allocated := func(t *testing.T, roots []string, mut func(*types.UserDrive)) (
		*driveCRUDStore, *recRecorder, uuid.UUID, func(body, query string) *httptest.ResponseRecorder) {
		t.Helper()
		st := newDriveCRUDStore()
		srv, rec := driveAdminServer(st, roots)
		d := *driveFixture(mut)
		st.drives[d.ID] = d
		st.grants[uuid.New()] = types.UserDriveGrant{
			ID: uuid.New(), SubjectType: types.CapabilitySubjectUser, Subject: "sub-bob", DriveID: d.ID, Enabled: true,
		}
		return st, rec, d.ID, func(body, query string) *httptest.ResponseRecorder {
			return driveCall(t, srv.handleUpdateUserDrive, http.MethodPut,
				"/api/v1/drives/"+d.ID.String()+query, body, map[string]string{"id": d.ID.String()})
		}
	}

	t.Run("a rename is refused, and writes nothing", func(t *testing.T) {
		st, rec, id, put := allocated(t, nil, nil)
		w := put(`{"name":"Corp NAS archive","backend":"docker_volume","size_mib":10240}`, "")
		if w.Code != http.StatusConflict {
			t.Fatalf("rename of an allocated drive = %d, want 409: %s", w.Code, w.Body.String())
		}
		if st.drives[id].Name != "Corp NAS" {
			t.Errorf("stored name = %q, want the refused write to have landed nothing", st.drives[id].Name)
		}
		// A refusal is not an event: the same silence a refused delete keeps.
		if len(rec.events) != 0 {
			t.Errorf("audit = %v, want nothing recorded for a refused edit", driveAuditActions(rec))
		}
	})

	t.Run("confirmed, it lands and the audit row says so", func(t *testing.T) {
		st, rec, id, put := allocated(t, nil, nil)
		w := put(`{"name":"Corp NAS archive","backend":"docker_volume","size_mib":10240}`, "?confirm=rehome")
		if w.Code != http.StatusOK {
			t.Fatalf("confirmed rename = %d, want 200: %s", w.Code, w.Body.String())
		}
		if st.drives[id].Name != "Corp NAS archive" {
			t.Errorf("stored name = %q, want the confirmed rename to have landed", st.drives[id].Name)
		}
		// THE POINT OF THE FIELD: one `drive.write` action covers both kinds of
		// edit, so without this the orphaning is invisible to an auditor.
		if data := driveAuditData(t, rec, "drive.write"); data["rehomed"] != true {
			t.Errorf("drive.write rehomed = %v, want true", data["rehomed"])
		}
	})

	// The guard asks types.DriveObjectName whether the object MOVES rather than
	// diffing a hand-listed set of fields — so an edit that folds to the same
	// slug is not a re-home, and a hand-listed rule would have refused it.
	t.Run("a name that folds to the same slug is not a rehome", func(t *testing.T) {
		_, rec, _, put := allocated(t, nil, nil)
		w := put(`{"name":"  Corp   NAS! ","backend":"docker_volume","size_mib":10240}`, "")
		if w.Code != http.StatusOK {
			t.Fatalf("cosmetic rename = %d, want 200 (the object does not move): %s", w.Code, w.Body.String())
		}
		if data := driveAuditData(t, rec, "drive.write"); data["rehomed"] != false {
			t.Errorf("drive.write rehomed = %v, want false", data["rehomed"])
		}
	})

	t.Run("the fields that name no storage stay editable", func(t *testing.T) {
		_, rec, _, put := allocated(t, nil, nil)
		w := put(`{"name":"Corp NAS","backend":"docker_volume","size_mib":20480,"writable":true,"reclaim":"delete"}`, "")
		if w.Code != http.StatusOK {
			t.Fatalf("size/mode/reclaim edit = %d, want 200: %s", w.Code, w.Body.String())
		}
		if data := driveAuditData(t, rec, "drive.write"); data["rehomed"] != false {
			t.Errorf("drive.write rehomed = %v, want false", data["rehomed"])
		}
	})

	// The OTHER two folds, so the guard is not a rename check wearing a general
	// name: host_root is the share's half of the object name, and the template
	// is the home's.
	t.Run("a host_root correction is refused", func(t *testing.T) {
		from, to := t.TempDir(), t.TempDir()
		_, _, _, put := allocated(t, []string{from, to}, func(d *types.UserDrive) {
			d.Backend, d.HomeTemplate, d.HostRoot = types.DriveBackendHostPath, types.HomeTemplateSub, from
		})
		w := put(`{"name":"Corp NAS","backend":"host_path","home_template":"sub","host_root":"`+to+`"}`, "")
		if w.Code != http.StatusConflict {
			t.Errorf("host_root change = %d, want 409: %s", w.Code, w.Body.String())
		}
	})

	t.Run("a home_template change is refused", func(t *testing.T) {
		root := t.TempDir()
		_, _, _, put := allocated(t, []string{root}, func(d *types.UserDrive) {
			d.Backend, d.HomeTemplate, d.HostRoot = types.DriveBackendHostPath, types.HomeTemplateSub, root
		})
		w := put(`{"name":"Corp NAS","backend":"host_path","home_template":"email_local","host_root":"`+root+`"}`, "")
		if w.Code != http.StatusConflict {
			t.Errorf("home_template change = %d, want 409: %s", w.Code, w.Body.String())
		}
	})

	// An UNALLOCATED drive is nobody's storage yet, so the guard must not stand
	// between an admin and a correction they are still free to make.
	t.Run("an unallocated drive renames freely", func(t *testing.T) {
		st := newDriveCRUDStore()
		srv, rec := driveAdminServer(st, nil)
		d := *driveFixture(nil)
		st.drives[d.ID] = d
		w := driveCall(t, srv.handleUpdateUserDrive, http.MethodPut, "/api/v1/drives/"+d.ID.String(),
			`{"name":"Corp NAS archive","backend":"docker_volume","size_mib":10240}`,
			map[string]string{"id": d.ID.String()})
		if w.Code != http.StatusOK {
			t.Fatalf("rename of an unallocated drive = %d, want 200: %s", w.Code, w.Body.String())
		}
		if data := driveAuditData(t, rec, "drive.write"); data["rehomed"] != false {
			t.Errorf("drive.write rehomed = %v, want false", data["rehomed"])
		}
	})

	// FAIL CLOSED. An unreadable allocation state is not permission to re-home
	// somebody quietly — the edit is unrecoverable in the product, so "I could
	// not check" must refuse.
	t.Run("an unreadable allocation state refuses rather than proceeds", func(t *testing.T) {
		st, _, id, put := allocated(t, nil, nil)
		st.listErr = errors.New("pg: connection refused")
		w := put(`{"name":"Corp NAS archive","backend":"docker_volume","size_mib":10240}`, "")
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("unreadable state = %d, want a refusal: %s", w.Code, w.Body.String())
		}
		if st.drives[id].Name != "Corp NAS" {
			t.Errorf("stored name = %q, want nothing written when the check could not run", st.drives[id].Name)
		}
	})
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

// TestUserDriveGrantRefusesOneDirectoryNameTwice pins the loophole in the rule
// above: a home_override on a GROUP row is refused because it would hand every
// member of that group the SAME directory, and TWO USER ROWS carrying one
// override is that same loss spelled with two rows instead of one.
//
// It bites hardest on a MANAGED drive. types.ValidateUserDrive now refuses a
// claim home_template there and a hash home folds the subject, so the override
// is the LAST way left to name an object Wardyn itself mints non-injectively —
// without this refusal Wardyn creates one volume and binds it into two people's
// sandboxes, read-write wherever the allocations are writable.
func TestUserDriveGrantRefusesOneDirectoryNameTwice(t *testing.T) {
	st := newDriveCRUDStore()
	srv, _ := driveAdminServer(st, nil)
	d := *driveFixture(nil) // docker_volume: a MANAGED drive, the object Wardyn mints
	st.drives[d.ID] = d
	other := *driveFixture(func(o *types.UserDrive) { o.ID, o.Name = uuid.New(), "Design scratch" })
	st.drives[other.ID] = other

	grant := func(subject, drive, home string) *httptest.ResponseRecorder {
		return driveCall(t, srv.handleUpsertUserDriveGrant, http.MethodPost, "/api/v1/drives/grants",
			`{"subject_type":"user","subject":"`+subject+`","drive_id":"`+drive+`","home_override":"`+home+`"}`, nil)
	}

	if w := grant("sub-bob", d.ID.String(), "bsmith"); w.Code != http.StatusCreated {
		t.Fatalf("first allocation = %d, want 201: %s", w.Code, w.Body.String())
	}
	// THE REFUSAL. A second person handed the same directory on the same drive.
	w := grant("sub-alice", d.ID.String(), "bsmith")
	if w.Code != http.StatusConflict {
		t.Fatalf("second subject on one directory = %d, want 409: %s", w.Code, w.Body.String())
	}
	// The admin has to be able to act on it: the message names the directory
	// they typed. It names no OTHER subject, for handleDeleteUserDrive's reason
	// — the allocations table already lists every override.
	if !strings.Contains(w.Body.String(), "bsmith") {
		t.Errorf("409 body = %s, want it to name the directory", w.Body.String())
	}

	// SCOPED, three ways, so the rule refuses collisions and nothing else.
	// (1) The SAME subject re-submitting its own row is a repoint, not a clash.
	if w := grant("sub-bob", d.ID.String(), "bsmith"); w.Code != http.StatusOK {
		t.Errorf("repointing the holder's own row = %d, want 200: %s", w.Code, w.Body.String())
	}
	// (2) The same name on a DIFFERENT drive is a different object.
	if w := grant("sub-alice", other.ID.String(), "bsmith"); w.Code != http.StatusCreated {
		t.Errorf("same name on another drive = %d, want 201: %s", w.Code, w.Body.String())
	}
	// (3) No override at all is the common case and never collides — every
	// grant without one derives its own home from its own subject.
	if w := grant("sub-carol", d.ID.String(), ""); w.Code != http.StatusCreated {
		t.Errorf("no override = %d, want 201: %s", w.Code, w.Body.String())
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

	// A REAL root, not a spelled one. The field promises "a host_path drive can
	// be authored here", which is why it is asked through the write boundary's
	// own ceiling check (userDriveHostRootsUsable) rather than through
	// len(roots): "/srv/homes" is not on this host, so a deployment configured
	// that way OFFERS host_path in the console and 422s every save. Asserting
	// the old len(roots) answer here would be asserting that offer-and-refuse.
	// Strictly more than it replaces: it still pins true-with-roots, and now
	// pins that the roots have to be usable — the dead-ceiling matrix is
	// TestHostRootsConfiguredMeansUsable.
	srv, _ = driveAdminServer(st, []string{t.TempDir()})
	w = driveCall(t, srv.handleGetUserDrives, http.MethodGet, "/api/v1/drives", "", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.HostRootsConfigured {
		t.Error("host_roots_configured = false with a usable root set")
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

	// THE ROW THAT WAS STORED BEFORE THE RESERVATION EXISTED. Everything above
	// is the write boundary, and it only ever ran on rows written since. A
	// stored policy is handed to dispatch verbatim — resolvePolicy does not
	// re-run validatePolicySpec, which runs for INLINE specs only — so a
	// workspace_repos row pointing into the drive reaches buildRepoRecords, and
	// this is where it has to stop.
	//
	// The direction matters more than the mechanism: a clone that landed inside
	// /home/agent/drive would write somebody else's repository into a member's
	// PERSISTENT storage — on a writable allocation it survives the run and
	// every run after it, and on a share it lands in the operator's NAS. The
	// record is dropped instead, so the run comes up with one fewer repo rather
	// than with a repo in the wrong place.
	t.Run("a stored repo row at the reserved target never reaches the clone", func(t *testing.T) {
		for _, target := range []string{runner.DriveTarget, runner.DriveTarget + "/x"} {
			if got := buildRepoRecords("", []types.WorkspaceRepo{{Repo: "octocat/hello", Target: target}}); got != "" {
				t.Errorf("a repo destined for %q reached WARDYN_REPOS: %q", target, got)
			}
		}
		// The control: an ordinary destination still clones, so the rule is the
		// reserved subtree rather than "repos with an explicit target".
		if got := buildRepoRecords("", []types.WorkspaceRepo{{Repo: "octocat/hello", Target: "/home/agent/work/hello"}}); !strings.Contains(got, "/home/agent/work/hello") {
			t.Errorf("an ordinary destination was dropped too: %q", got)
		}
	})
}

// TestUpdateAllocatedUserDriveRefusesASilentRehome is the second half of the
// "never silently un-allocate" rule the delete 409 already carries.
//
// UpsertUserDrive writes every column in place, so a PUT that changed
// `home_template` on a drive with grants answered 200 and re-homed every one of
// them: their next run mounts a different object, and the one holding their
// work is orphaned with nothing in the product naming it. The change is still
// available — an admin sometimes means it, and the FK denies them
// delete-and-recreate — but it has to be asked for.
func TestUpdateAllocatedUserDriveRefusesASilentRehome(t *testing.T) {
	// Two REAL roots inside the ceiling: the env check resolves a host_root on
	// this host and fails closed on one that is not there, so a share fixture
	// has to name directories that exist.
	base := t.TempDir()
	homes, other := filepath.Join(base, "homes"), filepath.Join(base, "other")
	for _, dir := range []string{homes, other} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	// The stored row every case starts from: a share, allocated to two subjects.
	stored := func(t *testing.T) (*driveCRUDStore, *Server, types.UserDrive) {
		t.Helper()
		st := newDriveCRUDStore()
		srv, _ := driveAdminServer(st, []string{homes, other})
		d := *driveFixture(func(d *types.UserDrive) {
			d.Name, d.Backend, d.HomeTemplate, d.HostRoot, d.SizeMiB =
				"nas", types.DriveBackendHostPath, types.HomeTemplateSub, homes, 10240
		})
		st.drives[d.ID] = d
		for _, subject := range []string{"alice", "bob"} {
			st.grants[uuid.New()] = types.UserDriveGrant{
				ID: uuid.New(), SubjectType: types.CapabilitySubjectUser, Subject: subject, DriveID: d.ID, Enabled: true,
			}
		}
		return st, srv, d
	}
	// share is the stored body with one field replaced, so each row below states
	// exactly the column it edits and nothing else.
	share := func(name, tmpl, root string) string {
		return fmt.Sprintf(`{"name":%q,"backend":"host_path","home_template":%q,"host_root":%q,"size_mib":10240}`, name, tmpl, root)
	}
	put := func(t *testing.T, srv *Server, id uuid.UUID, body, query string) *httptest.ResponseRecorder {
		t.Helper()
		return driveCall(t, srv.handleUpdateUserDrive, http.MethodPut,
			"/api/v1/drives/"+id.String()+query, body, map[string]string{"id": id.String()})
	}

	// Each of the FOUR identity columns, one at a time. They are not one rule
	// with four spellings: backend moves everyone onto a different substrate,
	// home_template re-derives every home, host_root binds the same names
	// against a different tree, and name re-homes every k8s claim.
	for _, tc := range []struct {
		field string
		body  string
		names string
	}{
		// backend is the ONE column that cannot move alone: since 2026-09-03 a
		// managed backend accepts only the `hash` template (the home segment is
		// concatenated into an object name `docker volume ls` prints, so a
		// subject-bearing template is refused there exactly as it is in a label).
		// So host_path+sub -> docker_volume necessarily carries the template with
		// it; sending backend alone is now a 400 for a different and correct
		// reason, and would never reach the re-home conflict this case exists for.
		{"backend", `{"name":"nas","backend":"docker_volume","home_template":"hash","size_mib":10240}`, `backend "host_path" → "docker_volume"`},
		{"home_template", share("nas", "email_local", homes), `home_template "sub" → "email_local"`},
		{"host_root", share("nas", "sub", other), fmt.Sprintf("host_root %q → %q", homes, other)},
		{"name", share("nas2", "sub", homes), `name "nas" → "nas2"`},
	} {
		t.Run(tc.field+" on an allocated drive is a 409", func(t *testing.T) {
			st, srv, d := stored(t)
			w := put(t, srv, d.ID, tc.body, "")
			if w.Code != http.StatusConflict {
				t.Fatalf("PUT = %d, want 409: %s", w.Code, w.Body.String())
			}
			body := refusalBody(t, w)
			// The refusal has to carry BOTH halves an admin decides on: what
			// changes, and how many allocations move.
			if !strings.Contains(body, tc.names) {
				t.Errorf("body = %q, want it to name the change %q", body, tc.names)
			}
			if !strings.Contains(body, "2 subjects") {
				t.Errorf("body = %q, want it to count the allocations it would re-home", body)
			}
			// THE REMEDY HAS TO BE ONE ITS READER CAN CARRY OUT. The console
			// PUTs /drives/{id} with no query and renders this body under
			// SAVE_REFUSED_TITLE, so "re-send with ?confirm=rehome" read as a
			// button an admin could not find. Byte-exact, because the whole
			// finding was the wording.
			const remedy = "Confirming is an API action, not a console one: re-send as PUT /drives/{id}?confirm=rehome."
			if !strings.Contains(body, remedy) {
				t.Errorf("body = %q, want it to end with %q", body, remedy)
			}
			// And NOTHING was written: a refused write must leave the row alone,
			// or the guard would only be telling an admin about a change it had
			// already made.
			if got := st.drives[d.ID]; got != d {
				t.Errorf("stored row = %+v, want it untouched by a refused PUT", got)
			}
		})

		t.Run(tc.field+" with ?confirm=rehome is allowed", func(t *testing.T) {
			st, srv, d := stored(t)
			if w := put(t, srv, d.ID, tc.body, "?confirm=rehome"); w.Code != http.StatusOK {
				t.Fatalf("confirmed PUT = %d, want 200: %s", w.Code, w.Body.String())
			}
			if st.drives[d.ID] == d {
				t.Error("the confirmed change did not land — the guard is a confirmation, not a wall")
			}
		})

		t.Run(tc.field+" on an UNALLOCATED drive is allowed", func(t *testing.T) {
			st, srv, d := stored(t)
			st.grants = map[uuid.UUID]types.UserDriveGrant{}
			if w := put(t, srv, d.ID, tc.body, ""); w.Code != http.StatusOK {
				t.Fatalf("PUT with no grants = %d, want 200: %s", w.Code, w.Body.String())
			}
		})
	}

	// The CONTROL, and it is what keeps the guard from being "PUT is refused on
	// an allocated drive": size, mode and reclaim re-home nobody — the object
	// name does not depend on them — so they are untouched by the gate.
	t.Run("a non-identity change on an allocated drive is unaffected", func(t *testing.T) {
		st, srv, d := stored(t)
		body := fmt.Sprintf(`{"name":"nas","backend":"host_path","home_template":"sub","host_root":%q,`+
			`"size_mib":20480,"writable":true,"reclaim":"delete"}`, homes)
		if w := put(t, srv, d.ID, body, ""); w.Code != http.StatusOK {
			t.Fatalf("PUT = %d, want 200 — size, mode and reclaim name no storage object: %s", w.Code, w.Body.String())
		}
		if got := st.drives[d.ID]; got.SizeMiB != 20480 || !got.Writable || got.Reclaim != types.DriveReclaimDelete {
			t.Errorf("stored row = %+v, want the non-identity edit landed", got)
		}
	})

	// A PUT that re-saves the row UNCHANGED is not a re-homing either, which is
	// the shape a console's "save" button produces when nothing was edited.
	t.Run("an unchanged re-save is unaffected", func(t *testing.T) {
		_, srv, d := stored(t)
		if w := put(t, srv, d.ID, share("nas", "sub", homes), ""); w.Code != http.StatusOK {
			t.Fatalf("unchanged re-save = %d, want 200: %s", w.Code, w.Body.String())
		}
	})

	// A PUT naming an id NO row holds still creates it (PUT means put): there is
	// nothing to re-home, so the guard must not turn the create-by-PUT path into
	// a 409.
	t.Run("a PUT that creates is unaffected", func(t *testing.T) {
		_, srv, _ := stored(t)
		id := uuid.New()
		if w := put(t, srv, id, driveCreateBody, ""); w.Code != http.StatusOK {
			t.Fatalf("PUT of an absent id = %d, want 200: %s", w.Code, w.Body.String())
		}
	})

	// FAIL CLOSED on an unreadable list: a read that could not answer cannot say
	// a drive is unallocated, and treating it as unallocated is how the gate
	// stops biting on exactly the deployment whose database is unhappy.
	t.Run("an unreadable grant count is a 500, never a quiet re-home", func(t *testing.T) {
		st, srv, d := stored(t)
		st.listErr = context.DeadlineExceeded
		// A docker_volume BODY, deliberately: driveHostRootNesting lists too and
		// runs FIRST, so a host_path body would answer the identical 500 from the
		// other gate and this sub-test would pass without the re-home guard's
		// fail-closed arm ever running. Only a non-share write reaches it alone.
		// `hash` because a managed backend accepts no other template since
		// 2026-09-03; the point here is only that the body is NON-SHARE.
		body := `{"name":"nas","backend":"docker_volume","home_template":"hash","size_mib":10240}`
		if w := put(t, srv, d.ID, body, ""); w.Code != http.StatusInternalServerError {
			t.Fatalf("PUT with an unreadable list = %d, want 500: %s", w.Code, w.Body.String())
		}
		if got := st.drives[d.ID]; got != d {
			t.Errorf("stored row = %+v, want it untouched", got)
		}
	})
}

// TestGetUserDrivesBoundsTheAllocationList pins the half of GET /drives whose
// cost tracked HEADCOUNT.
//
// The response composes two store reads with very different costs.
// ListUserDrives is an index-only scan and stays whole — there are as many
// drives as an admin registered. The GRANT list is one row per SUBJECT, two
// subjects per person, over an ORDER BY with no index: unbounded it sorted every
// allocation in the deployment on every load of one admin screen (measured at
// 50,000 allocations as a 5.5 MB on-disk external merge; bounded, a 301 kB
// top-N heapsort).
//
// THE DEFAULT IS THE PART THAT MUST NOT TAKE ROWS AWAY. It is maxListLimit, not
// defaultListLimit, so a deployment the console can actually render gets the
// answer it always got; past the cap the page says so on the wire twice — the
// X-Wardyn-Truncated header every paged list already sets, and grant_total,
// which is free because the per-drive counts in the same response sum to it.
func TestGetUserDrivesBoundsTheAllocationList(t *testing.T) {
	// BOTH BRANCHES, because the handler has two and only one of them is
	// production. driveCRUDStore is not a store.Pager, so it exercises the
	// in-Go fallback every test double takes; drivePagerStore is, so it
	// exercises the DB-level path PG takes — the one where the LIMIT is what
	// bounds the sort. A test that ran only the fallback would leave the
	// production branch free to regress while staying green, which is exactly
	// how the first draft of this test passed with the page deliberately
	// unbounded.
	for _, tc := range []struct {
		name  string
		build func() (*Server, *driveCRUDStore)
	}{
		{name: "fallback (not a Pager)", build: func() (*Server, *driveCRUDStore) {
			st := newDriveCRUDStore()
			srv, _ := driveAdminServer(st, nil)
			return srv, st
		}},
		{name: "pager (the production path)", build: func() (*Server, *driveCRUDStore) {
			st := newDriveCRUDStore()
			srv := New(Config{Store: &drivePagerStore{driveCRUDStore: st}, Audit: &recRecorder{},
				RunnerTarget: "docker", LocalMode: true})
			return srv, st
		}},
	} {
		t.Run(tc.name, func(t *testing.T) { runDriveListBoundsCase(t, tc.build) })
	}

	// AND THE BOUND REACHES THE STORE. Everything above is about the response,
	// which the Go-side trim makes identical whether the query was bounded or
	// not — so none of it can see the defect, which is that the DATABASE sorted
	// every allocation. This asserts the Page the handler handed the store.
	t.Run("the window is pushed into the query, not applied after it", func(t *testing.T) {
		st := newDriveCRUDStore()
		pager := &drivePagerStore{driveCRUDStore: st}
		srv := New(Config{Store: pager, Audit: &recRecorder{}, RunnerTarget: "docker", LocalMode: true})
		d := *driveFixture(nil)
		st.drives[d.ID] = d

		w := driveCall(t, srv.handleGetUserDrives, http.MethodGet, "/api/v1/drives?limit=3&offset=6", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("GET /drives = %d: %s", w.Code, w.Body.String())
		}
		// limit+1 is the overfetch that proves a next page exists without a
		// second query — the same probe servePage uses.
		if pager.asked.Limit != 4 || pager.asked.Offset != 6 {
			t.Errorf("store was asked for %+v, want Limit 4 (the caller's 3 plus the probe row) and Offset 6 — "+
				"an unbounded ask makes PostgreSQL sort every allocation in the deployment before the trim", pager.asked)
		}
		// A DEFAULT request is bounded too: absent ?limit is maxListLimit, never
		// "unbounded", which is the whole regression this closes.
		w = driveCall(t, srv.handleGetUserDrives, http.MethodGet, "/api/v1/drives", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("GET /drives = %d: %s", w.Code, w.Body.String())
		}
		if pager.asked.Limit != maxListLimit+1 {
			t.Errorf("default request asked for Limit %d, want %d — an absent ?limit must still bound the sort",
				pager.asked.Limit, maxListLimit+1)
		}
	})
}

// drivePagerStore is driveCRUDStore that IS a store.Pager: the embedded nil
// Pager supplies the six methods this path never calls, and the one it does call
// windows the double's own map exactly as the SQL LIMIT/OFFSET does.
type drivePagerStore struct {
	*driveCRUDStore
	store.Pager
	// asked records the Page the handler REQUESTED. It is the only place the
	// bound is observable: the handler trims in Go afterwards, so a response
	// built from an unbounded read is byte-identical to one built from a bounded
	// read — and asserting the response alone cannot tell a query that sorted
	// 50,000 rows from one that sorted 1,001. This is what the SQL was given.
	asked store.Page
}

func (s *drivePagerStore) ListUserDriveGrantsPage(ctx context.Context, p store.Page) ([]types.UserDriveGrant, error) {
	s.asked = p
	all, err := s.ListUserDriveGrants(ctx)
	if err != nil {
		return nil, err
	}
	off := min(p.Offset, len(all))
	rest := all[off:]
	if p.Limit > 0 && p.Limit < len(rest) {
		rest = rest[:p.Limit]
	}
	return rest, nil
}

func runDriveListBoundsCase(t *testing.T, build func() (*Server, *driveCRUDStore)) {
	t.Helper()
	srv, st := build()
	d := *driveFixture(nil)
	st.drives[d.ID] = d
	const seeded = 7
	for i := range seeded {
		id := uuid.New()
		st.grants[id] = types.UserDriveGrant{
			ID: id, SubjectType: types.CapabilitySubjectUser,
			Subject: fmt.Sprintf("sub-%02d", i), DriveID: d.ID, Enabled: true,
		}
	}

	// offset is passed rather than parsed back out of the query because the
	// header means "a FURTHER page exists", not "you did not get everything":
	// the last page carries fewer rows than the total and is not truncated, and
	// conflating the two is how a client loops forever or stops early.
	get := func(query string, offset int) userDrivesResponse {
		t.Helper()
		w := driveCall(t, srv.handleGetUserDrives, http.MethodGet, "/api/v1/drives"+query, "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("GET /drives%s = %d: %s", query, w.Code, w.Body.String())
		}
		var body userDrivesResponse
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		more := offset+len(body.Grants) < body.GrantTotal
		if got := w.Header().Get("X-Wardyn-Truncated"); (got == "true") != more {
			t.Errorf("X-Wardyn-Truncated = %q at offset %d with %d of %d grants — the header must mean "+
				"\"a further page exists\"", got, offset, len(body.Grants), body.GrantTotal)
		}
		return body
	}

	// THE TOTAL IS ALWAYS THE TRUE COUNT, page or no page: it is summed from the
	// per-drive counts, not from the rows this response happens to carry.
	whole := get("", 0)
	if whole.GrantTotal != seeded || len(whole.Grants) != seeded {
		t.Fatalf("unbounded: %d of %d grants, want all %d — the default must not take rows from a small deployment",
			len(whole.Grants), whole.GrantTotal, seeded)
	}

	page := get("?limit=3", 0)
	if len(page.Grants) != 3 {
		t.Errorf("?limit=3 returned %d grants, want 3", len(page.Grants))
	}
	if page.GrantTotal != seeded {
		t.Errorf("grant_total = %d on a page of 3, want the true %d — a client cannot tell it has all of them otherwise",
			page.GrantTotal, seeded)
	}

	// The window MOVES, and the last page is not marked truncated.
	second := get("?limit=3&offset=3", 3)
	if len(second.Grants) != 3 || second.Grants[0].ID == page.Grants[0].ID {
		t.Errorf("offset=3 returned %d grants starting at the same row — the offset is not applied", len(second.Grants))
	}
	last := get("?limit=3&offset=6", 6)
	if len(last.Grants) != 1 {
		t.Errorf("offset=6 returned %d grants, want the final 1", len(last.Grants))
	}

	// The drives half is UNTOUCHED by the window: it is the cheap read, and its
	// per-drive counts are what stay correct when the allocation table is a page.
	if len(whole.Drives) != 1 || whole.Drives[0].GrantCount != seeded {
		t.Errorf("drives = %+v, want the one drive still carrying its full count of %d", whole.Drives, seeded)
	}
}

// TestGrantRepointCannotSilentlyClearAPinnedHomeOverride is the grant-row half
// of the rule driveRehomeGuard states for the drive row: nothing may re-home an
// allocated member without saying so.
//
// home_override IS an identity field — DriveObjectName derives the object from
// the resolved home — so clearing one moves that person's storage. Before this
// guard, ANY write on their natural key did it silently: home_override decoded
// as a plain string, so "absent" and "empty" were one value, and the store's ON
// CONFLICT replaced the column wholesale. An admin repointing bsmith at another
// drive, or bumping a priority, wiped the directory name; the member's next run
// mounted wardyn-drive-<hash> and the object holding their work was left behind
// with nothing in Wardyn naming it.
//
// STATING THE FIELD IS THE CONFIRMATION, which is why there is no ?confirm=
// here and one on the drive PUT: a drive PUT cannot express "leave these four
// columns alone", while this write can express the one column exactly.
func TestGrantRepointCannotSilentlyClearAPinnedHomeOverride(t *testing.T) {
	st := newDriveCRUDStore()
	srv, _ := driveAdminServer(st, nil)
	d := *driveFixture(nil)
	st.drives[d.ID] = d
	other := *driveFixture(func(o *types.UserDrive) { o.ID, o.Name = uuid.New(), "Design scratch" })
	st.drives[other.ID] = other

	post := func(body string) *httptest.ResponseRecorder {
		return driveCall(t, srv.handleUpsertUserDriveGrant, http.MethodPost, "/api/v1/drives/grants", body, nil)
	}
	storedHome := func(t *testing.T) string {
		t.Helper()
		for _, g := range st.grants {
			if g.SubjectType == types.CapabilitySubjectUser && g.Subject == "sub-bob" {
				return g.HomeOverride
			}
		}
		t.Fatal("no grant stored for sub-bob")
		return ""
	}

	if w := post(`{"subject_type":"user","subject":"sub-bob","drive_id":"` + d.ID.String() +
		`","home_override":"bsmith"}`); w.Code != http.StatusCreated {
		t.Fatalf("allocate = %d, want 201: %s", w.Code, w.Body.String())
	}

	// THE REGRESSION: a repoint that never mentions the field.
	w := post(`{"subject_type":"user","subject":"sub-bob","drive_id":"` + other.ID.String() + `","priority":5}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("a repoint that never mentioned home_override = %d, want 409: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "home_override") {
		t.Errorf("409 body = %s, want it to name the field the admin has to state", w.Body.String())
	}
	if got := storedHome(t); got != "bsmith" {
		t.Fatalf("stored home_override = %q after the refused write, want it untouched at \"bsmith\"", got)
	}

	// STATING IT is the confirmation, in both directions. Keeping the name:
	if w := post(`{"subject_type":"user","subject":"sub-bob","drive_id":"` + other.ID.String() +
		`","priority":5,"home_override":"bsmith"}`); w.Code != http.StatusOK {
		t.Fatalf("a repoint that states the name = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := storedHome(t); got != "bsmith" {
		t.Errorf("stored home_override = %q, want the stated \"bsmith\"", got)
	}
	// And dropping it deliberately, which an admin must still be able to do.
	if w := post(`{"subject_type":"user","subject":"sub-bob","drive_id":"` + other.ID.String() +
		`","home_override":""}`); w.Code != http.StatusOK {
		t.Fatalf("an explicit empty home_override = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := storedHome(t); got != "" {
		t.Errorf("stored home_override = %q after an explicit \"\", want it cleared", got)
	}

	// THE SCOPE: a row that pins NOTHING is not protected by this guard, so the
	// ordinary allocation write is unaffected — the refusal is about losing a
	// name, not about the field being absent.
	if w := post(`{"subject_type":"user","subject":"sub-alice","drive_id":"` + d.ID.String() +
		`"}`); w.Code != http.StatusCreated {
		t.Fatalf("a first allocation with no home_override = %d, want 201: %s", w.Code, w.Body.String())
	}
	if w := post(`{"subject_type":"user","subject":"sub-alice","drive_id":"` + d.ID.String() +
		`","priority":3}`); w.Code != http.StatusOK {
		t.Fatalf("re-writing a row that pins nothing = %d, want 200: %s", w.Code, w.Body.String())
	}
}

// TestDriveHostRootNestingSeesThroughASymlink pins the half of the nesting gate
// that no other check can perform.
//
// The gate used to compare the two STORED STRINGS only, and delegated the
// symlink half to UserDriveHostRootCheck — which resolves ONE root against the
// deployment's env ceiling and has no second drive in scope, so it never
// compared two drives' roots at all. The literal nested path was refused and a
// SYMLINK to the same directory was accepted, with the deployment's own ceiling
// honoured throughout.
//
// What the accepted pair costs: drive A's members have writable homes inside
// A's tree, so any of them can replace a segment under it with a link and
// redirect every member of drive B — one drive's members authoring the other
// drive's storage, which is the outcome this gate exists to refuse.
func TestDriveHostRootNestingSeesThroughASymlink(t *testing.T) {
	base := t.TempDir()
	shares := filepath.Join(base, "shares")
	team := filepath.Join(shares, "alice", "team")
	if err := os.MkdirAll(team, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(base, "teamshare")
	if err := os.Symlink(team, link); err != nil {
		t.Skipf("symlinks unavailable on this host: %v", err)
	}

	st := newDriveCRUDStore()
	// BOTH paths are inside the ceiling, so nothing above this gate can refuse
	// either of them: the refusal has to come from the relationship of the two
	// rows, which is the whole claim.
	srv, _ := driveAdminServer(st, []string{base})
	create := func(name, root string) *httptest.ResponseRecorder {
		return driveCall(t, srv.handleCreateUserDrive, http.MethodPost, "/api/v1/drives",
			`{"name":"`+name+`","backend":"host_path","host_root":"`+root+`","home_template":"sub"}`, nil)
	}

	if w := create("Drive A", shares); w.Code != http.StatusCreated {
		t.Fatalf("create the outer drive = %d, want 201: %s", w.Code, w.Body.String())
	}
	w := create("Drive B", link)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a root that RESOLVES inside another drive's root = %d, want 422: %s", w.Code, w.Body.String())
	}
	// The admin cannot act on this without being told what they cannot see from
	// the form: which real directory their path landed in.
	if !strings.Contains(w.Body.String(), "resolves to") || !strings.Contains(w.Body.String(), "Drive A") {
		t.Errorf("422 body = %s, want it to name the other drive AND the resolved path", w.Body.String())
	}

	// THE CONTROL, in both directions. A sibling that resolves nowhere near the
	// other tree is still authorable, so the gate refuses nesting and not links.
	sibling := filepath.Join(base, "other")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	siblingLink := filepath.Join(base, "otherlink")
	if err := os.Symlink(sibling, siblingLink); err != nil {
		t.Skipf("symlinks unavailable on this host: %v", err)
	}
	if w := create("Drive C", siblingLink); w.Code != http.StatusCreated {
		t.Errorf("a link to a SIBLING tree = %d, want 201: %s", w.Code, w.Body.String())
	}
}

// TestHostRootsConfiguredMeansUsable pins what GET /drives' host_roots_configured
// actually promises: not "the operator set the variable" but "a host_path drive
// can be authored here".
//
// It was len(roots) > 0, and three configured values are DEAD ceilings — a root
// of "/" (which withinAnyRoot matches nothing under), a root beneath a denied
// bind prefix, and a root that does not resolve on this host. Every one of them
// reported the backend as available, so the console enabled `host_path` and the
// save 422'd — the exact offer-and-refuse this field exists to prevent, on the
// deployments least able to diagnose it.
func TestHostRootsConfiguredMeansUsable(t *testing.T) {
	live := t.TempDir()
	for _, tc := range []struct {
		name  string
		roots []string
		want  bool
	}{
		{"unset", nil, false},
		{"a live root", []string{live}, true},
		{"the dead \"/\" ceiling", []string{"/"}, false},
		{"a root under a denied bind prefix", []string{"/dev/shm"}, false},
		{"a root that is not on this host", []string{filepath.Join(live, "not-mounted")}, false},
		{"one dead root beside a live one", []string{"/dev/shm", live}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := driveAdminServer(newDriveCRUDStore(), tc.roots)
			w := driveCall(t, srv.handleGetUserDrives, http.MethodGet, "/api/v1/drives", "", nil)
			if w.Code != http.StatusOK {
				t.Fatalf("GET /drives = %d: %s", w.Code, w.Body.String())
			}
			var body struct {
				HostRootsConfigured bool `json:"host_roots_configured"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body.HostRootsConfigured != tc.want {
				t.Errorf("host_roots_configured = %v, want %v — the console enables the host_path option on this bit, "+
					"and every save under a dead ceiling is a 422", body.HostRootsConfigured, tc.want)
			}
		})
	}
}

// TestDriveTargetIsReservedOnEveryCompositionSeam is the half
// TestDriveTargetIsReservedFromAuthoring does not reach: the two seams that
// turn a STORED workspace's sources into a run policy.
//
// The counterfactual is what makes this worth writing. Reverting BOTH seams'
// runner.ValidateAuthoredTarget to runner.ValidateTarget — which is the SAME
// call minus the reserved-drive rule, i.e. exactly the regression a merge or a
// refactor would produce — left ./internal/api fully green. The authoring
// tests above cover validatePolicySpec, validateWorkspaceSource and
// buildRepoRecords; neither seedRequestWorkspace (the create path) nor
// wireWorkspaceSource (the record/verify path) had a case, and both compose a
// stored row that never passed the authoring gate.
//
// AND THE DISPATCH SEAM BELOW THEM, which cannot 422 because the run row
// already exists: a stored policy that names the reserved target reached the
// driver and failed the whole CreateSandbox, so every run under that policy
// died at STARTING with an internal reservation as its failure_hint.
func TestDriveTargetIsReservedOnEveryCompositionSeam(t *testing.T) {
	h := newHarness(t)
	for _, target := range []string{runner.DriveTarget, runner.DriveTarget + "/shared"} {
		for _, src := range []types.WorkspaceSource{
			{Type: types.WorkspaceSourceTypeEphemeral, Target: target},
			{Type: types.WorkspaceSourceTypeLocalDir, Path: "/srv/legacy", Target: target},
			{Type: types.WorkspaceSourceTypeRepo, Source: "octocat/hello", Target: target},
		} {
			t.Run("seedRequestWorkspace "+string(src.Type)+" "+target, func(t *testing.T) {
				wsID := uuid.New()
				ws := types.Workspace{ID: wsID, Sources: []types.WorkspaceSource{src}}
				srv := New(baseTestConfig(h, &workspaceStoreFake{ws: ws}))
				spec := &types.RunPolicySpec{}
				req := &createRunRequest{Agent: "claude-code", WorkspaceID: &wsID}

				dirs, _, code, err := srv.seedRequestWorkspace(context.Background(), spec, req)
				if err == nil {
					t.Fatalf("a stored %s source at %q composed unrefused: dirs=%v mounts=%+v repos=%+v",
						src.Type, target, dirs, spec.WorkspaceMounts, spec.WorkspaceRepos)
				}
				if code != http.StatusUnprocessableEntity {
					t.Errorf("code = %d, want 422 (the create path refuses, it does not drop): %v", code, err)
				}
				if !strings.Contains(err.Error(), "reserved") {
					t.Errorf("err = %v, want the reserved-target refusal", err)
				}
			})
		}
	}

	// THE POSITIVE CONTROL: a neighbouring target under the same allowed prefix
	// still composes, so the seam refuses the reserved subtree and not the
	// stored-source path in general.
	t.Run("a neighbouring target still composes", func(t *testing.T) {
		wsID := uuid.New()
		ws := types.Workspace{ID: wsID, Sources: []types.WorkspaceSource{
			{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/drives-report"},
		}}
		srv := New(baseTestConfig(h, &workspaceStoreFake{ws: ws}))
		spec := &types.RunPolicySpec{}
		req := &createRunRequest{Agent: "claude-code", WorkspaceID: &wsID}
		dirs, _, code, err := srv.seedRequestWorkspace(context.Background(), spec, req)
		if err != nil {
			t.Fatalf("neighbouring target refused: %d %v", code, err)
		}
		if len(dirs) != 1 || dirs[0] != "/home/agent/drives-report" {
			t.Errorf("ephemeralDirs = %v, want the neighbouring target", dirs)
		}
	})

	// DISPATCH. resolvePolicy hands a stored spec through verbatim, so this is
	// the last seam before the driver — and the driver's answer is to refuse
	// the whole sandbox.
	t.Run("buildRunMounts drops a stored mount at the reserved target", func(t *testing.T) {
		ro := false
		policy := types.RunPolicySpec{WorkspaceMounts: []types.WorkspaceMount{
			{Source: "/srv/legacy", Target: runner.DriveTarget, ReadOnly: &ro},
			{Source: "/srv/legacy2", Target: runner.DriveTarget + "/shared", ReadOnly: &ro},
			{Source: "/srv/work", Target: "/home/agent/work", ReadOnly: &ro},
		}}
		mounts := buildRunMounts(policy, llmTransport{}, memberMountPosture{})
		if len(mounts) != 1 || mounts[0].Target != "/home/agent/work" {
			t.Fatalf("mounts = %+v, want ONLY the ordinary bind — a reserved-target bind fails the whole "+
				"CreateSandbox, so every run under this stored policy dies at STARTING", mounts)
		}
	})
}

// TestUserDriveGrantAuditIsTheWholeRow is the allocation-side counterpart of
// TestUserDriveWriteAuditIsTheWholeRow, and it exists because the two grant
// rows were pinned by NEITHER key set NOR value.
//
// The counterfactual: redact `subject` to "REDACTED", zero `drive_id`,
// `priority` and `size_mib_override`, nil `writable_override`, force
// `home_override_set` false — and delete two of the keys outright — and every
// Go suite in the repo stays green. Only `enabled` (drive.grant.write) and
// `subject` (drive.grant.delete) were read at all, and neither by value against
// a known row. docs/AUDIT-ACTIONS.md declares the key set for both; nothing
// held the code to it.
//
// BY VALUE AND BY THE WHOLE MAP, the same treatment drive.write already gets: a
// key-set check alone would pass on a row whose every value came from the wrong
// allocation, and a value check alone would pass on a row that had quietly
// grown a field carrying the directory NAME — which is the one thing this
// payload deliberately reduces to a boolean.
func TestUserDriveGrantAuditIsTheWholeRow(t *testing.T) {
	st := newDriveCRUDStore()
	srv, rec := driveAdminServer(st, nil)
	d := *driveFixture(nil)
	st.drives[d.ID] = d

	w := driveCall(t, srv.handleUpsertUserDriveGrant, http.MethodPost, "/api/v1/drives/grants",
		`{"subject_type":"user","subject":"Sub-Bob","drive_id":"`+d.ID.String()+
			`","priority":7,"size_mib_override":2048,"writable_override":false,"home_override":"bsmith"}`, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("allocate = %d, want 201: %s", w.Code, w.Body.String())
	}
	var saved types.UserDriveGrant
	if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil {
		t.Fatalf("decode: %v", err)
	}

	ev := driveAuditEvent(t, rec, "drive.grant.write")
	if ev.Target != saved.ID.String() {
		t.Errorf("drive.grant.write target = %q, want the saved row's id %q", ev.Target, saved.ID)
	}
	var got map[string]any
	if err := json.Unmarshal(ev.Data, &got); err != nil {
		t.Fatalf("unmarshal drive.grant.write data: %v", err)
	}
	want := map[string]any{
		"subject_type": "user",
		// LOWERCASED, because that is what was stored and what the resolver
		// will match on: an audit row spelling the subject the way the request
		// typed it would not name the row that exists.
		"subject":           "sub-bob",
		"drive_id":          d.ID.String(),
		"priority":          float64(7),
		"size_mib_override": float64(2048),
		"writable_override": false,
		// A BOOLEAN AND NEVER THE VALUE: a directory name is a person's
		// username as often as not, and whether an admin pinned one is the
		// governance fact. If this key ever becomes a string, this assertion is
		// where that has to be argued for.
		"home_override_set": true,
		"enabled":           true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("drive.grant.write data = %#v,\nwant EXACTLY %#v", got, want)
	}
	if s := string(ev.Data); strings.Contains(s, "bsmith") {
		t.Errorf("drive.grant.write data = %s — it carries the directory NAME, which home_override_set exists to avoid", s)
	}

	// THE DELETE, which is the offboarding half: it deletes no data, so the row
	// has to say what the operator was told to do about the directory this
	// allocation was the last pointer to.
	rec.events = nil
	w = driveCall(t, srv.handleDeleteUserDriveGrant, http.MethodDelete, "/api/v1/drives/grants/"+saved.ID.String(),
		"", map[string]string{"id": saved.ID.String()})
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204: %s", w.Code, w.Body.String())
	}
	del := driveAuditEvent(t, rec, "drive.grant.delete")
	if del.Target != saved.ID.String() {
		t.Errorf("drive.grant.delete target = %q, want the removed row's id %q", del.Target, saved.ID)
	}
	var gotDel map[string]any
	if err := json.Unmarshal(del.Data, &gotDel); err != nil {
		t.Fatalf("unmarshal drive.grant.delete data: %v", err)
	}
	wantDel := map[string]any{
		"subject_type": "user",
		"subject":      "sub-bob",
		"drive_id":     d.ID.String(),
		// `drive` is the drive OBJECT's name and `reclaim` its declared intent
		// — the pair that makes this row worth writing, and the pair a store
		// lookup failure is allowed to degrade (best-effort, after the delete).
		"drive":   "Corp NAS",
		"reclaim": string(types.DriveReclaimRetain),
	}
	if !reflect.DeepEqual(gotDel, wantDel) {
		t.Errorf("drive.grant.delete data = %#v,\nwant EXACTLY %#v", gotDel, wantDel)
	}
}
