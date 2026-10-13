// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// memberDriveStore is driveCRUDStore with member drives on the same rows: the
// owner and the tombstone ride the row, as they do in user_drives. winners is
// what ResolveUserDrives answers per canonical sub (the caller's own view), and
// profile is the governance profile every caller resolves.
type memberDriveStore struct {
	*driveCRUDStore
	winners map[string][]store.UserDriveWinner
	profile *types.GovernanceProfile
	now     time.Time
}

func newMemberDriveStore() *memberDriveStore {
	st := &memberDriveStore{driveCRUDStore: newDriveCRUDStore(), winners: map[string][]store.UserDriveWinner{}, now: time.Now()}
	st.site = memberDriveSite(3, 0)
	return st
}

// memberDriveSite is a site config with the switch on at limit drives a person.
func memberDriveSite(limit, sizeMiB int) types.SiteConfig {
	return types.SiteConfig{WorkspaceProviders: &types.WorkspaceProviders{Storage: &types.StorageProviders{
		UserDrive: &types.UserDriveProvider{MemberCreate: &types.MemberDrivePolicy{Enabled: true, MaxPerPerson: limit, SizeMiB: sizeMiB}},
	}}}
}

func (s *memberDriveStore) owned(owner string) []types.UserDrive {
	var out []types.UserDrive
	for _, d := range s.drives {
		if d.OwnerSubject != "" && (owner == "" || d.OwnerSubject == owner) {
			out = append(out, d)
		}
	}
	return out
}

func (s *memberDriveStore) CreateMemberDrive(_ context.Context, d types.UserDrive, limit int) (types.UserDrive, error) {
	if d.OwnerSubject == "" {
		return types.UserDrive{}, errors.New("store: a member drive needs an owner") // as the store refuses it
	}
	mine := s.owned(d.OwnerSubject)
	if len(mine) >= limit {
		return types.UserDrive{}, store.ErrMemberDriveLimit
	}
	for _, o := range mine {
		if types.DriveSlug(o.Name) == types.DriveSlug(d.Name) {
			return types.UserDrive{}, store.ErrMemberDriveNameTaken
		}
	}
	d.ID = uuid.New()
	d.HomeTemplate, d.ObjectScheme, d.Writable, d.Reclaim = types.HomeTemplateHash, types.DriveObjectSchemeID, true, types.DriveReclaimDelete
	s.drives[d.ID] = d
	return d, nil
}

func (s *memberDriveStore) ListMemberDrivesOf(_ context.Context, owner string) ([]types.UserDrive, error) {
	return s.owned(owner), nil
}

func (s *memberDriveStore) GetMemberDriveOf(_ context.Context, owner string, id uuid.UUID) (types.UserDrive, error) {
	if d, ok := s.drives[id]; ok && d.OwnerSubject == owner && owner != "" {
		return d, nil
	}
	return types.UserDrive{}, store.ErrNotFound
}

func (s *memberDriveStore) BeginMemberDriveDelete(_ context.Context, owner string, id uuid.UUID) (types.UserDrive, time.Time, error) {
	d, ok := s.drives[id]
	if !ok || d.OwnerSubject == "" || owner != "" && d.OwnerSubject != owner {
		return types.UserDrive{}, time.Time{}, store.ErrNotFound
	}
	if d.DeleteStartedAt != nil && s.now.Sub(*d.DeleteStartedAt) < time.Minute {
		return types.UserDrive{}, time.Time{}, store.ErrMemberDriveBusy
	}
	at := s.now
	d.DeleteStartedAt = &at
	s.drives[id] = d
	return d, at, nil
}

func (s *memberDriveStore) BeginMemberDriveDeleteAdmin(ctx context.Context, id uuid.UUID) (types.UserDrive, time.Time, error) {
	return s.BeginMemberDriveDelete(ctx, "", id)
}

func (s *memberDriveStore) AbortMemberDriveDelete(_ context.Context, id uuid.UUID, started time.Time) error {
	d, ok := s.drives[id]
	if !ok || d.DeleteStartedAt == nil || !d.DeleteStartedAt.Equal(started) {
		return store.ErrNotFound
	}
	d.DeleteStartedAt = nil
	s.drives[id] = d
	return nil
}

