// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// Hosted-only (WARDYN_TEST_PG): the console-issuer record at boot and the split-role boot of the issuer
// portal writers. Each test migrates a schema of its own and drops it afterwards.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
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
	bindIssuerAs(t, pool, "https://idp.portal.example/oauth2/default")
	// The same issuer in another spelling that is equal under norm: refused, and nothing recorded.
	err := recordConsoleIssuer(context.Background(), pool, "HTTPS://IDP.portal.example:443/oauth2/default/", "r1")
	if !errors.Is(err, errConsoleIssuerIsPortal) || !strings.Contains(err.Error(), "no portal registration names it") {
		t.Fatalf("a console issuer equal to a binding no registration names: %v, want the boot refusal saying so", err)
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

// TestConsoleIssuer_BootRefusalNamesSharedIssuerPortals_PG: two portals share the clashing issuer, and the
// refusal names both registrations, the revoked one marked, and no portal of another issuer; with more than
// the bound it names that many and counts the rest. The refusal reads through its own transaction, so a pool
// of one connection answers it.
func TestConsoleIssuer_BootRefusalNamesSharedIssuerPortals_PG(t *testing.T) {
	pool := portalBootPool(t, "ovlnames")
	singleCfg, err := pgxpool.ParseConfig(pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	singleCfg.MaxConns = 1
	single, err := pgxpool.NewWithConfig(context.Background(), singleCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(single.Close)
	ctx := context.Background()
	b := bindIssuerAs(t, pool, "https://idp.shared.example/oauth2/default")
	one, _ := registerIssuerPortal(t, pool, b, "shared-one")
	two, _ := registerIssuerPortal(t, pool, b, "shared-two")
	if _, _, err := store.NewPG(pool).RevokeDelegate(ctx, two.ID, time.Now(), false); err != nil {
		t.Fatal(err)
	}
	other, _ := registerIssuerPortal(t, pool, bindIssuerAs(t, pool, "https://idp.other.example/"), "other")
	refuse := func() string {
		t.Helper()
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		err := recordConsoleIssuer(rctx, single, "https://idp.shared.example/oauth2/default", "r1")
		if !errors.Is(err, errConsoleIssuerIsPortal) {
			t.Fatalf("a console issuer portals share: %v, want the boot refusal", err)
		}
		return err.Error()
	}
	msg := refuse()
	for _, want := range []string{one.ID.String() + ` "shared-one"`, two.ID.String() + ` "shared-two" (revoked)`} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q does not name %s", msg, want)
		}
	}
	if strings.Contains(msg, other.ID.String()) || strings.Contains(msg, "could not be read") {
		t.Errorf("refusal %q names a portal of another issuer, or lost the names", msg)
	}
	for i := range portalRegistrationMax {
		registerIssuerPortal(t, pool, b, fmt.Sprintf("shared-more-%d", i))
	}
	msg = refuse()
	if n := strings.Count(msg, ` "shared-`); n != portalRegistrationMax || !strings.Contains(msg, " and 2 more)") {
		t.Errorf("refusal with %d registrations = %q, want %d named and 2 counted", portalRegistrationMax+2, msg, portalRegistrationMax)
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
	bindIssuerAs(t, pool, "https://third.example/oauth2")
}

// firstNewMigration is the first migration after the last release: the split-role test grants the app role
// the runbook's baseline as of the schema before it.
const firstNewMigration = "0294"

// newMigrationSQL is the text of every migration from firstNewMigration on, read from the source tree.
func newMigrationSQL(t *testing.T) []string {
	t.Helper()
	const dir = "../../internal/db/migrations"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") && e.Name() >= firstNewMigration {
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, string(b))
		}
	}
	if len(out) == 0 {
		t.Fatalf("no migration from %s on in %s", firstNewMigration, dir)
	}
	return out
}

var createdTable = regexp.MustCompile(`(?i)CREATE TABLE (?:IF NOT EXISTS )?([a-z_][a-z0-9_]*)`)

// grantBlocks is each DO block of sql that grants something: a migration's own grant to the app role.
func grantBlocks(sql string) []string {
	var out []string
	for {
		i := strings.Index(sql, "DO $")
		if i < 0 {
			return out
		}
		rest := sql[i+len("DO $"):]
		j := strings.IndexByte(rest, '$')
		if j < 0 {
			return out
		}
		tag := "$" + rest[:j] + "$"
		body := rest[j+1:]
		k := strings.Index(body, tag)
		if k < 0 {
			return out
		}
		if block := sql[i : len(sql)-len(body)+k+len(tag)]; strings.Contains(block, "GRANT ") {
			out = append(out, block)
		}
		sql = body[k+len(tag):]
	}
}

