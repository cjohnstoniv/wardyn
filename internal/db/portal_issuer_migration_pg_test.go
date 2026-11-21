// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const portalIssuerMigration = "0294_portal_issuer.sql"

// portalIssuerSeed writes a console portal and a workforce identity as 0.10.1 left them, before the migration.
func portalIssuerSeed(t *testing.T, pool *pgxpool.Pool, group string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	pgExec(t, pool, `INSERT INTO delegates (id, name, idp_client_id, scope_group, credential_sha256, registered_by)
		VALUES ($1, 'console', 'client', $2, $3, 'admin')`, id, group, uuid.NewString())
	return id
}

// The migration changes no existing row's meaning: a console portal keeps its group and reads as one, an
// identity row gets an empty display email and no introduction, the new tables start empty, the trigger and
// the constraints exist, and the file can run again over its own result.
func TestMigrationPortalIssuer_Additive_PG(t *testing.T) {
	pool, _ := partialSchemaPool(t, "0294")
	ctx := context.Background()
	portal := portalIssuerSeed(t, pool, "engineers")
	pgExec(t, pool, `INSERT INTO principal_identities (principal, issuer, email_lower) VALUES ('alice', 'https://idp.example.com', 'a@example.com')`)

	if err := applyMigration(ctx, pool, portalIssuerMigration, readMigration(t, portalIssuerMigration)); err != nil {
		t.Fatalf("apply %s: %v", portalIssuerMigration, err)
	}
	if got := pgScalar[string](t, pool, `SELECT scope_group FROM delegates WHERE id = $1`, portal); got != "engineers" {
		t.Errorf("console portal group = %q, want it kept", got)
	}
	var issuer string
	var managed bool
	var rev int64
	var domains []string
	if err := pool.QueryRow(ctx, `SELECT issuer, portal_managed_admission, admission_rev, email_domains FROM delegates WHERE id = $1`, portal).
		Scan(&issuer, &managed, &rev, &domains); err != nil {
		t.Fatal(err)
	}
	if issuer != "" || managed || rev != 1 || len(domains) != 0 {
		t.Errorf("console portal reads issuer %q, managed %v, rev %d, domains %v; want '', false, 1, none", issuer, managed, rev, domains)
	}
	if got := pgScalar[string](t, pool, `SELECT email_lower FROM principal_identities WHERE principal = 'alice'`); got != "a@example.com" {
		t.Errorf("identity email_lower = %q, want it kept", got)
	}
	if n := pgScalar[int](t, pool, `SELECT count(*) FROM principal_identities WHERE display_email <> '' OR introduced_by IS NOT NULL OR introduction_event_id IS NOT NULL`); n != 0 {
		t.Errorf("%d identity rows carry a portal value after the migration", n)
	}
	for _, table := range []string{"portal_issuers", "console_issuers", "portal_grants", "portal_person_portals"} {
		if n := pgScalar[int](t, pool, `SELECT count(*) FROM `+table); n != 0 {
			t.Errorf("%s has %d rows after the migration, want 0", table, n)
		}
	}
	if n := pgScalar[int](t, pool, `SELECT count(*) FROM pg_trigger WHERE tgname = 'principal_identities_portal_lifecycle' AND NOT tgisinternal`); n != 1 {
		t.Errorf("%d lifecycle triggers, want 1", n)
	}
	for _, c := range []string{"delegates_max_subject_token_age", "delegates_grant_ttl", "delegates_required_claim_shape", "delegates_admission_shape"} {
		if n := pgScalar[int](t, pool, `SELECT count(*) FROM pg_constraint WHERE conname = $1`, c); n != 1 {
			t.Errorf("constraint %s present %d times, want 1", c, n)
		}
	}

	// The same file over its own result: no error, no change.
	if _, err := pool.Exec(ctx, readMigration(t, portalIssuerMigration)); err != nil {
		t.Fatalf("running %s again: %v", portalIssuerMigration, err)
	}
	if n := pgScalar[int](t, pool, `SELECT count(*) FROM pg_trigger WHERE tgname = 'principal_identities_portal_lifecycle' AND NOT tgisinternal`); n != 1 {
		t.Errorf("%d lifecycle triggers after the second run, want 1", n)
	}
}

// A console row with an empty group stops the migration with a message that counts such rows, and nothing
// is applied.
func TestMigrationPortalIssuer_EmptyGroupRowNamed_PG(t *testing.T) {
	pool, _ := partialSchemaPool(t, "0294")
	ctx := context.Background()
	portalIssuerSeed(t, pool, "")
	portalIssuerSeed(t, pool, "")
	portalIssuerSeed(t, pool, "fine")

	err := applyMigration(ctx, pool, portalIssuerMigration, readMigration(t, portalIssuerMigration))
	if err == nil {
		t.Fatal("the migration applied over console portals with an empty group")
	}
	if !strings.Contains(err.Error(), "2 portal row(s)") || !strings.Contains(err.Error(), "scope_group") {
		t.Errorf("error = %v, want it to count the 2 rows and name scope_group", err)
	}
	if n := pgScalar[int](t, pool, `SELECT count(*) FROM information_schema.columns WHERE table_name = 'delegates' AND table_schema = current_schema() AND column_name = 'issuer'`); n != 0 {
		t.Errorf("the refused migration left the issuer column in place")
	}
}

// The label option replaces whatever application_name the DSN sets, on the parsed configuration and on a
// live connection; without it the DSN's value stands.
func TestDBConnect_ApplicationNameOverridesDSN(t *testing.T) {
	cfg, err := pgxpool.ParseConfig("postgres://u:p@localhost:5432/d?application_name=from-dsn")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ConnConfig.RuntimeParams["application_name"]; got != "from-dsn" {
		t.Fatalf("parsed application_name = %q, want the DSN's", got)
	}
	WithApplicationName("wardynd/0.10.2/replica-1")(cfg)
	if got := cfg.ConnConfig.RuntimeParams["application_name"]; got != "wardynd/0.10.2/replica-1" {
		t.Fatalf("after the option application_name = %q, want the label", got)
	}

	dsn := os.Getenv("WARDYN_TEST_PG")
	u, err := url.Parse(dsn)
	if dsn == "" || err != nil || u.Scheme == "" {
		t.Skip("WARDYN_TEST_PG is not a URL-form DSN; skipping the live connection check")
	}
	q := u.Query()
	q.Set("application_name", "from-dsn")
	u.RawQuery = q.Encode()
	ctx := context.Background()
	show := func(opts ...ConnectOption) string {
		pool, err := Connect(ctx, u.String(), opts...)
		if err != nil {
			t.Fatal(err)
		}
		defer pool.Close()
		return pgScalar[string](t, pool, `SHOW application_name`)
	}
	if got := show(); got != "from-dsn" {
		t.Errorf("without the option application_name = %q, want the DSN's", got)
	}
	if got := show(WithApplicationName("wardynd/0.10.2/replica-1")); got != "wardynd/0.10.2/replica-1" {
		t.Errorf("with the option application_name = %q, want the label", got)
	}
}
