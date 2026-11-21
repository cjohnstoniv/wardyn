// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func portalGrant(p types.Delegate, d types.UserDrive) types.UserDriveGrant {
	return types.UserDriveGrant{SubjectType: types.DriveSubjectPortal, Subject: p.ID.String(), DriveID: d.ID,
		Enabled: true, CreatedBy: "admin@example.com"}
}

// seedTeamDrive persists a team drive on docker_volume.
func seedTeamDrive(t *testing.T, st store.PG, name string) types.UserDrive {
	t.Helper()
	d := newUserDrive(name)
	d.Team, d.Writable = true, true
	saved, err := st.UpsertUserDrive(context.Background(), d, false)
	if err != nil {
		t.Fatalf("seed team drive %q: %v", name, err)
	}
	t.Cleanup(func() { _ = st.DeleteUserDrive(context.Background(), saved.ID) })
	return saved
}

// seedStaticTeamDrive persists a team drive on k8s_pvc_static: a backend a portal may hold that can also carry a
// second place (the schema allows a second place on host_path or k8s_pvc_static only).
func seedStaticTeamDrive(t *testing.T, st store.PG, name string) types.UserDrive {
	t.Helper()
	d := newUserDrive(name)
	d.Team, d.Writable, d.Backend = true, true, types.DriveBackendK8sPVCStatic
	saved, err := st.UpsertUserDrive(context.Background(), d, false)
	if err != nil {
		t.Fatalf("seed static team drive %q: %v", name, err)
	}
	t.Cleanup(func() { _ = st.DeleteUserDrive(context.Background(), saved.ID) })
	return saved
}

// withSecondPlace is d with a second place on a seeded personal runner: the one shape the schema accepts.
func withSecondPlace(t *testing.T, st store.PG, d types.UserDrive) types.UserDrive {
	t.Helper()
	rn, err := st.CreateRunner(context.Background(), newRunner("alice-"+uuid.NewString()[:8]+"@example.com"))
	if err != nil {
		t.Fatalf("seed a personal runner for the second place: %v", err)
	}
	d.AltBackend, d.AltHostRoot, d.AltRunnerID = types.DriveBackendHostPath, "/srv/alt", &rn.ID
	return d
}

func resolvePortal(t *testing.T, st store.PG, portal uuid.UUID) []store.UserDriveWinner {
	t.Helper()
	ws, err := st.ResolvePortalTeamDrives(context.Background(), portal)
	if err != nil {
		t.Fatalf("ResolvePortalTeamDrives: %v", err)
	}
	return ws
}

