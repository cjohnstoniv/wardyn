// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The allocation and preview routes sit on the security-admin tier. These tests
// pin what that tier may write there and what it reads back, beside the
// super-admin answers that stay as they were.

func driveSecurityAdminCtx() context.Context {
	return operatorCtx("sub-sec-admin", "sec@corp.example", oidc.RoleSecurityAdmin)
}

func driveSuperAdminCtx() context.Context {
	return operatorCtx("sub-super-admin", "root@corp.example", oidc.RoleAdmin)
}

// driveGrantAs posts one allocation as the caller on ctx.
func driveGrantAs(t *testing.T, srv *Server, ctx context.Context, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/drives/grants", strings.NewReader(body)).WithContext(ctx)
	w := httptest.NewRecorder()
	srv.handleUpsertUserDriveGrant(w, r)
	return w
}

func driveGrantOf(st *driveCRUDStore, subject string) (types.UserDriveGrant, bool) {
	for _, g := range st.grants {
		if g.SubjectType == types.CapabilitySubjectUser && g.Subject == subject {
			return g, true
		}
	}
	return types.UserDriveGrant{}, false
}

// TestSecurityAdminDriveAllocationsStayOpen is every allocation the security
// tier writes in ordinary use: allocating at each tier with the drive's own
// posture, narrowing to read-only, repointing to another drive, re-sending a
// pinned name or a writable override unchanged on a priority edit, and
// clearing a pinned name.
func TestSecurityAdminDriveAllocationsStayOpen(t *testing.T) {
	st := newDriveCRUDStore()
	srv, _ := driveAdminServer(st, nil)
	d := *driveFixture(func(d *types.UserDrive) { d.Writable = true })
	other := *driveFixture(func(o *types.UserDrive) { o.ID, o.Name, o.Writable = uuid.New(), "Design scratch", true })
	st.drives[d.ID], st.drives[other.ID] = d, other
	// Rows a super admin wrote: a pinned name and a writable override.
	pinned := grantFixture(d.ID, func(g *types.UserDriveGrant) { g.Subject, g.HomeOverride = "sub-pinned", "pinned-home" })
	rw := grantFixture(d.ID, func(g *types.UserDriveGrant) { g.Subject, g.WritableOverride = "sub-rw", boolPtr(true) })
	st.grants[pinned.ID], st.grants[rw.ID] = *pinned, *rw

	sec := driveSecurityAdminCtx()
	id := d.ID.String()
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"user tier, drive posture", `{"subject_type":"user","subject":"sub-carol","drive_id":"` + id + `"}`, http.StatusCreated},
		{"group tier, drive posture", `{"subject_type":"group","subject":"eng","drive_id":"` + id + `"}`, http.StatusCreated},
		{"all tier, drive posture", `{"subject_type":"all","subject":"","drive_id":"` + id + `"}`, http.StatusCreated},
		{"read-only", `{"subject_type":"user","subject":"sub-dave","drive_id":"` + id + `","writable_override":false}`, http.StatusCreated},
		{"repoint to another drive", `{"subject_type":"user","subject":"sub-carol","drive_id":"` + other.ID.String() + `"}`, http.StatusOK},
		{"pinned name re-sent unchanged", `{"subject_type":"user","subject":"sub-pinned","drive_id":"` + id + `","priority":5,"home_override":"Pinned-Home"}`, http.StatusOK},
		{"writable override re-sent unchanged", `{"subject_type":"user","subject":"sub-rw","drive_id":"` + id + `","priority":5,"writable_override":true}`, http.StatusOK},
		{"pinned name cleared", `{"subject_type":"user","subject":"sub-pinned","drive_id":"` + id + `","priority":5,"home_override":""}`, http.StatusOK},
	} {
		if w := driveGrantAs(t, srv, sec, tc.body); w.Code != tc.want {
			t.Errorf("%s = %d, want %d: %s", tc.name, w.Code, tc.want, w.Body.String())
		}
	}
	if g, _ := driveGrantOf(st, "sub-pinned"); g.HomeOverride != "" || g.Priority != 5 {
		t.Errorf("pinned row = %+v, want the priority edit kept and the name cleared", g)
	}
}

