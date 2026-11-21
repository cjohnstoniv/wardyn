// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

const providerServiceGrantMigration = "0295"

// TestMigration_ProviderServiceGrant_PG: the migration creates one table, keyed by provider and replica, whose
// outcome is ok or failed, and grants it to the role that already writes the delegates, found from the
// catalog; a role that only reads them gets nothing, and nobody gets TRUNCATE.
func TestMigration_ProviderServiceGrant_PG(t *testing.T) {
	pool, schema := partialSchemaPool(t, providerServiceGrantMigration)
	app, _ := auditRole(t, pgPool(t), "wardyn_pg_app")
	reader, _ := auditRole(t, pgPool(t), "wardyn_pg_reader")
	for _, role := range []string{app, reader} {
		pgExec(t, pool, fmt.Sprintf(`GRANT USAGE ON SCHEMA %s TO %s`, schema, role))
	}
	pgExec(t, pool, `GRANT SELECT, INSERT, UPDATE, DELETE ON delegates TO `+app)
	pgExec(t, pool, `GRANT SELECT ON delegates TO `+reader)

	migrateBefore(t, pool, "0296")

	for priv, want := range map[string]bool{"SELECT": true, "INSERT": true, "UPDATE": true, "DELETE": true, "TRUNCATE": false} {
		if got := pgScalar[bool](t, pool, `SELECT has_table_privilege($1, 'provider_service_grant_status', $2)`, app, priv); got != want {
			t.Errorf("app role %s on provider_service_grant_status = %v, want %v", priv, got, want)
		}
		if pgScalar[bool](t, pool, `SELECT has_table_privilege($1, 'provider_service_grant_status', $2)`, reader, priv) {
			t.Errorf("a role that only reads delegates holds %s on provider_service_grant_status", priv)
		}
	}

	insert := `INSERT INTO provider_service_grant_status (provider_uid, replica_id, replica_host, grant_digest, client_id, outcome, attempted_at, seen_at)
		VALUES ('p1', $1, 'host', 'd', 'c', $2, now(), now())`
	pgExec(t, pool, insert, "r1", "ok")
	_, err := pool.Exec(context.Background(), insert, "r1", "ok")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Errorf("a second row for one provider and replica = %v, want a unique violation", err)
	}
	_, err = pool.Exec(context.Background(), insert, "r2", "maybe")
	if !errors.As(err, &pgErr) || !strings.HasPrefix(pgErr.Code, "23514") {
		t.Errorf("an outcome that is not ok or failed = %v, want a CHECK violation", err)
	}
}