// An allocation is written for a team drive and a live issuer portal only; each refusal is its own sentinel and
// writes no row.
func TestPG_PortalDriveAllocation_TeamDriveAndIssuerPortalOnly(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()
	portal := newIssuerPortal(t, st, "").Delegate
	revoked := newIssuerPortal(t, st, "").Delegate
	if _, _, err := st.RevokeDelegate(ctx, revoked.ID, time.Now().UTC(), false); err != nil {
		t.Fatal(err)
	}
	plain := seedUserDrive(t, st, "plain-"+uuid.NewString()[:8])
	team := seedTeamDrive(t, st, "team-"+uuid.NewString()[:8])
	for _, tc := range []struct {
		name string
		g    types.UserDriveGrant
		want error
	}{
		{"a drive that is not a team drive", portalGrant(portal, plain), store.ErrDrivePortalNotTeam},
		{"a revoked portal", portalGrant(revoked, team), store.ErrDrivePortalNotLive},
		{"an unknown portal", portalGrant(types.Delegate{ID: uuid.New()}, team), store.ErrDrivePortalNotLive},
	} {
		if _, err := st.AddUserDriveGrant(ctx, tc.g, true); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	if n := rowCount(t, pool, `SELECT count(*) FROM user_drive_grants WHERE subject_type = 'portal'`); n != 0 {
		t.Errorf("%d portal rows after only refusals, want 0", n)
	}
	for name, subject := range map[string]string{"upper case": "ABCDEFAB-0000-0000-0000-000000000000", "not a uuid": "platform"} {
		if _, err := pool.Exec(ctx, `INSERT INTO user_drive_grants (id, subject_type, subject, drive_id, enabled, created_by)
			VALUES ($1, 'portal', $2, $3, true, 'x')`, uuid.New(), subject, team.ID); err == nil {
			t.Errorf("the schema accepted a portal subject that is %s", name)
		}
	}
	got, err := st.AddUserDriveGrant(ctx, portalGrant(portal, team), true)
	if err != nil {
		t.Fatalf("allocate to a live issuer portal: %v", err)
	}
	t.Cleanup(func() { _, _ = st.DeleteUserDriveGrant(ctx, got.ID) })
	if ws := resolvePortal(t, st, portal.ID); len(ws) != 1 || ws[0].Drive.ID != team.ID {
		t.Fatalf("resolve = %+v, want the one allocated drive", ws)
	}
}

// A portal's rows are read by ResolvePortalTeamDrives only; the person resolvers never return them.
func TestPG_PortalTeamDrive_OldResolverIgnoresPortalRows(t *testing.T) {
	st, _ := isolatedStore(t)
	ctx := context.Background()
	portal := newIssuerPortal(t, st, "").Delegate
	team := seedTeamDrive(t, st, "team-"+uuid.NewString()[:8])
	addUserDriveGrant(t, st, portalGrant(portal, team))
	for _, subjects := range [][]string{{portal.ID.String()}, {"sub-x"}} {
		ws, err := st.ResolveUserDrives(ctx, subjects, []string{portal.ID.String()}, portal.ID.String())
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range ws {
			if w.Drive.ID == team.ID {
				t.Errorf("the person resolver returned a portal row for subjects %v: %+v", subjects, w)
			}
		}
	}
}

// A portal reaches only the drives allocated to it: another portal, a revoked portal and a drive that is a
// group's alone resolve nothing.
func TestPG_PortalTeamDrive_OnlyPortalAllocationsReach(t *testing.T) {
	st, _ := isolatedStore(t)
	ctx := context.Background()
	mine, other := newIssuerPortal(t, st, "").Delegate, newIssuerPortal(t, st, "").Delegate
	allocated := seedTeamDrive(t, st, "a-"+uuid.NewString()[:8])
	groupOnly := seedTeamDrive(t, st, "g-"+uuid.NewString()[:8])
	addUserDriveGrant(t, st, portalGrant(mine, allocated))
	addUserDriveGrant(t, st, types.UserDriveGrant{SubjectType: types.CapabilitySubjectGroup, Subject: "platform",
		DriveID: groupOnly.ID, Enabled: true, CreatedBy: "admin@example.com"})
	if ws := resolvePortal(t, st, mine.ID); len(ws) != 1 || ws[0].Drive.ID != allocated.ID {
		t.Errorf("portal resolves %+v, want only its allocated drive", ws)
	}
	if ws := resolvePortal(t, st, other.ID); len(ws) != 0 {
		t.Errorf("another portal resolves %+v, want nothing", ws)
	}
	if _, _, err := st.RevokeDelegate(ctx, mine.ID, time.Now().UTC(), false); err != nil {
		t.Fatal(err)
	}
	if ws := resolvePortal(t, st, mine.ID); len(ws) != 0 {
		t.Errorf("a revoked portal resolves %+v, want nothing", ws)
	}
}

// A host path drive is refused, a second place is refused, and a drive edit that adds a second place, moves the
// backend or unmarks the team on a drive a portal holds is refused with its own sentinel, leaving the drive as it
// was. A drive no portal holds edits as before. Every fixture is a shape the schema accepts, so each refusal is the
// portal check and not a constraint.
func TestPG_PortalDriveAllocation_HostPathAndSecondPlaceRefused(t *testing.T) {
	st, _ := isolatedStore(t)
	ctx := context.Background()
	portal := newIssuerPortal(t, st, "").Delegate

	host := newUserDrive("host-" + uuid.NewString()[:8])
	host.Team, host.Backend, host.HostRoot = true, types.DriveBackendHostPath, "/srv/team"
	hostSaved, err := st.UpsertUserDrive(ctx, host, false)
	if err != nil {
		t.Fatalf("seed a host path team drive: %v", err)
	}
	t.Cleanup(func() { _ = st.DeleteUserDrive(ctx, hostSaved.ID) })
	if _, err := st.AddUserDriveGrant(ctx, portalGrant(portal, hostSaved), true); !errors.Is(err, store.ErrDrivePortalBackend) {
		t.Errorf("host path allocation: err = %v, want ErrDrivePortalBackend", err)
	}

	two := seedStaticTeamDrive(t, st, "two-"+uuid.NewString()[:8])
	if _, err := st.UpsertUserDrive(ctx, withSecondPlace(t, st, two), false); err != nil {
		t.Fatalf("a second place on a drive no portal holds: %v", err)
	}
	if _, err := st.AddUserDriveGrant(ctx, portalGrant(portal, two), true); !errors.Is(err, store.ErrDrivePortalSecondPlace) {
		t.Errorf("allocate a drive with a second place: err = %v, want ErrDrivePortalSecondPlace", err)
	}

	held := seedStaticTeamDrive(t, st, "held-"+uuid.NewString()[:8])
	addUserDriveGrant(t, st, portalGrant(portal, held))
	if _, err := st.UpsertUserDrive(ctx, withSecondPlace(t, st, held), false); !errors.Is(err, store.ErrDrivePortalSecondPlace) {
		t.Errorf("second place on a held drive: err = %v, want ErrDrivePortalSecondPlace", err)
	}
	moved := held
	moved.Backend, moved.HostRoot = types.DriveBackendHostPath, "/srv/moved"
	if _, err := st.UpsertUserDrive(ctx, moved, false); !errors.Is(err, store.ErrDrivePortalBackend) {
		t.Errorf("backend move of a held drive: err = %v, want ErrDrivePortalBackend", err)
	}
	unmarked := held
	unmarked.Team = false
	if _, err := st.UpsertUserDrive(ctx, unmarked, false); !errors.Is(err, store.ErrDrivePortalNotTeam) {
		t.Errorf("team switched off on a held drive: err = %v, want ErrDrivePortalNotTeam", err)
	}
	if got, err := st.GetUserDrive(ctx, held.ID); err != nil || got.Backend != types.DriveBackendK8sPVCStatic || got.AltBackend != "" || !got.Team {
		t.Errorf("held drive after the refused edits = %+v, %v; want unchanged", got, err)
	}
	if ws := resolvePortal(t, st, portal.ID); len(ws) != 1 || ws[0].Drive.ID != held.ID {
		t.Errorf("the portal resolves %+v after the refused edits, want its held drive", ws)
	}
	renamed := held
	renamed.Name = "renamed-" + uuid.NewString()[:8]
	if _, err := st.UpsertUserDrive(ctx, renamed, false); err != nil {
		t.Errorf("a plain edit of a held drive: %v", err)
	}
}

// A drive edit written from a stale read: the editor read a plain drive and sent it back after another admin made it
// a team drive and allocated it to a portal. The stale write is refused and the drive stays a team drive the portal
// resolves. A stale write of a drive no portal holds is written as before.
func TestPG_PortalDriveAllocation_StaleEditCannotUnmarkTeam(t *testing.T) {
	st, _ := isolatedStore(t)
	ctx := context.Background()
	portal := newIssuerPortal(t, st, "").Delegate
	stale := seedUserDrive(t, st, "stale-"+uuid.NewString()[:8]) // the first editor's read: not a team drive
	other := seedUserDrive(t, st, "free-"+uuid.NewString()[:8])

	team := stale
	team.Team, team.Writable = true, true
	if _, err := st.UpsertUserDrive(ctx, team, false); err != nil {
		t.Fatalf("the second admin marks the drive a team drive: %v", err)
	}
	addUserDriveGrant(t, st, portalGrant(portal, stale))

	cosmetic := stale
	cosmetic.Name = "renamed-" + uuid.NewString()[:8]
	if _, err := st.UpsertUserDrive(ctx, cosmetic, false); !errors.Is(err, store.ErrDrivePortalNotTeam) {
		t.Errorf("the delayed cosmetic edit: err = %v, want ErrDrivePortalNotTeam", err)
	}
	proposed := stale
	proposed.Backend, proposed.HostRoot = types.DriveBackendHostPath, "/srv/proposed"
	if _, err := st.UpsertUserDrive(ctx, proposed, false); !errors.Is(err, store.ErrDrivePortalNotTeam) {
		t.Errorf("the delayed edit that also moves the backend: err = %v, want ErrDrivePortalNotTeam", err)
	}
	got, err := st.GetUserDrive(ctx, stale.ID)
	if err != nil || !got.Team || got.Name != stale.Name || got.Backend != types.DriveBackendDockerVolume {
		t.Errorf("drive after the delayed edits = %+v, %v; want the team drive as the second admin left it", got, err)
	}
	if ws := resolvePortal(t, st, portal.ID); len(ws) != 1 || ws[0].Drive.ID != stale.ID {
		t.Errorf("the portal resolves %+v, want its drive", ws)
	}

	edited := other
	edited.Name = "edited-" + uuid.NewString()[:8]
	if got, err := st.UpsertUserDrive(ctx, edited, false); err != nil || got.Name != edited.Name {
		t.Errorf("an edit of a drive no portal holds = %+v, %v; want it written", got, err)
	}
}

// The re-home precondition counts an allocation only when the write moves its object, under the drive's row lock: a
// portal's folder is named by the portal and drive ids, so only a backend move reaches it; a group's team folder by the
// drive id, so a template change never does and a rename only under the slug scheme; any other allocation by all of
// the identity fields. ListUserDrives says how many of a drive's allocations are portals'.
func TestPG_PortalDriveAllocation_RehomePreconditionCountsOnlyWhatMoves(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()
	portal := newIssuerPortal(t, st, "").Delegate
	team := seedTeamDrive(t, st, "rehome-"+uuid.NewString()[:8])
	addUserDriveGrant(t, st, portalGrant(portal, team))
	allowed := func(label string, d types.UserDrive) types.UserDrive {
		t.Helper()
		got, err := st.UpsertUserDrive(ctx, d, true)
		if err != nil {
			t.Fatalf("%s under the precondition: %v, want it written", label, err)
		}
		return got
	}
	refused := func(label string, d types.UserDrive) {
		t.Helper()
		if _, err := st.UpsertUserDrive(ctx, d, true); !errors.Is(err, store.ErrDriveAllocated) {
			t.Errorf("%s under the precondition: err = %v, want ErrDriveAllocated", label, err)
		}
	}

	team.Name = "renamed-" + uuid.NewString()[:8]
	team = allowed("a rename of a portal-only drive", team)
	team.HomeTemplate = types.HomeTemplateSub
	team = allowed("a template change of a portal-only drive", team)
	moved := team
	moved.Backend = types.DriveBackendK8sPVCStatic
	refused("a backend move of a portal-only drive", moved)
	if got, err := st.GetUserDrive(ctx, team.ID); err != nil || got.Backend != types.DriveBackendDockerVolume {
		t.Errorf("drive after the refused move = %+v, %v; want docker_volume", got, err)
	}

	addUserDriveGrant(t, st, types.UserDriveGrant{SubjectType: types.CapabilitySubjectGroup, Subject: "platform",
		DriveID: team.ID, Enabled: true, CreatedBy: "admin@example.com"})
	team.HomeTemplate = types.HomeTemplateHash
	team = allowed("a template change once a group holds the ID-named team drive", team)
	team.Name = "again-" + uuid.NewString()[:8]
	team = allowed("a rename once a group holds the ID-named team drive", team)
	moved = team
	moved.Backend = types.DriveBackendK8sPVCStatic
	refused("a backend move once a group holds the drive", moved)
	if _, err := pool.Exec(ctx, `UPDATE user_drives SET object_scheme = 'slug', name_slug = $2 WHERE id = $1`, team.ID, types.DriveSlug(team.Name)); err != nil {
		t.Fatal(err)
	}
	team.Name = "slugged-" + uuid.NewString()[:8]
	refused("a rename of a slug-named team drive a group holds", team)

	items, err := st.ListUserDrives(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.ID == team.ID && (it.GrantCount != 2 || it.PortalGrantCount != 1) {
			t.Errorf("list item = %d allocations, %d portals; want 2 and 1", it.GrantCount, it.PortalGrantCount)
		}
	}

	own := seedUserDrive(t, st, "own-"+uuid.NewString()[:8])
	addUserDriveGrant(t, st, types.UserDriveGrant{SubjectType: types.CapabilitySubjectUser, Subject: "sub-alice",
		DriveID: own.ID, Enabled: true, CreatedBy: "admin@example.com"})
	own.HomeTemplate = types.HomeTemplateSub
	refused("a template change of an ordinary drive a person holds", own)
	own.HomeTemplate, own.Writable = types.HomeTemplateHash, !own.Writable
	allowed("an edit of an ordinary drive that names no storage", own)
}

// A portal allocation is added, never replaced: the replace and tier-write doors refuse it.
func TestPG_PortalDriveAllocation_AdditiveOnly(t *testing.T) {
	st, _ := isolatedStore(t)
	ctx := context.Background()
	portal := newIssuerPortal(t, st, "").Delegate
	team := seedTeamDrive(t, st, "team-"+uuid.NewString()[:8])
	g := portalGrant(portal, team)
	if _, _, err := st.ReplaceUserDriveGrant(ctx, g, true); !errors.Is(err, store.ErrDrivePortalAdditiveOnly) {
		t.Errorf("replace: err = %v, want ErrDrivePortalAdditiveOnly", err)
	}
	if _, _, err := st.UpsertUserDriveGrantWithinTier(ctx, g, true, true); !errors.Is(err, store.ErrDrivePortalAdditiveOnly) {
		t.Errorf("tier write: err = %v, want ErrDrivePortalAdditiveOnly", err)
	}
}

// A portal revoked while an allocation is written: the two orders both finish (no deadlock), and when the revoke
// won, the allocation is refused; when the allocation won, the revoked portal resolves nothing.
func TestPortalDriveAllocation_RacesRevoke_PG(t *testing.T) {
	st, _ := isolatedStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for i := 0; i < 20; i++ {
		portal := newIssuerPortal(t, st, "").Delegate
		team := seedTeamDrive(t, st, "race-"+uuid.NewString()[:8])
		var wg sync.WaitGroup
		var addErr, revErr error
		wg.Add(2)
		go func() { defer wg.Done(); _, addErr = st.AddUserDriveGrant(ctx, portalGrant(portal, team), true) }()
		go func() { defer wg.Done(); _, _, revErr = st.RevokeDelegate(ctx, portal.ID, time.Now().UTC(), false) }()
		wg.Wait()
		if revErr != nil {
			t.Fatalf("round %d revoke: %v", i, revErr)
		}
		if addErr != nil && !errors.Is(addErr, store.ErrDrivePortalNotLive) {
			t.Fatalf("round %d allocation: %v, want success or ErrDrivePortalNotLive", i, addErr)
		}
		if ws := resolvePortal(t, st, portal.ID); len(ws) != 0 {
			t.Fatalf("round %d: a revoked portal resolves %+v", i, ws)
		}
		if _, err := st.AddUserDriveGrant(ctx, portalGrant(portal, team), true); !errors.Is(err, store.ErrDrivePortalNotLive) {
			t.Fatalf("round %d: allocation after the revoke = %v, want ErrDrivePortalNotLive", i, err)
		}
	}
}

// A drive edit that adds a second place and an allocation of the same drive, written at once: exactly one is
// written and the other is refused with the portal sentinel, and no portal row ever sits on a drive with a second
// place.
func TestPortalDriveAllocation_RacesDriveEdit_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for i := 0; i < 20; i++ {
		portal := newIssuerPortal(t, st, "").Delegate
		team := seedStaticTeamDrive(t, st, "race-"+uuid.NewString()[:8])
		edit := withSecondPlace(t, st, team)
		var wg sync.WaitGroup
		var addErr, editErr error
		wg.Add(2)
		go func() { defer wg.Done(); _, addErr = st.AddUserDriveGrant(ctx, portalGrant(portal, team), true) }()
		go func() { defer wg.Done(); _, editErr = st.UpsertUserDrive(ctx, edit, false) }()
		wg.Wait()
		switch {
		case addErr == nil && editErr == nil:
			t.Fatalf("round %d: the allocation and the second place were both written", i)
		case addErr != nil && editErr != nil:
			t.Fatalf("round %d: both were refused (allocation %v, edit %v); one must win", i, addErr, editErr)
		case addErr != nil && !errors.Is(addErr, store.ErrDrivePortalSecondPlace):
			t.Fatalf("round %d: the allocation lost with %v, want ErrDrivePortalSecondPlace", i, addErr)
		case editErr != nil && !errors.Is(editErr, store.ErrDrivePortalSecondPlace):
			t.Fatalf("round %d: the edit lost with %v, want ErrDrivePortalSecondPlace", i, editErr)
		}
		if n := rowCount(t, pool, `SELECT count(*) FROM user_drive_grants g JOIN user_drives d ON d.id = g.drive_id
			WHERE g.subject_type = 'portal' AND d.alt_backend IS NOT NULL`); n != 0 {
			t.Fatalf("round %d: %d portal allocations on a drive with a second place", i, n)
		}
	}
}

