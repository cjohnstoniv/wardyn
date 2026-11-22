// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const legacyReleasePath = "/api/v1/admin/drives/legacy-docker/release"

// legacyEnv is a Docker deployment over a real database whose audit recorder writes the shared audit table, with
// the super administrator (sub-super), a security administrator and two members from poolEnv.
func newLegacyEnv(t *testing.T) *poolEnv {
	t.Helper()
	e := newPoolEnv(t, false)
	e.srv.cfg.RunnerTarget = "docker"
	e.srv.cfg.Audit = store.Recorder{Pool: e.pool}
	return e
}

// legacyDrive is owner's drive; marked sets legacy_unsettled as migration 0299 did for the rows it found.
func (e *poolEnv) legacyDrive(owner, name string, backend types.DriveBackend, home *uuid.UUID, marked bool) types.UserDrive {
	e.t.Helper()
	d, err := e.pg.CreateMemberDrive(context.Background(), types.UserDrive{Name: name, Backend: backend,
		OwnerSubject: types.CanonicalUserSubject(owner), CreatedBy: owner, HomeRunnerID: home}, 10)
	if err != nil {
		e.t.Fatalf("create drive %s: %v", name, err)
	}
	if marked {
		if _, err := e.pool.Exec(context.Background(), `UPDATE user_drives SET legacy_unsettled = true WHERE id = $1`, d.ID); err != nil {
			e.t.Fatal(err)
		}
	}
	return d
}

func (e *poolEnv) held(d types.UserDrive) bool {
	e.t.Helper()
	var v bool
	if err := e.pool.QueryRow(context.Background(), `SELECT legacy_unsettled FROM user_drives WHERE id = $1`, d.ID).Scan(&v); err != nil {
		e.t.Fatal(err)
	}
	return v
}

// attach is the check a run's create makes of one drive named by id, as the person who owns it.
func (e *poolEnv) attach(srv *Server, owner string, d types.UserDrive) *runRefusal {
	_, f := authorizeDrives(srv, personCtx(owner), drivesRequest(d.ID.String()))
	return f
}

// wantHeldRefusal says the attach was refused as a drive held since the upgrade: 422 drive_not_found with the
// sentence for its backend (drive_refusals.go legacyDriveHeldMsg, run_drives.go driveDenied).
func wantHeldRefusal(t *testing.T, what string, d types.UserDrive, f *runRefusal) {
	t.Helper()
	if f == nil || f.status != http.StatusUnprocessableEntity || f.body.Reason != string(authz.ReasonDriveNotFound) || f.body.Error != legacyDriveHeldMsg(d) {
		t.Errorf("%s: attach = %+v, want 422 drive_not_found %q", what, f, legacyDriveHeldMsg(d))
	}
}

// wantNotHeld says the attach no longer meets the hold (it may meet another check; the hold's sentence is gone).
func wantNotHeld(t *testing.T, what string, d types.UserDrive, f *runRefusal) {
	t.Helper()
	if f != nil && f.body.Error == legacyDriveHeldMsg(d) {
		t.Errorf("%s: attach is still refused as held: %+v", what, f)
	}
}

type releaseAnswer struct {
	Released int         `json:"released"`
	EventID  *uuid.UUID  `json:"event_id"`
	Replayed []uuid.UUID `json:"replayed"`
}

func callRelease(t *testing.T, e *poolEnv, srv *Server, c *http.Cookie) (int, releaseAnswer) {
	t.Helper()
	w := doSSO(t, srv, http.MethodPost, legacyReleasePath, c, "")
	var a releaseAnswer
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &a); err != nil {
			t.Fatalf("decode %s: %v", w.Body, err)
		}
	}
	return w.Code, a
}

// releaseRows is the audit table's rows of the release action, optionally of one id.
func releaseRows(t *testing.T, e *poolEnv, id *uuid.UUID) int {
	t.Helper()
	q, args := `SELECT count(*) FROM audit_events WHERE action = $1`, []any{legacyReleaseAction}
	if id != nil {
		q, args = q+` AND id = $2`, append(args, *id)
	}
	return e.count(q, args...)
}

