// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AuditDDLProtected reports whether pool's role is UNABLE to bypass the
// audit_events append-only triggers: it is neither a superuser nor a MEMBER
// (not just direct owner — membership follows the grant chain) of the table's
// owner role, does not hold the TRIGGER privilege on the table, and cannot SET
// session_replication_role, which silences every simply-enabled trigger
// without touching DDL at all. Fails safe: any ambiguity (missing table,
// error) reports NOT protected.
//
// The legs live in AuditDDLBypassRoutes, which names the routes that fired for
// the boot log; this function is that answer as a bool, with no second query
// that could disagree with it.
//
// The TRIGGER privilege matters because a role holding it — without owning or
// superusing the table — can CREATE its own BEFORE INSERT trigger and rewrite
// any field (prev_hash/row_hash included) before the shipped chain trigger
// hashes the forgery, regardless of trigger name order; ensureAuditTriggers
// guards against this by trigger SHAPE, and this function only asks whether
// the privilege to create one is held (0007_audit_least_privilege.sql revokes
// it from PUBLIC, but a deploy is free to grant it back).
//
// All four role legs test MEMBERSHIP, not a role attribute — reading rolsuper
// off current_user directly misses the ordinary managed-Postgres shape (GRANT
// some admin role TO the app role, NOINHERIT), which could SET ROLE to a
// superuser while every direct-attribute check reports PROTECTED. pg_has_role
// with 'MEMBER' follows the grant chain to any depth and ignores INHERIT.
//
// Deliberately NOT modeled: pg_write_all_data membership (confers write
// rights but the append-only triggers still fire and raise) and host-level
// roles like pg_execute_server_program (can escalate to superuser, but once
// the database host is compromised no claim here survives anyway).
func AuditDDLProtected(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	routes, err := AuditDDLBypassRoutes(ctx, pool)
	if err != nil {
		return false, err
	}
	return len(routes) == 0, nil
}

// The four routes, in the words the boot log hands an operator. The remedy
// differs per route: the first two need a different connecting role, the
// third a REVOKE, the fourth a REVOKE on a PARAMETER.
const (
	auditBypassSuperuser = "membership in a superuser role"
	auditBypassOwner     = "membership in audit_events' owner role"
	auditBypassTrigger   = "the TRIGGER privilege on audit_events (lets the role add its own BEFORE INSERT trigger and rewrite the row)"
	auditBypassReplica   = "the SET privilege on the session_replication_role parameter, held directly or through a role this one can SET ROLE into " +
		"(silences every simply-enabled trigger for the session, with no DDL at all)"
	auditBypassNoTable = "audit_events was not found, so no protection can be claimed for it"
)

// AuditDDLBypassRoutes names EVERY route by which pool's role can reach past
// audit_events' append-only guard. An empty slice means protected, and is what
// AuditDDLProtected is defined as (len(routes) == 0, no second query to
// disagree with it). Named routes let the boot log tell an operator which one
// to close, since the fix differs per route and a bare bool would mislead.
// Fails safe: a missing table, or any error, is a bypass rather than a
// protection claim.
func AuditDDLBypassRoutes(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	var found, superuser, owner, trigger bool
	err := pool.QueryRow(ctx, `
		SELECT count(*) > 0,
		       COALESCE(bool_or(EXISTS (SELECT 1 FROM pg_roles s
		                                 WHERE s.rolsuper
		                                   AND pg_has_role(current_user, s.oid, 'MEMBER'))), false),
		       COALESCE(bool_or(pg_has_role(current_user, c.relowner, 'MEMBER')), false),
		       COALESCE(bool_or(has_table_privilege(current_user, c.oid, 'TRIGGER')), false)
		FROM pg_class c
		WHERE c.relname = 'audit_events' AND c.relkind = 'r'`,
	).Scan(&found, &superuser, &owner, &trigger)
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
	if len(routes) > 0 {
		// Verdict already decided; a boot reaching a clean refusal must not
		// fail fatally on the version probe below instead.
		return routes, nil
	}
	// The fourth leg is not a DDL one: SET session_replication_role = 'replica'
	// makes every SIMPLY-ENABLED ('O') trigger stop firing for the session, so
	// a role holding only INSERT appends rows past all three guards above,
	// unchained, while the boot logs the deployment as protected (verified in
	// the probe beside this function: row_hash came back NULL). ENABLE ALWAYS
	// is the documented hardening for it, which is why auditForeignTriggers
	// reads tgenabled 'R' as armed.
	//
	// Separate, version-guarded query: has_parameter_privilege does not exist
	// before PostgreSQL 15 and a missing function is a PARSE error even inside
	// an untaken CASE branch, so folding this leg into the query above would
	// turn every pre-15 split-role boot into a hard refusal. Skipping it there
	// is correct, not a gap — parameter-level GRANT didn't exist before 15
	// either, so only a superuser could set the GUC, and the first leg covers
	// that.
	var granular bool
	if err := pool.QueryRow(ctx,
		`SELECT current_setting('server_version_num')::int >= 150000`).Scan(&granular); err != nil {
		return nil, fmt.Errorf("db: read server version for the session_replication_role check: %w", err)
	}
	if !granular {
		return nil, nil
	}
	// Follows role membership like the other legs: asking
	// has_parameter_privilege(current_user, …) alone misses the same
	// admin-role-grant shape the superuser leg guards against (verified in the
	// probe: row_hash came back NULL under exactly that grant). The EXISTS
	// below asks the same question of every role current_user can reach.
	//
	// 'MEMBER' rather than 'SET': it's the CONSERVATIVE choice, since a
	// membership granted WITH SET FALSE (PostgreSQL 16+) still reports MEMBER
	// = true, so this can only under-report a bypass, never over-report one —
	// this function's required direction of error. 'SET' is also not a
	// recognised pg_has_role privilege before PostgreSQL 16.
	var canSilenceTriggers bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_roles r
			 WHERE pg_has_role(current_user, r.oid, 'MEMBER')
			   AND has_parameter_privilege(r.oid, 'session_replication_role', 'SET'))`,
	).Scan(&canSilenceTriggers); err != nil {
		return nil, fmt.Errorf("db: check session_replication_role privilege: %w", err)
	}
	if canSilenceTriggers {
		return []string{auditBypassReplica}, nil
	}
	return nil, nil
}
