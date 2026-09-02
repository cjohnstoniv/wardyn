// Copyright 2025 The Wardyn Authors
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

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ─── the store double ─────────────────────────────────────────────────────────

// driveStore answers the two user-drive resolver reads over an otherwise
// empty deployment. It EMBEDS rather than sitting beside store.Store for
// noGovernanceStore's own reason: two embeds at the same depth would make the
// selector ambiguous and the double would silently stop implementing
// store.Store.
//
// The zero value is a deployment that has allocated NO drives — ErrNotFound and
// no group-tier rows — which is "byte for byte before this feature", and is
// what every case that is not about a match starts from.
type driveStore struct {
	// noGovernanceStore rather than a bare store.Store: /me resolves the
	// caller's CEILING beside their drive (the denied_by_profile field), so a
	// double that answered only the drive reads would panic on the governance
	// ones. Its own two drive methods below shadow noGovernanceStore's at depth
	// 0, so there is no ambiguity and no second answer.
	noGovernanceStore
	drive        *types.UserDrive
	grant        *types.UserDriveGrant
	tier         types.CapabilitySubjectType
	hasGroupTier bool
	// err fails BOTH reads, for the never-fail-quiet arm.
	err error
	// userTierOnly models the enforcement shape the stale branch produces: the
	// resolver is called a second time with NO groups, and a group-tier answer
	// must not come back from it.
	userTierOnly bool
	// nilAnswer is the OTHER shape "no grant matched" can arrive in: nil rows
	// with a nil error, rather than ErrNotFound.
	nilAnswer bool
}

func (s *driveStore) ResolveUserDrive(_ context.Context, _, groups []string) (
	*types.UserDrive, *types.UserDriveGrant, types.CapabilitySubjectType, error) {
	if s.err != nil {
		return nil, nil, "", s.err
	}
	if s.nilAnswer {
		return nil, nil, "", nil
	}
	if s.drive == nil || (s.userTierOnly && len(groups) == 0 && s.tier != types.CapabilitySubjectUser) {
		return nil, nil, "", store.ErrNotFound
	}
	return s.drive, s.grant, s.tier, nil
}

func (s *driveStore) HasGroupTierDriveGrants(context.Context) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	return s.hasGroupTier, nil
}

// ─── fixtures ─────────────────────────────────────────────────────────────────

// driveFixture is a MANAGED drive: read-only by default, hashed home, 10 GiB
// allocated. Read-only is the product default and the fixture keeps it, so a
// case that ends up writable had to have been made writable by a fold under
// test rather than by the fixture.
func driveFixture(mut func(*types.UserDrive)) *types.UserDrive {
	d := &types.UserDrive{
		ID: uuid.New(), Name: "Corp NAS", Backend: types.DriveBackendDockerVolume,
		HomeTemplate: types.HomeTemplateHash, SizeMiB: 10240, Reclaim: types.DriveReclaimRetain,
	}
	if mut != nil {
		mut(d)
	}
	return d
}

func grantFixture(driveID uuid.UUID, mut func(*types.UserDriveGrant)) *types.UserDriveGrant {
	g := &types.UserDriveGrant{
		ID: uuid.New(), SubjectType: types.CapabilitySubjectUser, Subject: "sub-drive-bob",
		DriveID: driveID, Enabled: true,
	}
	if mut != nil {
		mut(g)
	}
	return g
}

// driveServer builds a Server over st (nil for the no-store arm).
func driveServer(st *driveStore) *Server {
	cfg := Config{}
	if st != nil {
		cfg.Store = st
	}
	return &Server{cfg: cfg}
}

// driveMemberCtx is what humanOrAdminAuth publishes for a signed-in MEMBER:
// identity, group snapshot, and the snapshot's completeness bit.
func driveMemberCtx(groups []string, truncated bool) context.Context {
	return withOIDCGroupsTruncated(
		withOIDCGroups(operatorCtx("sub-drive-bob", "bob@corp.example", oidc.RoleMember), groups),
		truncated)
}

// ─── the resolver ─────────────────────────────────────────────────────────────

