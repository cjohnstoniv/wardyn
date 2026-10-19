// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A sign-in subject holding a rune that lower-cases into a-z (U+212A, U+0130)
// derives no drive home: the resolver refuses it with its own reason and no
// mount, at the launch door too, while a plain ASCII subject still resolves the
// home it always had, and an admin's home override still names one.
func TestResolveUserDrive_SubjectThatFoldsOntoASCIIIsRefused(t *testing.T) {
	d := driveFixture(nil) // managed, hash template
	st := &driveStore{drive: d, grant: grantFixture(d.ID, nil), tier: types.CapabilitySubjectUser}
	srv := driveServer(st)

	got, err := srv.resolveUserDrive(operatorCtx("kelly", "kelly@corp.example", oidc.RoleUser), 0)
	want, herr := types.DriveHomeName(*d, "kelly", "")
	if err != nil || herr != nil || got == nil || got.HomeName != want || got.SubjectHash != types.DriveSubjectHash("kelly") {
		t.Fatalf("ASCII subject: resolved %+v, %v (%v); want its existing home %q", got, err, herr, want)
	}

	share := driveFixture(func(d *types.UserDrive) {
		d.Backend, d.HomeTemplate, d.HostRoot = types.DriveBackendHostPath, types.HomeTemplateEmailLocal, "/srv/homes"
	})
	for name, c := range map[string]struct {
		st       *driveStore
		sub, eml string
	}{
		"U+212A in a hashed subject":      {st, "Kelly", "kelly2@corp.example"},
		"U+0130 in an email's local part": {&driveStore{drive: share, grant: grantFixture(share.ID, nil), tier: types.CapabilitySubjectUser}, "sub-ines", "İnes@corp.example"},
	} {
		got, err := driveServer(c.st).resolveUserDrive(operatorCtx(c.sub, c.eml, oidc.RoleUser), 0)
		if got != nil || !errors.Is(err, errDriveSubjectUnsupported) || !errors.Is(err, errDriveUnmountable) {
			t.Fatalf("%s: resolved %+v, %v; want errDriveSubjectUnsupported and no mount", name, got, err)
		}
		w := httptest.NewRecorder()
		writeDriveError(w, httptest.NewRequest(http.MethodPost, "/", nil), err)
		if w.Code != http.StatusUnprocessableEntity || errorReason(w) != driveRefusalSubjectUnsupported || strings.Contains(w.Body.String(), "drive_unmountable") {
			t.Errorf("%s: preview answer = %d %s, want 422 %s", name, w.Code, w.Body.String(), driveRefusalSubjectUnsupported)
		}
		if ref := driveRunError(err); ref == nil || ref.body.Reason != driveRefusalSubjectUnsupported || ref.status != http.StatusUnprocessableEntity {
			t.Errorf("%s: launch refusal = %+v, want 422 %s", name, ref, driveRefusalSubjectUnsupported)
		}
		if got := driveUnavailableReason(err); got != driveRefusalSubjectUnsupported {
			t.Errorf("%s: GET /me user_drive_unavailable = %q, want %q", name, got, driveRefusalSubjectUnsupported)
		}
	}

	// The admin reclaim names its object the same way, so it refuses the same subject.
	srv = &Server{cfg: Config{Store: reclaimGrantsStore{st}}}
	if object, _, code, reason, _ := srv.driveReclaimObject(t.Context(), *d, *st.grant, "\u212Aelly"); object != "" || code != http.StatusBadRequest || reason != driveRefusalSubjectUnsupported {
		t.Errorf("reclaim for a look-alike = %q %d %s, want no object and 400 %s", object, code, reason, driveRefusalSubjectUnsupported)
	}
	if object, _, _, reason, msg := srv.driveReclaimObject(t.Context(), *d, *st.grant, "kelly"); msg != "" || object != types.DriveObjectName(*d, want) {
		t.Errorf("reclaim for an ASCII subject = %q (%s %s), want %q", object, reason, msg, types.DriveObjectName(*d, want))
	}

	overridden := &driveStore{drive: d, grant: grantFixture(d.ID, func(g *types.UserDriveGrant) { g.HomeOverride = "kelly-k" }), tier: types.CapabilitySubjectUser}
	if got, err := driveServer(overridden).resolveUserDrive(operatorCtx("Kelly", "", oidc.RoleUser), 0); err != nil || got == nil || got.HomeName != "kelly-k" {
		t.Errorf("with a home override: resolved %+v, %v; want the override's home", got, err)
	}
}

// reclaimGrantsStore answers the reclaim's allocation read with the fixture's one grant.
type reclaimGrantsStore struct{ *driveStore }

func (s reclaimGrantsStore) ListUserDriveGrants(context.Context) ([]types.UserDriveGrant, error) {
	return []types.UserDriveGrant{*s.grant}, nil
}