// TestSuperAdminDriveOverridesStayOpen: the two fields the security tier may
// not newly state remain the super admin's to write.
func TestSuperAdminDriveOverridesStayOpen(t *testing.T) {
	st := newDriveCRUDStore()
	srv, _ := driveAdminServer(st, nil)
	d := *driveFixture(nil) // writable: false
	st.drives[d.ID] = d
	for _, ctx := range []context.Context{driveSuperAdminCtx(), context.Background()} {
		w := driveGrantAs(t, srv, ctx, `{"subject_type":"user","subject":"sub-erin","drive_id":"`+d.ID.String()+
			`","home_override":"erin-legacy","writable_override":true}`)
		if w.Code != http.StatusCreated && w.Code != http.StatusOK {
			t.Fatalf("super admin pinned name + writable = %d, want 201/200: %s", w.Code, w.Body.String())
		}
	}
	if g, _ := driveGrantOf(st, "sub-erin"); g.HomeOverride != "erin-legacy" || g.WritableOverride == nil || !*g.WritableOverride {
		t.Errorf("row = %+v, want the super admin's name and writable override stored", g)
	}
}

// TestSecurityAdminCannotPinADerivedHome: a directory name typed on an
// allocation is compared only against other allocations' typed names, so a name
// equal to another person's DERIVED home would mount that person's directory.
// The security tier may not set or change a name at all.
func TestSecurityAdminCannotPinADerivedHome(t *testing.T) {
	st := newDriveCRUDStore()
	srv, rec := driveAdminServer(st, nil)
	d := *driveFixture(func(d *types.UserDrive) {
		d.Backend, d.HomeTemplate, d.HostRoot, d.Writable = types.DriveBackendHostPath, types.HomeTemplateEmailLocal, "/srv/homes", true
	})
	other := *driveFixture(func(o *types.UserDrive) { o.ID, o.Name = uuid.New(), "Design scratch" })
	st.drives[d.ID], st.drives[other.ID] = d, other
	// Alice holds the drive through the all tier; her home is derived, not typed.
	all := grantFixture(d.ID, func(g *types.UserDriveGrant) { g.SubjectType, g.Subject = types.CapabilitySubjectAll, "" })
	// A row whose name a super admin pinned.
	pinned := grantFixture(d.ID, func(g *types.UserDriveGrant) { g.Subject, g.HomeOverride = "sub-pinned", "pinned-home" })
	st.grants[all.ID], st.grants[pinned.ID] = *all, *pinned
	aliceHome, err := types.DriveHomeName(d, "alice@corp.example", "")
	if err != nil || aliceHome != "alice" {
		t.Fatalf("derived home = %q, %v; want alice", aliceHome, err)
	}

	sec := driveSecurityAdminCtx()
	for _, tc := range []struct{ name, subject, drive, home string }{
		{"own allocation on another's derived home", "sub-sec-admin", d.ID.String(), aliceHome},
		{"new allocation with any name", "sub-frank", d.ID.String(), "frank"},
		{"pinned name changed", "sub-pinned", d.ID.String(), "elsewhere"},
		{"pinned name carried to another drive", "sub-pinned", other.ID.String(), "pinned-home"},
	} {
		before, _ := driveGrantOf(st, tc.subject)
		w := driveGrantAs(t, srv, sec, `{"subject_type":"user","subject":"`+tc.subject+`","drive_id":"`+tc.drive+
			`","home_override":"`+tc.home+`"}`)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "home_override") {
			t.Errorf("%s = %d, want 403 naming home_override: %s", tc.name, w.Code, w.Body.String())
		}
		if after, _ := driveGrantOf(st, tc.subject); after != before {
			t.Errorf("%s: row changed to %+v on a refused write", tc.name, after)
		}
	}
	if acts := driveAuditActions(rec); len(acts) == 0 || acts[len(acts)-1] != "authz.denied" {
		t.Errorf("audit actions = %v, want the refusal recorded as authz.denied", acts)
	}
}

