// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// Pins for the routes past audit_events' append-only guard that the boot check
// did not count: TRIGGER held by a role the app role can SET ROLE into but does
// not inherit from, or that its session role reaches; TRIGGER on, or ownership
// of, one partition or bookkeeping table and not the parent; ownership of a
// function the guard runs; CREATE on the schema; a SECURITY DEFINER function
// there whose caller picks where it looks names up; membership in a predefined
// role that reads or writes the server's files or runs its programs, or in one
// with CREATEROLE before PostgreSQL 16; and the SET privilege on
// session_replication_role reached through the session role. Where a subtest
// can show the route is real as the app role, it does so in a transaction it
// rolls back, then asserts the route is named. The CREATE subtest also shows
// that the audit functions no longer run a function the app role adds to their
// schema. One throwaway database (pgPoolIsolated), because the function subtest
// hands audit_events_chain() to another role, which would stop every other
// test's audit append on the shared one.

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/testfloor"
)

func TestPG_AuditDDLBypassRoutesCountWhatTheAppRoleCanReach(t *testing.T) {
	testfloor.Mark(t, "pg")
	pool := pgPoolIsolated(t)
	ctx := context.Background()

	var super bool
	var dbName string
	if err := pool.QueryRow(ctx, `SELECT rolsuper, current_database() FROM pg_roles WHERE rolname = current_user`).Scan(&super, &dbName); err != nil {
		t.Fatalf("read role: %v", err)
	}
	base, err := url.Parse(os.Getenv("WARDYN_TEST_PG"))
	urlDSN := err == nil && base.Scheme != "" && base.Host != ""
	mustNotSkip := probeMustNotSkip(t, super, urlDSN)
	if !super {
		skipOrFatal(t, mustNotSkip, "WARDYN_TEST_PG role is not a superuser; handing a shipped function to another role needs it")
	}
	if !urlDSN {
		skipOrFatal(t, mustNotSkip, "WARDYN_TEST_PG is not a URL-form DSN; cannot derive an app-role DSN from it")
	}
	base.Path = "/" + dbName

	must := func(t *testing.T, q string) {
		t.Helper()
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// PostgreSQL 14 and earlier grant CREATE on public to every role; this
	// database takes it back so each subtest holds only the route it grants.
	must(t, `REVOKE CREATE ON SCHEMA public FROM PUBLIC`)

	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000_000)
	const pw = "reach-probe-pw"
	// connect opens a pool as role, with any extra connection parameters.
	connect := func(t *testing.T, role string, params url.Values) *pgxpool.Pool {
		t.Helper()
		u := *base
		u.User = url.UserPassword(role, pw)
		q := u.Query()
		for k, v := range params {
			q[k] = v
		}
		u.RawQuery = q.Encode()
		p, err := Connect(ctx, u.String())
		if err != nil {
			skipOrFatal(t, mustNotSkip, "cannot connect as the app role (pg_hba may not password-auth a new role): %v", err)
		}
		t.Cleanup(p.Close)
		return p
	}
	// newApp creates a NOINHERIT login role with the documented app posture
	// (USAGE on the schema, SELECT on audit_events, EXECUTE on audit_append) and
	// a NOLOGIN role it can SET ROLE into, and connects as the first.
	newApp := func(t *testing.T, tag string) (app, adm string, appPool *pgxpool.Pool) {
		t.Helper()
		app, adm = "wardyn_reach_app_"+tag+"_"+suffix, "wardyn_reach_adm_"+tag+"_"+suffix
		t.Cleanup(func() {
			for _, q := range []string{`DROP OWNED BY ` + app + `, ` + adm, `DROP ROLE IF EXISTS ` + app, `DROP ROLE IF EXISTS ` + adm} {
				if _, err := pool.Exec(context.Background(), q); err != nil {
					t.Errorf("cleanup %s: %v", q, err)
				}
			}
		})
		must(t, `CREATE ROLE `+adm+` NOLOGIN`)
		must(t, `CREATE ROLE `+app+` NOINHERIT LOGIN PASSWORD '`+pw+`'`)
		must(t, `GRANT `+adm+` TO `+app)
		must(t, `GRANT USAGE ON SCHEMA public TO `+app)
		must(t, `GRANT SELECT ON audit_events TO `+app)
		must(t, `GRANT EXECUTE ON FUNCTION audit_append(uuid, timestamptz, uuid, text, text, text, text, text, text, jsonb) TO `+app)
		return app, adm, connect(t, app, nil)
	}
	begin := func(t *testing.T, p *pgxpool.Pool) pgx.Tx {
		t.Helper()
		tx, err := p.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		t.Cleanup(func() { tx.Rollback(context.Background()) }) //nolint:errcheck
		return tx
	}
	routesOf := func(t *testing.T, appPool *pgxpool.Pool) []string {
		t.Helper()
		routes, err := AuditDDLBypassRoutes(ctx, appPool)
		if err != nil {
			t.Fatalf("AuditDDLBypassRoutes: %v", err)
		}
		return routes
	}
	named := func(t *testing.T, appPool *pgxpool.Pool, want string) {
		t.Helper()
		if routes := routesOf(t, appPool); !slices.Contains(routes, want) {
			t.Errorf("routes = %v, want %q named: the boot would log the append-only guard as DDL-protected for a "+
				"role that can reach past it", routes, want)
		}
	}
	const appendRow = `SELECT row_hash FROM audit_append(gen_random_uuid(), now(), NULL, 'system', 'reach-probe', 'test.reach.probe', '', 'success', '', NULL)`

	t.Run("the documented app posture names no route", func(t *testing.T) {
		_, _, appPool := newApp(t, "clean")
		if routes := routesOf(t, appPool); len(routes) != 0 {
			t.Fatalf("AuditDDLBypassRoutes = %v; want no route for a role with only the documented grants", routes)
		}
	})

	t.Run("TRIGGER held by a role the app role can SET ROLE into", func(t *testing.T) {
		_, adm, appPool := newApp(t, "trg")
		must(t, `GRANT TRIGGER ON audit_events TO `+adm)
		tx := begin(t, appPool)
		var own, reached bool
		if err := tx.QueryRow(ctx, `SELECT has_table_privilege(current_user, 'audit_events', 'TRIGGER')`).Scan(&own); err != nil {
			t.Fatalf("read TRIGGER as the app role: %v", err)
		}
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE `+adm); err != nil {
			t.Fatalf("SET LOCAL ROLE %s: %v", adm, err)
		}
		if err := tx.QueryRow(ctx, `SELECT has_table_privilege(current_user, 'audit_events', 'TRIGGER')`).Scan(&reached); err != nil {
			t.Fatalf("read TRIGGER after SET ROLE: %v", err)
		}
		if own || !reached {
			t.Fatalf("precondition: TRIGGER as the app role = %v, after SET ROLE %s = %v; want false, true", own, adm, reached)
		}
		named(t, appPool, auditBypassTrigger)
	})

	t.Run("TRIGGER reached through the session role, not the current one", func(t *testing.T) {
		app, adm, _ := newApp(t, "sess")
		cur := "wardyn_reach_cur_" + suffix
		must(t, `CREATE ROLE `+cur+` NOLOGIN`)
		t.Cleanup(func() { pool.Exec(context.Background(), `DROP ROLE IF EXISTS `+cur) }) //nolint:errcheck
		must(t, `GRANT `+cur+` TO `+app)
		must(t, `GRANT TRIGGER ON audit_events TO `+adm)
		// A connection that starts as another role, as a DSN can ask; SET ROLE is still checked against the
		// role that logged in.
		curPool := connect(t, app, url.Values{"options": {"-c role=" + cur}})
		var cu, su string
		if err := curPool.QueryRow(ctx, `SELECT current_user, session_user`).Scan(&cu, &su); err != nil || cu != cur || su != app {
			t.Fatalf("precondition: current_user, session_user = %q, %q (%v); want %q, %q", cu, su, err, cur, app)
		}
		tx := begin(t, curPool)
		var reached bool
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE `+adm); err != nil {
			t.Fatalf("SET LOCAL ROLE %s as %s: %v", adm, cur, err)
		}
		if err := tx.QueryRow(ctx, `SELECT has_table_privilege(current_user, 'audit_events', 'TRIGGER')`).Scan(&reached); err != nil || !reached {
			t.Fatalf("precondition: TRIGGER after SET ROLE %s = %v, %v; want true", adm, reached, err)
		}
		named(t, curPool, auditBypassTrigger)
	})

	t.Run("TRIGGER on, or ownership of, one partition and not the parent", func(t *testing.T) {
		app, adm, appPool := newApp(t, "part")
		var leaf string
		if err := pool.QueryRow(ctx, `SELECT inhrelid::regclass::text FROM pg_inherits WHERE inhparent = 'audit_events'::regclass ORDER BY 1 LIMIT 1`).Scan(&leaf); err != nil {
			t.Fatalf("find a partition of audit_events: %v", err)
		}
		must(t, `GRANT TRIGGER ON `+leaf+` TO `+app)
		must(t, `GRANT EXECUTE ON FUNCTION audit_events_append_only() TO `+app)
		tx := begin(t, appPool)
		// A BEFORE INSERT trigger of the role's own on the partition fires for every row routed there.
		if _, err := tx.Exec(ctx, `CREATE TRIGGER reach_probe BEFORE INSERT ON `+leaf+` FOR EACH ROW EXECUTE FUNCTION audit_events_append_only()`); err != nil {
			t.Fatalf("CREATE TRIGGER on %s as the app role: %v", leaf, err)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("rollback: %v", err)
		}
		named(t, appPool, auditBypassTrigger)

		must(t, `REVOKE TRIGGER ON `+leaf+` FROM `+app)
		must(t, `ALTER TABLE `+leaf+` OWNER TO `+adm)
		t.Cleanup(func() { pool.Exec(context.Background(), `ALTER TABLE `+leaf+` OWNER TO CURRENT_USER`) }) //nolint:errcheck
		named(t, appPool, auditBypassOwner)
	})

	t.Run("TRIGGER on, or ownership of, a bookkeeping table", func(t *testing.T) {
		_, adm, appPool := newApp(t, "book")
		// audit_append writes audit_partition_meta as its owner, so a trigger there runs as that owner.
		must(t, `GRANT TRIGGER ON audit_partition_meta TO `+adm)
		named(t, appPool, auditBypassTrigger)

		must(t, `REVOKE TRIGGER ON audit_partition_meta FROM `+adm)
		must(t, `ALTER TABLE audit_chain_anchors OWNER TO `+adm)
		t.Cleanup(func() { pool.Exec(context.Background(), `ALTER TABLE audit_chain_anchors OWNER TO CURRENT_USER`) }) //nolint:errcheck
		named(t, appPool, auditBypassOwner)
	})

	t.Run("ownership of a function a bookkeeping table's trigger runs, outside the schema", func(t *testing.T) {
		_, adm, appPool := newApp(t, "booktrg")
		// audit_append updates audit_partition_meta as its owner, so this trigger's function runs as that
		// owner, and its owner can replace the body at will.
		sch := "wardyn_reach_trg_" + suffix
		must(t, `CREATE SCHEMA `+sch)
		t.Cleanup(func() { pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+sch+` CASCADE`) }) //nolint:errcheck
		must(t, `CREATE FUNCTION `+sch+`.reach_probe_trg() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$`)
		must(t, `ALTER FUNCTION `+sch+`.reach_probe_trg() OWNER TO `+adm)
		must(t, `CREATE TRIGGER reach_probe_trg BEFORE UPDATE ON audit_partition_meta FOR EACH ROW EXECUTE FUNCTION `+sch+`.reach_probe_trg()`)
		const dropTrg = `DROP TRIGGER IF EXISTS reach_probe_trg ON audit_partition_meta`
		t.Cleanup(func() { pool.Exec(context.Background(), dropTrg) }) //nolint:errcheck
		named(t, appPool, auditBypassFunction)
	})

	t.Run("ownership of a function the guard runs", func(t *testing.T) {
		_, adm, appPool := newApp(t, "fn")
		must(t, `ALTER FUNCTION audit_events_chain() OWNER TO `+adm)
		t.Cleanup(func() { pool.Exec(context.Background(), `ALTER FUNCTION audit_events_chain() OWNER TO CURRENT_USER`) }) //nolint:errcheck
		tx := begin(t, appPool)
		// The owner drops the chain function, and the chain trigger with it: the next append is stored unchained.
		for _, q := range []string{`SET LOCAL ROLE ` + adm, `DROP FUNCTION audit_events_chain() CASCADE`, `RESET ROLE`} {
			if _, err := tx.Exec(ctx, q); err != nil {
				t.Fatalf("%s as the app role: %v", q, err)
			}
		}
		var rowHash *string
		if err := tx.QueryRow(ctx, appendRow).Scan(&rowHash); err != nil {
			t.Fatalf("audit_append after the drop: %v", err)
		}
		if rowHash != nil {
			t.Fatalf("row_hash = %q: the chain trigger still ran after its function was dropped", *rowHash)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("rollback: %v", err)
		}
		named(t, appPool, auditBypassFunction)

		// A function no trigger runs counts too: the chain function calls audit_row_hash as its owner.
		must(t, `ALTER FUNCTION audit_events_chain() OWNER TO CURRENT_USER`)
		const rowHashFn = `audit_row_hash(text, uuid, timestamptz, uuid, text, text, text, text, text, text, jsonb)`
		must(t, `ALTER FUNCTION `+rowHashFn+` OWNER TO `+adm)
		t.Cleanup(func() { pool.Exec(context.Background(), `ALTER FUNCTION `+rowHashFn+` OWNER TO CURRENT_USER`) }) //nolint:errcheck
		named(t, appPool, auditBypassFunction)
	})

	t.Run("CREATE on the schema is named, and the audit functions do not run what it adds", func(t *testing.T) {
		app, _, appPool := newApp(t, "create")
		must(t, `GRANT CREATE ON SCHEMA public TO `+app)
		must(t, `GRANT EXECUTE ON FUNCTION audit_ensure_partitions(integer) TO `+app)
		// Two functions of the app role's own, each a closer match than pg_catalog's for a call the audit
		// functions make: nextval of a quoted name (audit_append) and format of a regclass
		// (audit_harden_relation, which audit_ensure_partitions runs for each month it adds).
		for _, q := range []string{
			`CREATE FUNCTION public.nextval(text) RETURNS bigint LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'reach probe ran as %', current_user; END $$`,
			`CREATE FUNCTION public.format(text, regclass) RETURNS text LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'reach probe ran as %', current_user; END $$`,
		} {
			if _, err := appPool.Exec(ctx, q); err != nil {
				t.Fatalf("%s as the app role: %v", q, err)
			}
		}
		// A caller whose search_path holds the schema runs them.
		for _, q := range []string{`SELECT nextval('audit_events_seq')`, `SELECT format('%s', 'audit_events'::regclass)`} {
			if _, err := appPool.Exec(ctx, q); err == nil || !strings.Contains(err.Error(), "reach probe ran as "+app) {
				t.Fatalf("precondition: %s as the app role = %v; want the app role's own function to run", q, err)
			}
		}
		tx := begin(t, appPool)
		var rowHash *string
		if err := tx.QueryRow(ctx, appendRow).Scan(&rowHash); err != nil || rowHash == nil {
			t.Fatalf("audit_append beside the app role's nextval(text) = row_hash %v, %v; want a chained row, "+
				"with the app role's function never run as audit_append's owner", rowHash, err)
		}
		var added int
		if err := tx.QueryRow(ctx, `SELECT audit_ensure_partitions(24)`).Scan(&added); err != nil || added == 0 {
			t.Fatalf("audit_ensure_partitions(24) beside the app role's format(text, regclass) = %d, %v; want months "+
				"added, with the app role's function never run as their owner", added, err)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("rollback: %v", err)
		}
		// Still a route: migrations look names up in the schema as the migrator.
		routes := routesOf(t, appPool)
		if !slices.Contains(routes, auditBypassCreate) || slices.Contains(routes, auditBypassUnpinned) {
			t.Errorf("routes = %v; want %q named and %q not", routes, auditBypassCreate, auditBypassUnpinned)
		}
	})

	t.Run("a SECURITY DEFINER function whose caller picks where it looks names up", func(t *testing.T) {
		_, _, appPool := newApp(t, "unpin")
		const fn = `public.reach_probe_definer()`
		must(t, `CREATE FUNCTION `+fn+` RETURNS integer LANGUAGE sql SECURITY DEFINER AS 'SELECT 1'`)
		t.Cleanup(func() { pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS `+fn) }) //nolint:errcheck
		named(t, appPool, auditBypassUnpinned)
		// A path of its own that does not end in pg_temp still puts a caller's temporary tables first.
		must(t, `ALTER FUNCTION `+fn+` SET search_path = pg_catalog, public`)
		named(t, appPool, auditBypassUnpinned)
		// A last schema whose name only ends in pg_temp is not pg_temp.
		must(t, `ALTER FUNCTION `+fn+` SET search_path = pg_catalog, wardyn_pg_temp`)
		named(t, appPool, auditBypassUnpinned)
		must(t, `ALTER FUNCTION `+fn+` SET search_path = pg_catalog, pg_temp`)
		if routes := routesOf(t, appPool); slices.Contains(routes, auditBypassUnpinned) {
			t.Errorf("routes = %v; %q is named for a function pinned to pg_catalog, pg_temp", routes, auditBypassUnpinned)
		}
	})

	t.Run("membership in a role that acts on the server's files or programs", func(t *testing.T) {
		for _, c := range []struct {
			role, tag string
			proof     []string
		}{
			{"pg_write_server_files", "srvw", []string{`COPY (SELECT 1) TO '/dev/null'`}},
			{"pg_read_server_files", "srvr", []string{`CREATE TEMP TABLE reach_probe_read (x integer)`, `COPY reach_probe_read FROM '/dev/null'`}},
			{"pg_execute_server_program", "srvx", []string{`COPY (SELECT 1) TO PROGRAM 'cat > /dev/null'`}},
		} {
			t.Run(c.role, func(t *testing.T) {
				_, adm, appPool := newApp(t, c.tag)
				must(t, `GRANT `+c.role+` TO `+adm)
				tx := begin(t, appPool)
				for _, q := range append([]string{`SET LOCAL ROLE ` + adm}, c.proof...) {
					if _, err := tx.Exec(ctx, q); err != nil {
						t.Fatalf("%s as the app role: %v", q, err)
					}
				}
				named(t, appPool, auditBypassSuperuser)
			})
		}
	})

	t.Run("CREATEROLE counts before PostgreSQL 16 only", func(t *testing.T) {
		_, adm, appPool := newApp(t, "crole")
		must(t, `ALTER ROLE `+adm+` CREATEROLE`)
		// Before 16 a role with CREATEROLE can grant itself any role but a superuser one; from 16 it needs
		// ADMIN OPTION on that role.
		var pre16 bool
		if err := pool.QueryRow(ctx, `SELECT current_setting('server_version_num')::int < 160000`).Scan(&pre16); err != nil {
			t.Fatalf("read server version: %v", err)
		}
		routes := routesOf(t, appPool)
		if got := slices.Contains(routes, auditBypassSuperuser); got != pre16 {
			t.Errorf("routes = %v; %q named = %v, want %v on this server", routes, auditBypassSuperuser, got, pre16)
		}
	})

	t.Run("SET on session_replication_role reached through the session role, not the current one", func(t *testing.T) {
		var pg15 bool
		if err := pool.QueryRow(ctx, `SELECT current_setting('server_version_num')::int >= 150000`).Scan(&pg15); err != nil {
			t.Fatalf("read server version: %v", err)
		}
		if !pg15 {
			t.Skip("GRANT ... ON PARAMETER is PostgreSQL 15+")
		}
		app, adm, _ := newApp(t, "repl")
		cur := "wardyn_reach_rcur_" + suffix
		must(t, `CREATE ROLE `+cur+` NOLOGIN`)
		t.Cleanup(func() { pool.Exec(context.Background(), `DROP ROLE IF EXISTS `+cur) }) //nolint:errcheck
		must(t, `GRANT `+cur+` TO `+app)
		must(t, `GRANT SET ON PARAMETER session_replication_role TO `+adm)
		const revoke = `REVOKE SET ON PARAMETER session_replication_role FROM `
		t.Cleanup(func() { pool.Exec(context.Background(), revoke+adm) }) //nolint:errcheck
		curPool := connect(t, app, url.Values{"options": {"-c role=" + cur}})
		tx := begin(t, curPool)
		for _, q := range []string{`SET LOCAL ROLE ` + adm, `SET LOCAL session_replication_role = replica`} {
			if _, err := tx.Exec(ctx, q); err != nil {
				t.Fatalf("%s as %s: %v", q, cur, err)
			}
		}
		named(t, curPool, auditBypassReplica)
	})
}

// routesBeyondPublicCreate drops the CREATE route where PUBLIC itself holds CREATE on audit_events'
// schema (PostgreSQL 14 and earlier grant it on public to every role, and a database upgraded from one
// can keep it). There it names every role, so the clean-posture pins on the shared database discount it,
// and only there.
func routesBeyondPublicCreate(t *testing.T, pool *pgxpool.Pool, routes []string) []string {
	t.Helper()
	var public bool
	if err := pool.QueryRow(context.Background(), `SELECT has_schema_privilege('public', c.relnamespace, 'CREATE')
		FROM pg_class c WHERE c.oid = 'audit_events'::regclass`).Scan(&public); err != nil {
		t.Fatalf("read PUBLIC's CREATE on audit_events' schema: %v", err)
	}
	if !public {
		return routes
	}
	return slices.DeleteFunc(slices.Clone(routes), func(r string) bool { return r == auditBypassCreate })
}
