// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// openTable removes, for the rest of the transaction, everything that would stop a one-column row from being
// inserted into table: its constraints, its not-null marks, its unique indexes and its user triggers. The
// caller rolls the transaction back.
func openTable(ctx context.Context, t *testing.T, tx pgx.Tx, table string) {
	t.Helper()
	ident := pgx.Identifier{table}.Sanitize()
	block := fmt.Sprintf(`
DO $open$
DECLARE r record;
BEGIN
  FOR r IN SELECT conname FROM pg_constraint WHERE conrelid = %[1]s::regclass LOOP
    BEGIN EXECUTE format('ALTER TABLE %%s DROP CONSTRAINT %%I CASCADE', %[2]s, r.conname);
    EXCEPTION WHEN OTHERS THEN NULL; END;
  END LOOP;
  FOR r IN SELECT attname FROM pg_attribute WHERE attrelid = %[1]s::regclass AND attnum > 0 AND NOT attisdropped AND attnotnull LOOP
    BEGIN EXECUTE format('ALTER TABLE %%s ALTER COLUMN %%I DROP NOT NULL', %[2]s, r.attname);
    EXCEPTION WHEN OTHERS THEN NULL; END;
  END LOOP;
  FOR r IN SELECT indexrelid::regclass::text AS idx FROM pg_index WHERE indrelid = %[1]s::regclass AND indisunique LOOP
    BEGIN EXECUTE 'DROP INDEX ' || r.idx;
    EXCEPTION WHEN OTHERS THEN NULL; END;
  END LOOP;
  BEGIN EXECUTE format('ALTER TABLE %%s DISABLE TRIGGER USER', %[2]s);
  EXCEPTION WHEN OTHERS THEN NULL; END;
END
$open$`, "'"+strings.ReplaceAll(ident, "'", "''")+"'", "'"+strings.ReplaceAll(ident, "'", "''")+"'")
	if _, err := tx.Exec(ctx, block); err != nil {
		t.Fatalf("open %s: %v", table, err)
	}
}

