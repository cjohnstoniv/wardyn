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

// A host path drive is refused, a second place is refused, and a drive edit that adds either to a drive a portal
// holds is refused with the sentinel, leaving the drive as it was. A drive no portal holds edits as before.
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

	two := seedTeamDrive(t, st, "two-"+uuid.NewString()[:8])
	free := two
	free.AltBackend, free.AltHostRoot = types.DriveBackendHostPath, "/srv/alt"
	if _, err := st.UpsertUserDrive(ctx, free, false); err != nil {
		t.Fatalf("a second place on a drive no portal holds: %v", err)
	}
	if _, err := st.AddUserDriveGrant(ctx, portalGrant(portal, two), true); !errors.Is(err, store.ErrDrivePortalSecondPlace) {
		t.Errorf("allocate a drive with a second place: err = %v, want ErrDrivePortalSecondPlace", err)
	}

	held := seedTeamDrive(t, st, "held-"+uuid.NewString()[:8])
	addUserDriveGrant(t, st, portalGrant(portal, held))
	withAlt := held
	withAlt.AltBackend, withAlt.AltHostRoot = types.DriveBackendHostPath, "/srv/alt"
	if _, err := st.UpsertUserDrive(ctx, withAlt, false); !errors.Is(err, store.ErrDrivePortalSecondPlace) {
		t.Errorf("second place on a held drive: err = %v, want ErrDrivePortalSecondPlace", err)
	}
	moved := held
	moved.Backend, moved.HostRoot = types.DriveBackendHostPath, "/srv/moved"
	if _, err := st.UpsertUserDrive(ctx, moved, false); !errors.Is(err, store.ErrDrivePortalBackend) {
		t.Errorf("backend move of a held drive: err = %v, want ErrDrivePortalBackend", err)
	}
	if got, err := st.GetUserDrive(ctx, held.ID); err != nil || got.Backend != types.DriveBackendDockerVolume || got.AltBackend != "" {
		t.Errorf("held drive after the refused edits = %+v, %v; want unchanged", got, err)
	}
	renamed := held
	renamed.Name = "renamed-" + uuid.NewString()[:8]
	if _, err := st.UpsertUserDrive(ctx, renamed, false); err != nil {
		t.Errorf("a plain edit of a held drive: %v", err)
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
func TestPG_PortalDriveAllocation_RacesRevoke(t *testing.T) {
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

// A drive edit that adds a second place races an allocation: either the edit is refused, or the allocation is,
// never both written.
func TestPG_PortalDriveAllocation_RacesDriveEdit(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for i := 0; i < 20; i++ {
		portal := newIssuerPortal(t, st, "").Delegate
		team := seedTeamDrive(t, st, "race-"+uuid.NewString()[:8])
		edit := team
		edit.AltBackend, edit.AltHostRoot = types.DriveBackendHostPath, "/srv/alt"
		var wg sync.WaitGroup
		var addErr, editErr error
		wg.Add(2)
		go func() { defer wg.Done(); _, addErr = st.AddUserDriveGrant(ctx, portalGrant(portal, team), true) }()
		go func() { defer wg.Done(); _, editErr = st.UpsertUserDrive(ctx, edit, false) }()
		wg.Wait()
		if addErr == nil && editErr == nil {
			t.Fatalf("round %d: the allocation and the second place were both written", i)
		}
		if n := rowCount(t, pool, `SELECT count(*) FROM user_drive_grants g JOIN user_drives d ON d.id = g.drive_id
			WHERE g.subject_type = 'portal' AND d.alt_backend IS NOT NULL`); n != 0 {
			t.Fatalf("round %d: %d portal allocations on a drive with a second place", i, n)
		}
	}
}

// A run with portal drives is inserted under the portal's row lock: after the portal is revoked every create
// path refuses it, and a run without portal drives is created as before.
func TestPG_PortalRun_InsertOrderedAgainstRevoke(t *testing.T) {
	st, _ := isolatedStore(t)
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
		if _, err := f(mk(false)); errors.Is(err, store.ErrPortalRevoked) {
			t.Errorf("%s after the revoke without portal drives was refused as revoked: %v", name, err)
		}
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

// Allocations to groups, users and portals written at once on one drive all finish: the lock order is the same
// for every subject, so no pair deadlocks, and every row lands.
func TestPG_PortalTeamDrive_ConcurrentTierWritesOneDriveNoDeadlock(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	team := seedTeamDrive(t, st, "team-"+uuid.NewString()[:8])
	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, 2*n)
	for i := 0; i < n; i++ {
		portal := newIssuerPortal(t, st, "").Delegate
		group := types.UserDriveGrant{SubjectType: types.CapabilitySubjectGroup, Subject: "grp-" + uuid.NewString()[:8],
			DriveID: team.ID, Enabled: true, CreatedBy: "admin@example.com"}
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, err := st.AddUserDriveGrant(ctx, portalGrant(portal, team), true)
			errs <- err
		}()
		go func() { defer wg.Done(); _, err := st.AddUserDriveGrant(ctx, group, true); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("a concurrent allocation failed: %v", err)
		}
	}
	if got := rowCount(t, pool, `SELECT count(*) FROM user_drive_grants WHERE drive_id = $1`, team.ID); got != 2*n {
		t.Errorf("%d allocations landed, want %d", got, 2*n)
	}
}
