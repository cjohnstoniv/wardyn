// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_DriveSecondPlace_RoundTripsAndStaysOffWithout: a share's second place
// (0211) survives the write and every read the resolver and the admin list use,
// clears back to NULL, and a drive that never had one reads none.
func TestPG_DriveSecondPlace_RoundTripsAndStaysOffWithout(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	uniq := uuid.NewString()[:8]
	rn, err := st.CreateRunner(ctx, newRunner("alice-"+uniq+"@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	d := newUserDrive("share-" + uniq)
	d.Backend, d.HostRoot, d.HomeTemplate, d.SizeMiB = types.DriveBackendHostPath, "/srv/shares/"+uniq, types.HomeTemplateEmailLocal, 0
	d.AltBackend, d.AltHostRoot, d.AltRunnerID = types.DriveBackendHostPath, "/Volumes/team-share", &rn.ID
	saved, err := st.UpsertUserDrive(ctx, d, false)
	if err != nil {
		t.Fatalf("write a drive with a second place: %v", err)
	}
	t.Cleanup(func() { _ = st.DeleteUserDrive(ctx, saved.ID) })
	want := func(where string, got types.UserDrive) {
		t.Helper()
		if got.AltBackend != types.DriveBackendHostPath || got.AltHostRoot != "/Volumes/team-share" || got.AltRunnerID == nil || *got.AltRunnerID != rn.ID {
			t.Fatalf("%s: second place = %q %q %v, want host_path /Volumes/team-share %s", where, got.AltBackend, got.AltHostRoot, got.AltRunnerID, rn.ID)
		}
	}
	want("write", saved)
	got, err := st.GetUserDrive(ctx, saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	want("get", got)
	subject := "sub-" + uniq
	addUserDriveGrant(t, st, types.UserDriveGrant{SubjectType: types.CapabilitySubjectUser, Subject: subject,
		DriveID: saved.ID, Enabled: true, CreatedBy: "admin@example.com"})
	one, _, _, err := st.ResolveUserDrive(ctx, []string{subject}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	want("resolve", *one)
	items, err := st.ListUserDrives(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.ID == saved.ID {
			want("list", it.UserDrive)
		}
	}

	d.ID = saved.ID
	d.AltBackend, d.AltHostRoot, d.AltRunnerID = "", "", nil
	cleared, err := st.UpsertUserDrive(ctx, d, false)
	if err != nil {
		t.Fatalf("clear the second place: %v", err)
	}
	var altBackend, altRoot *string
	var altRunner *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT alt_backend, alt_host_root, alt_runner_id FROM user_drives WHERE id = $1`, saved.ID).Scan(&altBackend, &altRoot, &altRunner); err != nil {
		t.Fatal(err)
	}
	if cleared.AltRunnerID != nil || cleared.AltBackend != "" || altBackend != nil || altRoot != nil || altRunner != nil {
		t.Fatalf("a cleared second place left %+v / columns %v %v %v; want NULL", cleared, altBackend, altRoot, altRunner)
	}

	plain := seedUserDrive(t, st, "plain-"+uniq)
	if plain.AltRunnerID != nil || plain.AltBackend != "" || plain.AltHostRoot != "" {
		t.Fatalf("a drive with one place reads a second: %+v", plain)
	}
}

// TestPG_DriveSecondPlace_CheckRefusesTheWrongShape: 0211's check keeps a second
// place off a managed volume, never managed itself, and never half-set.
func TestPG_DriveSecondPlace_CheckRefusesTheWrongShape(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	uniq := uuid.NewString()[:8]
	rn, err := st.CreateRunner(ctx, newRunner("bob-"+uniq+"@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	managed := seedUserDrive(t, st, "managed-"+uniq)
	share := newUserDrive("share-" + uniq)
	share.Backend, share.HostRoot, share.HomeTemplate, share.SizeMiB = types.DriveBackendHostPath, "/srv/shares/"+uniq, types.HomeTemplateSub, 0
	share, err = st.UpsertUserDrive(ctx, share, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.DeleteUserDrive(ctx, share.ID) })
	for name, c := range map[string]struct {
		id   uuid.UUID
		sets string
	}{
		"a managed volume":       {managed.ID, `alt_backend = 'host_path', alt_host_root = '/Volumes/x', alt_runner_id = $2`},
		"no runner":              {share.ID, `alt_backend = 'host_path', alt_host_root = '/Volumes/x'`},
		"no path":                {share.ID, `alt_backend = 'host_path', alt_runner_id = $2`},
		"a second managed place": {share.ID, `alt_backend = 'docker_volume', alt_host_root = '/Volumes/x', alt_runner_id = $2`},
	} {
		_, err := pool.Exec(ctx, `UPDATE user_drives SET `+c.sets+` WHERE id = $1 AND $2::uuid IS NOT NULL`, c.id, rn.ID)
		if err == nil || !strings.Contains(err.Error(), "user_drives_second_place_shape") {
			t.Errorf("%s: err = %v, want the second-place check", name, err)
		}
	}
}
