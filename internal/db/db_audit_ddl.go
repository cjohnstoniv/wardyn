// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AuditDDLProtected reports whether the given (application) pool's role is
// UNABLE to bypass the audit_events append-only triggers — i.e. it is neither a
// superuser nor a MEMBER of the table's owner role (membership, not just direct
// ownership: a role GRANTed the owner role inherits DROP TRIGGER /
// ALTER ... DISABLE TRIGGER rights), it does not hold the TRIGGER privilege on
// the table, AND it cannot SET session_replication_role, which silences every
// simply-enabled trigger without touching DDL at all (the fourth leg, below). The N4 role-separation only protects the append-only guarantee
// when this is true, so the two-DSN deploy must be VERIFIED here rather than
// assumed (honesty: never log a protection claim stronger than the enforcing
// role setup). Fails safe: any ambiguity (missing table, error) reports NOT
// protected.
//
// THE LEGS THEMSELVES LIVE IN AuditDDLBypassRoutes, which answers the same
// question and NAMES the routes that fired so the boot log can tell an operator
// which one to close. This function is that answer as a bool, and there is no
// second query anywhere that could disagree with it.
//
// THE TRIGGER PRIVILEGE IS PART OF THE CLAIM, and it is the least obvious third
// of it. A role that is neither owner nor superuser but holds
// GRANT TRIGGER ON audit_events cannot drop the shipped triggers — it can do
// something quieter: CREATE its own row-level BEFORE INSERT trigger, which is
// handed NEW and whose changes are what Postgres stores — so it can rewrite any
// field, choose prev_hash/row_hash, or drop the row entirely, minting records
// that say whatever it wants while every shipped guard stays armed and every
// catalog check still finds them. Name order is NOT what makes that work: a
// trigger sorting after audit_events_chain (same-event row triggers fire in name
// order) runs last and can overwrite the hashes directly, but one sorting BEFORE
// it is easier still — it rewrites NEW and the shipped chain trigger then hashes
// the forgery for it. ensureAuditTriggers refuses the boot over either, keyed on
// the trigger's SHAPE rather than its name; this function is the PREVENTIVE half
// and only asks whether the privilege to create one is held.
// 0007_audit_least_privilege.sql revokes
// TRIGGER from PUBLIC precisely because of that, but a deploy is free to grant
// it back, so the claim has to be checked and not inferred.
//
// ALL FOUR ROLE LEGS TEST MEMBERSHIP, not a role attribute. The superuser leg asks
// whether current_user is a member of ANY role with rolsuper — not whether
// current_user itself has rolsuper. Reading the attribute off the current_user
// row missed the ordinary managed-Postgres shape (GRANT some admin role TO the
// app role): that role has rolsuper = false, is not a member of the table's
// owner, and holds no TRIGGER privilege, so it was reported PROTECTED while it
// could SET ROLE to a superuser and ALTER TABLE ... DISABLE TRIGGER. pg_has_role
// with 'MEMBER' is what makes this honest: 'MEMBER' is the right to SET ROLE, so
// it follows the grant chain to any depth AND ignores INHERIT — a NOINHERIT role
// that can still SET ROLE is caught. A role is a member of itself, so a directly
// superuser role is reported exactly as it was before.
//
// WHAT THIS DELIBERATELY DOES NOT MODEL, stated so the next reader does not
// widen it by guesswork. Membership in pg_write_all_data is NOT a bypass and is
// NOT tested for: it confers INSERT/UPDATE/DELETE rights, but the append-only
// triggers still fire and raise — measured in the probe beside this function,
// not assumed. Roles that own the HOST rather than the guard —
// pg_execute_server_program, pg_write_server_files — can escalate to superuser
// by documented PostgreSQL behaviour and are still reported protected here. The
// predicate stays a closed, catalog-derived test (rolsuper) rather than a list
// of role names that rots with every Postgres release: once the database host is
// compromised, no claim Wardyn makes about that database survives anyway.
func AuditDDLProtected(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	routes, err := AuditDDLBypassRoutes(ctx, pool)
	if err != nil {
		return false, err
	}
	return len(routes) == 0, nil
}

