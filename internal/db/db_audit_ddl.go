// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AuditDDLProtected reports whether pool's role is UNABLE to bypass the
// audit_events append-only triggers: not a superuser or MEMBER (via the grant
// chain, not just ownership) of the owner role of audit_events, of any of its
// partitions or of its bookkeeping tables, holds no TRIGGER privilege on any of
// them, owns no function in their schema or run by their triggers, cannot
// CREATE in that schema, finds no SECURITY DEFINER function there whose caller
// chooses its search_path, and cannot SET session_replication_role (silences
// every simply-enabled trigger, no DDL needed). Fails safe on any ambiguity
// (missing table, error). Delegates to AuditDDLBypassRoutes so no second query
// can disagree.
//
// SECURITY: TRIGGER privilege alone — without owning or superusing the table
// — lets a role add its own BEFORE INSERT trigger and rewrite any field
// (prev_hash/row_hash included) before the chain trigger hashes the forgery;
// ensureAuditTriggers guards the trigger SHAPE, this only checks whether the
// privilege to create one is held.
//
// Every leg that reads a role's rights reads them for every role the session
// role or current_user can SET ROLE into, not for current_user alone: reading
// rolsuper or has_table_privilege(current_user, …) misses the managed-Postgres
// shape (GRANT admin role TO app role, NOINHERIT) that can SET ROLE to the
// holder while every direct check reports protected, and a pool whose
// connection sets a role can RESET ROLE to the one it logged in as.
//
// Deliberately NOT modeled: pg_write_all_data (write rights, but append-only
// triggers still fire) and host-level escalation other than the three predefined
// roles that give it (a compromised host survives no claim here anyway).
func AuditDDLProtected(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	routes, err := AuditDDLBypassRoutes(ctx, pool)
	if err != nil {
		return false, err
	}
	return len(routes) == 0, nil
}

// The routes, in the words the boot log hands an operator; the remedy differs
// per route (first two: a different connecting role; TRIGGER: a REVOKE;
// function: ALTER FUNCTION … OWNER TO the migrator; CREATE: a REVOKE on the
// schema; unpinned: ALTER FUNCTION … SET search_path; last: a REVOKE on a
// PARAMETER).
const (
	auditBypassSuperuser = "membership in a superuser role, or in pg_execute_server_program, pg_read_server_files, pg_write_server_files or " +
		"(before PostgreSQL 16) a role with CREATEROLE, each of which can be used to gain a superuser's access"
	auditBypassOwner   = "membership in the owner role of audit_events, of one of its partitions or of one of its bookkeeping tables"
	auditBypassTrigger = "the TRIGGER privilege on audit_events, one of its partitions or one of its bookkeeping tables, held directly or " +
		"through a role this one can SET ROLE into (lets the role add its own trigger, which runs as the audit functions' owner or rewrites the row)"
	auditBypassFunction = "membership in the owner role of a function in audit_events' schema or of a function its triggers run " +
		"(the owner can alter that function, or drop it together with the triggers that run it)"
	auditBypassCreate = "the CREATE privilege on audit_events' schema, held directly or through a role this one can SET ROLE into " +
		"(migrations, and any function whose search_path names that schema, look up the names they call there, so a function " +
		"created there can run as the migrator or as that function's owner)"
	auditBypassUnpinned = "a SECURITY DEFINER function in audit_events' schema, or run by its triggers, whose own search_path is not set " +
		"or does not end in pg_temp (the names it uses can then resolve to the caller's own tables or functions, which it reads or runs as its owner)"
	auditBypassReplica = "the SET privilege on the session_replication_role parameter, held directly or through a role this one can SET ROLE into " +
		"(silences every simply-enabled trigger for the session, with no DDL at all)"
	auditBypassNoTable = "audit_events was not found, so no protection can be claimed for it"
)