// grantSplitRoleBaseline gives role, on the migrated schema mig points at, what the split-role runbook
// (docs/OPERATIONS.md, "Splitting the migrator and app roles", steps 1 to 3b) grants an app role on the last
// release's schema, without step 4's default privileges. The tables the newer migrations created are taken
// back, and those migrations' own grant blocks run again as the migrator, as they run on an upgrade: so a new
// table reaches the role only through its migration's grant.
func grantSplitRoleBaseline(t *testing.T, mig *pgxpool.Pool, schema, role string) {
	t.Helper()
	ctx := context.Background()
	stmts := []string{
		fmt.Sprintf(`GRANT USAGE ON SCHEMA %s TO %s`, schema, role),
		fmt.Sprintf(`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA %s TO %s`, schema, role),
		`REVOKE UPDATE, DELETE, TRUNCATE, TRIGGER, REFERENCES ON audit_events FROM ` + role,
		`REVOKE INSERT ON audit_events FROM ` + role,
		`REVOKE INSERT, UPDATE, DELETE, TRUNCATE, TRIGGER, REFERENCES ON audit_partition_meta, audit_chain_anchors FROM ` + role,
		`DO $$
DECLARE p regclass;
BEGIN
  FOR p IN SELECT inhrelid::regclass FROM pg_inherits WHERE inhparent = 'audit_events'::regclass LOOP
    EXECUTE format('REVOKE INSERT, UPDATE, DELETE, TRUNCATE, TRIGGER, REFERENCES ON %s FROM ` + role + `', p);
  END LOOP;
END $$`,
		`GRANT EXECUTE ON FUNCTION
  audit_append(uuid, timestamptz, uuid, text, text, text, text, text, text, jsonb),
  audit_ensure_partitions(integer),
  audit_partition_digest(text),
  audit_retention_drop(text, text, text, text),
  audit_retention_set_policy(integer),
  audit_retention_partitions(text, boolean),
  audit_retention_window() TO ` + role,
	}
	sqls := newMigrationSQL(t)
	for _, sql := range sqls {
		for _, m := range createdTable.FindAllStringSubmatch(sql, -1) {
			stmts = append(stmts, `REVOKE ALL ON `+m[1]+` FROM `+role)
		}
	}
	for _, sql := range sqls {
		stmts = append(stmts, grantBlocks(sql)...)
	}
	for _, q := range stmts {
		if _, err := mig.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// TestSplitRole_AppRolePortalExchangeRevokeRetire_PG: migrations run as the migrator; the application role
// holds the runbook's baseline and the new migrations' own grants, nothing more. It boots, registers a portal,
// exchanges, revokes the portal, retires an unused binding and records the console issuer, with no permission
// error. Taking back one grant a write needs makes that write fail.
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
	mig, err := connectAndMigrate(ctx, dsn, "", 30*time.Second, 2*time.Minute, false)
	if err != nil {
		t.Fatalf("migrate as the migrator: %v", err)
	}
	t.Cleanup(mig.Close)
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s'`, role, pw)); err != nil {
		t.Fatalf("create role: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DROP OWNED BY `+role)
		_, _ = admin.Exec(context.Background(), `DROP ROLE IF EXISTS `+role)
	})
	grantSplitRoleBaseline(t, mig, schema, role)
	appURL := *u
	appURL.User = url.UserPassword(role, pw)
	app, err := connectAndMigrate(ctx, appURL.String(), dsn, 30*time.Second, 2*time.Minute, false)
	if err != nil {
		t.Fatalf("the application role boot: %v", err)
	}
	t.Cleanup(app.Close)
	rec, _, _, _, err := buildAuditChain(t.Context(), "", "", "", app, secretmask.NewRegistry(), newAuditSealSource(audit.SealOff))
	if err != nil {
		t.Fatal(err)
	}
	as := func(q string) {
		t.Helper()
		if _, err := mig.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := mig.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}

	// Register and exchange as the app role.
	const issuer = "https://idp.split.example/oauth2/default"
	p, cred := registerIssuerPortal(t, app, bindIssuerAs(t, app, issuer), "split-portal")
	// An ended grant the exchange's sweep must delete once its grant commits, as the app role.
	ended := func() uuid.UUID {
		t.Helper()
		id := uuid.New()
		if _, err := mig.Exec(ctx, `
			INSERT INTO portal_grants (id, delegate_id, token_sha256, principal, user_type, chain_id, subject_issued_at, created_at, expires_at)
			VALUES ($1, $2, $3, $4, 'standard', $1, now() - interval '2 hours', now() - interval '2 hours', now() - interval '1 hour')`,
			id, p.ID, "ended-"+id.String(), oidc.PortalPrincipal(issuer, "ended")); err != nil {
			t.Fatalf("seed an ended grant: %v", err)
		}
		return id
	}
	gone := ended()
	h := newPortalAPI(t, store.NewPG(app), rec, app, "u1")
	w := portalExchangeOn(h, p, cred)
	if w.Code != http.StatusOK {
		t.Fatalf("exchange as the app role = %d %s", w.Code, w.Body)
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.AccessToken == "" {
		t.Fatalf("exchange response %s: %v", w.Body, err)
	}
	u1 := oidc.PortalPrincipal(issuer, "u1")
	if n := count(`SELECT count(*) FROM portal_grants WHERE principal = $1`, u1); n != 1 {
		t.Fatalf("portal grants of the person = %d, want 1", n)
	}
	if n := count(`SELECT count(*) FROM principal_identities WHERE principal = $1 AND introduction_audited_at IS NOT NULL`, u1); n != 1 {
		t.Fatalf("delivered introductions = %d, want the person's introduction appended and marked as the app role", n)
	}
	if n := count(`SELECT count(*) FROM audit_events WHERE action = 'delegation.exchange' AND outcome = 'success' AND target = $1`, u1); n != 1 {
		t.Fatalf("successful exchange rows = %d, want 1", n)
	}
	// The two writes after the grant's commit only log a failure, so the test reads what they wrote: the
	// portal's last use (UPDATE on delegates) and the sweep of ended grants (UPDATE and DELETE on portal_grants).
	if n := count(`SELECT count(*) FROM delegates WHERE id = $1 AND last_used_at IS NOT NULL`, p.ID); n != 1 {
		t.Fatal("the exchange did not stamp the portal's last use as the app role")
	}
	if n := count(`SELECT count(*) FROM portal_grants WHERE id = $1`, gone); n != 0 {
		t.Fatal("the exchange's sweep did not delete the ended grant as the app role")
	}
	// Without DELETE on portal_grants the exchange still answers, and the ended grant stays: the check above can fail.
	as(`REVOKE DELETE ON portal_grants FROM ` + role)
	kept := ended()
	if w := portalExchangeOn(newPortalAPI(t, store.NewPG(app), rec, app, "u3"), p, cred); w.Code != http.StatusOK {
		t.Fatalf("exchange without DELETE on portal_grants = %d %s, want 200 with the sweep failing", w.Code, w.Body)
	}
	if n := count(`SELECT count(*) FROM portal_grants WHERE id = $1`, kept); n != 1 {
		t.Fatalf("ended grant rows without DELETE = %d, want it left in place", n)
	}
	as(`GRANT DELETE ON portal_grants TO ` + role)
	// The exchange's grant write needs INSERT on portal_grants, which only 0294's grant gives this role.
	as(`REVOKE INSERT ON portal_grants FROM ` + role)
	if w := portalExchangeOn(newPortalAPI(t, store.NewPG(app), rec, app, "u2"), p, cred); w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "mint portal grant failed") {
		t.Fatalf("exchange without INSERT on portal_grants = %d %s, want 500 at the grant write", w.Code, w.Body)
	}
	if n := count(`SELECT count(*) FROM portal_grants WHERE principal = $1`, oidc.PortalPrincipal(issuer, "u2")); n != 0 {
		t.Fatalf("an exchange that could not write its grant left %d grants", n)
	}
	as(`GRANT INSERT ON portal_grants TO ` + role)

	// Revoke the portal as the app role: its grant ends.
	revoke := httptest.NewRequest(http.MethodDelete, "/api/v1/admin/delegates/"+p.ID.String(), nil)
	revoke.Header.Set("Authorization", "Bearer "+portalAPIAdminToken)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, revoke)
	if rw.Code != http.StatusNoContent {
		t.Fatalf("revoke the portal as the app role = %d %s", rw.Code, rw.Body)
	}
	me := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	me.Header.Set("Authorization", "Bearer "+out.AccessToken)
	mw := httptest.NewRecorder()
	h.ServeHTTP(mw, me)
	if mw.Code != http.StatusUnauthorized {
		t.Fatalf("GET /me with the revoked portal's grant = %d, want 401", mw.Code)
	}

	// Retire a binding no portal uses, as the app role.
	st := store.NewPG(app)
	retire := func(key string) error {
		t.Helper()
		tx, err := app.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := store.LockPortalIssuerRegistry(ctx, tx); err != nil {
			t.Fatal(err)
		}
		if err := st.RetirePortalIssuerQ(ctx, tx, key, nil); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if err := retire(bindIssuerAs(t, app, "https://idp.unused.example/").IssuerKey); err != nil {
		t.Fatalf("retire an unused binding as the app role: %v", err)
	}
	// The retirement deletes the binding, which needs DELETE on portal_issuers.
	as(`REVOKE DELETE ON portal_issuers FROM ` + role)
	var pgErr *pgconn.PgError
	if err := retire(bindIssuerAs(t, app, "https://idp.unused-two.example/").IssuerKey); !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("retire without DELETE on portal_issuers: %v, want permission denied (42501)", err)
	}
	as(`GRANT DELETE ON portal_issuers TO ` + role)

	if err := recordConsoleIssuer(ctx, app, "https://login.split.example/", "split-1"); err != nil {
		t.Fatalf("record the console issuer as the app role: %v", err)
	}
}
