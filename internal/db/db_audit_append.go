// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// auditAppendSignature is audit_append's catalog identity (0111): every in-tree audit writer calls it,
// and the boot checks below ask about exactly this function rather than any same-named one.
const auditAppendSignature = "audit_append(uuid,timestamptz,uuid,text,text,text,text,text,text,jsonb)"

// AuditAppendPosture is what the serving pool's role can do to the audit log, as the catalog says it.
type AuditAppendPosture struct {
	// Role is current_user on the pool asked.
	Role string
	// CanAppend is EXECUTE on audit_append. Every audit write goes through that function, so a role
	// without it cannot write one audit row.
	CanAppend bool
	// DirectInsert is INSERT on audit_events itself. Always true for the owner (a single-DSN
	// install); the chain trigger still refuses a row audit_append did not allocate, which binds a
	// well-behaved or mistaken writer, not a hostile owner.
	DirectInsert bool
	// PublicExecute names the audit functions any role may execute (a NULL ACL means the PUBLIC
	// default). 0111 revokes it; an owner's later REPLACE or an ALTER DEFAULT PRIVILEGES can bring
	// it back.
	PublicExecute []string
}

// AuditAppendPostureOf reads the AuditAppendPosture of pool's role. Read-only; it fails closed (a
// missing function reads as "cannot append").
func AuditAppendPostureOf(ctx context.Context, pool *pgxpool.Pool) (AuditAppendPosture, error) {
	var p AuditAppendPosture
	err := pool.QueryRow(ctx, `
		SELECT current_user::text,
		       COALESCE(has_function_privilege(current_user, to_regprocedure($1), 'EXECUTE'), false),
		       COALESCE(has_table_privilege(current_user, to_regclass('audit_events'), 'INSERT'), false),
		       COALESCE((SELECT array_agg(p.proname::text ORDER BY p.proname)
		                   FROM pg_proc p
		                  WHERE p.oid = ANY(ARRAY[to_regprocedure($1),
		                                          to_regprocedure('audit_ensure_partitions(integer)'),
		                                          to_regprocedure('audit_partition_digest(text)'),
		                                          to_regprocedure('audit_retention_drop(text,text,text)'),
		                                          to_regprocedure('audit_retention_set_policy(integer)'),
		                                          to_regprocedure('audit_retention_partitions(text,boolean)'),
		                                          to_regprocedure('audit_retention_window()'),
		                                          to_regprocedure('audit_events_chain()')]::oid[])
		                    AND (p.proacl IS NULL
		                         OR EXISTS (SELECT 1 FROM aclexplode(p.proacl) a
		                                     WHERE a.grantee = 0 AND a.privilege_type = 'EXECUTE'))), ARRAY[]::text[])`,
		auditAppendSignature,
	).Scan(&p.Role, &p.CanAppend, &p.DirectInsert, &p.PublicExecute)
	if err != nil {
		return AuditAppendPosture{}, fmt.Errorf("db: read the audit append privileges: %w", err)
	}
	return p, nil
}
