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
	"github.com/cjohnstoniv/wardyn/internal/types"
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

// viewerStore is a store on a role that cannot read other roles' sessions (no pg_read_all_stats).
func viewerStore(t *testing.T, pool *pgxpool.Pool) store.PG {
	t.Helper()
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
	return store.NewPG(viewerPool)
}

// A role that cannot read another role's session still sees its application name (PostgreSQL fills it in
// before the statistics-privilege check) but not its backend type or address. The sample behind the activation
// veto and the hourly alarm (UntaggedClientSessions) keeps such a session, because the old filter dropped
// every row with a NULL backend type, and judges it by the label it can read: a wardynd label of 0.10.2 or
// later passes, any other label is reported with no address.
func TestPortalIssuersActivate_MaskedClientCountsAsSuspect_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()
	viewer := viewerStore(t, pool)

	heldSession(t, pool, "wardynd/0.10.2/replica-new")
	heldSession(t, pool, "wardynd/0.10.1/replica-old")
	heldSession(t, pool, "psql")

	// The viewer cannot read the owner's sessions: they show backend_type NULL, which the old filter dropped.
	var masked int
	if err := viewer.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
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
	viewed := map[string]types.ClientSession{}
	for _, c := range seen {
		viewed[c.Label] = c
	}
	if _, ok := viewed["wardynd/0.10.2/replica-new"]; ok {
		t.Errorf("the viewer reports a 0.10.2 writer whose label it can read: %v", seen)
	}
	for _, label := range []string{"wardynd/0.10.1/replica-old", "psql"} {
		c, ok := viewed[label]
		if !ok {
			t.Fatalf("the viewer does not report %q, a masked session with a readable label: %v", label, seen)
		}
		if c.ClientAddr != "" {
			t.Errorf("%q reported with address %q, want none: the viewer cannot read it", label, c.ClientAddr)
		}
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

// A client that sets no application_name has nothing for the veto to pass on, whatever role reads it.
func TestPortalIssuersActivate_UnlabelledClientIsReported_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()
	viewer := viewerStore(t, pool)

	count := func(s store.PG) int {
		seen, err := s.UntaggedClientSessions(ctx)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, c := range seen {
			if c.Label == "" || c.Label == store.PortalSessionUnknown {
				n++
			}
		}
		return n
	}
	ownerBefore, viewerBefore := count(st), count(viewer)
	heldSession(t, pool, "")
	if got := count(st); got != ownerBefore+1 {
		t.Errorf("the owner reports %d unlabelled sessions after one more, before %d", got, ownerBefore)
	}
	if got := count(viewer); got != viewerBefore+1 {
		t.Errorf("the viewer reports %d unlabelled sessions after one more, before %d", got, viewerBefore)
	}
}
