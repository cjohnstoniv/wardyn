// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// PIN for the reporting half of the audit-bypass enumeration finding: the check
// must name the route that FIRED, not a posture the caller then has to guess a
// cause for.
//
// The shape that made the old boot warning wrong is exactly this one: a role
// that owns nothing, is nobody's superuser and holds no TRIGGER privilege, but
// reaches SET on the session_replication_role parameter. It was told it "still
// owns audit_events or is a superuser" — two things it is not — and prescribed a
// remedy (connect as a different non-owner role) that closes neither the
// parameter grant nor a TRIGGER privilege.
//
// The probe roles are created, granted, connected as, and dropped; nothing is
// written to audit_events.

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestPG_AuditDDLBypassRoutesNamesTheRouteThatFired(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()

	var super bool
	if err := pool.QueryRow(ctx, `SELECT rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&super); err != nil {
		t.Fatalf("read role: %v", err)
	}
	u, err := url.Parse(os.Getenv("WARDYN_TEST_PG"))
	urlDSN := err == nil && u.Scheme != "" && u.Host != ""
	mustNotSkip := probeMustNotSkip(t, super, urlDSN)
	if !super {
		skipOrFatal(t, mustNotSkip, "WARDYN_TEST_PG role is not a superuser; GRANT SET ON PARAMETER needs it")
	}
	if !urlDSN {
		skipOrFatal(t, mustNotSkip, "WARDYN_TEST_PG is not a URL-form DSN; cannot derive an app-role DSN from it")
	}
	var pg15 bool
	if err := pool.QueryRow(ctx, `SELECT current_setting('server_version_num')::int >= 150000`).Scan(&pg15); err != nil {
		t.Fatalf("read server version: %v", err)
	}
	if !pg15 {
		t.Skip("GRANT ... ON PARAMETER is PostgreSQL 15+")
	}

	app := fmt.Sprintf("wardyn_f99r_app_%d", time.Now().UnixNano()%1_000_000_000)
	const pw = "f99r-probe-pw"
	must := func(q string) {
		t.Helper()
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	t.Cleanup(func() {
		for _, q := range []string{
			`REVOKE SET ON PARAMETER session_replication_role FROM ` + app,
			`REVOKE ALL PRIVILEGES ON ALL TABLES IN SCHEMA public FROM ` + app,
			`REVOKE ALL PRIVILEGES ON SCHEMA public FROM ` + app,
			`DROP ROLE IF EXISTS ` + app,
		} {
			if _, err := pool.Exec(context.Background(), q); err != nil {
				t.Errorf("cleanup %s: %v", q, err)
			}
		}
	})
	must(`CREATE ROLE ` + app + ` LOGIN PASSWORD '` + pw + `'`)
	must(`GRANT USAGE ON SCHEMA public TO ` + app)
	must(`GRANT SELECT, INSERT ON audit_events TO ` + app)

	u.User = url.UserPassword(app, pw)
	appPool, err := Connect(ctx, u.String())
	if err != nil {
		skipOrFatal(t, mustNotSkip, "cannot connect as the app role (pg_hba may not password-auth a new role): %v", err)
	}
	t.Cleanup(appPool.Close)

	// CLEAN: no route at all, and the bool agrees.
	routes, err := AuditDDLBypassRoutes(ctx, appPool)
	if err != nil {
		t.Fatalf("AuditDDLBypassRoutes(clean app role): %v", err)
	}
	if len(routes) != 0 {
		t.Fatalf("a role holding only SELECT/INSERT on audit_events reports routes %v, want none", routes)
	}
	if ok, err := AuditDDLProtected(ctx, appPool); err != nil || !ok {
		t.Fatalf("AuditDDLProtected = %v, %v; want true, nil — the bool and the routes must be one answer", ok, err)
	}

	// THE PARAMETER GRANT ALONE, which is the shape the old warning misdescribed.
	must(`GRANT SET ON PARAMETER session_replication_role TO ` + app)
	routes, err = AuditDDLBypassRoutes(ctx, appPool)
	if err != nil {
		t.Fatalf("AuditDDLBypassRoutes(parameter-granted app role): %v", err)
	}
	if !slices.Contains(routes, auditBypassReplica) {
		t.Fatalf("routes = %v, want the session_replication_role route named. Without it the boot log can only report a "+
			"posture, and the sentence it used to guess (\"still owns audit_events or is a superuser\") is false for "+
			"this role", routes)
	}
	for _, wrong := range []string{auditBypassSuperuser, auditBypassOwner, auditBypassTrigger} {
		if slices.Contains(routes, wrong) {
			t.Errorf("routes = %v, and %q is among them for a role that holds none of those — the report would send the "+
				"operator after the wrong remedy", routes, wrong)
		}
	}
	if ok, err := AuditDDLProtected(ctx, appPool); err != nil || ok {
		t.Errorf("AuditDDLProtected = %v, %v; want false, nil", ok, err)
	}

	// The route text has to be actionable on its own: it names the capability
	// AND the parameter, because the remedy is a REVOKE on a PARAMETER that no
	// role-swapping reaches.
	if !strings.Contains(auditBypassReplica, "session_replication_role") || !strings.Contains(auditBypassReplica, "SET") {
		t.Errorf("the route text %q does not name the SET privilege on session_replication_role, so it names no remedy",
			auditBypassReplica)
	}
}