func (s *memberDriveStore) FinishMemberDriveDelete(_ context.Context, id uuid.UUID, started time.Time) error {
	d, ok := s.drives[id]
	if !ok || d.DeleteStartedAt == nil || !d.DeleteStartedAt.Equal(started) {
		return store.ErrNotFound
	}
	delete(s.drives, id)
	return nil
}

func (s *memberDriveStore) ListMemberDrives(_ context.Context, p store.Page) ([]types.UserDrive, int, error) {
	all := s.owned("")
	return pageOf(all, p), len(all), nil
}

func pageOf(all []types.UserDrive, p store.Page) []types.UserDrive {
	if p.Offset >= len(all) {
		return nil
	}
	all = all[p.Offset:]
	if p.Limit > 0 && p.Limit < len(all) {
		all = all[:p.Limit]
	}
	return all
}

func (s *memberDriveStore) ResolveUserDrives(_ context.Context, users, _ []string, _ string) ([]store.UserDriveWinner, error) {
	if len(users) == 0 {
		return nil, nil
	}
	return s.winners[users[0]], nil
}

func (s *memberDriveStore) ResolveGovernanceProfile(context.Context, []string, []string, string) (*types.GovernanceProfile, types.CapabilitySubjectType, error) {
	if s.profile == nil {
		return nil, "", store.ErrNotFound
	}
	return s.profile, types.CapabilitySubjectUser, nil
}

// memberDriveRunner is a substrate that can reclaim and answers the lifecycle
// check with lifecycleErr.
type memberDriveRunner struct {
	reclaimRunner
	lifecycleErr error
}

func (r *memberDriveRunner) CanManageDrives(context.Context) error { return r.lifecycleErr }

func memberDriveServer(st *memberDriveStore, rn runner.Runner, target string) (*Server, *recRecorder) {
	rec := &recRecorder{}
	return New(Config{BaseCtx: context.Background(), Store: st, Audit: rec, Runner: rn, RunnerTarget: target}), rec
}

const (
	aliceSub = "sub-member-alice"
	bobSub   = "sub-member-bob"
)

// personCtx is a signed-in member's session.
func personCtx(sub string) context.Context {
	return withOIDCGroups(operatorCtx(sub, sub+"@corp.example", oidc.RoleUser), []string{"eng"})
}

// memberCall runs h as ctx's caller with chi params.
func memberCall(ctx context.Context, h http.HandlerFunc, method, path, body string, params map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	rctx := chi.NewRouteContext()
	for k, v := range params {
		rctx.URLParams.Add(k, v)
	}
	w := httptest.NewRecorder()
	h(w, r.WithContext(context.WithValue(ctx, chi.RouteCtxKey, rctx)))
	return w
}

func memberDriveJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func createMemberDrive(srv *Server, ctx context.Context, name string) *httptest.ResponseRecorder {
	return memberCall(ctx, srv.handleCreateMemberDrive, http.MethodPost, "/api/v1/me/drives", `{"name":`+memberDriveJSON(name)+`}`, nil)
}

func wantReason(t *testing.T, w *httptest.ResponseRecorder, status int, reason string) errorBody {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d: %s", w.Code, status, w.Body.String())
	}
	var body errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Reason != reason {
		t.Fatalf("reason = %q (%v), want %q: %s", body.Reason, err, reason, w.Body.String())
	}
	return body
}

func countAction(rec *recRecorder, action string) int {
	n := 0
	for _, ev := range rec.snapshot() {
		if ev.Action == action {
			n++
		}
	}
	return n
}