// AuditDDLBypassRoutes names EVERY route by which pool's role can reach past
// audit_events' append-only guard. An empty slice means protected (what
// AuditDDLProtected checks for). Named routes let the boot log tell an
// operator which one to close, since the fix differs per route and a bare
// bool would mislead. Fails safe: a missing table, or any error, is a bypass.
func AuditDDLBypassRoutes(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	// SECURITY: tree is every relation named audit_events and every partition
	// under it: a row is stored in, and its clone of each trigger fires on, the
	// leaf it is routed to, so TRIGGER on or ownership of one partition is a
	// bypass whatever the parent's ACL says. guard adds the bookkeeping tables
	// audit_append and audit_retention_drop write as their owner, where a
	// trigger runs as that owner. reach is every role the session role or
	// current_user can SET ROLE into, themselves included (see
	// AuditDDLProtected). funcs is every function the guard can run: what the
	// audit functions call is in pg_catalog or their own schema, and a
	// trigger's function may be anywhere. A SECURITY DEFINER one with no
	// search_path of its own, or one that does not end in pg_temp (where
	// PostgreSQL otherwise looks relation names up first), looks names up where
	// its caller chooses; pg_temp counts only as the whole last element, never as
	// the end of another schema's name. The superuser leg also counts the three
	// predefined roles that act on the server's files and programs, and, before
	// PostgreSQL 16, CREATEROLE, which could grant itself any role but a
	// superuser one.
	var found, superuser, owner, trigger, function, create, unpinned bool
	err := pool.QueryRow(ctx, `
		WITH RECURSIVE tree(oid) AS (
			SELECT c.oid FROM pg_class c WHERE c.relname = 'audit_events' AND c.relkind IN ('r', 'p')
			UNION ALL
			SELECT i.inhrelid FROM pg_inherits i JOIN tree ON i.inhparent = tree.oid),
		reach(oid) AS (
			SELECT r.oid FROM pg_roles r
			 WHERE pg_has_role(session_user, r.oid, 'MEMBER') OR pg_has_role(current_user, r.oid, 'MEMBER')),
		nsp(oid) AS (SELECT DISTINCT c.relnamespace FROM pg_class c JOIN tree ON tree.oid = c.oid),
		guard(oid) AS (
			SELECT oid FROM tree
			UNION
			SELECT c.oid FROM pg_class c
			 WHERE c.relkind = 'r' AND c.relnamespace IN (SELECT oid FROM nsp)
			   AND c.relname IN ('audit_partition_meta', 'audit_chain_anchors')),
		funcs(proowner, unpinned) AS (
			SELECT p.proowner, p.prosecdef AND NOT EXISTS (
			           SELECT 1 FROM pg_catalog.unnest(p.proconfig) AS s(v)
			            WHERE s.v = 'search_path=pg_temp' OR s.v LIKE 'search\_path=%,pg\_temp' OR s.v LIKE 'search\_path=%, pg\_temp')
			  FROM pg_proc p
			 WHERE p.pronamespace IN (SELECT oid FROM nsp)
			    OR p.oid IN (SELECT g.tgfoid FROM pg_trigger g JOIN guard ON guard.oid = g.tgrelid))
		SELECT EXISTS (SELECT 1 FROM tree),
		       EXISTS (SELECT 1 FROM pg_roles s JOIN reach ON reach.oid = s.oid
		                WHERE s.rolsuper
		                   OR s.rolname IN ('pg_execute_server_program', 'pg_read_server_files', 'pg_write_server_files')
		                   OR (s.rolcreaterole AND current_setting('server_version_num')::int < 160000)),
		       EXISTS (SELECT 1 FROM pg_class c JOIN guard ON guard.oid = c.oid JOIN reach ON reach.oid = c.relowner),
		       EXISTS (SELECT 1 FROM guard, reach WHERE has_table_privilege(reach.oid, guard.oid, 'TRIGGER')),
		       EXISTS (SELECT 1 FROM funcs JOIN reach ON reach.oid = funcs.proowner),
		       EXISTS (SELECT 1 FROM nsp, reach WHERE has_schema_privilege(reach.oid, nsp.oid, 'CREATE')),
		       EXISTS (SELECT 1 FROM funcs WHERE unpinned)`,
	).Scan(&found, &superuser, &owner, &trigger, &function, &create, &unpinned)
	if err != nil {
		return nil, fmt.Errorf("db: check audit ddl protection: %w", err)
	}
	if !found {
		return []string{auditBypassNoTable}, nil
	}
	var routes []string
	if superuser {
		routes = append(routes, auditBypassSuperuser)
	}
	if owner {
		routes = append(routes, auditBypassOwner)
	}
	if trigger {
		routes = append(routes, auditBypassTrigger)
	}
	if function {
		routes = append(routes, auditBypassFunction)
	}
	if create {
		routes = append(routes, auditBypassCreate)
	}
	if unpinned {
		routes = append(routes, auditBypassUnpinned)
	}
	if len(routes) > 0 {
		// Verdict already decided; don't fail fatally on the version probe below.
		return routes, nil
	}
	// SECURITY: the last leg isn't DDL — SET session_replication_role =
	// 'replica' stops every SIMPLY-ENABLED ('O') trigger for the session, so a
	// role holding only INSERT appends rows past all three audit triggers,
	// unchained, while the boot logs the deployment as protected. ENABLE
	// ALWAYS is the hardening for it, which is why auditForeignTriggers reads
	// tgenabled 'R' as armed.
	//
	// Separate, version-guarded query: has_parameter_privilege doesn't exist
	// before PostgreSQL 15, and a missing function is a PARSE error even in an
	// untaken CASE branch, so folding this leg in would hard-refuse every
	// pre-15 split-role boot. Skipping it there is correct, not a gap:
	// parameter-level GRANT didn't exist before 15 either, so only a superuser
	// could set the GUC, and the first leg covers that.
	var granular bool
	if err := pool.QueryRow(ctx,
		`SELECT current_setting('server_version_num')::int >= 150000`).Scan(&granular); err != nil {
		return nil, fmt.Errorf("db: read server version for the session_replication_role check: %w", err)
	}
	if !granular {
		return nil, nil
	}
	// Follows role membership like the other legs: has_parameter_privilege
	// (current_user, …) alone misses the same admin-role-grant shape the
	// superuser leg guards against, so the EXISTS below asks the same question
	// of every role the session role or current_user can reach.
	//
	// SECURITY: 'MEMBER' rather than 'SET' is the CONSERVATIVE choice — a
	// membership granted WITH SET FALSE (PostgreSQL 16+) still reports MEMBER
	// = true, so this can only over-report a bypass, never under-report one,
	// this function's required direction of error. 'SET' is also not a
	// recognised pg_has_role privilege before PostgreSQL 16.
	var canSilenceTriggers bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_roles r
			 WHERE (pg_has_role(session_user, r.oid, 'MEMBER') OR pg_has_role(current_user, r.oid, 'MEMBER'))
			   AND has_parameter_privilege(r.oid, 'session_replication_role', 'SET'))`,
	).Scan(&canSilenceTriggers); err != nil {
		return nil, fmt.Errorf("db: check session_replication_role privilege: %w", err)
	}
	if canSilenceTriggers {
		return []string{auditBypassReplica}, nil
	}
	return nil, nil
}