// Test 27. A legacy Docker volume is held (its attach refused 422 drive_not_found with the Docker sentence). A
// non-operator calling the release route is refused and releases nothing. The super administrator's call clears
// every person's legacy Docker volume, answers {released: n, event_id}, and writes drive.legacy_docker.release
// naming the caller with the count; a second call releases 0 and writes nothing. A new Docker drive and a legacy
// mac_disk row are untouched by it.
func TestLegacyDockerRelease_HoldsReleasesAndTouchesNothingElse_PG(t *testing.T) {
	e := newLegacyEnv(t)
	mac := e.claimedRunner(e.personP)
	mine := e.legacyDrive(e.personP, "Mine", types.DriveBackendDockerVolume, nil, true)
	theirs := e.legacyDrive(e.otherP, "Theirs", types.DriveBackendDockerVolume, nil, true)
	disk := e.legacyDrive(e.personP, "Disk", types.DriveBackendMacDisk, &mac, true)
	fresh := e.legacyDrive(e.personP, "Fresh", types.DriveBackendDockerVolume, nil, false)

	wantHeldRefusal(t, "before the release", mine, e.attach(e.srv, e.personP, mine))
	wantHeldRefusal(t, "before the release", theirs, e.attach(e.srv, e.otherP, theirs))

	// Not an operator: a member and a security administrator are refused, and nothing is released or recorded.
	for name, c := range map[string]*http.Cookie{"member": e.person, "security administrator": e.alice} {
		// requireOperator (http.go:505-516) refuses with ReasonAdminSurface, which the registry (authz/registry.go:166) makes a
		// 403 deny and refuse (refusal.go:56-59) writes as the wire reason.
		if w := doSSO(t, e.srv, http.MethodPost, legacyReleasePath, c, ""); w.Code != http.StatusForbidden || errorReason(w) != string(authz.ReasonAdminSurface) {
			t.Errorf("%s calling the release = %d reason %q, want 403 %s", name, w.Code, errorReason(w), authz.ReasonAdminSurface)
		}
	}
	for _, d := range []types.UserDrive{mine, theirs, disk} {
		if !e.held(d) {
			t.Errorf("a refused caller released %s", d.Name)
		}
	}
	if n := e.count(`SELECT count(*) FROM drive_legacy_releases`) + releaseRows(t, e, nil); n != 0 {
		t.Errorf("a refused caller left %d record or audit rows", n)
	}

	code, first := callRelease(t, e, e.srv, e.super)
	if code != http.StatusOK || first.Released != 2 || first.EventID == nil || len(first.Replayed) != 0 {
		t.Fatalf("the release = %d %+v, want 200 released 2 with an event_id and no replay", code, first)
	}
	for _, d := range []types.UserDrive{mine, theirs} {
		if e.held(d) {
			t.Errorf("the release left %s held", d.Name)
		}
	}
	if !e.held(disk) {
		t.Error("the Docker release cleared a legacy mac_disk row")
	}
	if e.held(fresh) {
		t.Error("the release marked a new Docker drive")
	}
	wantNotHeld(t, "after the release", mine, e.attach(e.srv, e.personP, mine))
	wantHeldRefusal(t, "a Mac disk after the Docker release", disk, e.attach(e.srv, e.personP, disk))

	var actor, actorType, released string
	if err := e.pool.QueryRow(context.Background(), `SELECT actor, actor_type, data->>'released' FROM audit_events WHERE action = $1 AND id = $2`,
		legacyReleaseAction, *first.EventID).Scan(&actor, &actorType, &released); err != nil {
		t.Fatalf("the release's audit row: %v", err)
	}
	if actor != "sub-super" || actorType != string(types.ActorHuman) || released != "2" {
		t.Errorf("audit row = %s/%s released %s, want human/sub-super released 2", actorType, actor, released)
	}
	if n := releaseRows(t, e, nil); n != 1 {
		t.Errorf("audit rows of the release = %d, want 1", n)
	}

	code, second := callRelease(t, e, e.srv, e.super)
	if code != http.StatusOK || second.Released != 0 || second.EventID != nil || len(second.Replayed) != 0 {
		t.Errorf("the second call = %d %+v, want 200 {released: 0}", code, second)
	}
	if n := releaseRows(t, e, nil); n != 1 {
		t.Errorf("audit rows after the second call = %d, want still 1", n)
	}
}