// Every column the census reads is found when it holds a string that starts with a key's prefix: a row with
// only that column set (and the subject type its filter wants) makes the census report that column for that
// key, and no other key.
func TestNamespaceCensus_FindsEveryColumn_PG(t *testing.T) {
	_, pool := isolatedStore(t)
	ctx := context.Background()
	key := types.PortalIssuerKey("https://census.example.com")
	otherKey := types.PortalIssuerKey("https://other-census.example.com")
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The registry a column of every table is read against: the columns the store names, by the same list.
	cols := store.NamespaceCensusColumns()
	opened := map[string]bool{}
	for _, c := range cols {
		if !opened[c.Table] {
			openTable(ctx, t, tx, c.Table)
			opened[c.Table] = true
		}
		var colType string
		if err := tx.QueryRow(ctx, `SELECT format_type(a.atttypid, a.atttypmod) FROM pg_attribute a
			WHERE a.attrelid = $1::regclass AND a.attname = $2 AND NOT a.attisdropped`, pgx.Identifier{c.Table}.Sanitize(), c.Column).Scan(&colType); err != nil {
			t.Fatalf("%s.%s is not a column of the schema: %v", c.Table, c.Column, err)
		}
		value := types.PortalPrincipal(key, "census")
		if c.JSON {
			b, _ := json.Marshal(map[string]any{"value": "user:" + value})
			value = string(b)
		}
		stmt := fmt.Sprintf(`INSERT INTO %s (%s) VALUES ($1::%s) RETURNING ctid`, pgx.Identifier{c.Table}.Sanitize(), pgx.Identifier{c.Column}.Sanitize(), colType)
		if c.Where != "" {
			// The only filter the census uses is a subject type of user.
			if c.Where != "subject_type = 'user'" {
				t.Fatalf("%s.%s has a filter the test does not know: %s", c.Table, c.Column, c.Where)
			}
			stmt = fmt.Sprintf(`INSERT INTO %s (%s, subject_type) VALUES ($1::%s, 'user') RETURNING ctid`, pgx.Identifier{c.Table}.Sanitize(), pgx.Identifier{c.Column}.Sanitize(), colType)
		}
		var ctid string
		if err := tx.QueryRow(ctx, strings.Replace(stmt, "RETURNING ctid", "RETURNING ctid::text", 1), value).Scan(&ctid); err != nil {
			t.Fatalf("insert into %s.%s: %v", c.Table, c.Column, err)
		}
		hits, err := store.NamespaceCensusQ(ctx, tx, key)
		if err != nil {
			t.Fatal(err)
		}
		if hits[c.Table+"."+c.Column] < 1 {
			t.Errorf("%s.%s holding %q is not found: %v", c.Table, c.Column, value, hits)
		}
		if other, err := store.NamespaceCensusQ(ctx, tx, otherKey); err != nil || len(other) != 0 {
			t.Errorf("%s.%s: another key finds %v, %v; want nothing", c.Table, c.Column, other, err)
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE ctid = $1::text::tid`, pgx.Identifier{c.Table}.Sanitize()), ctid); err != nil {
			t.Fatal(err)
		}
	}
	if hits, err := store.NamespaceCensusQ(ctx, tx, key); err != nil || len(hits) != 0 {
		t.Fatalf("after every probe row was removed the census finds %v, %v", hits, err)
	}
	// A subject that is not a user is not counted, and a key of the wrong shape is refused.
	if _, err := store.NamespaceCensusQ(ctx, tx, "short"); err == nil {
		t.Error("a malformed key was accepted")
	}
}

// historicalSource is one place a policy document naming a portal person can sit from before the issuer was
// bound: how to seed one real-shaped record naming key, and the census column that must report it.
type historicalSource struct {
	name, column string
	seed         func(t *testing.T, key string)
}

// seedPolicyAudit writes one audit row of action for run, whose data holds a resolved policy naming key.
func seedPolicyAudit(t *testing.T, pool *pgxpool.Pool, run types.AgentRun, action, key string) {
	t.Helper()
	id := run.ID
	data := `{"policy":` + uiAppsJSON(key) + `}`
	if err := store.InsertAuditEvent(context.Background(), pool, &types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), RunID: &id,
		ActorType: types.ActorSystem, Actor: "test", Action: action, Outcome: "success", Data: json.RawMessage(data)}); err != nil {
		t.Fatal(err)
	}
}

// The census reports every historical source of a policy document naming a portal person on its own, for a
// key no binding holds: each record is written while the key is bound, by the real writer (or, for the two
// run records no writer rules, shaped as one) and the binding is then removed, as in a database an older
// release wrote. A running run's resolved policy and a queued run's launch are found; the same rows of a run
// that ended, and a running run's other audit rows, are not. So a registration blocks on each source alone
// and succeeds when only an ended run names the key.
func TestNamespaceCensus_SpecColumnsAndRunningRunPolicy_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()
	unboundKey := func(name string) string {
		t.Helper()
		issuer := "https://census-" + name + "-" + uuid.NewString() + ".example.com"
		bound, err := st.BindPortalIssuerQ(ctx, pool, issuer, "https://internal-"+uuid.NewString()+".example.com", "uid", "test")
		if err != nil {
			t.Fatal(err)
		}
		return bound.IssuerKey
	}
	unbind := func(key string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `DELETE FROM portal_issuers WHERE issuer_key = $1`, key); err != nil {
			t.Fatal(err)
		}
	}

	var sources []historicalSource
	for _, w := range uiWriters(st, pool) {
		sources = append(sources, historicalSource{w.name, w.census, func(t *testing.T, key string) {
			if err := w.write(key); err != nil {
				t.Fatalf("seed %s: %v", w.name, err)
			}
		}})
	}
	sources = append(sources,
		historicalSource{"a queued run's launch", "run_queue.launch", func(t *testing.T, key string) {
			poolID := uuid.New()
			if _, err := pool.Exec(ctx, `INSERT INTO runner_pools (id, name, scope, owner) VALUES ($1, $2, 'user', 'sub-census')`, poolID, "pool-"+uuid.NewString()); err != nil {
				t.Fatal(err)
			}
			run := persistRun(t, ctx, pool, newRun(types.RunQueued))
			if _, err := pool.Exec(ctx, `INSERT INTO run_queue (run_id, pool_id, owner, deadline_at, launch) VALUES ($1, $2, $3, now() + interval '1 hour', $4::jsonb)`,
				run.ID, poolID, run.CreatedBy, `{"task":"x","inline_policy":`+uiAppsJSON(key)+`}`); err != nil {
				t.Fatal(err)
			}
		}},
		historicalSource{"a running run's resolved policy", "audit_events.data", func(t *testing.T, key string) {
			seedPolicyAudit(t, pool, persistRun(t, ctx, pool, newRun(types.RunRunning)), "run.policy.resolve", key)
		}},
		historicalSource{"a running run's policy generation", "audit_events.data", func(t *testing.T, key string) {
			seedPolicyAudit(t, pool, persistRun(t, ctx, pool, newRun(types.RunRunning)), "run.policy.generation", key)
		}},
	)
	for _, src := range sources {
		t.Run(src.name, func(t *testing.T) {
			key := unboundKey("source")
			src.seed(t, key)
			unbind(key)
			hits, err := store.NamespaceCensusQ(ctx, pool, key)
			if err != nil {
				t.Fatal(err)
			}
			if len(hits) != 1 || hits[src.column] != 1 {
				t.Errorf("census = %v, want only %s once", hits, src.column)
			}
			other := unboundKey("other")
			unbind(other)
			if hits, err := store.NamespaceCensusQ(ctx, pool, other); err != nil || len(hits) != 0 {
				t.Errorf("another key's census = %v, %v; want nothing", hits, err)
			}
		})
	}

	t.Run("only an ended run", func(t *testing.T) {
		key := unboundKey("ended")
		ended := persistRun(t, ctx, pool, newRun(types.RunCompleted))
		seedPolicyAudit(t, pool, ended, "run.policy.resolve", key)
		seedPolicyAudit(t, pool, ended, "run.policy.generation", key)
		seedPolicyAudit(t, pool, persistRun(t, ctx, pool, newRun(types.RunRunning)), "run.started", key) // not a policy action
		unbind(key)
		if hits, err := store.NamespaceCensusQ(ctx, pool, key); err != nil || len(hits) != 0 {
			t.Errorf("census = %v, %v; want nothing blocking: an ended run's rows and a running run's other rows are not read", hits, err)
		}
	})
}