// The four routes, in the words the boot log hands an operator. Each one names
// the capability AND what it buys, because the remedy differs per route: the
// first two are fixed by connecting as a different role, the third by a REVOKE,
// and the fourth by a REVOKE on a PARAMETER that no amount of table-privilege
// tidying touches.
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
// AuditDDLProtected is defined as.
//
// IT EXISTS SO THE BOOT LOG CAN NAME THE ROUTE. A bare bool made the daemon
// guess: its WARN told the operator the app role "still owns audit_events or is
// a superuser" and prescribed "connect wardynd as a distinct non-owner role",
// which is the wrong remedy for two of the four routes and actively misleading
// for the fourth — a role that owns nothing and is nobody's superuser, but holds
// GRANT SET ON PARAMETER session_replication_role, gets a warning naming two
// things it is not. Reporting the route is also the operator-facing half of the
// decision taken on this finding: report the replication-role route rather than
// arming ENABLE ALWAYS.
//
// ONE PREDICATE, so the report and the verdict cannot disagree: the bool is
// len(routes) == 0 and there is no second query anywhere that answers it.
//
// FAILS SAFE, exactly as the bool did: a missing table, or any error, is a
// bypass rather than a protection claim.
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
		// SHORT-CIRCUITED, and deliberately: the verdict is already decided, and
		// the two extra round trips below are the only place this function can
		// fail on a server that answered the first query — a boot that used to
		// reach a clean refusal must not start failing fatally on the version
		// probe instead. The routes reported are the ones that fired.
		return routes, nil
	}
	// THE FOURTH LEG, AND IT IS NOT A DDL ONE. The three above ask who can DROP
	// or DISABLE a trigger. `SET session_replication_role = 'replica'` needs no
	// DDL at all: it makes every SIMPLY-ENABLED ('O') trigger stop firing for the
	// session, so a role holding nothing but INSERT appends rows past all three
	// audit guards — unchained, unrewritten by the chain trigger, and the boot
	// meanwhile logged the deployment as DDL-protected. Executed in the probe
	// beside this function, not assumed: row_hash came back NULL. It is also the
	// reason ENABLE ALWAYS is the documented hardening — an 'A' trigger fires
	// regardless of replication role — and why auditForeignTriggers now reads
	// tgenabled 'R' as armed.
	//
	// GRANTABLE SINCE POSTGRESQL 15, which is what makes it a leg rather than a
	// restatement of the superuser one. GRANT SET ON PARAMETER
	// session_replication_role TO app is exactly the narrow grant a DBA hands an
	// application role for a bulk load, and it survives as a standing capability.
	//
	// SEPARATE QUERY, AND VERSION-GUARDED, deliberately. has_parameter_privilege
	// does not exist before PostgreSQL 15, and a missing function is a PARSE
	// error — it would fail even inside an untaken CASE branch — so folding this
	// into the query above would turn every pre-15 split-role boot into a hard
	// refusal (cmd/wardynd treats an error here as fatal). Skipping the leg there
	// is not a gap but the correct answer: parameter-level GRANT did not exist
	// before 15 either, so on those servers only a superuser can set the GUC, and
	// the first leg already covers that.
	var granular bool
	if err := pool.QueryRow(ctx,
		`SELECT current_setting('server_version_num')::int >= 150000`).Scan(&granular); err != nil {
		return nil, fmt.Errorf("db: read server version for the session_replication_role check: %w", err)
	}
	if !granular {
		return nil, nil
	}
	//
	// AND IT FOLLOWS ROLE MEMBERSHIP, exactly as the three legs above do. Asking
	// has_parameter_privilege(current_user, …) alone answers only for the role's
	// OWN and inherited grants, so the ordinary managed-Postgres shape the
	// superuser leg was itself rewritten for — GRANT some admin role TO the app
	// role, the app role NOINHERIT — slipped straight through this leg: the
	// parameter grant sits on the admin role, current_user's own answer is false,
	// and the app role then runs SET ROLE admin; SET session_replication_role =
	// 'replica'; RESET ROLE and appends unchained rows as ITSELF. Executed rather
	// than argued in the probe beside this function: row_hash came back NULL and
	// a DELETE removed an audit row past the append-only guard, both while this
	// function reported PROTECTED. The EXISTS below asks the same question of
	// every role current_user can reach; a role is a member of itself, so the
	// direct grant is simply the r = current_user row and nothing is lost.
	//
	// 'MEMBER' RATHER THAN 'SET' is deliberate on both axes. It is the privilege
	// the other three legs use, and it is the CONSERVATIVE one: since PostgreSQL
	// 16 a membership can be granted WITH SET FALSE, which pg_has_role reports as
	// MEMBER = true, SET = false — so MEMBER is a superset of "can SET ROLE into
	// it" and can only ever report protection WEAKER than the role setup, never
	// stronger, which is this function's stated direction of error. 'SET' is also
	// not a recognised pg_has_role privilege before PostgreSQL 16, while this leg
	// runs from 15 up.
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