// legacyFailingRecorder is a replica whose audit store is down: every write fails.
type legacyFailingRecorder struct{}

func (legacyFailingRecorder) Record(context.Context, types.AuditEvent) error {
	return errors.New("the audit store is down")
}

// Test 29. The release commits and then the process stops before any audit row reaches the table (the recorder is
// down, or the process is killed between the commit and the delivery): the drives are released and the record is
// pending with its id. A retry of the call on a fresh server over the same database delivers that same id exactly
// once with the admin named and the count, settles it (audited_at set, actor cleared) and answers
// {released: 0, replayed: [id]}; a third call delivers nothing; the audit table holds one row for the id. Run with
// issuer portals off and on, and for both ways of stopping.
func TestLegacyDockerRelease_CommitThenStopIsDeliveredOnceByARetry_PG(t *testing.T) {
	for _, portalsOn := range []bool{false, true} {
		for _, how := range []string{"recorder down", "stopped before delivery"} {
			t.Run(fmt.Sprintf("portals on=%v/%s", portalsOn, how), func(t *testing.T) {
				ctx := context.Background()
				e := newLegacyEnv(t)
				a := e.legacyDrive(e.personP, "A", types.DriveBackendDockerVolume, nil, true)
				b := e.legacyDrive(e.otherP, "B", types.DriveBackendDockerVolume, nil, true)
				if portalsOn {
					if won, err := e.pg.ActivatePortalIssuersAs(ctx, store.PortalActivation{ActorType: string(types.ActorHuman), Actor: "ops@example.com", Sessions: []string{}}); err != nil || !won {
						t.Fatalf("activate portals = %v, %v", won, err)
					}
				}

				var id uuid.UUID
				switch how {
				case "recorder down":
					e.srv.cfg.Audit = legacyFailingRecorder{}
					code, ans := callRelease(t, e, e.srv, e.super)
					if code != http.StatusOK || ans.Released != 2 || ans.EventID == nil {
						t.Fatalf("the release with the recorder down = %d %+v, want 200 released 2 with its event_id", code, ans)
					}
					id = *ans.EventID
				default:
					rec, err := e.pg.ReleaseLegacyDockerDrives(ctx, string(types.ActorHuman), "sub-super")
					if err != nil || rec.Released != 2 {
						t.Fatalf("the release statement = %+v, %v, want 2 released", rec, err)
					}
					id = rec.ID
				}

				// The drives are released, and the record waits with its id and the principal, with no audit row.
				for _, d := range []types.UserDrive{a, b} {
					if e.held(d) {
						t.Errorf("%s is still held after the commit", d.Name)
					}
				}
				var released int
				var actor string
				var audited *time.Time
				var eventAt time.Time
				if err := e.pool.QueryRow(ctx, `SELECT released, actor, audited_at, event_at FROM drive_legacy_releases WHERE id = $1`, id).Scan(&released, &actor, &audited, &eventAt); err != nil {
					t.Fatalf("the pending record: %v", err)
				}
				if released != 2 || actor != "sub-super" || audited != nil {
					t.Fatalf("pending record = released %d actor %q audited %v, want 2, sub-super, not audited", released, actor, audited)
				}
				if n := releaseRows(t, e, nil); n != 0 {
					t.Fatalf("audit rows before the retry = %d, want 0", n)
				}

				// A fresh server over the same database, with the recorder back, answers the retry.
				fresh := e.otherServer(func(c *Config) { c.Audit = store.Recorder{Pool: e.pool}; c.RunnerTarget = "docker" })
				code, retry := callRelease(t, e, fresh, e.super)
				if code != http.StatusOK || retry.Released != 0 || retry.EventID != nil || len(retry.Replayed) != 1 || retry.Replayed[0] != id {
					t.Fatalf("the retry = %d %+v, want 200 {released: 0, replayed: [%s]}", code, retry, id)
				}
				var rows int
				var rowActor, rowType, count string
				var sameTime bool
				if err := e.pool.QueryRow(ctx, `SELECT count(*), COALESCE(max(actor), ''), COALESCE(max(actor_type), ''), COALESCE(max(data->>'released'), ''), COALESCE(bool_and("time" = $2), false)
					FROM audit_events WHERE action = $3 AND id = $1`, id, eventAt, legacyReleaseAction).Scan(&rows, &rowActor, &rowType, &count, &sameTime); err != nil {
					t.Fatal(err)
				}
				if rows != 1 || rowActor != "sub-super" || rowType != string(types.ActorHuman) || count != "2" || !sameTime {
					t.Errorf("the delivered row = %d rows %s/%s released %s sameTime %v, want 1 row, human/sub-super, 2, the record's time", rows, rowType, rowActor, count, sameTime)
				}
				if err := e.pool.QueryRow(ctx, `SELECT actor, audited_at FROM drive_legacy_releases WHERE id = $1`, id).Scan(&actor, &audited); err != nil {
					t.Fatal(err)
				}
				if actor != "" || audited == nil {
					t.Errorf("settled record = actor %q audited %v, want the principal cleared and audited_at set", actor, audited)
				}

				code, third := callRelease(t, e, fresh, e.super)
				if code != http.StatusOK || third.Released != 0 || third.EventID != nil || len(third.Replayed) != 0 {
					t.Errorf("the third call = %d %+v, want 200 {released: 0} with nothing replayed", code, third)
				}
				if n := releaseRows(t, e, &id); n != 1 {
					t.Errorf("audit rows for the id after the third call = %d, want 1", n)
				}
				if n := releaseRows(t, e, nil); n != 1 {
					t.Errorf("audit rows of the release action = %d, want 1", n)
				}
			})
		}
	}
}