// TestResolveUserDrive walks the resolver's order. Each case is a decision that
// could have gone the other way, and the counterfactual is named.
func TestResolveUserDrive(t *testing.T) {
	t.Run("no store means no drive, with no read", func(t *testing.T) {
		// A nil store would PANIC on any drive read (store.Store is a nil
		// interface here), so this arm proves the short-circuit really happens
		// before the read — and it is what keeps the nil-store doubles in this
		// package alive once D2 calls this from run create.
		got, err := driveServer(nil).resolveUserDrive(driveMemberCtx([]string{"eng"}, false))
		if err != nil || got != nil {
			t.Errorf("nil store = %+v, %v; want no drive and no error", got, err)
		}
	})

	t.Run("a caller with no subjects gets no drive", func(t *testing.T) {
		// An admin token and local mode carry no per-human identity. An
		// `all`-tier grant WOULD match them, and serving it would hand every
		// identity-less caller ONE shared directory named after nobody.
		// Counterfactual: drop the len(users) check and this returns the
		// everyone drive with a home hashed from the empty string.
		//
		// It also has to run BEFORE the stale branch: context.Background() has
		// a nil group snapshot, so a subject check placed after it would answer
		// 403 to every admin-token call on a deployment with group grants.
		d := driveFixture(nil)
		st := &driveStore{drive: d, grant: grantFixture(d.ID, func(g *types.UserDriveGrant) {
			g.SubjectType, g.Subject = types.CapabilitySubjectAll, ""
		}), tier: types.CapabilitySubjectAll, hasGroupTier: true}
		got, err := driveServer(st).resolveUserDrive(context.Background())
		if err != nil || got != nil {
			t.Errorf("admin token = %+v, %v; want no drive and no error", got, err)
		}
	})

	t.Run("a store failure is an ERROR, never a quiet no-drive", func(t *testing.T) {
		// THE fail-quiet test. Carrying on with "no drive" would drop a
		// member's storage out of a run on a database hiccup, and the run would
		// then write its work into a container layer nobody keeps.
		boom := errors.New("pg: connection refused")
		got, err := driveServer(&driveStore{err: boom}).resolveUserDrive(driveMemberCtx([]string{"eng"}, false))
		if err == nil {
			t.Fatalf("a store failure resolved to %+v instead of erroring", got)
		}
		if !errors.Is(err, boom) {
			t.Errorf("error = %v, want the store failure wrapped", err)
		}
		w := httptest.NewRecorder()
		writeDriveError(w, err)
		if w.Code != http.StatusInternalServerError {
			t.Errorf("writeDriveError(store failure) = %d, want 500", w.Code)
		}
	})

	t.Run("no grant matched means no drive", func(t *testing.T) {
		// Absent row, absent behaviour: a deployment that has allocated nothing
		// mounts nothing, exactly as before the feature existed. Asserted for
		// BOTH shapes a store can say it in — ErrNotFound, and a bare
		// (nil, nil, "", nil) — because the second must mean the same thing
		// rather than dereferencing a nil row.
		for _, st := range []*driveStore{{}, {nilAnswer: true}} {
			got, err := driveServer(st).resolveUserDrive(driveMemberCtx([]string{"eng"}, false))
			if err != nil || got != nil {
				t.Errorf("no grant = %+v, %v; want no drive and no error", got, err)
			}
		}
	})

	t.Run("a matched grant resolves to a mountable drive", func(t *testing.T) {
		d := driveFixture(nil)
		st := &driveStore{drive: d, grant: grantFixture(d.ID, nil), tier: types.CapabilitySubjectUser}
		got, err := driveServer(st).resolveUserDrive(driveMemberCtx([]string{"eng"}, false))
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if got == nil {
			t.Fatal("a matched grant resolved to no drive")
		}
		if !strings.HasPrefix(got.HomeName, "d-") || got.ObjectName != "wardyn-drive-"+got.HomeName {
			t.Errorf("home/object = %q/%q, want the hashed home and its volume name", got.HomeName, got.ObjectName)
		}
		if got.SizeMiB != 10240 || got.Writable {
			t.Errorf("size/writable = %d/%v, want the drive's 10240 and read-only", got.SizeMiB, got.Writable)
		}
		// A Docker named volume has NO byte cap; saying anything else here is
		// what the enforcement vocabulary exists to prevent.
		if got.Enforcement != types.StorageEnforcementNone {
			t.Errorf("enforcement = %q, want %q", got.Enforcement, types.StorageEnforcementNone)
		}
		if got.Tier != types.CapabilitySubjectUser || got.Grant.ID == uuid.Nil {
			t.Errorf("resolved = tier %q / grant %s, want the winning row carried through", got.Tier, got.Grant.ID)
		}
	})

	// ─── the stale/truncated 403 and its scoping ──────────────────────────────

	t.Run("stale snapshot + a group-tier grant + no user match = 403", func(t *testing.T) {
		// nil groups is a pre-0.6 cookie: the group identity is unanswerable, a
		// group grant exists that might apply, and nothing user-tier settles
		// it. Counterfactual: fall through and an `all`-tier WRITABLE drive
		// wins over the group's read-only one by alphabetical luck.
		st := &driveStore{hasGroupTier: true}
		_, err := driveServer(st).resolveUserDrive(driveMemberCtx(nil, false))
		if !errors.Is(err, errGroupsSnapshotStale) {
			t.Fatalf("err = %v, want errGroupsSnapshotStale", err)
		}
		w := httptest.NewRecorder()
		writeDriveError(w, err)
		if w.Code != http.StatusForbidden {
			t.Errorf("writeDriveError(stale) = %d, want 403", w.Code)
		}
		if body := w.Body.String(); !containsAll(body, "groups_snapshot_stale", "sign in again") {
			t.Errorf("403 body does not name the condition and the remedy: %s", body)
		}
	})

	t.Run("TRUNCATED snapshot is treated exactly like a stale one", func(t *testing.T) {
		// The snapshot is present and non-nil — it just is not all of them, and
		// the group carrying this member's drive is as likely to have been
		// dropped as any other.
		st := &driveStore{hasGroupTier: true}
		_, err := driveServer(st).resolveUserDrive(driveMemberCtx([]string{"a-team"}, true))
		if !errors.Is(err, errGroupsSnapshotStale) {
			t.Fatalf("a truncated snapshot resolved (err = %v); truncated must be as unanswerable as nil", err)
		}
	})

	t.Run("a user-tier match is served despite an unusable snapshot", func(t *testing.T) {
		// user > group > all, so an explicitly named principal's drive is fully
		// determined whatever their groups are. Refusing would lock out exactly
		// the people an admin took the trouble to name.
		d := driveFixture(nil)
		st := &driveStore{drive: d, grant: grantFixture(d.ID, nil),
			tier: types.CapabilitySubjectUser, hasGroupTier: true, userTierOnly: true}
		got, err := driveServer(st).resolveUserDrive(driveMemberCtx(nil, false))
		if err != nil || got == nil {
			t.Fatalf("user-tier match with a nil snapshot = %+v, %v; want the drive served", got, err)
		}
	})

	t.Run("no group-tier grant anywhere means an unusable snapshot is harmless", func(t *testing.T) {
		// Nothing an unknown group could have matched, so nothing a nil
		// snapshot could be hiding. Refusing here would break "no grant ⇒ no
		// drive" for every pre-upgrade session on a deployment that allocates
		// by user only.
		got, err := driveServer(&driveStore{}).resolveUserDrive(driveMemberCtx(nil, false))
		if err != nil || got != nil {
			t.Errorf("= %+v, %v; want no drive and no refusal", got, err)
		}
	})

	// ─── the folds ────────────────────────────────────────────────────────────

	t.Run("the grant's overrides win over the drive's defaults", func(t *testing.T) {
		yes := true
		d := driveFixture(nil) // read-only, 10240 MiB
		st := &driveStore{drive: d, tier: types.CapabilitySubjectUser,
			grant: grantFixture(d.ID, func(g *types.UserDriveGrant) {
				g.SizeMiBOverride, g.WritableOverride, g.HomeOverride = 512, &yes, "bsmith"
			})}
		got, err := driveServer(st).resolveUserDrive(driveMemberCtx([]string{"eng"}, false))
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if got.SizeMiB != 512 {
			t.Errorf("size = %d, want the grant's 512", got.SizeMiB)
		}
		// COALESCE, not intersection: both values are admin-authored and the
		// grant is the more specific statement. The narrowing rule lives one
		// layer up, on the run request.
		if !got.Writable {
			t.Error("writable = false; an explicit writable_override on a read-only drive must win")
		}
		if got.HomeName != "bsmith" || got.ObjectName != "wardyn-drive-bsmith" {
			t.Errorf("home/object = %q/%q, want the override's", got.HomeName, got.ObjectName)
		}
	})

	t.Run("an unset writable override leaves the drive's posture alone", func(t *testing.T) {
		// nil is NOT false: a grant that says nothing about writability must
		// inherit, or a writable allocation would silently go read-only.
		d := driveFixture(func(d *types.UserDrive) { d.Writable = true })
		st := &driveStore{drive: d, grant: grantFixture(d.ID, nil), tier: types.CapabilitySubjectUser}
		got, err := driveServer(st).resolveUserDrive(driveMemberCtx([]string{"eng"}, false))
		if err != nil || !got.Writable {
			t.Errorf("writable = %v (err %v), want the drive's true", got, err)
		}
	})

	t.Run("an explicit false override narrows a writable drive", func(t *testing.T) {
		no := false
		d := driveFixture(func(d *types.UserDrive) { d.Writable = true })
		st := &driveStore{drive: d, tier: types.CapabilitySubjectUser,
			grant: grantFixture(d.ID, func(g *types.UserDriveGrant) { g.WritableOverride = &no })}
		got, err := driveServer(st).resolveUserDrive(driveMemberCtx([]string{"eng"}, false))
		if err != nil || got.Writable {
			t.Errorf("writable = %+v (err %v), want the override's false", got, err)
		}
	})

	t.Run("a home override on a group grant is ignored", func(t *testing.T) {
		// The write boundary refuses to store one; this is the second half of
		// that rule, because a row written by an older binary — or by hand —
		// would otherwise hand an entire group ONE directory.
		d := driveFixture(nil)
		st := &driveStore{drive: d, tier: types.CapabilitySubjectGroup,
			grant: grantFixture(d.ID, func(g *types.UserDriveGrant) {
				g.SubjectType, g.Subject, g.HomeOverride = types.CapabilitySubjectGroup, "eng", "shared"
			})}
		got, err := driveServer(st).resolveUserDrive(driveMemberCtx([]string{"eng"}, false))
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if got.HomeName == "shared" {
			t.Error("a group-tier home override was honoured; every member of that group would share one directory")
		}
		if !strings.HasPrefix(got.HomeName, "d-") {
			t.Errorf("home = %q, want the drive's own template to have decided it", got.HomeName)
		}
	})

	t.Run("a home that cannot be derived is unmountable, not a guess", func(t *testing.T) {
		// A share templated on a claim this caller does not carry. Refusing is
		// the point: a fabricated segment lands one member in another's
		// directory, or outside the drive entirely.
		d := driveFixture(func(d *types.UserDrive) {
			d.Backend, d.HomeTemplate, d.HostRoot = types.DriveBackendHostPath, types.HomeTemplateEmailLocal, "/srv/homes"
		})
		st := &driveStore{drive: d, grant: grantFixture(d.ID, nil), tier: types.CapabilitySubjectUser}
		// A caller carrying a sub and NO email claim: email_local has nothing to
		// truncate, so the home cannot be derived at all.
		noEmail := withOIDCGroups(operatorCtx("sub-drive-bob", "", oidc.RoleMember), []string{"eng"})
		_, err := driveServer(st).resolveUserDrive(noEmail)
		if !errors.Is(err, errDriveUnmountable) {
			t.Fatalf("err = %v, want errDriveUnmountable", err)
		}
		w := httptest.NewRecorder()
		writeDriveError(w, err)
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("writeDriveError(unmountable) = %d, want 422 — the caller is authorized, there is simply nothing to mount", w.Code)
		}
	})
}