// The two commit orders of an allocation and a drive edit, forced: a test transaction stands in for the writer that
// is already in flight, holding the drive as that writer holds it, and the other writer must wait and then see the
// committed row. The allocation in flight (the drive FOR SHARE, its row inserted) holds the edit, which then
// refuses; the edit in flight (the drive FOR UPDATE, the second place written) holds the allocation, which then
// refuses.
func TestPortalDriveAllocation_RacesDriveEdit_PG_Orders(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()
	t.Run("allocation first, edit second", func(t *testing.T) {
		portal := newIssuerPortal(t, st, "").Delegate
		team := seedStaticTeamDrive(t, st, "first-"+uuid.NewString()[:8])
		edit := withSecondPlace(t, st, team)
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck // already committed on the happy path
		if _, err := tx.Exec(ctx, `SELECT 1 FROM user_drives WHERE id = $1 FOR SHARE`, team.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO user_drive_grants (id, subject_type, subject, drive_id, enabled, created_by)
			VALUES ($1, 'portal', $2, $3, true, 'x')`, uuid.New(), portal.ID.String(), team.ID); err != nil {
			t.Fatal(err)
		}
		blocked, result := parked(func() error { _, err := st.UpsertUserDrive(ctx, edit, false); return err })
		if !blocked {
			_ = tx.Commit(ctx)
			t.Fatal("the edit did not wait for the allocation in flight")
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := result(); !errors.Is(err, store.ErrDrivePortalSecondPlace) {
			t.Errorf("the edit behind a committed allocation: err = %v, want ErrDrivePortalSecondPlace", err)
		}
		if got, err := st.GetUserDrive(ctx, team.ID); err != nil || got.AltBackend != "" {
			t.Errorf("drive = %+v, %v; want no second place", got, err)
		}
	})
	t.Run("edit first, allocation second", func(t *testing.T) {
		portal := newIssuerPortal(t, st, "").Delegate
		team := seedStaticTeamDrive(t, st, "second-"+uuid.NewString()[:8])
		edit := withSecondPlace(t, st, team)
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck // already committed on the happy path
		if _, err := tx.Exec(ctx, `UPDATE user_drives SET alt_backend = $2, alt_host_root = $3, alt_runner_id = $4 WHERE id = $1`,
			team.ID, string(edit.AltBackend), edit.AltHostRoot, edit.AltRunnerID); err != nil {
			t.Fatal(err)
		}
		blocked, result := parked(func() error { _, err := st.AddUserDriveGrant(ctx, portalGrant(portal, team), true); return err })
		if !blocked {
			_ = tx.Commit(ctx)
			t.Fatal("the allocation did not wait for the edit in flight")
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := result(); !errors.Is(err, store.ErrDrivePortalSecondPlace) {
			t.Errorf("the allocation behind a committed edit: err = %v, want ErrDrivePortalSecondPlace", err)
		}
		if n := rowCount(t, pool, `SELECT count(*) FROM user_drive_grants WHERE drive_id = $1`, team.ID); n != 0 {
			t.Errorf("%d allocation row(s) on a drive with a second place, want none", n)
		}
	})
}

// A run with portal drives is inserted under the portal's row lock: after the portal is revoked every create
// path refuses it, and a run without portal drives is created as before.
func TestPortalRun_InsertOrderedAgainstRevoke_PG(t *testing.T) {
	st, rows := isolatedStore(t)
	ctx := context.Background()
	portal := newIssuerPortal(t, st, "").Delegate
	pool := cappedPool(t, st, 1000)
	mk := func(portalDrives bool) types.AgentRun {
		r := newRun(types.RunRunning)
		r.CreatedVia, r.PortalDrives = &portal.ID, portalDrives
		return r
	}
	create := map[string]func(types.AgentRun) (types.AgentRun, error){
		"CreateRun":         func(r types.AgentRun) (types.AgentRun, error) { return st.CreateRun(ctx, r) },
		"CreateRunUnderCap": func(r types.AgentRun) (types.AgentRun, error) { return st.CreateRunUnderCap(ctx, r, 100) },
		"CreateQueuedRun": func(r types.AgentRun) (types.AgentRun, error) {
			now := time.Now().UTC()
			q := poolRun(pool, types.RunQueued, r.CreatedBy)
			q.CreatedVia, q.PortalDrives = r.CreatedVia, r.PortalDrives
			return st.CreateQueuedRun(ctx, q, store.RunQueueEntry{QueuedAt: now, DeadlineAt: now.Add(time.Hour)}, 0)
		},
	}
	for name, f := range create {
		got, err := f(mk(true))
		if err != nil || !got.PortalDrives {
			t.Fatalf("%s before the revoke: %+v, %v; want a run that keeps portal_drives", name, got, err)
		}
	}
	if _, _, err := st.RevokeDelegate(ctx, portal.ID, time.Now().UTC(), false); err != nil {
		t.Fatal(err)
	}
	for name, f := range create {
		if _, err := f(mk(true)); !errors.Is(err, store.ErrPortalRevoked) {
			t.Errorf("%s after the revoke with portal drives: err = %v, want ErrPortalRevoked", name, err)
		}
		if got, err := f(mk(false)); err != nil || got.PortalDrives {
			t.Errorf("%s after the revoke without portal drives = %+v, %v; want an ordinary run created", name, got, err)
		}
	}
	if n := rowCount(t, rows, `SELECT count(*) FROM agent_runs WHERE created_via = $1 AND portal_drives`, portal.ID); n != len(create) {
		t.Errorf("%d portal run row(s) committed, want the %d created before the revoke and none after", n, len(create))
	}
}

// The fence holds at the schema as well: a row marked portal_drives cannot carry no portal.
func TestPG_PortalRun_SchemaFenceNeedsAPortal(t *testing.T) {
	st, pool := isolatedStore(t)
	r := newRun(types.RunRunning)
	got, err := st.CreateRun(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE agent_runs SET portal_drives = true WHERE id = $1`, got.ID); err == nil {
		t.Error("the schema accepted portal_drives on a run with no created_via")
	}
}

// Removing the allocation ends the portal's reach at once, and a portal registered again (a new id) reaches
// nothing the old one held and gets its own folder, never the old one's bytes.
func TestPG_PortalTeamDrive_RemovedAndReRegisteredPortalsNeverShare(t *testing.T) {
	st, _ := isolatedStore(t)
	ctx := context.Background()
	old := newIssuerPortal(t, st, "").Delegate
	team := seedTeamDrive(t, st, "team-"+uuid.NewString()[:8])
	g, err := st.AddUserDriveGrant(ctx, portalGrant(old, team), true)
	if err != nil {
		t.Fatal(err)
	}
	if ws := resolvePortal(t, st, old.ID); len(ws) != 1 {
		t.Fatalf("before the removal the portal resolves %d drives, want 1", len(ws))
	}
	if _, err := st.DeleteUserDriveGrant(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	if ws := resolvePortal(t, st, old.ID); len(ws) != 0 {
		t.Errorf("after the removal the portal resolves %+v, want nothing", ws)
	}
	fresh := newIssuerPortal(t, st, "").Delegate
	if ws := resolvePortal(t, st, fresh.ID); len(ws) != 0 {
		t.Errorf("a re-registered portal resolves %+v, want nothing until it is allocated", ws)
	}
	again := addUserDriveGrant(t, st, portalGrant(fresh, team))
	if types.DrivePortalHome(team, old.ID) == types.DrivePortalHome(team, fresh.ID) || again.Subject == old.ID.String() {
		t.Error("the re-registered portal's folder is the old portal's")
	}
}

// The allocation writers that share a subject or a drive, run at once on independent connections: the tier-judged
// add, the tier-judged replace, an operator's replace, all for one group on one team drive, and a portal's allocation
// of the same drive. None deadlocks (the context is bounded), a writer judged against no row may lose only with
// ErrUserDriveGrantChanged, the group ends with exactly one row on the drive, the replace removed the group's row on
// the other drive, and the portal's row landed.
func TestPortalDriveAllocation_ConcurrentTierWritesOneDriveNoDeadlock_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	for i := 0; i < 10; i++ {
		team := seedTeamDrive(t, st, "team-"+uuid.NewString()[:8])
		elsewhere := seedTeamDrive(t, st, "else-"+uuid.NewString()[:8])
		portal := newIssuerPortal(t, st, "").Delegate
		group := "grp-" + uuid.NewString()[:8]
		mk := func(d types.UserDrive) types.UserDriveGrant {
			return types.UserDriveGrant{SubjectType: types.CapabilitySubjectGroup, Subject: group, DriveID: d.ID,
				Enabled: true, CreatedBy: "admin@example.com"}
		}
		addUserDriveGrant(t, st, mk(elsewhere))
		var wg sync.WaitGroup
		errs := make(chan error, 4)
		run := func(name string, f func() error) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := f(); err != nil && !errors.Is(err, store.ErrUserDriveGrantChanged) {
					errs <- errors.New(name + ": " + err.Error())
				}
			}()
		}
		run("tier add", func() error { _, _, err := st.UpsertUserDriveGrantWithinTier(ctx, mk(team), false, true); return err })
		run("tier replace", func() error { _, _, err := st.UpsertUserDriveGrantWithinTier(ctx, mk(team), false, false); return err })
		run("operator replace", func() error { _, _, err := st.ReplaceUserDriveGrant(ctx, mk(team), false); return err })
		run("portal allocation", func() error { _, err := st.AddUserDriveGrant(ctx, portalGrant(portal, team), true); return err })
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Errorf("round %d: a concurrent allocation write failed: %v", i, err)
		}
		if ctx.Err() != nil {
			t.Fatalf("round %d: the writers did not finish: %v", i, ctx.Err())
		}
		if got := rowCount(t, pool, `SELECT count(*) FROM user_drive_grants WHERE subject_type = 'group' AND subject = $1 AND drive_id = $2`, group, team.ID); got != 1 {
			t.Errorf("round %d: %d group rows on the drive, want exactly 1", i, got)
		}
		if got := rowCount(t, pool, `SELECT count(*) FROM user_drive_grants WHERE subject_type = 'group' AND subject = $1 AND drive_id = $2`, group, elsewhere.ID); got != 0 {
			t.Errorf("round %d: the replace left the group's row on the other drive", i)
		}
		if got := rowCount(t, pool, `SELECT count(*) FROM user_drive_grants WHERE subject_type = 'portal' AND subject = $1 AND drive_id = $2`, portal.ID.String(), team.ID); got != 1 {
			t.Errorf("round %d: %d portal rows on the drive, want 1", i, got)
		}
	}
}