// TestMemberDriveCreateNeedsOrgSwitch: off, not enabled, or drives disabled
// deployment-wide is 422 member_drives_disabled; on is 201 with the person's
// drive at its server-derived path, and one drive.member.create row.
func TestMemberDriveCreateNeedsOrgSwitch(t *testing.T) {
	for name, site := range map[string]types.SiteConfig{
		"no switch":          {},
		"switch not enabled": {WorkspaceProviders: &types.WorkspaceProviders{Storage: &types.StorageProviders{UserDrive: &types.UserDriveProvider{MemberCreate: &types.MemberDrivePolicy{MaxPerPerson: 3}}}}},
		"drives disabled too": func() types.SiteConfig {
			s := memberDriveSite(3, 0)
			s.WorkspaceProviders.Storage.UserDrive.Disabled = true
			return s
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			st := newMemberDriveStore()
			st.site = site
			srv, rec := memberDriveServer(st, &memberDriveRunner{}, "docker")
			wantReason(t, createMemberDrive(srv, personCtx(aliceSub), "Notes"), http.StatusUnprocessableEntity, reasonMemberDrivesDisabled)
			if len(st.owned("")) != 0 || countAction(rec, "drive.member.create") != 0 {
				t.Fatalf("a refused create wrote a drive or an audit row")
			}
		})
	}
	st := newMemberDriveStore()
	srv, rec := memberDriveServer(st, &memberDriveRunner{}, "docker")
	w := createMemberDrive(srv, personCtx(aliceSub), "Investigation files")
	if w.Code != http.StatusCreated {
		t.Fatalf("on: status = %d, want 201: %s", w.Code, w.Body.String())
	}
	var got client.MeDrive
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Owned || got.Target != "/home/agent/drives/investigation-files" || got.Section != client.MeDriveSectionUser ||
		got.Source != client.MeDriveSourcePersonal || !got.Writable || got.Backend != types.DriveBackendDockerVolume {
		t.Errorf("created drive = %+v", got)
	}
	if mine := st.owned(aliceSub); len(mine) != 1 || mine[0].OwnerSubject != aliceSub {
		t.Fatalf("stored = %+v, want one drive owned by %s", mine, aliceSub)
	}
	data := driveAuditData(t, rec, "drive.member.create")
	if data["count"] != float64(1) || data["limit"] != float64(3) || data["drive"] != "Investigation files" {
		t.Errorf("drive.member.create data = %v", data)
	}
}

// TestMemberDriveCreateProfileDoors: deny_member_drives and deny_user_drive
// refuse with 403 and one authz.denied row; an operator is exempt; a denying
// profile with a blank name still refuses.
func TestMemberDriveCreateProfileDoors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limits types.GovernanceLimits
	}{
		{"deny_member_drives", types.GovernanceLimits{DenyMemberDrives: true}},
		{"deny_user_drive", types.GovernanceLimits{DenyUserDrive: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newMemberDriveStore()
			st.profile = &types.GovernanceProfile{ID: uuid.New(), Name: "contractors", Limits: tc.limits}
			srv, rec := memberDriveServer(st, &memberDriveRunner{}, "docker")
			w := createMemberDrive(srv, personCtx(aliceSub), "Notes")
			if body := wantReason(t, w, http.StatusForbidden, reasonMemberDrivesDeniedByProfile); body.Error != memberDrivesDeniedByProfileMsg("contractors") {
				t.Errorf("body = %s", w.Body.String())
			}
			if n := countAction(rec, authz.AuditAction); n != 1 {
				t.Errorf("authz.denied rows = %d, want 1", n)
			}
			if len(st.owned("")) != 0 {
				t.Errorf("the door let a drive through")
			}
			admin := withOIDCGroups(operatorCtx("sub-admin", "admin@corp.example", oidc.RoleAdmin), []string{"eng"})
			if w := createMemberDrive(srv, admin, "Notes"); w.Code != http.StatusCreated {
				t.Errorf("operator: status = %d, want 201 (exempt from the door): %s", w.Code, w.Body.String())
			}
		})
	}
	t.Run("blank-named profile", func(t *testing.T) {
		st := newMemberDriveStore()
		st.profile = &types.GovernanceProfile{ID: uuid.New(), Name: "", Limits: types.GovernanceLimits{DenyMemberDrives: true}}
		srv, _ := memberDriveServer(st, &memberDriveRunner{}, "docker")
		wantReason(t, createMemberDrive(srv, personCtx(aliceSub), "Notes"), http.StatusForbidden, reasonMemberDrivesDeniedByProfile)
		if len(st.owned("")) != 0 {
			t.Errorf("a blank-named denying profile let a drive through")
		}
	})
}