// macSession is the runner side of a session as the READY handler reads it: the caps it sent.
type macSession struct {
	caps runnerwire.Caps
	have bool
}

func (m macSession) Caps() (runnerwire.Caps, bool) { return m.caps, m.have }

// freshMac is a session whose caps carry the macOS VM block with PriorSessions n; a nil n sends no count (0.10.1).
func freshMac(n *int) macSession {
	return macSession{caps: runnerwire.Caps{MacOSVM: &runnerwire.MacVMCaps{PriorSessions: n}}, have: true}
}

func sameSessionAlways() bool { return true }

// Test 15a. A runner whose caps carry PriorSessions 0 at a fresh session clears legacy_unsettled on that runner's
// mac_disk drives only: another runner's stay held, so does a Docker volume. Sessions that are no evidence (the
// process served before, no count, no macOS VM caps, no caps) clear nothing. The clear is the stored write, so it
// survives a restart: a server started afterwards over the same database finds the drive released.
func TestLegacyMacDrives_EvidenceClearsThatRunnersDisksOnly_PG(t *testing.T) {
	ctx := context.Background()
	e := newLegacyEnv(t)
	r1, r2 := e.claimedRunner(e.personP), e.claimedRunner(e.otherP)
	d1 := e.legacyDrive(e.personP, "Disk one", types.DriveBackendMacDisk, &r1, true)
	d1b := e.legacyDrive(e.otherP, "Disk one of another", types.DriveBackendMacDisk, &r1, true)
	d2 := e.legacyDrive(e.personP, "Disk two", types.DriveBackendMacDisk, &r2, true)
	vol := e.legacyDrive(e.personP, "Volume", types.DriveBackendDockerVolume, nil, true)

	for name, sub := range map[string]macSession{
		"the process served before (PriorSessions 1)": freshMac(intp(1)),
		"no count (0.10.1)":                           freshMac(nil),
		"no macOS VM caps":                            {caps: runnerwire.Caps{}, have: true},
		"no caps yet":                                 {},
	} {
		e.srv.releaseLegacyMacDrives(ctx, r1, sub, sameSessionAlways)
		for _, d := range []types.UserDrive{d1, d1b, d2, vol} {
			if !e.held(d) {
				t.Errorf("%s: %s was released", name, d.Name)
			}
		}
	}

	e.srv.releaseLegacyMacDrives(ctx, r1, freshMac(intp(0)), sameSessionAlways)
	if e.held(d1) || e.held(d1b) {
		t.Errorf("PriorSessions 0 on the runner left its disks held: %v %v", e.held(d1), e.held(d1b))
	}
	if !e.held(d2) {
		t.Error("PriorSessions 0 on one runner released another runner's disk")
	}
	if !e.held(vol) {
		t.Error("PriorSessions 0 on a Mac released a Docker volume")
	}
	wantHeldRefusal(t, "another runner's disk", d2, e.attach(e.srv, e.personP, d2))

	// A restart: another server over the same database sees the release, and a later session of the same process
	// (PriorSessions above 0) does not take it back.
	restarted := e.otherServer(func(c *Config) { c.RunnerTarget = "docker" })
	restarted.releaseLegacyMacDrives(ctx, r1, freshMac(intp(1)), sameSessionAlways)
	if e.held(d1) || e.held(d1b) {
		t.Error("the release did not survive a restart")
	}
	wantNotHeld(t, "after the restart", d1, e.attach(restarted, e.personP, d1))
	if !e.held(d2) {
		t.Error("the restart released the other runner's disk")
	}

	restarted.releaseLegacyMacDrives(ctx, r2, freshMac(intp(0)), sameSessionAlways)
	if e.held(d2) {
		t.Error("the other runner's own evidence did not release its disk")
	}
	if !e.held(vol) {
		t.Error("a Mac's evidence released a Docker volume")
	}
}