// fixtureT is the part of *testing.T a held-lock fixture uses, so a test can show that a failed observation unwinds one.
type fixtureT interface {
	Helper()
	Fatal(args ...any)
	Cleanup(func())
}

// holdTx opens a transaction that runs stmt and holds what it locked until the returned release commits it. The
// rollback is registered as a cleanup at once, before anything can fail, and release and the cleanup are one
// idempotent step, so a test that stops on a failed observation still lets go of the lock: the cleanup runs before the
// pool's own (which closes the pool before it terminates the backends, and would wait on a held lock).
func holdTx(t fixtureT, pool *pgxpool.Pool, stmt string, args ...any) (release func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	finish := func(commit bool) (err error) {
		once.Do(func() {
			// Its own deadline: the one above may be spent by the time a test stops.
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if commit {
				err = tx.Commit(ctx)
			} else {
				err = tx.Rollback(ctx)
			}
		})
		return err
	}
	t.Cleanup(func() { _ = finish(false) })
	if _, err := tx.Exec(ctx, stmt, args...); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := finish(true); err != nil {
			t.Fatal(err)
		}
	}
}

// holdPortalRow is an open transaction holding the portal's row as a create's insert holds it (FOR SHARE) or as
// a revoke's UPDATE does, released by the returned commit.
func holdPortalRow(t fixtureT, pool *pgxpool.Pool, portal uuid.UUID, revoke bool) (release func()) {
	t.Helper()
	q := `SELECT 1 FROM delegates WHERE id = $1 FOR SHARE`
	if revoke {
		q = `UPDATE delegates SET revoked_at = now() WHERE id = $1`
	}
	return holdTx(t, pool, q, portal)
}