// TestSecurityAdminCannotWidenDriveWritability: the security tier allocates a
// drive with its own posture or narrower, never wider — not by a new writable
// override, and not by dropping a read-only override on an existing row.
func TestSecurityAdminCannotWidenDriveWritability(t *testing.T) {
	st := newDriveCRUDStore()
	srv, _ := driveAdminServer(st, nil)
	ro := *driveFixture(nil) // writable: false
	rwDrive := *driveFixture(func(o *types.UserDrive) { o.ID, o.Name, o.Writable = uuid.New(), "Design scratch", true })
	st.drives[ro.ID], st.drives[rwDrive.ID] = ro, rwDrive
	narrowed := grantFixture(rwDrive.ID, func(g *types.UserDriveGrant) { g.Subject, g.WritableOverride = "sub-narrowed", boolPtr(false) })
	st.grants[narrowed.ID] = *narrowed

	sec := driveSecurityAdminCtx()
	for _, tc := range []struct{ name, subject, body string }{
		{"writable override on a read-only drive", "sub-sec-admin",
			`{"subject_type":"user","subject":"sub-sec-admin","drive_id":"` + ro.ID.String() + `","writable_override":true}`},
		{"writable override on a writable drive", "sub-grace",
			`{"subject_type":"user","subject":"sub-grace","drive_id":"` + rwDrive.ID.String() + `","writable_override":true}`},
		{"read-only override dropped", "sub-narrowed",
			`{"subject_type":"user","subject":"sub-narrowed","drive_id":"` + rwDrive.ID.String() + `","priority":3}`},
		{"read-only override flipped", "sub-narrowed",
			`{"subject_type":"user","subject":"sub-narrowed","drive_id":"` + rwDrive.ID.String() + `","writable_override":true}`},
	} {
		before, _ := driveGrantOf(st, tc.subject)
		w := driveGrantAs(t, srv, sec, tc.body)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "writable_override") {
			t.Errorf("%s = %d, want 403 naming writable_override: %s", tc.name, w.Code, w.Body.String())
		}
		if after, _ := driveGrantOf(st, tc.subject); after.ID != before.ID || after.Priority != before.Priority ||
			(after.WritableOverride == nil) != (before.WritableOverride == nil) {
			t.Errorf("%s: row changed to %+v on a refused write", tc.name, after)
		}
	}
	// Dropping the read-only override where the drive itself is read-only
	// widens nothing.
	n2 := grantFixture(ro.ID, func(g *types.UserDriveGrant) { g.Subject, g.WritableOverride = "sub-ro", boolPtr(false) })
	st.grants[n2.ID] = *n2
	if w := driveGrantAs(t, srv, sec, `{"subject_type":"user","subject":"sub-ro","drive_id":"`+ro.ID.String()+`"}`); w.Code != http.StatusOK {
		t.Errorf("read-only override dropped on a read-only drive = %d, want 200: %s", w.Code, w.Body.String())
	}
}

// TestSecurityAdminDrivePreviewOmitsTheHostPath: a host_path object name is a
// path under the drive's host_root, which the security tier is not shown
// (GET /drives stays super-only for that reason). The home name and every other
// field stay; a super admin's answer and a Wardyn-named object are unchanged.
func TestSecurityAdminDrivePreviewOmitsTheHostPath(t *testing.T) {
	preview := func(t *testing.T, srv *Server, ctx context.Context) userDrivePreviewResponse {
		t.Helper()
		body, _ := json.Marshal(governancePreviewRequest{UserSubjects: []string{"sub-abc", "alice@corp.example"}, Groups: []string{"eng"}})
		r := httptest.NewRequest(http.MethodPost, "/api/v1/drives/preview", strings.NewReader(string(body))).WithContext(ctx)
		w := httptest.NewRecorder()
		srv.handlePreviewUserDrive(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("preview = %d, want 200: %s", w.Code, w.Body.String())
		}
		var got userDrivePreviewResponse
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return got
	}
	share := driveFixture(func(d *types.UserDrive) {
		d.Backend, d.HomeTemplate = types.DriveBackendHostPath, types.HomeTemplateEmailLocal
	})
	st := &driveStore{drive: share, grant: grantFixture(share.ID, nil), tier: types.CapabilitySubjectUser}
	srv := drivePreviewShareServer(t, st, "alice")

	sec := preview(t, srv, driveSecurityAdminCtx())
	if sec.ObjectName != "" {
		t.Errorf("security admin object_name = %q, want it omitted for a host_path drive", sec.ObjectName)
	}
	if sec.HomeName != "alice" || sec.DriveName != share.Name {
		t.Errorf("security admin home/drive = %q/%q, want alice/%q", sec.HomeName, sec.DriveName, share.Name)
	}
	if sup := preview(t, srv, driveSuperAdminCtx()); sup.ObjectName != types.DriveObjectName(*share, "alice") {
		t.Errorf("super admin object_name = %q, want the host path", sup.ObjectName)
	}

	managed := driveFixture(nil)
	mst := &driveStore{drive: managed, grant: grantFixture(managed.ID, nil), tier: types.CapabilitySubjectUser}
	if got := preview(t, driveServerOn(mst, "docker"), driveSecurityAdminCtx()); got.ObjectName == "" || got.ObjectName != types.DriveObjectName(*managed, got.HomeName) {
		t.Errorf("security admin managed object_name = %q, want the volume name", got.ObjectName)
	}
}

