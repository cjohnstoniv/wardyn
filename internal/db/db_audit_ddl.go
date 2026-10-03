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
// chain, not just ownership) of the owner role, holds no TRIGGER privilege,
// and cannot SET session_replication_role (silences every simply-enabled
// trigger, no DDL needed). Fails safe on any ambiguity (missing table, error).
// Delegates to AuditDDLBypassRoutes so no second query can disagree.
//
// SECURITY: TRIGGER privilege alone — without owning or superusing the table
// — lets a role add its own BEFORE INSERT trigger and rewrite any field
// (prev_hash/row_hash included) before the chain trigger hashes the forgery;
// ensureAuditTriggers guards the trigger SHAPE, this only checks whether the
// privilege to create one is held.
//
// All four legs test MEMBERSHIP, not a role attribute: reading rolsuper off
// current_user directly misses the managed-Postgres shape (GRANT admin role
// TO app role, NOINHERIT) that can SET ROLE to superuser while every
// direct-attribute check reports protected.
//
// Deliberately NOT modeled: pg_write_all_data (write rights, but append-only
// triggers still fire) and host-level escalation (a compromised host survives
// no claim here anyway).
func AuditDDLProtected(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	routes, err := AuditDDLBypassRoutes(ctx, pool)
	if err != nil {
		return false, err
	}
	return len(routes) == 0, nil
}

// The four routes, in the words the boot log hands an operator; the remedy
// differs per route (first two: a different connecting role; third: a
// REVOKE; fourth: a REVOKE on a PARAMETER).
const (
	auditBypassSuperuser = "membership in a superuser role"
	auditBypassOwner     = "membership in audit_events' owner role"
	auditBypassTrigger   = "the TRIGGER privilege on audit_events (lets the role add its own BEFORE INSERT trigger and rewrite the row)"
	auditBypassReplica   = "the SET privilege on the session_replication_role parameter, held directly or through a role this one can SET ROLE into " +
		"(silences every simply-enabled trigger for the session, with no DDL at all)"
	auditBypassNoTable = "audit_events was not found, so no protection can be claimed for it"
)

// AuditDDLBypassRoutes names EVERY route by which pool's role can reach past
// audit_events' append-only guard. An empty slice means protected (what
// AuditDDLProtected checks for). Named routes let the boot log tell an
// operator which one to close, since the fix differs per route and a bare
// bool would mislead. Fails safe: a missing table, or any error, is a bypass.
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
		WHERE c.relname = 'audit_events' AND c.relkind IN ('r', 'p')`,
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
		// Verdict already decided; don't fail fatally on the version probe below.
		return routes, nil
	}
	// SECURITY: the fourth leg isn't DDL — SET session_replication_role =
	// 'replica' stops every SIMPLY-ENABLED ('O') trigger for the session, so a
	// role holding only INSERT appends rows past all three guards above,
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
	// of every role current_user can reach.
	//
	// SECURITY: 'MEMBER' rather than 'SET' is the CONSERVATIVE choice — a
	// membership granted WITH SET FALSE (PostgreSQL 16+) still reports MEMBER
	// = true, so this can only under-report a bypass, never over-report one,
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
