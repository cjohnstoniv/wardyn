// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// refuseAppSchemaDrift is the split-DSN boot's check that the app pool serves the schema the migrate
// pool just migrated. Migrations, and with them the refusal of a schema a newer wardynd migrated, run
// on the migrate pool only, so an app DSN whose database or search_path reaches another schema (a
// restore a newer release migrated, or one missing a migration) would be served unchecked. Both
// pools must record the same applied migrations. WARDYN_ALLOW_UNKNOWN_MIGRATIONS does not cover a
// difference: it lets one schema boot under an older binary, not two schemas stand in for one.
//
// ponytail: compares what each schema records, not which schema it is, so two copies at the same
// migrations pass; the app pool's own audit checks in connectAndMigrate still run on its copy.
func refuseAppSchemaDrift(ctx context.Context, mpool, pool *pgxpool.Pool) error {
	want, err := schemaMigrationNames(ctx, mpool)
	if err != nil {
		return fmt.Errorf("read the applied migrations through WARDYN_PG_MIGRATE_DSN: %w", err)
	}
	got, err := schemaMigrationNames(ctx, pool)
	if err != nil {
		return fmt.Errorf("the app pool (WARDYN_PG_DSN) cannot read schema_migrations, so wardynd cannot tell whether it serves "+
			"the schema WARDYN_PG_MIGRATE_DSN migrated; grant the app role SELECT on schema_migrations, or point WARDYN_PG_DSN "+
			"at the same database and search_path: %w", err)
	}
	var diffs []string
	if extra := onlyIn(got, want); len(extra) > 0 {
		diffs = append(diffs, fmt.Sprintf("%d recorded only on the app pool, newest %q", len(extra), extra[len(extra)-1]))
	}
	if missing := onlyIn(want, got); len(missing) > 0 {
		diffs = append(diffs, fmt.Sprintf("%d missing from it, newest %q", len(missing), missing[len(missing)-1]))
	}
	if len(diffs) == 0 {
		return nil
	}
	return fmt.Errorf("the app pool (WARDYN_PG_DSN) reaches a schema whose applied migrations differ from the one "+
		"WARDYN_PG_MIGRATE_DSN migrated (%s); point both DSNs at the same database and search_path", strings.Join(diffs, "; "))
}

// schemaMigrationNames lists schema_migrations in byte order, as the downgrade refusal does.
func schemaMigrationNames(ctx context.Context, p *pgxpool.Pool) ([]string, error) {
	var names []string
	err := p.QueryRow(ctx, `SELECT COALESCE(array_agg(filename ORDER BY filename COLLATE "C"), ARRAY[]::text[]) FROM schema_migrations`).Scan(&names)
	return names, err
}

// onlyIn returns the names of a that b does not hold, in a's order.
func onlyIn(a, b []string) []string {
	var out []string
	for _, n := range a {
		if !slices.Contains(b, n) {
			out = append(out, n)
		}
	}
	return out
}
