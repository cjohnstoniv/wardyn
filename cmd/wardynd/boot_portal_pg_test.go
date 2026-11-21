// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// Hosted-only (WARDYN_TEST_PG): the console-issuer record at boot and the split-role boot of the issuer
// portal writers. Each test migrates a schema of its own and drops it afterwards.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// portalPGSchema creates a schema of its own on WARDYN_TEST_PG and returns the DSN that points at it, with
// the admin pool that owns cleanup.
func portalPGSchema(t *testing.T, tag string) (schemaDSN string, admin *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping the issuer portal boot test")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" || u.Host == "" {
		t.Skip("WARDYN_TEST_PG is not a URL-form DSN; cannot derive a schema-scoped DSN from it")
	}
	ctx := context.Background()
	admin, err = db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(admin.Close)
	schema := fmt.Sprintf("wardyn_pb_%s_%d", tag, time.Now().UnixNano()%1_000_000_000)
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`) })
	v := *u
	q := v.Query()
	q.Set("search_path", schema)
	v.RawQuery = q.Encode()
	return v.String(), admin
}

func portalBootPool(t *testing.T, tag string) *pgxpool.Pool {
	t.Helper()
	dsn, _ := portalPGSchema(t, tag)
	pool, err := connectAndMigrate(t.Context(), dsn, "", 30*time.Second, 2*time.Minute, false)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// bindPortalIssuer registers a binding the way a portal registration does, under the registry lock.
func bindPortalIssuer(t *testing.T, pool *pgxpool.Pool, issuer string) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := store.LockPortalIssuerRegistry(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.NewPG(pool).BindPortalIssuerQ(ctx, tx, issuer, "", "uid", "test"); err != nil {
		t.Fatalf("bind %s: %v", issuer, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func recordedConsoleIssuers(t *testing.T, pool *pgxpool.Pool) []types.ConsoleIssuer {
	t.Helper()
	got, err := store.NewPG(pool).ConsoleIssuersQ(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestConsoleIssuer_BootRefusedOnPortalOverlap_PG(t *testing.T) {
	pool := portalBootPool(t, "ovl")
	bindPortalIssuer(t, pool, "https://idp.portal.example/oauth2/default")
	// The same issuer in another spelling that is equal under norm: refused, and nothing recorded.
	err := recordConsoleIssuer(context.Background(), pool, "HTTPS://IDP.portal.example:443/oauth2/default/", "r1")
	if !errors.Is(err, errConsoleIssuerIsPortal) {
		t.Fatalf("a console issuer equal to a binding's: %v, want the boot refusal", err)
	}
	if got := recordedConsoleIssuers(t, pool); len(got) != 0 {
		t.Fatalf("a refused boot recorded %+v", got)
	}
	// An issuer that is not a binding's boots and is recorded.
	if err := recordConsoleIssuer(context.Background(), pool, "https://login.console.example/", "r1"); err != nil {
		t.Fatalf("an unrelated console issuer: %v", err)
	}
	if got := recordedConsoleIssuers(t, pool); len(got) != 1 {
		t.Fatalf("recorded = %+v, want one", got)
	}
}

func TestConsoleIssuer_EveryBootedIssuerRecorded_PG(t *testing.T) {
	pool := portalBootPool(t, "rec")
	ctx := context.Background()
	for i, iss := range []string{"https://login.one.example/", "https://login.two.example/"} {
		if err := recordConsoleIssuer(ctx, pool, iss, fmt.Sprintf("replica-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	// A second boot of the first is a no-op, not a second row.
	if err := recordConsoleIssuer(ctx, pool, "https://LOGIN.one.example", "replica-9"); err != nil {
		t.Fatal(err)
	}
	got := recordedConsoleIssuers(t, pool)
	if len(got) != 2 {
		t.Fatalf("recorded = %+v, want both issuers, once each", got)
	}
	norms := map[string]string{}
	for _, c := range got {
		norms[c.IssuerNorm] = c.FirstBootedByReplica
	}
	if norms[types.NormIssuerURL("https://login.one.example/")] != "replica-0" || norms[types.NormIssuerURL("https://login.two.example/")] != "replica-1" {
		t.Errorf("recorded norms = %v, want each issuer under the replica that first booted it", norms)
	}
}

func TestConsoleIssuer_OIDCUnsetRecordsNothing_PG(t *testing.T) {
	pool := portalBootPool(t, "unset")
	ctx := context.Background()
	if err := recordConsoleIssuer(ctx, pool, "", "r1"); err != nil {
		t.Fatal(err)
	}
	if err := recordConsoleIssuer(ctx, pool, "  ", "r1"); err != nil {
		t.Fatal(err)
	}
	if got := recordedConsoleIssuers(t, pool); len(got) != 0 {
		t.Fatalf("a boot without console OIDC recorded %+v", got)
	}
	// Then one with it, then a binding on a third issuer: all succeed.
	if err := recordConsoleIssuer(ctx, pool, "https://login.one.example/", "r2"); err != nil {
		t.Fatal(err)
	}
	bindPortalIssuer(t, pool, "https://third.example/oauth2")
}

// TestSplitRole_AppRolePortalExchangeRevokeRetire_PG: migrations run as the migrator, then the application
// role (the runbook's grants: DML on the tables, EXECUTE on audit_append, no ownership) boots and writes what
// an issuer portal's exchange, a revoke, a retirement and the console-issuer record write, with no permission
// error.
func TestSplitRole_AppRolePortalExchangeRevokeRetire_PG(t *testing.T) {
	dsn, admin := portalPGSchema(t, "split")
	ctx := context.Background()
	var canCreate bool
	if err := admin.QueryRow(ctx, `SELECT rolsuper OR rolcreaterole FROM pg_roles WHERE rolname = current_user`).Scan(&canCreate); err != nil || !canCreate {
		t.Skip("WARDYN_TEST_PG role cannot CREATE ROLE")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := u.Query().Get("search_path")
	role := fmt.Sprintf("wardyn_pb_r_%d", time.Now().UnixNano()%1_000_000_000)
	const pw = "pb-test-pw"
	first, err := connectAndMigrate(ctx, dsn, "", 30*time.Second, 2*time.Minute, false)
	if err != nil {
		t.Fatalf("migrate as the migrator: %v", err)
	}
	first.Close()
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s'`, role, pw)); err != nil {
		t.Fatalf("create role: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DROP OWNED BY `+role)
		_, _ = admin.Exec(context.Background(), `DROP ROLE IF EXISTS `+role)
	})
	for _, q := range []string{
		fmt.Sprintf(`GRANT USAGE ON SCHEMA %s TO %s`, schema, role),
		fmt.Sprintf(`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA %s TO %s`, schema, role),
		fmt.Sprintf(`GRANT USAGE ON ALL SEQUENCES IN SCHEMA %s TO %s`, schema, role),
		fmt.Sprintf(`GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA %s TO %s`, schema, role),
	} {
		if _, err := admin.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	appURL := *u
	appURL.User = url.UserPassword(role, pw)
	app, err := connectAndMigrate(ctx, appURL.String(), dsn, 30*time.Second, 2*time.Minute, false)
	if err != nil {
		t.Fatalf("the application role boot: %v", err)
	}
	t.Cleanup(app.Close)
	st := store.NewPG(app)
	const issuer = "https://idp.split.example/oauth2/default"
	tx, err := app.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.LockPortalIssuerRegistry(ctx, tx); err != nil {
		t.Fatalf("the registry lock as the app role: %v", err)
	}
	b, err := st.BindPortalIssuerQ(ctx, tx, issuer, "", "uid", "split")
	if err != nil {
		t.Fatalf("bind as the app role: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// Retiring a binding no portal uses, and recording a console issuer, as the app role.
	rtx, err := app.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.LockPortalIssuerRegistry(ctx, rtx); err != nil {
		t.Fatal(err)
	}
	if err := st.RetirePortalIssuerQ(ctx, rtx, b.IssuerKey); err != nil {
		t.Fatalf("retire as the app role: %v", err)
	}
	if err := rtx.Commit(ctx); err != nil {
		t.Fatalf("commit the retirement as the app role: %v", err)
	}
	if err := recordConsoleIssuer(ctx, app, "https://login.split.example/", "split-1"); err != nil {
		t.Fatalf("record the console issuer as the app role: %v", err)
	}
}