// TestDriveHomeSubject pins the ONE selection types.DriveHomeName cannot make:
// which of the caller's claims the template names. It is POSITIONAL by
// contract — capabilitySubjects builds [sub, email] and both entrances preserve
// that order — and deliberately not a shape guess, for the reason the
// governance preview refuses one.
func TestDriveHomeSubject(t *testing.T) {
	users := []string{"sub-abc", "alice@corp.example"}
	for _, tc := range []struct {
		tmpl types.HomeTemplate
		want string
	}{
		{types.HomeTemplateHash, "sub-abc"},
		{types.HomeTemplateSub, "sub-abc"},
		{types.HomeTemplateEmailLocal, "alice@corp.example"},
	} {
		if got := driveHomeSubject(tc.tmpl, users); got != tc.want {
			t.Errorf("driveHomeSubject(%q) = %q, want %q", tc.tmpl, got, tc.want)
		}
	}
	// A caller who presented only one claim gets it whatever the template says;
	// whether it FITS is DriveHomeName's refusal to make, not a guess here.
	if got := driveHomeSubject(types.HomeTemplateEmailLocal, []string{"sub-only"}); got != "sub-only" {
		t.Errorf("single-claim caller = %q, want the one claim they have", got)
	}
	if got := driveHomeSubject(types.HomeTemplateHash, nil); got != "" {
		t.Errorf("no claims = %q, want empty", got)
	}
}

