// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// bandProbe is the test-only migration the both-orders test adds at the last prefix of band b, so each band holds
// a file in every order before a line ships one. Additive and idempotent, as a patch migration must be.
func bandProbe(b migrationBand) (name, sql string) {
	return fmt.Sprintf("%04d_band_probe.sql", b.last), fmt.Sprintf(
		"CREATE TABLE IF NOT EXISTS band_probe_%04d (k text PRIMARY KEY, v text NOT NULL DEFAULT '');\n"+
			"INSERT INTO band_probe_%04d (k, v) VALUES ('probe', '%04d') ON CONFLICT (k) DO NOTHING;\n",
		b.first, b.first, b.first)
}

// The negative control: an older-band and a newer-band file that add one column with different defaults, so the
// column depends on which runs first. scripts/lib/migration-overlap.py refuses such a pair at arrival.
const (
	bandControlOlder = "ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS band_ctl text DEFAULT 'older';\n"
	bandControlNewer = "ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS band_ctl text DEFAULT 'newer';\n"
)

// bandStateQueries read what the orders must agree on. Each row is one text line; a kind's lines are compared as
// a set. Partitions are left out (the audit partitions are made by month at boot) and so is column position: two
// bands that add columns to one table are refused at arrival, so position cannot differ for a reason that matters.
var bandStateQueries = []struct{ kind, sql string }{
	{"column", `SELECT c.relname || '.' || a.attname || ' ' || format_type(a.atttypid, a.atttypmod)
		|| CASE WHEN a.attnotnull THEN ' not null' ELSE '' END || COALESCE(' default ' || pg_get_expr(d.adbin, d.adrelid), '')
		FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid
		LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		WHERE c.relnamespace = 'public'::regnamespace AND c.relkind IN ('r', 'p', 'v', 'm') AND NOT c.relispartition
		  AND a.attnum > 0 AND NOT a.attisdropped`},
	{"constraint", `SELECT c.conrelid::regclass::text || ' ' || c.conname || ' ' || pg_get_constraintdef(c.oid)
		FROM pg_constraint c WHERE c.connamespace = 'public'::regnamespace
		  AND c.conrelid NOT IN (SELECT oid FROM pg_class WHERE relispartition)`},
	{"not valid", `SELECT c.conrelid::regclass::text || ' ' || c.conname FROM pg_constraint c
		WHERE c.connamespace = 'public'::regnamespace AND NOT c.convalidated`},
	{"index", `SELECT i.tablename || ' ' || i.indexname || ' ' || i.indexdef FROM pg_indexes i
		WHERE i.schemaname = 'public'
		  AND i.tablename NOT IN (SELECT relname FROM pg_class WHERE relispartition AND relnamespace = 'public'::regnamespace)`},
	{"trigger", `SELECT t.tgrelid::regclass::text || ' ' || t.tgname || ' ' || t.tgenabled::text || ' ' || pg_get_triggerdef(t.oid)
		FROM pg_trigger t JOIN pg_class r ON r.oid = t.tgrelid
		WHERE NOT t.tgisinternal AND r.relnamespace = 'public'::regnamespace AND NOT r.relispartition`},
	{"function", `SELECT p.proname || '(' || pg_get_function_identity_arguments(p.oid) || ') ' || md5(pg_get_functiondef(p.oid))
		FROM pg_proc p WHERE p.pronamespace = 'public'::regnamespace AND p.prokind IN ('f', 'p')`},
	{"view", `SELECT viewname || ' ' || md5(definition) FROM pg_views WHERE schemaname = 'public'`},
	{"policy", `SELECT tablename || ' ' || policyname || ' ' || cmd || ' ' || COALESCE(qual, '') || ' ' || COALESCE(with_check, '')
		FROM pg_policies WHERE schemaname = 'public'`},
	{"sequence", `SELECT sequencename FROM pg_sequences WHERE schemaname = 'public'`},
	{"enum", `SELECT t.typname || ' ' || e.enumlabel FROM pg_enum e JOIN pg_type t ON t.oid = e.enumtypid
		WHERE t.typnamespace = 'public'::regnamespace`},
	{"applied", `SELECT filename FROM schema_migrations`},
	{"seeded run", `SELECT id::text || ' ' || created_by || ' ' || agent || ' ' || repo || ' ' || confinement_class::text
		|| ' ' || state::text || ' ' || spiffe_id || ' ' || runner_target::text FROM agent_runs WHERE created_by LIKE 'band-seed-%'`},
	{"seeded audit", `SELECT id::text || ' ' || actor || ' ' || action || ' ' || outcome::text
		FROM audit_events WHERE actor LIKE 'band-seed-%'`},
}

