// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
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
