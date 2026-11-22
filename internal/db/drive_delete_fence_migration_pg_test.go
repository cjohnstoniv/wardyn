// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"fmt"
	"testing"
)

const driveDeleteFenceMigration = "0299"

// TestMigration_DriveDeleteFence_Backfill_PG: a person's Docker volume or Mac disk that exists when 0299 runs is marked
// legacy_unsettled, whatever its stored creation time says (a database clock set back, here a created_at in the
// year 2100; a Mac disk likewise); a row still carrying a delete mark starts with delete_issues 1; an admin drive, a person's other
// backend and a row inserted afterwards are not marked.
func TestMigration_DriveDeleteFence_Backfill_PG(t *testing.T) {
	pool, _ := partialSchemaPool(t, driveDeleteFenceMigration)
	pgExec(t, pool, `INSERT INTO runners (id, owner, public_key, key_fingerprint, state) VALUES ('00000000-0000-0000-0000-0000000000a1', 'person', 'k', 'f', 'claimed')`)
	member := `INSERT INTO user_drives (id, name, name_slug, backend, owner_subject, object_scheme, created_at, delete_started_at, home_runner_id)
		VALUES ($1::uuid, $2, $2, $3, 'person', 'id', $4::timestamptz, $5::timestamptz, $6::uuid)`
	pgExec(t, pool, member, "00000000-0000-0000-0000-000000000001", "legacy-docker", "docker_volume", "2100-01-01", nil, nil)
	pgExec(t, pool, member, "00000000-0000-0000-0000-000000000002", "marked-docker", "docker_volume", "2020-01-01", "2020-01-02", nil)
	pgExec(t, pool, member, "00000000-0000-0000-0000-000000000003", "legacy-pvc", "k8s_pvc", "2020-01-01", nil, nil)
	pgExec(t, pool, member, "00000000-0000-0000-0000-000000000006", "legacy-mac", "mac_disk", "2100-01-01", nil, "00000000-0000-0000-0000-0000000000a1")
	pgExec(t, pool, `INSERT INTO user_drives (id, name, backend) VALUES ('00000000-0000-0000-0000-000000000004', 'admin-drive', 'docker_volume')`)

	migrateBefore(t, pool, "0300")

	type want struct {
		legacy bool
		issues int
	}
	for id, w := range map[string]want{
		"00000000-0000-0000-0000-000000000001": {true, 0},
		"00000000-0000-0000-0000-000000000002": {true, 1},
		"00000000-0000-0000-0000-000000000003": {false, 0},
		"00000000-0000-0000-0000-000000000004": {false, 0},
		"00000000-0000-0000-0000-000000000006": {true, 0},
	} {
		if got := pgScalar[bool](t, pool, `SELECT legacy_unsettled FROM user_drives WHERE id = $1::uuid`, id); got != w.legacy {
			t.Errorf("drive %s legacy_unsettled = %v, want %v", id[len(id)-1:], got, w.legacy)
		}
		if got := pgScalar[int32](t, pool, `SELECT delete_issues FROM user_drives WHERE id = $1::uuid`, id); int(got) != w.issues {
			t.Errorf("drive %s delete_issues = %d, want %d", id[len(id)-1:], got, w.issues)
		}
	}
	pgExec(t, pool, `INSERT INTO user_drives (id, name, name_slug, backend, owner_subject, object_scheme, created_at)
		VALUES ('00000000-0000-0000-0000-000000000005', 'new-docker', 'new-docker', 'docker_volume', 'person', 'id', '1999-01-01')`)
	if pgScalar[bool](t, pool, `SELECT legacy_unsettled FROM user_drives WHERE id = '00000000-0000-0000-0000-000000000005'`) {
		t.Error("a row inserted after the migration is marked legacy_unsettled")
	}
}

// TestMigration_DriveDeleteFence_SplitRoleGrants_PG: the app role, found from the catalog as a role holding UPDATE
// on user_drives (the table the release updates), gets SELECT, INSERT and UPDATE on drive_legacy_releases and
// not DELETE; a role that holds nothing on user_drives gets none of them. The fixture sets no default privileges.
func TestMigration_DriveDeleteFence_SplitRoleGrants_PG(t *testing.T) {
	pool, schema := partialSchemaPool(t, driveDeleteFenceMigration)
	app, _ := auditRole(t, pgPool(t), "wardyn_dd_app")
	other, _ := auditRole(t, pgPool(t), "wardyn_dd_other")
	for _, role := range []string{app, other} {
		pgExec(t, pool, fmt.Sprintf(`GRANT USAGE ON SCHEMA %s TO %s`, schema, role))
	}
	pgExec(t, pool, `GRANT SELECT, INSERT, UPDATE, DELETE ON user_drives TO `+app)

	migrateBefore(t, pool, "0300")

	for priv, want := range map[string]bool{"SELECT": true, "INSERT": true, "UPDATE": true, "DELETE": false, "TRUNCATE": false} {
		if got := pgScalar[bool](t, pool, `SELECT has_table_privilege($1, 'drive_legacy_releases', $2)`, app, priv); got != want {
			t.Errorf("app role %s on drive_legacy_releases = %v, want %v", priv, got, want)
		}
		if pgScalar[bool](t, pool, `SELECT has_table_privilege($1, 'drive_legacy_releases', $2)`, other, priv) {
			t.Errorf("a role with nothing on user_drives holds %s on drive_legacy_releases", priv)
		}
	}
}
