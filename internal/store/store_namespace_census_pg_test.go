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

// The census reads the policy documents a write stores (a policy, a unit's ceiling, a preset, a template
// revision) and the resolved policy of a run that is still running, and not the same of a run that ended.
func TestNamespaceCensus_SpecColumnsAndRunningRunPolicy_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()
	p := newIssuerPortal(t, st, "") // binds the key, so the writes below are accepted
	caller := "user:" + types.PortalPrincipal(p.Key, "alice")
	spec := types.RunPolicySpec{UIApps: []types.UIApp{{Name: "app", Port: 8080, Inbound: true, Callers: []string{caller}}}}
	now := time.Now().UTC()

	if _, err := st.CreatePolicy(ctx, types.RunPolicy{ID: uuid.New(), Name: "census-" + uuid.NewString(), CreatedAt: now, UpdatedAt: now, Spec: spec}); err != nil {
		t.Fatal(err)
	}
	req, _ := json.Marshal(map[string]any{"policy": spec})
	if _, _, err := st.PutLaunchPreset(ctx, types.LaunchPreset{Name: "census-" + uuid.NewString(), UserTypes: []string{}, Request: req, CreatedBy: "admin@example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTemplate(ctx, types.RunTemplate{ID: uuid.New(), Owner: types.TemplateOwner{Scope: types.TemplateScopePerson, Person: "alice"},
		Kind: types.TemplateKindTemplate, Name: "census-" + uuid.NewString(), Document: req, CreatedBy: "alice"}, nil); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.WriteGovernanceUnitQ(ctx, tx, uuid.New(), func(_ []types.GovernanceUnit, _ []types.GovernanceMembership) (types.GovernanceUnit, error) {
		return types.GovernanceUnit{Name: "census-" + uuid.NewString(), Ceiling: spec, CreatedBy: "admin@example.com"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	resolved, _ := json.Marshal(map[string]any{"policy": spec})
	auditFor := func(run types.AgentRun, action string) {
		t.Helper()
		id := run.ID
		if err := store.InsertAuditEvent(ctx, pool, &types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), RunID: &id,
			ActorType: types.ActorSystem, Actor: "test", Action: action, Outcome: "success", Data: resolved}); err != nil {
			t.Fatal(err)
		}
	}
	running := persistRun(t, ctx, pool, newRun(types.RunRunning))
	auditFor(running, "run.policy.resolve")
	ended := persistRun(t, ctx, pool, newRun(types.RunCompleted))
	auditFor(ended, "run.policy.generation")
	other := persistRun(t, ctx, pool, newRun(types.RunRunning))
	auditFor(other, "run.started") // not a policy action

	hits, err := store.NamespaceCensusQ(ctx, pool, p.Key)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"run_policies.spec", "governance_units.ceiling", "launch_presets.request", "run_template_revisions.document"} {
		if hits[want] != 1 {
			t.Errorf("%s = %d, want 1 (all hits: %v)", want, hits[want], hits)
		}
	}
	if hits["audit_events.data"] != 1 {
		t.Errorf("audit_events.data = %d, want 1: the running run's resolved policy only (all hits: %v)", hits["audit_events.data"], hits)
	}
}