// parked runs f on its own goroutine and reports whether it is still blocked after a pause. result returns f's answer
// exactly once whichever way it finished: an answer that arrived inside the pause is kept, and a call still blocked is
// waited for at most 30 seconds.
func parked(f func() error) (blocked bool, result func() error) {
	return parkedFor(400*time.Millisecond, f)
}

// parkedFor is parked with the pause given: a call that must complete while a lock is held elsewhere is given a
// deadline generous enough for a healthy write on a loaded runner.
func parkedFor(pause time.Duration, f func() error) (blocked bool, result func() error) {
	ch := make(chan error, 1)
	go func() { ch <- f() }()
	select {
	case err := <-ch:
		return false, func() error { return err }
	case <-time.After(pause):
		return true, func() error {
			select {
			case err := <-ch:
				return err
			case <-time.After(30 * time.Second):
				return errors.New("the parked call did not finish within 30 seconds of its release")
			}
		}
	}
}

// onAnotherGoroutine runs f where the caller's held connection is not: the pool guard fails a goroutine that asks for a
// second connection while it holds one, and the tests below hold a transaction on the test's own goroutine.
func onAnotherGoroutine(t *testing.T, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("an observation on another goroutine did not finish within 30 seconds")
	}
}

// observeCtx bounds one observation query, so a hung read cannot outlive the wait that asked for it.
func observeCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 20*time.Second)
}

