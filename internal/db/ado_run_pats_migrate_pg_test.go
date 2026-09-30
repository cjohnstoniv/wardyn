// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// TestMigrate_AdoRunPATsTable (0102, #1428) pins the run-token table through
// the catalog and the server: the columns and their nullability, the
// (run_id, authorization_id) key refusing a second row, and that a run can
// hold several tokens (renewal and widening leave the old one to its valid_to).
func TestMigrate_AdoRunPATsTable(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()

	rows, err := pool.Query(ctx, `SELECT column_name, is_nullable, data_type FROM information_schema.columns
		WHERE table_name = 'ado_run_pats' ORDER BY ordinal_position`)
	if err != nil {
		t.Fatalf("read columns: %v", err)
	}
	type col struct{ nullable, typ string }
	got := map[string]col{}
	var order []string
	for rows.Next() {
		var name string
		var c col
		if err := rows.Scan(&name, &c.nullable, &c.typ); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[name] = c
		order = append(order, name)
	}
	rows.Close()
	want := []struct{ name, nullable, typ string }{
		{"run_id", "NO", "uuid"}, {"authorization_id", "NO", "uuid"}, {"owner", "NO", "text"},
		{"provider_row_id", "NO", "text"}, {"org", "NO", "text"}, {"scope", "NO", "text"},
		{"valid_to", "NO", "timestamp with time zone"}, {"created_at", "NO", "timestamp with time zone"},
		{"revoked_at", "YES", "timestamp with time zone"}, {"revoke_reason", "NO", "text"}, {"last_error", "NO", "text"},
	}
	if len(order) != len(want) {
		t.Fatalf("ado_run_pats columns = %v, want %d", order, len(want))
	}
	for i, w := range want {
		if order[i] != w.name || got[w.name].nullable != w.nullable || got[w.name].typ != w.typ {
			t.Errorf("column %d = %s %+v, want %s nullable=%s %s", i, order[i], got[order[i]], w.name, w.nullable, w.typ)
		}
	}

	run, auth := uuid.New(), uuid.New()
	insert := func(a uuid.UUID) error {
		_, err := pool.Exec(ctx, `INSERT INTO ado_run_pats (run_id, authorization_id, owner, provider_row_id, org, scope, valid_to)
			VALUES ($1, $2, 'oidc:alice', 'ado', 'contoso', 'vso.code', now() + interval '1 hour')`, run, a)
		return err
	}
	if err := insert(auth); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	var pgErr *pgconn.PgError
	if err := insert(auth); !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Errorf("second insert of the same (run, authorization) = %v, want a unique violation", err)
	}
	if err := insert(uuid.New()); err != nil {
		t.Errorf("a second token for the same run: %v", err)
	}
}