// ─── POST /drives/preview ─────────────────────────────────────────────────────

// previewDriveHTTP posts one preview straight at the handler. The route is D2's
// (SUPER-only, beside the /drives CRUD), so this drives the handler directly —
// which is also the sharper test: it pins the handler's own contract rather
// than the router's.
func previewDriveHTTP(t *testing.T, srv *Server, users, groups []string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(governancePreviewRequest{UserSubjects: users, Groups: groups})
	if err != nil {
		t.Fatalf("marshal preview request: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/drives/preview", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	srv.handlePreviewUserDrive(w, r)
	return w
}

// TestPreviewUserDrive is the endpoint's contract: it answers through THE
// resolver, and its answer is the object name an admin can copy into an
// offboarding command rather than compute from a hash by hand.
func TestPreviewUserDrive(t *testing.T) {
	t.Run("a match is answered with the object name", func(t *testing.T) {
		d := driveFixture(func(d *types.UserDrive) {
			d.Name, d.Backend, d.HomeTemplate = "Corp NAS", types.DriveBackendK8sPVC, types.HomeTemplateEmailLocal
		})
		st := &driveStore{drive: d, grant: grantFixture(d.ID, nil), tier: types.CapabilitySubjectUser}
		w := previewDriveHTTP(t, driveServer(st), []string{"sub-abc", "Alice@Corp.Example"}, []string{"Eng"})
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		var got userDrivePreviewResponse
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v (body=%s)", err, w.Body.String())
		}
		// The claims are folded exactly as the enforcement path folds them —
		// grants are stored lowercased, and a preview that passed `Alice@…`
		// through raw would answer a home this member's run would never get.
		if got.HomeName != "alice" {
			t.Errorf("home_name = %q, want the folded email-local %q", got.HomeName, "alice")
		}
		if got.ObjectName != "wardyn-drive-corp-nas-alice" {
			t.Errorf("object_name = %q, want the PVC name an admin can delete by", got.ObjectName)
		}
		if got.DriveName != "Corp NAS" || got.MatchedTier != types.CapabilitySubjectUser {
			t.Errorf("drive/tier = %q/%q, want the winning row's", got.DriveName, got.MatchedTier)
		}
		// A PVC's size is a REQUEST, and only a block storage class binds it.
		if got.Enforcement != types.StorageEnforcementRequest {
			t.Errorf("enforcement = %q, want %q", got.Enforcement, types.StorageEnforcementRequest)
		}
	})

	t.Run("no match is the empty object, not a 404", func(t *testing.T) {
		w := previewDriveHTTP(t, driveServer(&driveStore{}), []string{"nobody"}, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200", w.Code)
		}
		if got := strings.TrimSpace(w.Body.String()); got != "{}" {
			t.Errorf("body = %s, want {} — an absent grant is a RESULT, not a failure", got)
		}
	})

	t.Run("a drive whose home cannot be derived answers 422", func(t *testing.T) {
		// This IS the answer the admin came for: their template does not fit
		// this person's claims, said before anyone tries to start a run.
		d := driveFixture(func(d *types.UserDrive) {
			d.Backend, d.HomeTemplate, d.HostRoot = types.DriveBackendHostPath, types.HomeTemplateEmailLocal, "/srv/homes"
		})
		st := &driveStore{drive: d, grant: grantFixture(d.ID, nil), tier: types.CapabilitySubjectUser}
		w := previewDriveHTTP(t, driveServer(st), []string{"sub-with-no-at-sign"}, nil)
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("code = %d, want 422; body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("a store failure is a 500, never an empty answer", func(t *testing.T) {
		// An empty answer here would tell an admin their allocation does not
		// bind someone it does.
		st := &driveStore{err: errors.New("pg: connection refused")}
		w := previewDriveHTTP(t, driveServer(st), []string{"sub-abc"}, nil)
		if w.Code != http.StatusInternalServerError {
			t.Errorf("code = %d, want 500; body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("a nil store answers the empty object", func(t *testing.T) {
		// The preview is the other door into the resolver, and the nil-store
		// doubles in this package must not panic on it.
		w := previewDriveHTTP(t, driveServer(nil), []string{"sub-abc"}, nil)
		if w.Code != http.StatusOK {
			t.Errorf("code = %d, want 200", w.Code)
		}
	})

	t.Run("an unknown body field is refused", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/drives/preview",
			strings.NewReader(`{"user_subjects":["a"],"drive_id":"x"}`))
		w := httptest.NewRecorder()
		driveServer(&driveStore{}).handlePreviewUserDrive(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("code = %d, want 400 — the preview takes claims, and a caller naming a drive is asking a different question", w.Code)
		}
	})
}
