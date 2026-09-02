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

// pausedDriveStore is the deployment where the caller's allocation WINS and is
// DISABLED — the one answer the resolver now has to tell apart from a mountable
// one, and it differs from the mounted fixture in exactly one column. The grant
// is grantFixture's disabled twin, so the two doubles cannot drift.
func pausedDriveStore(mut func(*types.UserDrive)) *driveStore {
	d := driveFixture(mut)
	return &driveStore{
		drive: d, tier: types.CapabilitySubjectUser,
		grant: grantFixture(d.ID, func(g *types.UserDriveGrant) { g.Enabled = false }),
	}
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
		// The other side of the one column that now decides: an ENABLED winner
		// is mounted, and the paused branch is not something a live allocation
		// can fall into.
		if got.Paused {
			t.Error("an enabled grant resolved as PAUSED")
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

	// ─── a paused allocation is TOLD, not resolved to nothing ─────────────────

	t.Run("a DISABLED winner resolves to PAUSED, not to nothing", func(t *testing.T) {
		// Before this arm the store's WHERE excluded the row, so a member whose
		// allocation an admin paused was told "no user drive is allocated to
		// you" — sent to their admin for what that admin had just turned off —
		// and the frozen NR_PAUSED / REFUSED_PAUSED copy was unreachable.
		//
		// NOTHING IS DERIVED, and that is the fold worth naming: a paused drive
		// mounts nothing, so a home name (and the object name built from it)
		// would be a directory no consumer ever asks for — and a derivation
		// that FAILED would answer a paused member with the wrong refusal
		// entirely, a 422 about a claim that cannot name a directory.
		yes := true
		d := driveFixture(func(d *types.UserDrive) {
			d.Backend, d.HomeTemplate = types.DriveBackendK8sPVC, types.HomeTemplateHash
		})
		g := grantFixture(d.ID, func(g *types.UserDriveGrant) {
			g.Enabled = false
			g.SizeMiBOverride, g.WritableOverride, g.HomeOverride = 512, &yes, "bsmith"
		})
		st := &driveStore{drive: d, grant: g, tier: types.CapabilitySubjectUser}
		got, err := driveServer(st).resolveUserDrive(driveMemberCtx([]string{"eng"}, false))
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if got == nil || !got.Paused {
			t.Fatalf("resolved = %+v, want the paused allocation", got)
		}
		if got.Drive.ID != d.ID || got.Grant.ID != g.ID || got.Tier != types.CapabilitySubjectUser {
			t.Errorf("resolved = drive %s / grant %s / tier %q, want the disabled row carried through",
				got.Drive.ID, got.Grant.ID, got.Tier)
		}
		// The size and mode folds STILL run: the member is shown what is
		// paused, and reading the drive's own 10240 where the grant says 512
		// would name a different allocation than the one that is off.
		if got.SizeMiB != 512 || !got.Writable {
			t.Errorf("size/writable = %d/%v, want the grant's 512 and its writable override", got.SizeMiB, got.Writable)
		}
		if got.Enforcement != types.StorageEnforcementRequest {
			t.Errorf("enforcement = %q, want %q — a paused PVC's size means exactly what it always meant",
				got.Enforcement, types.StorageEnforcementRequest)
		}
		if got.HomeName != "" || got.ObjectName != "" {
			t.Errorf("home/object = %q/%q, want both EMPTY — a paused drive mounts nothing, and the grant's home override must not derive one",
				got.HomeName, got.ObjectName)
		}
	})

	t.Run("a paused home that could NOT be derived is still paused, not unmountable", func(t *testing.T) {
		// The ordering inside the fold, asserted rather than assumed: the same
		// drive and claims answer errDriveUnmountable when the grant is
		// enabled (the sub-test above), so a paused row reaching the derivation
		// would answer a 422 about a directory name for an allocation that was
		// never going to mount — the wrong sentence, and one no admin can act
		// on by fixing what it names.
		d := driveFixture(func(d *types.UserDrive) {
			d.Backend, d.HomeTemplate, d.HostRoot = types.DriveBackendHostPath, types.HomeTemplateEmailLocal, "/srv/homes"
		})
		st := &driveStore{drive: d, tier: types.CapabilitySubjectUser,
			grant: grantFixture(d.ID, func(g *types.UserDriveGrant) { g.Enabled = false })}
		noEmail := withOIDCGroups(operatorCtx("sub-drive-bob", "", oidc.RoleMember), []string{"eng"})
		got, err := driveServer(st).resolveUserDrive(noEmail)
		if err != nil {
			t.Fatalf("resolve = %v, want the paused answer rather than an unmountable refusal", err)
		}
		if got == nil || !got.Paused {
			t.Fatalf("resolved = %+v, want the paused allocation", got)
		}
	})

	t.Run("a paused USER row is fully determined despite an unusable snapshot", func(t *testing.T) {
		// user > group > all, so a named principal's answer does not depend on
		// their groups — and PAUSED is an answer. Refusing 403 here would lock
		// exactly the people an admin took the trouble to name out of being
		// told why their drive stopped, on the deployment shape (a group-tier
		// grant exists) where the refusal otherwise fires.
		st := pausedDriveStore(nil)
		st.hasGroupTier, st.userTierOnly = true, true
		got, err := driveServer(st).resolveUserDrive(driveMemberCtx(nil, false))
		if err != nil {
			t.Fatalf("resolve with an unusable snapshot: %v", err)
		}
		if got == nil || !got.Paused {
			t.Fatalf("resolved = %+v, want the paused allocation served", got)
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

	t.Run("a PAUSED allocation renders paused, with nothing derived", func(t *testing.T) {
		// The admin's answer to "why does Bob say he has no drive": the row IS
		// allocated and it is off. home_name/object_name are ABSENT rather than
		// empty — nothing mounts, so there is no object to paste into an
		// offboarding command, and omitempty is what says so.
		st := pausedDriveStore(func(d *types.UserDrive) { d.Name = "Corp NAS" })
		w := previewDriveHTTP(t, driveServer(st), []string{"sub-abc", "alice@corp.example"}, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v (body=%s)", err, w.Body.String())
		}
		if body["paused"] != true {
			t.Errorf("paused = %v, want true (body=%s)", body["paused"], w.Body.String())
		}
		if body["drive_name"] != "Corp NAS" || body["matched_tier"] != string(types.CapabilitySubjectUser) {
			t.Errorf("drive/tier = %v/%v, want the paused row's", body["drive_name"], body["matched_tier"])
		}
		for _, k := range []string{"home_name", "object_name"} {
			if _, ok := body[k]; ok {
				t.Errorf("%s = %v is present; a paused drive derives no name", k, body[k])
			}
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

	t.Run("no user_subjects is a 400 in the ADMIN's voice", func(t *testing.T) {
		// A claims-less preview must not fall through to the resolver's step 2,
		// which would answer the EMPTY OBJECT — "nobody is allocated this" — for
		// a question nobody asked, and send an admin looking for a grant that is
		// sitting right there. Groups alone are the same answer: a home name is
		// derived from a USER claim.
		for _, tc := range []struct {
			name   string
			users  []string
			groups []string
		}{
			{name: "no claims at all"},
			{name: "blank claims", users: []string{"", "   "}},
			{name: "groups only", groups: []string{"eng"}},
		} {
			w := previewDriveHTTP(t, driveServer(&driveStore{}), tc.users, tc.groups)
			if w.Code != http.StatusBadRequest {
				t.Errorf("%s: code = %d, want 400; body=%s", tc.name, w.Code, w.Body.String())
				continue
			}
			// The ADMIN's voice. §7.7's member sentences are the member's doors,
			// and answering an admin's empty form with one would put a
			// member-facing refusal on a screen no member can reach.
			const want = "user_subjects: at least one claim is needed to derive the directory name"
			if got := refusalBody(t, w); got != want {
				t.Errorf("%s: body = %q, want %q", tc.name, got, want)
			}
		}
	})

	// ─── which claim the answer keys on ──────────────────────────────────────

	t.Run("home_subject names the claim the home was derived from", func(t *testing.T) {
		// The endpoint's request cannot LABEL a claim — the console sends one
		// kind-less box and the resolver reads position — so the response says
		// which one it used. Without it, an admin reads a well-formed object
		// name and has no way to tell it apart from one derived off the claim
		// they did not mean.
		for _, tc := range []struct {
			name  string
			tmpl  types.HomeTemplate
			users []string
			want  string
		}{
			// hash and sub read users[0]; email_local reads the LAST claim.
			{name: "hash", tmpl: types.HomeTemplateHash, users: []string{"sub-abc", "alice@corp.example"}, want: "sub-abc"},
			{name: "sub", tmpl: types.HomeTemplateSub, users: []string{"sub-abc", "alice@corp.example"}, want: "sub-abc"},
			{name: "email_local", tmpl: types.HomeTemplateEmailLocal, users: []string{"sub-abc", "Alice@Corp.Example"}, want: "alice@corp.example"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				backend := types.DriveBackendDockerVolume
				if tc.tmpl != types.HomeTemplateHash {
					backend = types.DriveBackendHostPath // a share, where a claim template is legal
				}
				d := driveFixture(func(d *types.UserDrive) {
					d.Backend, d.HomeTemplate, d.HostRoot = backend, tc.tmpl, "/srv/homes"
					if backend == types.DriveBackendDockerVolume {
						d.HostRoot = ""
					}
				})
				st := &driveStore{drive: d, grant: grantFixture(d.ID, nil), tier: types.CapabilitySubjectUser}
				w := previewDriveHTTP(t, driveServer(st), tc.users, nil)
				if w.Code != http.StatusOK {
					t.Fatalf("code = %d: %s", w.Code, w.Body.String())
				}
				var got userDrivePreviewResponse
				if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if got.HomeSubject != tc.want {
					t.Errorf("home_subject = %q, want the claim the home keys on (%q)", got.HomeSubject, tc.want)
				}
			})
		}
	})

	t.Run("an email-first preview of a subject-keyed drive is warned about", func(t *testing.T) {
		// THE DEFECT THIS CLOSES, and `hash` is the shape that carries it: an
		// admin pastes only the address they know, sha256 hashes it happily, and
		// a perfectly well-formed object name comes back that NO RUN WILL EVER
		// MOUNT — the run derives from the sign-in subject. The answer stays
		// exactly what enforcement would do for those claims; the warning is
		// what says the question was asked with the wrong claim first.
		const want = "the directory name keys on the sign-in subject; paste it first"
		d := driveFixture(nil) // docker_volume + hash
		st := &driveStore{drive: d, grant: grantFixture(d.ID, nil), tier: types.CapabilitySubjectUser}

		var got userDrivePreviewResponse
		w := previewDriveHTTP(t, driveServer(st), []string{"alice@corp.example"}, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d: %s", w.Code, w.Body.String())
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v (body=%s)", err, w.Body.String())
		}
		if got.Warning != want {
			t.Errorf("warning = %q, want %q", got.Warning, want)
		}
		// The ANSWER is untouched: the warning is advisory and never edits what
		// the resolver said.
		if got.HomeSubject != "alice@corp.example" || got.ObjectName == "" {
			t.Errorf("preview = %+v, want the resolver's own answer beside the warning", got)
		}

		// The subject FIRST is the shape the console is meant to send, and it
		// must not be warned about — a warning that fires on the correct input
		// is one an admin learns to ignore.
		got = userDrivePreviewResponse{}
		w = previewDriveHTTP(t, driveServer(st), []string{"sub-abc", "alice@corp.example"}, nil)
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.Warning != "" {
			t.Errorf("warning = %q on a subject-first preview, want none", got.Warning)
		}

		// email_local READS the address, so an address-first preview is exactly
		// right for it and a warning there would be wrong.
		ed := driveFixture(func(d *types.UserDrive) {
			d.Backend, d.HomeTemplate, d.HostRoot = types.DriveBackendHostPath, types.HomeTemplateEmailLocal, "/srv/homes"
		})
		est := &driveStore{drive: ed, grant: grantFixture(ed.ID, nil), tier: types.CapabilitySubjectUser}
		got = userDrivePreviewResponse{}
		w = previewDriveHTTP(t, driveServer(est), []string{"alice@corp.example"}, nil)
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("email_local: decode: %v (body=%s)", err, w.Body.String())
		}
		if got.Warning != "" {
			t.Errorf("email_local: warning = %q, want none — that template reads the address", got.Warning)
		}
		if got.HomeName != "alice" {
			t.Errorf("email_local: home_name = %q, want the answer unchanged by the warning rule", got.HomeName)
		}
	})

	t.Run("the sub template refuses the address before it can answer wrongly", func(t *testing.T) {
		// `sub` is the OTHER template that keys on users[0], and it never
		// reaches the warning: an address carries an "@", which is not a legal
		// segment character, so the derivation refuses first. The rule still
		// covers it — asserted at the predicate, where the 422 hides it — so a
		// later template change cannot quietly drop `sub` out of the guard.
		d := driveFixture(func(d *types.UserDrive) {
			d.Backend, d.HomeTemplate, d.HostRoot = types.DriveBackendHostPath, types.HomeTemplateSub, "/srv/homes"
		})
		st := &driveStore{drive: d, grant: grantFixture(d.ID, nil), tier: types.CapabilitySubjectUser}
		if w := previewDriveHTTP(t, driveServer(st), []string{"alice@corp.example"}, nil); w.Code != http.StatusUnprocessableEntity {
			t.Errorf("code = %d, want 422; body=%s", w.Code, w.Body.String())
		}
		if got := drivePreviewWarning(types.HomeTemplateSub, []string{"alice@corp.example"}); got == "" {
			t.Error("drivePreviewWarning(sub) = \"\", want the rule to cover both users[0] templates")
		}
	})

	t.Run("a paused row derives no home_subject", func(t *testing.T) {
		// Same rule as home_name/object_name: nothing is derived, so there is no
		// claim the answer keys on to name.
		st := pausedDriveStore(nil)
		w := previewDriveHTTP(t, driveServer(st), []string{"sub-abc"}, nil)
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v (body=%s)", err, w.Body.String())
		}
		if _, ok := body["home_subject"]; ok {
			t.Errorf("home_subject = %v is present on a paused row", body["home_subject"])
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
