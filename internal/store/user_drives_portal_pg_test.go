// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
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
		blocked, done := parked(func() error { _, err := st.UpsertUserDrive(ctx, edit, false); return err })
		if !blocked {
			_ = tx.Commit(ctx)
			t.Fatal("the edit did not wait for the allocation in flight")
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-done; !errors.Is(err, store.ErrDrivePortalSecondPlace) {
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
		blocked, done := parked(func() error { _, err := st.AddUserDriveGrant(ctx, portalGrant(portal, team), true); return err })
		if !blocked {
			_ = tx.Commit(ctx)
			t.Fatal("the allocation did not wait for the edit in flight")
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-done; !errors.Is(err, store.ErrDrivePortalSecondPlace) {
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

// holdPortalRow is an open transaction holding the portal's row as a create's insert holds it (FOR SHARE) or as
// a revoke's UPDATE does, released by the returned commit.
func holdPortalRow(t *testing.T, pool *pgxpool.Pool, portal uuid.UUID, revoke bool) (release func()) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	q := `SELECT 1 FROM delegates WHERE id = $1 FOR SHARE`
	if revoke {
		q = `UPDATE delegates SET revoked_at = now() WHERE id = $1`
	}
	if _, err := tx.Exec(ctx, q, portal); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	return func() {
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

// parked reports whether f is still blocked after a pause, and the channel its result arrives on.
func parked(f func() error) (blocked bool, done <-chan error) {
	ch := make(chan error, 1)
	go func() { ch <- f() }()
	select {
	case <-ch:
		return false, ch
	case <-time.After(400 * time.Millisecond):
		return true, ch
	}
}

// The two commit orders of a portal run's insert against the portal's revoke, on every create path. A revoke
// that is in flight holds the insert behind it, and the insert is then refused with ErrPortalRevoked and leaves no
// row; an insert in flight holds the revoke behind it, so the run exists before the revoke commits.
func TestPortalRun_InsertOrderedAgainstRevoke_PG_Orders(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()
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
	qpool := cappedPool(t, st, 1000)
	for _, path := range []string{"CreateRun", "CreateRunUnderCap", "CreateQueuedRun"} {
		t.Run(path+" behind a revoke in flight", func(t *testing.T) {
			portal := newIssuerPortal(t, st, "").Delegate
			release := holdPortalRow(t, pool, portal.ID, true)
			blocked, done := parked(func() error { _, err := creators(portal, qpool, true)[path](); return err })
			if !blocked {
				release()
				t.Fatal("the insert did not wait for the revoke in flight")
			}
			// An ordinary run of the same person is not behind the fence: it is created while the revoke is still open.
			if ordinaryBlocked, ordinaryDone := parked(func() error { _, err := creators(portal, qpool, false)[path](); return err }); ordinaryBlocked {
				release()
				t.Fatal("an ordinary run waited for the portal's revoke")
			} else if err := <-ordinaryDone; err != nil {
				release()
				t.Fatalf("an ordinary run during the revoke: %v", err)
			}
			release()
			if err := <-done; !errors.Is(err, store.ErrPortalRevoked) {
				t.Errorf("insert after the revoke committed: err = %v, want ErrPortalRevoked", err)
			}
			if n := rowCount(t, pool, `SELECT count(*) FROM agent_runs WHERE created_via = $1`, portal.ID); n != 0 {
				t.Errorf("%d run row(s) for a refused insert, want none", n)
			}
		})
		t.Run(path+" ahead of a revoke", func(t *testing.T) {
			portal := newIssuerPortal(t, st, "").Delegate
			release := holdPortalRow(t, pool, portal.ID, false) // the insert's own hold on the portal row
			blocked, done := parked(func() error { _, _, err := st.RevokeDelegate(ctx, portal.ID, time.Now().UTC(), false); return err })
			if !blocked {
				release()
				t.Fatal("the revoke did not wait for the insert in flight")
			}
			release()
			if err := <-done; err != nil {
				t.Errorf("revoke after the insert committed: %v", err)
			}
			if _, err := creators(portal, qpool, true)[path](); !errors.Is(err, store.ErrPortalRevoked) {
				t.Errorf("a later insert: err = %v, want ErrPortalRevoked", err)
			}
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