// changedTierStore answers every tier-judged write with the store's
// "changed meanwhile" sentinel.
type changedTierStore struct{ *driveCRUDStore }

func (changedTierStore) UpsertUserDriveGrantWithinTier(context.Context, types.UserDriveGrant, bool, bool) (types.UserDriveGrant, []uuid.UUID, error) {
	return types.UserDriveGrant{}, nil, store.ErrUserDriveGrantChanged
}

// TestSecurityAdminAllocationIsJudgedByTheStore: the security tier's write goes
// through the store's tier-judged write. A store without one refuses it rather
// than write unjudged, a write that lost a race is a 409, and a super admin's
// write is the ordinary upsert either way.
func TestSecurityAdminAllocationIsJudgedByTheStore(t *testing.T) {
	st := newDriveCRUDStore()
	srv, _ := driveAdminServer(st, nil)
	d := *driveFixture(func(d *types.UserDrive) { d.Writable = true })
	st.drives[d.ID] = d
	body := func(subject string) string {
		return `{"subject_type":"user","subject":"` + subject + `","drive_id":"` + d.ID.String() + `"}`
	}

	srv.cfg.Store = struct{ store.Store }{st}
	if w := driveGrantAs(t, srv, driveSecurityAdminCtx(), body("sub-h")); w.Code != http.StatusInternalServerError {
		t.Errorf("no tier writer = %d, want 500: %s", w.Code, w.Body.String())
	}
	if _, ok := driveGrantOf(st, "sub-h"); ok {
		t.Error("an allocation was written with no tier writer to judge it")
	}
	if w := driveGrantAs(t, srv, driveSuperAdminCtx(), body("sub-i")); w.Code != http.StatusCreated {
		t.Errorf("super admin with no tier writer = %d, want 201: %s", w.Code, w.Body.String())
	}

	srv.cfg.Store = changedTierStore{st}
	w := driveGrantAs(t, srv, driveSecurityAdminCtx(), body("sub-j"))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "re-send") {
		t.Errorf("changed meanwhile = %d, want 409 asking for a re-send: %s", w.Code, w.Body.String())
	}
}