// backendsWaiting counts the database's backends that are waiting on a lock, read on another goroutine. advisory selects
// the waiters on an advisory lock (the paused insert) or on a row (the revoke); sql is a fragment of the statement.
func backendsWaiting(t *testing.T, pool *pgxpool.Pool, advisory bool, sql string) int {
	t.Helper()
	var n int
	onAnotherGoroutine(t, func() {
		ctx, cancel := observeCtx()
		defer cancel()
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock' AND (wait_event = 'advisory') = $1 AND query ILIKE $2`,
			advisory, "%"+sql+"%").Scan(&n); err != nil {
			t.Errorf("read pg_stat_activity: %v", err)
		}
	})
	return n
}

// waitForPortalLock polls cond for at most 10 seconds.
func waitForPortalLock(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// holdPause is an open transaction holding the advisory lock the pause trigger below waits on, released by the commit.
func holdPause(t fixtureT, pool *pgxpool.Pool) (release func()) {
	t.Helper()
	return holdTx(t, pool, `SELECT pg_advisory_xact_lock(7770042)`)
}

// failedFixtureT is a test that has failed: Fatal stops the goroutine that called it, as testing.T's does, and the
// cleanups it was given are run by the test, in the order testing runs them.
type failedFixtureT struct {
	cleanups []func()
}

// lockedT is failedFixtureT shared by two goroutines, which serialise the cleanups they register.
type lockedT struct {
	*failedFixtureT
	mu *sync.Mutex
}

func (l *lockedT) Cleanup(fn func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failedFixtureT.Cleanup(fn)
}

func (f *failedFixtureT) Helper()           {}
func (f *failedFixtureT) Cleanup(fn func()) { f.cleanups = append(f.cleanups, fn) }
func (f *failedFixtureT) Fatal(...any)      { runtime.Goexit() }
func (f *failedFixtureT) unwind() {
	for i := len(f.cleanups) - 1; i >= 0; i-- {
		f.cleanups[i]()
	}
}

// A test that stops on a failed observation lets go of the locks its fixtures hold: the pause lock and a portal row held
// as a revoke holds it are held, the test fails on its first observation, and once its cleanups have run both locks are
// free, so no later case and no pool close waits on them.
func TestPG_PortalRunFixtures_FailedObservationUnwinds(t *testing.T) {
	st, pool := isolatedStore(t)
	portal := newIssuerPortal(t, st, "").Delegate
	free := func(sql string, args ...any) bool {
		var ok bool
		onAnotherGoroutine(t, func() {
			ctx, cancel := observeCtx()
			defer cancel()
			if err := pool.QueryRow(ctx, sql, args...).Scan(&ok); err != nil {
				t.Errorf("probe: %v", err)
			}
		})
		return ok
	}
	const pauseFree = `SELECT pg_try_advisory_xact_lock(7770042)`
	const rowFree = `SELECT EXISTS (SELECT 1 FROM delegates WHERE id = $1 FOR UPDATE SKIP LOCKED)`
	// One goroutine per fixture: the pool guard fails a goroutine that holds a connection and asks for another.
	ft := &failedFixtureT{}
	var mu sync.Mutex
	locked := func(f func(fixtureT)) <-chan struct{} {
		done := make(chan struct{})
		go func() {
			defer close(done)
			f(&lockedT{failedFixtureT: ft, mu: &mu})
			ft.Fatal("the observation failed")
		}()
		return done
	}
	held := []<-chan struct{}{
		locked(func(f fixtureT) { holdPause(f, pool) }),
		locked(func(f fixtureT) { holdPortalRow(f, pool, portal.ID, true) }),
	}
	for _, done := range held {
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("a failed test's goroutine did not stop within 30 seconds")
		}
	}
	// The goroutines have stopped but their cleanups have not run: both locks are still held here.
	if free(pauseFree) || free(rowFree, portal.ID) {
		t.Fatal("a lock the fixtures should hold was free before the cleanups ran")
	}
	ft.unwind()
	if !free(pauseFree) || !free(rowFree, portal.ID) {
		t.Error("a lock a failed test's fixture held was still held after its cleanups ran")
	}
}

