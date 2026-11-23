// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func orgSitePool(t *testing.T, pg store.PG, name string) types.RunnerPool {
	t.Helper()
	p, err := pg.CreateRunnerPool(context.Background(), types.RunnerPool{ID: uuid.New(), Name: name, Scope: types.RunnerPoolOrganization}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func orgSite(t *testing.T, pg store.PG, name string) types.OrgSite {
	t.Helper()
	s, err := pg.CreateOrgSite(context.Background(), types.OrgSite{ID: uuid.New(), Name: name, StorageLabel: "NAS"})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func adminDrive(t *testing.T, pg store.PG, name string) types.UserDrive {
	t.Helper()
	d, err := pg.UpsertUserDrive(context.Background(), types.UserDrive{Name: name, Backend: types.DriveBackendDockerVolume,
		HomeTemplate: types.HomeTemplateHash, Reclaim: types.DriveReclaimRetain, CreatedBy: "admin@example.com"}, false)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// Only an organization pool can be at a site: a user pool is refused by scope, nothing is written and its
// revision does not move.
func TestPG_OrgSites_UserPoolRefused(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	ctx := context.Background()
	site := orgSite(t, pg, "Office")
	mine := ownPool(t, pg, "sub-a", "Mine")
	if _, _, err := pg.AssignPoolSite(ctx, mine.ID, site.ID); !errors.Is(err, store.ErrRunnerPoolScope) {
		t.Fatalf("a user pool at a site = %v, want ErrRunnerPoolScope", err)
	}
	if _, _, err := pg.ClearPoolSite(ctx, mine.ID); !errors.Is(err, store.ErrRunnerPoolScope) {
		t.Errorf("clearing a user pool's site = %v, want ErrRunnerPoolScope", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM org_site_pools`).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d site pools (%v)", n, err)
	}
	if p, _ := pg.GetRunnerPool(ctx, mine.ID); p.Revision != mine.Revision {
		t.Errorf("the refused assign moved the revision from %d to %d", mine.Revision, p.Revision)
	}
	if _, _, err := pg.AssignPoolSite(ctx, uuid.New(), site.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("an unknown pool = %v, want ErrNotFound", err)
	}
	org := orgSitePool(t, pg, "Builds")
	if _, _, err := pg.AssignPoolSite(ctx, org.ID, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("an unknown site = %v, want ErrNotFound", err)
	}
	if p, _ := pg.GetRunnerPool(ctx, org.ID); p.Revision != org.Revision {
		t.Errorf("the refused assign moved the revision from %d to %d", org.Revision, p.Revision)
	}
}

// Moving a pool between sites raises its revision in the same transaction, so a run admitted against the pool
// before the move fails the run insert's fence and is never recorded at the old site. Taking the pool off its
// site fences the same way; an assign that changes nothing does not.
func TestPG_OrgSites_PoolMoveFencesAdmittedRun(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	ctx := context.Background()
	office, edge := orgSite(t, pg, "Office"), orgSite(t, pg, "Edge")
	p := orgSitePool(t, pg, "Office cluster")
	from, rev, err := pg.AssignPoolSite(ctx, p.ID, office.ID)
	if err != nil || from != nil || rev != p.Revision+1 {
		t.Fatalf("assign = from %v, revision %d, %v; want no site before and revision %d", from, rev, err, p.Revision+1)
	}
	if _, again, err := pg.AssignPoolSite(ctx, p.ID, office.ID); err != nil || again != rev {
		t.Fatalf("a repeated assign = revision %d, %v; want %d unchanged", again, err, rev)
	}
	admitted := poolRun(types.RunnerPool{ID: p.ID, Revision: rev}, types.RunPending, "alice")
	admitted.SiteID = &office.ID
	from, moved, err := pg.AssignPoolSite(ctx, p.ID, edge.ID)
	if err != nil || from == nil || *from != office.ID || moved != rev+1 {
		t.Fatalf("move = from %v, revision %d, %v; want from Office at revision %d", from, moved, err, rev+1)
	}
	if _, err := pg.CreateRun(ctx, admitted); !errors.Is(err, store.ErrRunnerPoolStale) {
		t.Fatalf("a run admitted before the move = %v, want ErrRunnerPoolStale", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_runs WHERE id = $1`, admitted.ID).Scan(&n); err != nil || n != 0 {
		t.Errorf("the fenced run left %d rows (%v)", n, err)
	}
	if site, ok, err := pg.PoolSite(ctx, p.ID); err != nil || !ok || site.ID != edge.ID || site.Name != "Edge" {
		t.Errorf("PoolSite after the move = %+v, %v, %v; want Edge", site, ok, err)
	}

	beforeClear := poolRun(types.RunnerPool{ID: p.ID, Revision: moved}, types.RunPending, "alice")
	beforeClear.SiteID = &edge.ID
	from, cleared, err := pg.ClearPoolSite(ctx, p.ID)
	if err != nil || from == nil || *from != edge.ID || cleared != moved+1 {
		t.Fatalf("clear = from %v, revision %d, %v; want from Edge at revision %d", from, cleared, err, moved+1)
	}
	if _, err := pg.CreateRun(ctx, beforeClear); !errors.Is(err, store.ErrRunnerPoolStale) {
		t.Errorf("a run admitted before the clear = %v, want ErrRunnerPoolStale", err)
	}
	if _, ok, err := pg.PoolSite(ctx, p.ID); err != nil || ok {
		t.Errorf("PoolSite after the clear = %v, %v; want none", ok, err)
	}
	if from, again, err := pg.ClearPoolSite(ctx, p.ID); err != nil || from != nil || again != cleared {
		t.Errorf("a second clear = from %v, revision %d, %v; want nothing and %d", from, again, err, cleared)
	}
}

// A site is a tombstone: delete is refused while a pool, live or deleted, or a drive is assigned to it, and once
// it is empty the delete keeps the row, clears its default and frees its name.
func TestPG_OrgSites_DeleteRefusedWhileReferenced(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	ctx := context.Background()
	site := orgSite(t, pg, "Office")
	p := orgSitePool(t, pg, "Office cluster")
	if _, _, err := pg.AssignPoolSite(ctx, p.ID, site.ID); err != nil {
		t.Fatal(err)
	}
	cur, _ := pg.GetOrgSite(ctx, site.ID)
	if _, err := pg.UpdateOrgSite(ctx, site.ID, cur.Revision, cur.Name, cur.StorageLabel, cur.StorageURL, &p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.DeleteOrgSite(ctx, site.ID); !errors.Is(err, store.ErrOrgSiteInUse) {
		t.Fatalf("delete with a pool = %v, want ErrOrgSiteInUse", err)
	}
	if _, err := pg.DeleteRunnerPool(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := pg.AssignPoolSite(ctx, p.ID, site.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("assigning a deleted pool = %v, want ErrNotFound", err)
	}
	if _, err := pg.DeleteOrgSite(ctx, site.ID); !errors.Is(err, store.ErrOrgSiteInUse) {
		t.Fatalf("delete with a deleted pool still assigned = %v, want ErrOrgSiteInUse", err)
	}
	if _, _, err := pg.ClearPoolSite(ctx, p.ID); err != nil {
		t.Fatalf("clearing a deleted pool's site = %v", err)
	}
	drive := adminDrive(t, pg, "designs-"+uuid.NewString()[:8])
	if _, err := pg.SetDriveSite(ctx, drive.ID, site.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.DeleteOrgSite(ctx, site.ID); !errors.Is(err, store.ErrOrgSiteInUse) {
		t.Fatalf("delete with a drive = %v, want ErrOrgSiteInUse", err)
	}
	if from, err := pg.ClearDriveSite(ctx, drive.ID); err != nil || from == nil || *from != site.ID {
		t.Fatalf("clear drive = %v, %v", from, err)
	}
	gone, err := pg.DeleteOrgSite(ctx, site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gone.State != types.OrgSiteDeleted || gone.DefaultPoolID != nil || gone.Revision <= cur.Revision {
		t.Errorf("deleted site = %+v; want a deleted row with no default and a raised revision", gone)
	}
	if _, err := pg.DeleteOrgSite(ctx, site.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a second delete = %v, want ErrNotFound", err)
	}
	if _, err := pg.SetDriveSite(ctx, drive.ID, site.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("homing a drive at a deleted site = %v, want ErrNotFound", err)
	}
	live := orgSitePool(t, pg, "Other cluster")
	if _, _, err := pg.AssignPoolSite(ctx, live.ID, site.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("assigning a pool to a deleted site = %v, want ErrNotFound", err)
	}
	if list, err := pg.ListOrgSites(ctx); err != nil || len(list) != 0 {
		t.Errorf("live sites after the delete = %v, %v", list, err)
	}
	if _, err := pg.CreateOrgSite(ctx, types.OrgSite{ID: uuid.New(), Name: "office"}); err != nil {
		t.Errorf("a new site under the deleted one's name = %v", err)
	}
	if _, err := pg.CreateOrgSite(ctx, types.OrgSite{ID: uuid.New(), Name: "OFFICE"}); !errors.Is(err, store.ErrConflict) {
		t.Errorf("a second live site of the same name = %v, want ErrConflict", err)
	}
}

// A run placed at a site keeps its site after the site is deleted: the row stays as a tombstone.
func TestPG_OrgSites_TombstoneKeepsRunSite(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	ctx := context.Background()
	site := orgSite(t, pg, "Office")
	p := orgSitePool(t, pg, "Office cluster")
	_, rev, err := pg.AssignPoolSite(ctx, p.ID, site.ID)
	if err != nil {
		t.Fatal(err)
	}
	r := poolRun(types.RunnerPool{ID: p.ID, Revision: rev}, types.RunPending, "alice")
	r.SiteID = &site.ID
	created, err := pg.CreateRun(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if created.SiteID == nil || *created.SiteID != site.ID {
		t.Fatalf("created run's site = %v, want %s", created.SiteID, site.ID)
	}
	if _, _, err := pg.ClearPoolSite(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.DeleteOrgSite(ctx, site.ID); err != nil {
		t.Fatal(err)
	}
	got, err := pg.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SiteID == nil || *got.SiteID != site.ID {
		t.Errorf("the run's site after the site was deleted = %v, want %s", got.SiteID, site.ID)
	}
	if s, err := pg.GetOrgSite(ctx, site.ID); err != nil || s.State != types.OrgSiteDeleted || s.Name != "Office" {
		t.Errorf("the deleted site's row = %+v, %v", s, err)
	}
	plain, err := pg.CreateRun(ctx, newRun(types.RunPending))
	if err != nil || plain.SiteID != nil {
		t.Errorf("a run with no site = %v, %v; want a nil site", plain.SiteID, err)
	}
}

// site_default is a selection the run row admits, and it reads back with the run's site.
func TestPG_RunSelectionSiteDefaultStored(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	ctx := context.Background()
	site := orgSite(t, pg, "Office")
	p := orgSitePool(t, pg, "Office cluster")
	_, rev, err := pg.AssignPoolSite(ctx, p.ID, site.ID)
	if err != nil {
		t.Fatal(err)
	}
	r := poolRun(types.RunnerPool{ID: p.ID, Revision: rev}, types.RunPending, "alice")
	r.RunnerPoolSelection, r.SiteID = types.RunnerPoolSelectedSite, &site.ID
	if _, err := pg.CreateRun(ctx, r); err != nil {
		t.Fatalf("a site_default run = %v", err)
	}
	got, err := pg.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RunnerPoolSelection != types.RunnerPoolSelectedSite || got.SiteID == nil || *got.SiteID != site.ID {
		t.Errorf("read back selection %q, site %v; want site_default at %s", got.RunnerPoolSelection, got.SiteID, site.ID)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET runner_pool_selection = 'site' WHERE id = $1`, r.ID); err == nil {
		t.Error("the run row admitted a selection outside the four")
	}
}

// A site's default is one of its own live pools, and it goes when its pool leaves the site.
func TestPG_OrgSites_DefaultPoolIsTheSitesOwn(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	ctx := context.Background()
	office, edge := orgSite(t, pg, "Office"), orgSite(t, pg, "Edge")
	p := orgSitePool(t, pg, "Office cluster")
	if _, err := pg.CreateOrgSite(ctx, types.OrgSite{ID: uuid.New(), Name: "New", DefaultPoolID: &p.ID}); !errors.Is(err, store.ErrOrgSiteDefaultPool) {
		t.Errorf("a new site with a default = %v, want ErrOrgSiteDefaultPool", err)
	}
	if _, err := pg.UpdateOrgSite(ctx, office.ID, office.Revision, "Office", "", "", &p.ID); !errors.Is(err, store.ErrOrgSiteDefaultPool) {
		t.Fatalf("a default not at the site = %v, want ErrOrgSiteDefaultPool", err)
	}
	if _, _, err := pg.AssignPoolSite(ctx, p.ID, office.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.UpdateOrgSite(ctx, edge.ID, edge.Revision, "Edge", "", "", &p.ID); !errors.Is(err, store.ErrOrgSiteDefaultPool) {
		t.Errorf("another site's pool as default = %v, want ErrOrgSiteDefaultPool", err)
	}
	set, err := pg.UpdateOrgSite(ctx, office.ID, office.Revision, "Office", "Office NAS", "https://nas.office.example/share", &p.ID)
	if err != nil || set.DefaultPoolID == nil || *set.DefaultPoolID != p.ID || set.Revision != office.Revision+1 || len(set.PoolIDs) != 1 {
		t.Fatalf("set default = %+v, %v", set, err)
	}
	if _, err := pg.UpdateOrgSite(ctx, edge.ID, edge.Revision, "OFFICE", "", "", nil); !errors.Is(err, store.ErrConflict) {
		t.Errorf("renaming a site to another live site's name = %v, want ErrConflict", err)
	}
	if _, err := pg.UpdateOrgSite(ctx, office.ID, office.Revision, "Office", "", "", nil); !errors.Is(err, store.ErrOrgSiteStale) {
		t.Errorf("an update at an old revision = %v, want ErrOrgSiteStale", err)
	}
	if same, err := pg.UpdateOrgSite(ctx, office.ID, set.Revision, set.Name, set.StorageLabel, set.StorageURL, set.DefaultPoolID); err != nil || same.Revision != set.Revision {
		t.Errorf("an update that changes nothing = revision %d, %v; want %d", same.Revision, err, set.Revision)
	}
	if _, _, err := pg.AssignPoolSite(ctx, p.ID, edge.ID); err != nil {
		t.Fatal(err)
	}
	after, err := pg.GetOrgSite(ctx, office.ID)
	if err != nil || after.DefaultPoolID != nil || after.Revision != set.Revision+1 || len(after.PoolIDs) != 0 {
		t.Errorf("Office after its default pool moved = %+v, %v; want no default and a raised revision", after, err)
	}
}

// Only an admin drive has a home site, and DriveHomeSites reads homes at live sites only.
func TestPG_OrgSites_DriveHomesAdminDrivesOnly(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	ctx := context.Background()
	office, edge := orgSite(t, pg, "Office"), orgSite(t, pg, "Edge")
	member, err := pg.CreateMemberDrive(ctx, types.UserDrive{Name: "Notes", Backend: types.DriveBackendDockerVolume, OwnerSubject: "alice", CreatedBy: "alice"}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pg.SetDriveSite(ctx, member.ID, office.ID); !errors.Is(err, store.ErrOrgSiteMemberDrive) {
		t.Errorf("a member drive's home = %v, want ErrOrgSiteMemberDrive", err)
	}
	if _, err := pg.SetDriveSite(ctx, uuid.New(), office.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("an unknown drive = %v, want ErrNotFound", err)
	}
	designs, other := adminDrive(t, pg, "designs-"+uuid.NewString()[:8]), adminDrive(t, pg, "other-"+uuid.NewString()[:8])
	if from, err := pg.SetDriveSite(ctx, designs.ID, office.ID); err != nil || from != nil {
		t.Fatalf("home designs = %v, %v", from, err)
	}
	if from, err := pg.SetDriveSite(ctx, designs.ID, edge.ID); err != nil || from == nil || *from != office.ID {
		t.Fatalf("move designs = %v, %v; want from Office", from, err)
	}
	homes, err := pg.DriveHomeSites(ctx, []uuid.UUID{designs.ID, other.ID, member.ID})
	if err != nil || len(homes) != 1 || homes[designs.ID] != edge.ID {
		t.Errorf("homes = %v, %v; want designs at Edge only", homes, err)
	}
	if empty, err := pg.DriveHomeSites(ctx, nil); err != nil || len(empty) != 0 {
		t.Errorf("homes of no drives = %v, %v", empty, err)
	}
	list, err := pg.ListOrgSiteDrives(ctx)
	if err != nil || len(list) != 1 || list[0].DriveID != designs.ID || list[0].SiteName != "Edge" || list[0].DriveName != designs.Name {
		t.Errorf("site drives = %+v, %v", list, err)
	}
	// A home at a site that is no longer live is not read: the site cannot be deleted while homed, so the
	// tombstone is written directly here.
	if _, err := pool.Exec(ctx, `UPDATE org_sites SET state = 'deleted' WHERE id = $1`, edge.ID); err != nil {
		t.Fatal(err)
	}
	if homes, err := pg.DriveHomeSites(ctx, []uuid.UUID{designs.ID}); err != nil || len(homes) != 0 {
		t.Errorf("homes at a deleted site = %v, %v; want none", homes, err)
	}
	if err := pg.DeleteUserDrive(ctx, other.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.SetDriveSite(ctx, other.ID, office.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a deleted drive = %v, want ErrNotFound", err)
	}
}