// TestSecurityAdminAddBesideIsJudged: `additional` allocates a drive beside the subject's others,
// and a security admin's is judged like any other write, against the subject's row for THAT drive.
// A widening add is refused and writes nothing; a plain add is written and the other row stays.
func TestSecurityAdminAddBesideIsJudged(t *testing.T) {
	st := newDriveCRUDStore()
	srv, _ := driveAdminServer(st, nil)
	ro := *driveFixture(nil) // writable: false
	rwDrive := *driveFixture(func(o *types.UserDrive) { o.ID, o.Name, o.Writable = uuid.New(), "Design scratch", true })
	st.drives[ro.ID], st.drives[rwDrive.ID] = ro, rwDrive
	held := grantFixture(ro.ID, func(g *types.UserDriveGrant) { g.Subject, g.WritableOverride = "sub-two", boolPtr(false) })
	st.grants[held.ID] = *held
	rows := func() (n int) {
		for _, g := range st.grants {
			if g.Subject == "sub-two" {
				n++
			}
		}
		return n
	}

	sec := driveSecurityAdminCtx()
	for _, tc := range []struct{ name, field, body string }{
		{"writable beside", "writable_override",
			`{"subject_type":"user","subject":"sub-two","drive_id":"` + rwDrive.ID.String() + `","additional":true,"writable_override":true}`},
		{"a directory name beside", "home_override",
			`{"subject_type":"user","subject":"sub-two","drive_id":"` + rwDrive.ID.String() + `","additional":true,"home_override":"alice"}`},
	} {
		w := driveGrantAs(t, srv, sec, tc.body)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), tc.field) {
			t.Errorf("%s = %d, want 403 naming %s: %s", tc.name, w.Code, tc.field, w.Body.String())
		}
		if rows() != 1 {
			t.Errorf("%s: the subject holds %d rows after a refused add, want 1", tc.name, rows())
		}
	}
	// The read-only override on the OTHER drive is not this write's row: a plain add is the
	// drive's own posture and is written beside it.
	w := driveGrantAs(t, srv, sec, `{"subject_type":"user","subject":"sub-two","drive_id":"`+rwDrive.ID.String()+`","additional":true}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("plain add beside = %d, want 201: %s", w.Code, w.Body.String())
	}
	if kept := st.grants[held.ID]; rows() != 2 || kept.DriveID != ro.ID || kept.WritableOverride == nil || *kept.WritableOverride {
		t.Errorf("after the add the subject holds %d rows and the first is %+v, want 2 and the read-only row untouched", rows(), kept)
	}
	// A super admin's add is not judged.
	if w := driveGrantAs(t, srv, driveSuperAdminCtx(), `{"subject_type":"user","subject":"sub-three","drive_id":"`+ro.ID.String()+`","writable_override":true}`); w.Code != http.StatusCreated {
		t.Errorf("super admin writable allocation = %d, want 201: %s", w.Code, w.Body.String())
	}
}

// TestSecurityAdminReplaceOfSeveralRowsIsJudgedAgainstTheKeptRow: a subject that holds several
// allocations is judged against the row the replace keeps (the one on the drive, else the oldest),
// and a replace that passes still audits the drives it took away.
func TestSecurityAdminReplaceOfSeveralRowsIsJudgedAgainstTheKeptRow(t *testing.T) {
	st := newDriveCRUDStore()
	srv, audit := driveAdminServer(st, nil)
	a := *driveFixture(func(o *types.UserDrive) { o.Writable = true })
	b := *driveFixture(func(o *types.UserDrive) { o.ID, o.Name = uuid.New(), "Design scratch" }) // writable: false
	st.drives[a.ID], st.drives[b.ID] = a, b
	older := grantFixture(a.ID, func(g *types.UserDriveGrant) {
		g.Subject, g.WritableOverride, g.CreatedAt = "sub-many", boolPtr(false), time.Unix(100, 0)
	})
	exception := grantFixture(b.ID, func(g *types.UserDriveGrant) {
		g.ID, g.Subject, g.WritableOverride, g.CreatedAt = uuid.New(), "sub-many", boolPtr(true), time.Unix(200, 0)
	})
	st.grants[older.ID], st.grants[exception.ID] = *older, *exception

	sec := driveSecurityAdminCtx()
	// Dropping the older row's read-only override by replacing with a plain allocation of its own
	// writable drive is refused, and neither row is removed.
	w := driveGrantAs(t, srv, sec, `{"subject_type":"user","subject":"sub-many","drive_id":"`+a.ID.String()+`"}`)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "writable_override") {
		t.Errorf("replace dropping the kept row's read-only override = %d, want 403 naming writable_override: %s", w.Code, w.Body.String())
	}
	writes := func() (rows []string) {
		for _, ev := range audit.events {
			if ev.Action == "drive.grant.write" {
				rows = append(rows, string(ev.Data))
			}
		}
		return rows
	}
	if len(st.grants) != 2 || len(writes()) != 0 {
		t.Fatalf("a refused replace left %d rows and %d drive.grant.write rows, want 2 and 0", len(st.grants), len(writes()))
	}
	// The unchanged re-send of the super admin's exception on ITS drive is judged against that
	// row, not the older one, so it passes; the older drive is taken away and audited.
	w = driveGrantAs(t, srv, sec, `{"subject_type":"user","subject":"sub-many","drive_id":"`+b.ID.String()+`","writable_override":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("unchanged re-send on the kept row's drive = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got, ok := st.grants[exception.ID]; len(st.grants) != 1 || !ok || got.DriveID != b.ID {
		t.Errorf("after the replace the store holds %d rows (%+v), want the one on the kept drive", len(st.grants), st.grants)
	}
	if got := writes(); len(got) != 1 || !strings.Contains(got[0], `"removed_drive_ids":["`+a.ID.String()+`"]`) {
		t.Errorf("drive.grant.write rows = %v, want one naming the removed drive %s", got, a.ID)
	}
}