// failingMacRelease is a store whose marker write fails the first n times, and notes whether the drive was still
// held when it did.
type failingMacRelease struct {
	store.PG
	fail        int
	calls       int
	heldAtFail  []bool
	driveToRead uuid.UUID
}

func (f *failingMacRelease) ReleaseLegacyMacDrives(ctx context.Context, runnerID uuid.UUID) (int64, error) {
	f.calls++
	if f.fail > 0 {
		f.fail--
		var held bool
		if err := f.Pool.QueryRow(ctx, `SELECT legacy_unsettled FROM user_drives WHERE id = $1`, f.driveToRead).Scan(&held); err != nil {
			return 0, err
		}
		f.heldAtFail = append(f.heldAtFail, held)
		return 0, errors.New("the write failed")
	}
	return f.PG.ReleaseLegacyMacDrives(ctx, runnerID)
}

// Test 15c. A failed marker write leaves the hold, and the write is tried again while the session lives; when the
// session ends first (the link drops), the hold stays. And the old-writer case: a drive a 0.10.1 server was
// deleting (S NULL, N even, the mark cleared on an ambiguous remove) is held when 0299 runs; the Mac's old process
// is still the one connected (PriorSessions 1) and its reclaim is running. A deterministic fake Mac (oldWriterMac)
// answers reclaim_drive: in use while the old delete runs (a concurrent delete is refused and the drive kept), no call
// reaches it while the link is down and the old delete completes, and after the new process's evidence (PriorSessions
// 0) the hold lifts with the delete columns unchanged and the image is found already absent, never reused.
func TestLegacyMacDrives_FailedWriteAndOldWriterStayHeld_PG(t *testing.T) {
	ctx := context.Background()
	saved := readySweepRetries
	readySweepRetries = []time.Duration{20 * time.Millisecond}
	t.Cleanup(func() { readySweepRetries = saved })

	t.Run("a failed write leaves the hold and is retried", func(t *testing.T) {
		e := newLegacyEnv(t)
		mac := e.claimedRunner(e.personP)
		d := e.legacyDrive(e.personP, "Disk", types.DriveBackendMacDisk, &mac, true)
		fs := &failingMacRelease{PG: e.pg, fail: 2, driveToRead: d.ID}
		cfg := e.cfg
		cfg.Store = fs
		srv := New(cfg)
		srv.releaseLegacyMacDrives(ctx, mac, freshMac(intp(0)), sameSessionAlways)
		if fs.calls != 3 || len(fs.heldAtFail) != 2 || !fs.heldAtFail[0] || !fs.heldAtFail[1] {
			t.Errorf("writes = %d, held at each failure = %v, want 3 writes and the drive held at both failures", fs.calls, fs.heldAtFail)
		}
		if e.held(d) {
			t.Error("the retry after the failed writes did not release the drive")
		}
	})

	t.Run("a session that ends after a failed write leaves the hold", func(t *testing.T) {
		e := newLegacyEnv(t)
		mac := e.claimedRunner(e.personP)
		d := e.legacyDrive(e.personP, "Disk", types.DriveBackendMacDisk, &mac, true)
		fs := &failingMacRelease{PG: e.pg, fail: 5, driveToRead: d.ID}
		cfg := e.cfg
		cfg.Store = fs
		srv := New(cfg)
		srv.releaseLegacyMacDrives(ctx, mac, freshMac(intp(0)), func() bool { return false })
		if fs.calls != 1 || !e.held(d) {
			t.Errorf("writes = %d, held = %v, want one failed write and the hold kept", fs.calls, e.held(d))
		}
		wantHeldRefusal(t, "after the failed write", d, e.attach(e.srv, e.personP, d))
	})

	t.Run("the old writer's disk is held until the new process connects", func(t *testing.T) {
		e := newLegacyEnv(t)
		mac := e.claimedRunner(e.personP)
		d := e.legacyDrive(e.personP, "Disk", types.DriveBackendMacDisk, &mac, true)
		// What a 0.10.1 server left: no tombstone, two deletes issued (even), the mark gone.
		if _, err := e.pool.Exec(ctx, `UPDATE user_drives SET delete_started_at = NULL, delete_issues = 2 WHERE id = $1`, d.ID); err != nil {
			t.Fatal(err)
		}
		deleteColumns := func() string {
			var s string
			if err := e.pool.QueryRow(ctx, `SELECT COALESCE(delete_started_at::text, 'NULL') || '/' || delete_issues FROM user_drives WHERE id = $1`, d.ID).Scan(&s); err != nil {
				t.Fatal(err)
			}
			return s
		}
		rows := func() int { return e.count(`SELECT count(*) FROM user_drives WHERE id = $1`, d.ID) }
		owner := types.CanonicalUserSubject(e.personP)
		ownerDelete := func() memberDriveDeletion {
			tomb, started, err := e.pg.BeginMemberDriveDelete(ctx, owner, d.ID)
			if err != nil {
				t.Fatalf("begin the owner's delete: %v", err)
			}
			return e.srv.deleteMemberDrive(ctx, e.pg, tomb, started, memberDriveByOwner)
		}
		// The Mac still runs the 0.10.1 server's delete of this disk when the old process's link comes up.
		fake := &oldWriterMac{capsTransport: capsTransport{online: true}, image: imageDeleting}
		fake.link(t, e.srv, mac, true)

		// A concurrent use: the owner's delete reaches the Mac while its old delete is in flight, and is refused
		// as in use; the drive is kept, still held, and the image is not touched.
		wantHeldRefusal(t, "the old process connected", d, e.attach(e.srv, e.personP, d))
		e.srv.releaseLegacyMacDrives(ctx, mac, freshMac(intp(1)), sameSessionAlways)
		if res := ownerDelete(); res.reason != reasonMemberDriveInUse {
			t.Errorf("a delete while the old delete was in flight = %+v, want refused in use", res)
		}
		if got := fake.log; len(got) != 1 || got[0] != "reclaim: in use" || fake.image != imageDeleting {
			t.Errorf("the Mac's log = %v image %s, want one refused reclaim and the image still deleting", got, fake.image)
		}
		if rows() != 1 || !e.held(d) {
			t.Fatalf("after the refused delete: rows %d held %v, want the drive kept and held", rows(), e.held(d))
		}

		// The link drops. The old reclaim completes with no daemon watching, and the sweep has nothing to
		// reach: no call arrives, and the drive stays held and refused while the Mac is away.
		fake.link(t, e.srv, mac, false)
		fake.finishOldDelete()
		if err := e.srv.finishMacDiskDeletes(ctx); err != nil {
			t.Fatal(err)
		}
		wantHeldRefusal(t, "after the old reclaim completed with the link down", d, e.attach(e.srv, e.personP, d))
		if len(fake.log) != 1 || !e.held(d) {
			t.Fatalf("with the link down: Mac log %v held %v, want no new call and the drive held", fake.log, e.held(d))
		}

		// The Mac's process is replaced: the new one has no earlier session, so no delete of an earlier daemon
		// can still be running there. The hold lifts, and the old image is absent: a delete finds nothing to
		// remove and the row goes. The Mac was never asked to use or recreate the disk.
		fake.link(t, e.srv, mac, true)
		beforeRelease := deleteColumns()
		e.srv.releaseLegacyMacDrives(ctx, mac, freshMac(intp(0)), sameSessionAlways)
		if e.held(d) {
			t.Error("the new process's evidence did not release the drive")
		}
		wantNotHeld(t, "after the new process connected", d, e.attach(e.srv, e.personP, d))
		if after := deleteColumns(); after != beforeRelease {
			t.Errorf("delete columns changed from %s to %s; the release must write the mark and nothing else", beforeRelease, after)
		}
		res := ownerDelete()
		if res.reason != "" || res.outcome != string(runner.DriveReclaimAlreadyAbsent) {
			t.Errorf("a delete after the evidence = %+v, want the image already absent", res)
		}
		if want := []string{"reclaim: in use", "reclaim: already absent"}; len(fake.log) != 2 || fake.log[1] != want[1] || fake.image != imageGone {
			t.Errorf("the Mac's log = %v image %s, want %v and the image gone", fake.log, fake.image, want)
		}
		if rows() != 0 {
			t.Error("the drive row is still there after its image was found absent")
		}
	})
}

