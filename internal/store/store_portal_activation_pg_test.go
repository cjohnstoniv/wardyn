// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// heldSession opens one connection on pool that carries label and keeps it open until the test ends.
func heldSession(t *testing.T, pool *pgxpool.Pool, label string) {
	t.Helper()
	ctx := context.Background()
	cfg := pool.Config().Copy()
	cfg.MaxConns, cfg.MinConns = 1, 1
	cfg.ConnConfig.RuntimeParams["application_name"] = label
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	if err := p.Ping(ctx); err != nil {
		t.Fatal(err)
	}
}

// The sample behind the activation veto and the hourly alarm (UntaggedClientSessions) sees another role's
// sessions even when the viewing role cannot read them, and reports them as unknown; a session it can read is
// reported with its label unless that label is a wardynd label of 0.10.2 or later.
func TestPortalIssuersActivate_MaskedClientCountsAsSuspect_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()

	role := "wardyn_viewer_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := pool.Exec(ctx, `CREATE ROLE `+role+` LOGIN PASSWORD 'pw'`); err != nil {
		t.Fatalf("create role: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP ROLE IF EXISTS `+role) })
	cfg := pool.Config().Copy()
	cfg.ConnConfig.User, cfg.ConnConfig.Password = role, "pw"
	viewerPool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(viewerPool.Close)
	viewer := store.NewPG(viewerPool)

	heldSession(t, pool, "wardynd/0.10.2/replica-new")
	heldSession(t, pool, "wardynd/0.10.1/replica-old")
	heldSession(t, pool, "psql")

	// The viewer cannot read the owner's sessions: they show backend_type NULL, which the old filter dropped.
	var masked int
	if err := viewerPool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
		WHERE datname = current_database() AND pid <> pg_backend_pid() AND backend_type IS NULL AND usename IS NOT NULL`).Scan(&masked); err != nil {
		t.Fatal(err)
	}
	if masked == 0 {
		t.Fatal("the viewing role reads another role's sessions; the test cannot show the masked case")
	}

	seen, err := viewer.UntaggedClientSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	unknown := 0
	for _, c := range seen {
		if c.Label == store.PortalSessionUnknown && c.ClientAddr == store.PortalSessionUnknown {
			unknown++
		}
	}
	if unknown < 3 {
		t.Fatalf("the viewer reports %d unknown sessions (%v); the owner holds at least 3, all masked to it", unknown, seen)
	}
	if len(seen) != unknown {
		t.Fatalf("the viewer reports readable sessions %v, but every session it can see is masked", seen)
	}

	// The owner reads every label: the new writer passes, the old writer and the unlabelled tool are reported.
	owner, err := st.UntaggedClientSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	labels := map[string]int{}
	for _, c := range owner {
		labels[c.Label]++
		if c.Label == store.PortalSessionUnknown {
			t.Errorf("the owner reports a session as unknown: %+v", c)
		}
	}
	if labels["wardynd/0.10.2/replica-new"] != 0 {
		t.Errorf("a 0.10.2 writer is reported: %v", labels)
	}
	if labels["wardynd/0.10.1/replica-old"] != 1 || labels["psql"] != 1 {
		t.Errorf("owner's report = %v, want the 0.10.1 writer and psql once each", labels)
	}
}