// The two commit orders of a portal run's insert against the portal's revoke, on every create path, each with a real
// insert. A revoke in flight holds the insert behind it, and the insert is then refused with ErrPortalRevoked and leaves
// no portal run. A real insert is stopped after it has taken its lock on the portal's row (a trigger that waits on an
// advisory lock the test holds), a revoke is started and seen waiting on a lock, and when the insert is let go it commits
// first: the run exists and the revoke follows it. An ordinary run, in a case of its own with its own pool and cap, is
// created while a revoke is in flight.
func TestPortalRun_InsertOrderedAgainstRevoke_PG_Orders(t *testing.T) {
	st, pool := isolatedStore(t)
	// Every writer and the DDL below run on a deadline, so a lock that is never let go ends them with an error.
	ctx, cancelCtx := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancelCtx()
	for _, ddl := range []string{
		`CREATE FUNCTION zz_pause_portal_insert() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(7770042); RETURN NEW; END $$`,
		`CREATE TRIGGER zz_pause_portal_insert BEFORE INSERT ON agent_runs FOR EACH ROW WHEN (NEW.portal_drives) EXECUTE FUNCTION zz_pause_portal_insert()`,
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("install the insert pause: %v", err)
		}
	}
	creators := func(portal types.Delegate, qpool types.RunnerPool, portalDrives bool) map[string]func() (types.AgentRun, error) {
		mk := func() types.AgentRun {
			r := newRun(types.RunRunning)
			r.CreatedVia, r.PortalDrives = &portal.ID, portalDrives
			return r
		}
		return map[string]func() (types.AgentRun, error){
			"CreateRun":         func() (types.AgentRun, error) { return st.CreateRun(ctx, mk()) },
			"CreateRunUnderCap": func() (types.AgentRun, error) { return st.CreateRunUnderCap(ctx, mk(), 100) },
			"CreateQueuedRun": func() (types.AgentRun, error) {
				now := time.Now().UTC()
				q := poolRun(qpool, types.RunQueued, "alice")
				q.CreatedVia, q.PortalDrives = &portal.ID, portalDrives
				return st.CreateQueuedRun(ctx, q, store.RunQueueEntry{QueuedAt: now, DeadlineAt: now.Add(time.Hour)}, 0)
			},
		}
	}
	portalRuns := func(t *testing.T, portal types.Delegate) int {
		t.Helper()
		var n int
		onAnotherGoroutine(t, func() {
			qctx, cancel := observeCtx()
			defer cancel()
			if err := pool.QueryRow(qctx, `SELECT count(*) FROM agent_runs WHERE created_via = $1 AND portal_drives`, portal.ID).Scan(&n); err != nil {
				t.Errorf("count portal runs: %v", err)
			}
		})
		return n
	}
	for _, path := range []string{"CreateRun", "CreateRunUnderCap", "CreateQueuedRun"} {
		t.Run(path+" behind a revoke in flight", func(t *testing.T) {
			portal := newIssuerPortal(t, st, "").Delegate
			create := creators(portal, cappedPool(t, st, 1000), true)[path]
			release := holdPortalRow(t, pool, portal.ID, true)
			blocked, result := parked(func() error { _, err := create(); return err })
			if !blocked {
				release()
				t.Fatal("the insert did not wait for the revoke in flight")
			}
			release()
			if err := result(); !errors.Is(err, store.ErrPortalRevoked) {
				t.Errorf("insert after the revoke committed: err = %v, want ErrPortalRevoked", err)
			}
			if n := portalRuns(t, portal); n != 0 {
				t.Errorf("%d portal run row(s) for a refused insert, want none", n)
			}
		})
		t.Run(path+" ahead of a revoke", func(t *testing.T) {
			portal := newIssuerPortal(t, st, "").Delegate
			create := creators(portal, cappedPool(t, st, 1000), true)[path]
			release := holdPause(t, pool)
			insertBlocked, insertResult := parked(func() error { _, err := create(); return err })
			if !insertBlocked {
				release()
				t.Fatal("the insert did not stop at the pause")
			}
			waitForPortalLock(t, "the insert to wait on the advisory lock", func() bool { return backendsWaiting(t, pool, true, "agent_runs") > 0 })
			revokeBlocked, revokeResult := parked(func() error { _, _, err := st.RevokeDelegate(ctx, portal.ID, time.Now().UTC(), false); return err })
			if !revokeBlocked {
				release()
				t.Fatal("the revoke did not wait for the insert in flight")
			}
			waitForPortalLock(t, "the revoke to wait on the portal's row", func() bool { return backendsWaiting(t, pool, false, "delegates") > 0 })
			if n := portalRuns(t, portal); n != 0 {
				t.Errorf("%d portal run row(s) visible before the insert committed, want none", n)
			}
			release()
			if err := insertResult(); err != nil {
				t.Errorf("the insert that took its lock before the revoke: %v, want it created", err)
			}
			if err := revokeResult(); err != nil {
				t.Errorf("the revoke behind the insert: %v", err)
			}
			if n := portalRuns(t, portal); n != 1 {
				t.Errorf("%d portal run row(s) after both finished, want the one the insert committed", n)
			}
			if _, err := create(); !errors.Is(err, store.ErrPortalRevoked) {
				t.Errorf("a later insert: err = %v, want ErrPortalRevoked", err)
			}
		})
		t.Run(path+" ordinary run during a revoke in flight", func(t *testing.T) {
			portal := newIssuerPortal(t, st, "").Delegate
			create := creators(portal, cappedPool(t, st, 1000), false)[path]
			release := holdPortalRow(t, pool, portal.ID, true)
			// A healthy insert takes well under a second, but a loaded runner can take longer: it is given 30 seconds to
			// finish while the revoke is held, and only an insert that has not finished by then waited for it.
			blocked, result := parkedFor(30*time.Second, func() error { _, err := create(); return err })
			if blocked {
				release()
				_ = result()
				t.Fatal("an ordinary run waited for the portal's revoke")
			}
			if err := result(); err != nil {
				t.Errorf("an ordinary run during the revoke: %v", err)
			}
			release()
		})
	}
}