const (
	imageDeleting = "deleting" // an older server's delete is still running on the Mac
	imageGone     = "gone"
)

// oldWriterMac is a deterministic Mac that keeps one disk image: reclaim_drive answers in use while an older
// server's delete of it is running and already_absent once it has finished. It records every call that reaches it.
type oldWriterMac struct {
	capsTransport
	image string
	log   []string
}

func (m *oldWriterMac) Call(ctx context.Context, method string, in, out any, opts ...runnerwire.CallOption) error {
	if method != runnerwire.MethodReclaimDrive {
		return m.capsTransport.Call(ctx, method, in, out, opts...)
	}
	res := out.(*runnerwire.ReclaimDriveResult)
	switch m.image {
	case imageDeleting:
		m.log = append(m.log, "reclaim: in use")
		res.InUse = true
	default:
		m.log = append(m.log, "reclaim: already absent")
		res.Outcome = runner.DriveReclaimAlreadyAbsent
	}
	return nil
}

func (m *oldWriterMac) finishOldDelete() { m.image = imageGone }

// link puts this Mac's session in the server's table (a new process connecting) or takes it out (the link dropped).
func (m *oldWriterMac) link(t *testing.T, srv *Server, id uuid.UUID, up bool) {
	t.Helper()
	srv.cfg.RunnerOnline = func(r uuid.UUID) bool { return up && r == id }
	srv.runnerStreams.subsMu.Lock()
	defer srv.runnerStreams.subsMu.Unlock()
	if !up {
		delete(srv.runnerStreams.subs, id)
		return
	}
	sub := remote.New(id.String(), m, remote.Options{})
	data, err := json.Marshal(runnerwire.Caps{MacOSVM: &runnerwire.MacVMCaps{DiskDrives: true}})
	if err != nil {
		t.Fatal(err)
	}
	sub.HandleEvent(runnerwire.Event{Kind: runnerwire.EventCaps, Data: data})
	if srv.runnerStreams.subs == nil {
		srv.runnerStreams.subs = map[uuid.UUID]runnerEndpoint{}
	}
	srv.runnerStreams.subs[id] = runnerEndpoint{sub: sub, link: &remote.Link{}}
}
