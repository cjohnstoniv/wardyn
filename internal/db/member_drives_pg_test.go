// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// memberDrivesMigration finds the member-drives migration by its slug, so the
// tests survive a renumber.
func memberDrivesMigration(t *testing.T) string {
	t.Helper()
	for _, n := range readMigrationNames(t) {
		if strings.HasSuffix(n, "_member_drives.sql") {
			return n
		}
	}
	t.Fatal("no *_member_drives.sql migration")
	return ""
}

// insertMemberDriveRow writes one user_drives row and returns the error's constraint
// name, or "" on success.
func insertMemberDriveRow(t *testing.T, pool *pgxpool.Pool, name, slug, backend, hostRoot, home, scheme, owner string, tombstone bool) string {
	t.Helper()
	_, err := pool.Exec(context.Background(), `INSERT INTO user_drives
		(id, name, name_slug, backend, host_root, home_template, object_scheme, owner_subject, delete_started_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, CASE WHEN $9 THEN now() END)`,
		uuid.New(), name, slug, backend, hostRoot, home, scheme, owner, tombstone)
	if err == nil {
		return ""
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("insert %s: %v", name, err)
	}
	return pgErr.ConstraintName
}

// TestMemberDriveStoreShape: the member CHECK refuses every row that would
// name a host path, a pre-provisioned claim, a claim-derived home, a host
// root or a slug-scheme object; the tombstone CHECK refuses one on an admin row.
func TestMemberDriveStoreShape(t *testing.T) {
	pool := pgPoolIsolated(t)
	const member = "user_drives_member_managed"
	cases := []struct {
		name, backend, hostRoot, home, scheme, owner string
		tombstone                                    bool
		want                                         string
	}{
		{"docker", "docker_volume", "", "hash", "id", "alice", false, ""},
		{"k8s", "k8s_pvc", "", "hash", "id", "alice", true, ""},
		{"host path", "host_path", "", "hash", "id", "alice", false, member},
		{"static claim", "k8s_pvc_static", "", "hash", "id", "alice", false, member},
		{"email home", "docker_volume", "", "email_local", "id", "alice", false, member},
		{"sub home", "docker_volume", "", "sub", "id", "alice", false, member},
		{"slug scheme", "docker_volume", "", "hash", "slug", "alice", false, member},
		{"host root", "docker_volume", "/srv", "hash", "id", "alice", false, member},
		{"admin host path", "host_path", "/srv", "sub", "slug", "", false, ""},
		{"admin tombstone", "docker_volume", "", "hash", "id", "", true, "user_drives_tombstone_member_only"},
	}
	for _, c := range cases {
		name := c.name + " " + uuid.NewString()[:8]
		if got := insertMemberDriveRow(t, pool, name, strings.ReplaceAll(name, " ", "-"), c.backend, c.hostRoot, c.home, c.scheme,
			c.owner, c.tombstone); got != c.want {
			t.Errorf("%s: refused by %q, want %q", c.name, got, c.want)
		}
	}
}

// TestPG_MemberDrivesUpgrade applies the member-drives migration over 0.9
// admin drives: the rows are unchanged, admin names still fold globally, an
// admin and a member drive may share a name, and a member host_path row fails.
func TestPG_MemberDrivesUpgrade(t *testing.T) {
	mig := memberDrivesMigration(t)
	pool, _ := partialSchemaPool(t, mig)
	ctx := context.Background()

	var kind string
	if err := pool.QueryRow(ctx, `SELECT contype::text FROM pg_constraint
		WHERE conname = 'user_drives_name_key' AND conrelid = 'user_drives'::regclass`).Scan(&kind); err != nil || kind != "u" {
		t.Fatalf("0054's inline UNIQUE(name) is not user_drives_name_key (contype %q, %v); the migration's DROP would miss it", kind, err)
	}
	for _, n := range []string{"alpha", "beta"} {
		if _, err := pool.Exec(ctx, `INSERT INTO user_drives (id, name, name_slug, backend, home_template, size_mib, writable)
			VALUES ($1, $2, $2, 'docker_volume', 'hash', 1024, true)`, uuid.New(), n); err != nil {
			t.Fatalf("seed %s: %v", n, err)
		}
	}
	const rows = `SELECT coalesce(json_agg(json_build_object('id', id, 'name', name, 'slug', name_slug, 'backend', backend,
		'root', host_root, 'home', home_template, 'size', size_mib, 'w', writable, 'reclaim', reclaim, 'scheme', object_scheme,
		'created', created_at, 'updated', updated_at) ORDER BY name), '[]')::text FROM user_drives`
	var before, after string
	if err := pool.QueryRow(ctx, rows).Scan(&before); err != nil {
		t.Fatal(err)
	}

	if err := applyMigration(ctx, pool, mig, readMigration(t, mig)); err != nil {
		t.Fatalf("apply %s: %v", mig, err)
	}
	if err := pool.QueryRow(ctx, rows).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Errorf("admin rows changed on upgrade:\n before %s\n after  %s", before, after)
	}
	var members int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM user_drives WHERE owner_subject <> '' OR delete_started_at IS NOT NULL`).Scan(&members); err != nil || members != 0 {
		t.Errorf("legacy rows read as member or tombstoned: %d, %v", members, err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conname = 'user_drives_name_key'`).Scan(&left); err != nil || left != 0 {
		t.Errorf("user_drives_name_key still present (%d, %v)", left, err)
	}

	steps := []struct {
		what, name, backend, owner, want string
	}{
		{"admin Notes", "Notes", "docker_volume", "", ""},
		{"admin notes beside admin Notes", "notes", "docker_volume", "", "user_drives_name_slug_uniq"},
		{"admin Notes twice", "Notes", "docker_volume", "", "user_drives_admin_name_uniq"},
		{"member notes beside admin Notes", "notes", "docker_volume", "alice", ""},
		{"member Notes beside admin Notes", "Notes", "docker_volume", "bob", ""},
		{"member host_path", "share", "host_path", "alice", "user_drives_member_managed"},
	}
	for _, s := range steps {
		if got := insertMemberDriveRow(t, pool, s.name, strings.ToLower(s.name), s.backend, "", "hash", "id", s.owner, false); got != s.want {
			t.Errorf("%s: refused by %q, want %q", s.what, got, s.want)
		}
	}
}