// Erasing a person never touches a team drive: the workforce purge's user-subject delete leaves every drive row
// and every allocation (a portal's, a group's and the person's own user-tier row) and the portal's resolution as it
// was.
func TestPortalTeamDrive_PersonPurgeLeavesDrive_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()
	portal := newIssuerPortal(t, st, "").Delegate
	team := seedTeamDrive(t, st, "team-"+uuid.NewString()[:8])
	addUserDriveGrant(t, st, portalGrant(portal, team))
	addUserDriveGrant(t, st, types.UserDriveGrant{SubjectType: types.CapabilitySubjectGroup, Subject: "platform",
		DriveID: team.ID, Enabled: true, CreatedBy: "admin@example.com"})
	person := types.PortalPrincipal(newIssuerPortal(t, st, "").Key, "alice")
	own := seedUserDrive(t, st, "own-"+uuid.NewString()[:8])
	addUserDriveGrant(t, st, types.UserDriveGrant{SubjectType: types.CapabilitySubjectUser, Subject: person,
		DriveID: own.ID, Enabled: true, CreatedBy: "admin@example.com"})

	drives := rowCount(t, pool, `SELECT count(*) FROM user_drives`)
	grants := rowCount(t, pool, `SELECT count(*) FROM user_drive_grants`)
	if _, err := st.DeleteUserSubjectRows(ctx, uuid.New(), []string{person}, nil); err != nil {
		t.Fatalf("purge the person's user-subject rows: %v", err)
	}
	if got := rowCount(t, pool, `SELECT count(*) FROM user_drives`); got != drives {
		t.Errorf("user_drives rows %d -> %d, want none touched", drives, got)
	}
	if got := rowCount(t, pool, `SELECT count(*) FROM user_drive_grants`); got != grants {
		t.Errorf("user_drive_grants rows %d -> %d, want none touched", grants, got)
	}
	if ws := resolvePortal(t, st, portal.ID); len(ws) != 1 || ws[0].Drive.ID != team.ID {
		t.Errorf("the portal resolves %+v after the purge, want its drive", ws)
	}
}