// TestMemberDriveRequiresPerson: only a person's session or own token creates
// or deletes; every other caller is 403 before anything is read.
func TestMemberDriveRequiresPerson(t *testing.T) {
	person := personCtx(aliceSub)
	for name, ctx := range map[string]context.Context{
		"admin token":      context.Background(),
		"local mode":       withLocalPrincipal(context.Background(), "local:alice"),
		"device":           context.WithValue(person, deviceCtxKey{}, types.Device{ID: uuid.New()}),
		"delegated":        audit.WithDelegation(person, types.DelegationVia{}),
		"service identity": withServiceIdentity(person, "ci-bot", aliceSub),
		"scim":             withSCIMCaller(person, "primary"),
	} {
		t.Run(name, func(t *testing.T) {
			st := newMemberDriveStore()
			srv, _ := memberDriveServer(st, &memberDriveRunner{}, "docker")
			wantReason(t, createMemberDrive(srv, ctx, "Notes"), http.StatusForbidden, reasonMemberDriveRequiresPerson)
			id := uuid.NewString()
			wantReason(t, memberCall(ctx, srv.handleDeleteMyDrive, http.MethodDelete, "/api/v1/me/drives/"+id, "", map[string]string{"id": id}),
				http.StatusForbidden, reasonMemberDriveRequiresPerson)
		})
	}
	st := newMemberDriveStore()
	srv, _ := memberDriveServer(st, &memberDriveRunner{}, "docker")
	if w := createMemberDrive(srv, person, "Notes"); w.Code != http.StatusCreated {
		t.Fatalf("person: status = %d, want 201: %s", w.Code, w.Body.String())
	}
}

// TestMemberDriveNameCollisionOwnViewOnly: a managed admin drive granted to
// Alice blocks her from naming a drive to the same slug; Bob, who can't see
// it, creates the name freely. Her own second "notes" is the store's refusal.
func TestMemberDriveNameCollisionOwnViewOnly(t *testing.T) {
	st := newMemberDriveStore()
	team := types.UserDrive{ID: uuid.New(), Name: "Team data", Backend: types.DriveBackendDockerVolume, HomeTemplate: types.HomeTemplateHash}
	st.drives[team.ID] = team
	st.winners[aliceSub] = []store.UserDriveWinner{{Drive: team, Grant: types.UserDriveGrant{ID: uuid.New(), SubjectType: types.CapabilitySubjectUser,
		Subject: aliceSub, DriveID: team.ID, Enabled: true}}}
	srv, _ := memberDriveServer(st, &memberDriveRunner{}, "docker")
	wantReason(t, createMemberDrive(srv, personCtx(aliceSub), "team-data"), http.StatusConflict, reasonMemberDriveNameTaken)
	if w := createMemberDrive(srv, personCtx(bobSub), "team-data"); w.Code != http.StatusCreated {
		t.Fatalf("bob: status = %d, want 201 (he cannot see Alice's drive): %s", w.Code, w.Body.String())
	}
	if w := createMemberDrive(srv, personCtx(aliceSub), "Notes"); w.Code != http.StatusCreated {
		t.Fatalf("alice notes: %d %s", w.Code, w.Body.String())
	}
	wantReason(t, createMemberDrive(srv, personCtx(aliceSub), "notes"), http.StatusConflict, reasonMemberDriveNameTaken)
}

// TestMemberDriveCreateRefusedWhenUndeletable: a runner that cannot reclaim, or
// whose lifecycle check says no, never gets a drive it could not delete.
func TestMemberDriveCreateRefusedWhenUndeletable(t *testing.T) {
	for name, rn := range map[string]runner.Runner{
		"no reclaimer":      &noReclaimRunner{},
		"lifecycle says no": &memberDriveRunner{lifecycleErr: runner.ErrDriveLifecycleUnavailable},
		"no checker":        &reclaimRunner{},
	} {
		t.Run(name, func(t *testing.T) {
			st := newMemberDriveStore()
			srv, _ := memberDriveServer(st, rn, "docker")
			wantReason(t, createMemberDrive(srv, personCtx(aliceSub), "Notes"), http.StatusUnprocessableEntity, reasonMemberDriveDeleteUnavailable)
			if len(st.owned("")) != 0 {
				t.Errorf("an undeletable drive was created")
			}
		})
	}
}