// bandOrderFiles is the filename set every order applies: the shipped migrations and one probe per band.
func bandOrderFiles(t *testing.T, bands []migrationBand) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	for _, name := range readMigrationNames(t) {
		files[name] = []byte(readMigration(t, name))
	}
	for _, b := range bands {
		name, sql := bandProbe(b)
		for have := range files {
			if have[:4] == name[:4] {
				t.Fatalf("the probe %s takes prefix %s, which the shipped %s already has; move the probe", name, name[:4], have)
			}
		}
		files[name] = []byte(sql)
	}
	return files
}

// seedBandRows writes the rows seeded before band b: one run and one audit row, each with an id fixed by b, so
// the orders' databases hold the same rows.
func seedBandRows(t *testing.T, pool *pgxpool.Pool, b migrationBand) {
	t.Helper()
	ctx := context.Background()
	who := fmt.Sprintf("band-seed-%04d", b.first)
	id := uuid.NewSHA1(uuid.NameSpaceURL, []byte("wardyn-band-seed/"+who))
	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_runs
			(id, created_by, agent, repo, confinement_class, state, spiffe_id, runner_target)
		VALUES ($1, $2, 'claude-code', 'acme/widgets', 'CC2', 'COMPLETED', $3, 'docker')`,
		id, who, "spiffe://wardyn.local/run/"+id.String()); err != nil {
		t.Fatalf("seed agent_runs before band %s: %v", b.label(), err)
	}
	if _, err := pool.Exec(ctx, `SELECT audit_append($1, now(), NULL, 'system', $2, 'test.band.seed', '', 'success', '', NULL)`,
		id, who); err != nil {
		t.Fatalf("seed audit_events before band %s: %v", b.label(), err)
	}
}

// applyBandOrder migrates a fresh database in stages: the pre-band set, then for each band of order its seed rows
// and Migrate over every file of the bands applied so far. Each stage's set holds the last one's, as a newer
// binary's does, so the downgrade refusal never fires.
func applyBandOrder(t *testing.T, files map[string][]byte, bands, order []migrationBand) *pgxpool.Pool {
	t.Helper()
	pool := pgDatabaseIsolated(t)
	ctx := context.Background()
	included := map[int]bool{}
	stage := func(what string) {
		fsys := fstest.MapFS{}
		for name, data := range files {
			if b := bandFirstOf(name, bands); b == 0 || included[b] {
				fsys["migrations/"+name] = &fstest.MapFile{Data: data}
			}
		}
		conn, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("%s: acquire: %v", bandOrderName(order), err)
		}
		defer conn.Release()
		if err := migrateOn(ctx, conn, false, fsys); err != nil {
			t.Fatalf("%s: migrate %s: %v", bandOrderName(order), what, err)
		}
	}
	stage("the pre-band set")
	for _, b := range order {
		seedBandRows(t, pool, b)
		included[b.first] = true
		stage("band " + b.label())
	}
	return pool
}

// bandOrderState reads bandStateQueries and each probe table, as kind -> set of lines, and checks the invariants
// one database must hold on its own: the applied set is the whole filename set, every seed row is there once,
// every probe holds its one row, and the audit chain still chains.
func bandOrderState(t *testing.T, pool *pgxpool.Pool, files map[string][]byte, bands, order []migrationBand) map[string]map[string]bool {
	t.Helper()
	ctx := context.Background()
	out := map[string]map[string]bool{}
	read := func(kind, sql string) {
		rows, err := pool.Query(ctx, sql)
		if err != nil {
			t.Fatalf("%s: read %s: %v", bandOrderName(order), kind, err)
		}
		lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("%s: read %s: %v", bandOrderName(order), kind, err)
		}
		set := map[string]bool{}
		for _, l := range lines {
			set[l] = true
		}
		out[kind] = set
	}
	for _, q := range bandStateQueries {
		read(q.kind, q.sql)
	}
	for _, b := range bands {
		read(fmt.Sprintf("probe %04d", b.first), fmt.Sprintf(`SELECT k || ' ' || v FROM band_probe_%04d`, b.first))
	}
	name := bandOrderName(order)
	if got, want := slices.Sorted(maps.Keys(out["applied"])), slices.Sorted(maps.Keys(files)); !slices.Equal(got, want) {
		t.Errorf("%s: schema_migrations records %d files, the set has %d; they must be the same names", name, len(got), len(want))
	}
	for _, kind := range []string{"seeded run", "seeded audit"} {
		if len(out[kind]) != len(bands) {
			t.Errorf("%s: %d %s rows, want %d (one seeded before each band)", name, len(out[kind]), kind, len(bands))
		}
	}
	for _, b := range bands {
		if p := out[fmt.Sprintf("probe %04d", b.first)]; len(p) != 1 || !p[fmt.Sprintf("probe %04d", b.first)] {
			t.Errorf("%s: band_probe_%04d holds %v, want the one row \"probe %04d\"", name, b.first, slices.Sorted(maps.Keys(p)), b.first)
		}
	}
	if err := AuditChainCanary(ctx, pool); err != nil {
		t.Errorf("%s: the audit chain canary failed: %v", name, err)
	}
	return out
}

// diffBandOrderStates lists every line one state has and the other lacks, kind by kind.
func diffBandOrderStates(want, got map[string]map[string]bool) []string {
	var out []string
	kinds := slices.Sorted(maps.Keys(want))
	for k := range got {
		if !slices.Contains(kinds, k) {
			kinds = append(kinds, k)
		}
	}
	for _, kind := range kinds {
		for _, l := range slices.Sorted(maps.Keys(want[kind])) {
			if !got[kind][l] {
				out = append(out, "only in the lexical order: "+kind+": "+l)
			}
		}
		for _, l := range slices.Sorted(maps.Keys(got[kind])) {
			if !want[kind][l] {
				out = append(out, "only in this order: "+kind+": "+l)
			}
		}
	}
	return out
}

// TestPG_MigrateBothBandOrders applies one filename set (the shipped migrations plus a probe in every band of
// migration_bands.tsv) in each order a database can meet the bands, each on its own database, with rows seeded
// before each band, and compares every result with the lexical order a fresh install takes. A forward-ported
// 0.10.x file reaches an RC database after that database ran the 0.11 files, so Migrate runs it later there than
// on a fresh install: the bands hold only if both end with the same schema, seeded data and applied set. The
// second subtest is the control: a pair of files that alter one column differently must make the comparison fail.
func TestPG_MigrateBothBandOrders(t *testing.T) {
	if os.Getenv("WARDYN_TEST_PG") == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping Postgres-backed migration test")
	}
	bands := readMigrationBands(t)
	orders := legalBandOrders(bands)
	files := bandOrderFiles(t, bands)

	t.Run("every legal order matches the lexical one", func(t *testing.T) {
		var want map[string]map[string]bool
		if !t.Run(bandOrderName(orders[0]), func(t *testing.T) {
			want = bandOrderState(t, applyBandOrder(t, files, bands, orders[0]), files, bands, orders[0])
		}) {
			return
		}
		for _, order := range orders[1:] {
			t.Run(bandOrderName(order), func(t *testing.T) {
				got := bandOrderState(t, applyBandOrder(t, files, bands, order), files, bands, order)
				if diff := diffBandOrderStates(want, got); len(diff) > 0 {
					t.Errorf("%s ends differently from the lexical order %s:\n%s", bandOrderName(order), bandOrderName(orders[0]), strings.Join(diff, "\n"))
				}
			})
		}
	})

	t.Run("a conflicting pair is caught", func(t *testing.T) {
		ctl := maps.Clone(files)
		older, newer := fmt.Sprintf("%04d_band_ctl.sql", bands[0].last-1), fmt.Sprintf("%04d_band_ctl.sql", bands[1].last-1)
		for _, n := range []string{older, newer} {
			if _, taken := ctl[n]; taken {
				t.Fatalf("the control file %s takes a shipped prefix; move it", n)
			}
		}
		ctl[older], ctl[newer] = []byte(bandControlOlder), []byte(bandControlNewer)
		i := slices.IndexFunc(orders, func(o []migrationBand) bool {
			return slices.IndexFunc(o, func(b migrationBand) bool { return b.first == bands[1].first }) <
				slices.IndexFunc(o, func(b migrationBand) bool { return b.first == bands[0].first })
		})
		if i < 0 {
			t.Fatalf("no legal order runs band %s before band %s; the control needs one", bands[1].label(), bands[0].label())
		}
		diff := diffBandOrderStates(
			bandOrderState(t, applyBandOrder(t, ctl, bands, orders[0]), ctl, bands, orders[0]),
			bandOrderState(t, applyBandOrder(t, ctl, bands, orders[i]), ctl, bands, orders[i]))
		if !slices.ContainsFunc(diff, func(l string) bool { return strings.Contains(l, "workspaces.band_ctl") }) {
			t.Fatalf("the control pair gives workspaces.band_ctl a different default by order, and the comparison did not report it; it reported:\n%s",
				strings.Join(diff, "\n"))
		}
	})
}