// TestMemberDriveBackendPerTarget: Docker makes a docker_volume with no size;
// Kubernetes a k8s_pvc at the switch's size clamped by the deployment ceiling,
// with its storage class; a Kubernetes switch with no size is refused rather
// than made into an unsized claim; any other target can't create.
func TestMemberDriveBackendPerTarget(t *testing.T) {
	create := func(t *testing.T, target string, site types.SiteConfig) (*memberDriveStore, *httptest.ResponseRecorder) {
		st := newMemberDriveStore()
		st.site = site
		srv, _ := memberDriveServer(st, &memberDriveRunner{}, target)
		return st, createMemberDrive(srv, personCtx(aliceSub), "Notes")
	}
	_, w := create(t, "docker", memberDriveSite(3, 0))
	var got client.MeDrive
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Backend != types.DriveBackendDockerVolume || got.SizeMiB != 0 {
		t.Fatalf("docker: %d %s", w.Code, w.Body.String())
	}
	k8s := memberDriveSite(3, 4096)
	k8s.WorkspaceProviders.Storage.UserDrive.MaxSizeMiB = 1024
	k8s.WorkspaceProviders.Storage.UserDrive.MemberCreate.StorageClass = "fast"
	st, w := create(t, "k8s", k8s)
	if w.Code != http.StatusCreated {
		t.Fatalf("k8s: %d %s", w.Code, w.Body.String())
	}
	if d := st.owned(aliceSub)[0]; d.Backend != types.DriveBackendK8sPVC || d.SizeMiB != 1024 || d.StorageClass != "fast" {
		t.Errorf("k8s drive = %+v, want k8s_pvc, 1024 MiB (clamped), class fast", d)
	}
	st, w = create(t, "k8s", memberDriveSite(3, 0))
	wantReason(t, w, http.StatusUnprocessableEntity, reasonMemberDriveUnsupported)
	if !strings.Contains(w.Body.String(), memberDriveUnsizedMsg()) || len(st.owned("")) != 0 {
		t.Errorf("unsized k8s: %s", w.Body.String())
	}
	_, w = create(t, "local", memberDriveSite(3, 0))
	wantReason(t, w, http.StatusUnprocessableEntity, reasonMemberDriveUnsupported)
}

// TestMemberDriveCountCapAndNameRule: the count cap's refusal names the count,
// and a name with no letter or digit is 400.
func TestMemberDriveCountCapAndNameRule(t *testing.T) {
	st := newMemberDriveStore()
	st.site = memberDriveSite(1, 0)
	srv, _ := memberDriveServer(st, &memberDriveRunner{}, "docker")
	if w := createMemberDrive(srv, personCtx(aliceSub), "One"); w.Code != http.StatusCreated {
		t.Fatalf("first: %d %s", w.Code, w.Body.String())
	}
	w := createMemberDrive(srv, personCtx(aliceSub), "Two")
	wantReason(t, w, http.StatusUnprocessableEntity, reasonMemberDriveLimitReached)
	if !strings.Contains(w.Body.String(), memberDriveLimitReachedMsg(1, 1)) {
		t.Errorf("body = %s", w.Body.String())
	}
	wantReason(t, createMemberDrive(srv, personCtx(bobSub), "!!!"), http.StatusBadRequest, reasonMemberDriveNameInvalid)
}

// TestMemberDriveRateLimited: creates and deletes share one bucket per person,
// burst 3; a fourth change in the same instant is 429, and another person's
// bucket is untouched.
func TestMemberDriveRateLimited(t *testing.T) {
	st := newMemberDriveStore()
	st.site = memberDriveSite(20, 0)
	srv, _ := memberDriveServer(st, &memberDriveRunner{}, "docker")
	at := time.Now()
	srv.cfg.Now = func() time.Time { return at }
	alice := personCtx(aliceSub)
	for _, n := range []string{"a1", "a2"} {
		if w := createMemberDrive(srv, alice, n); w.Code != http.StatusCreated {
			t.Fatalf("%s: %d %s", n, w.Code, w.Body.String())
		}
	}
	id := uuid.NewString()
	if w := memberCall(alice, srv.handleDeleteMyDrive, http.MethodDelete, "/x", "", map[string]string{"id": id}); w.Code != http.StatusNotFound {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	wantReason(t, createMemberDrive(srv, alice, "a3"), http.StatusTooManyRequests, reasonMemberDriveRateLimited)
	if w := createMemberDrive(srv, personCtx(bobSub), "b1"); w.Code != http.StatusCreated {
		t.Fatalf("bob: %d %s", w.Code, w.Body.String())
	}
}
