// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Integration tests for run templates (migration 0165_run_templates). Guarded
// by WARDYN_TEST_PG; each test works in its own throwaway database, so the
// per-audience counts it checks are its own.
package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func templatesPG(t *testing.T) (store.PG, *pgxpool.Pool) {
	t.Helper()
	pool := runsPGPoolIsolated(t)
	return store.NewPG(pool), pool
}

var personOwner = types.TemplateOwner{Scope: types.TemplateScopePerson, Person: "alice"}

func pgTemplate(owner types.TemplateOwner, name string) types.RunTemplate {
	return types.RunTemplate{ID: uuid.New(), Owner: owner, Kind: types.TemplateKindTemplate, Name: name,
		Document: json.RawMessage(`{"intent":{"name":"` + name + `"}}`), CreatedBy: "alice"}
}

// TestPG_RunTemplates_RevisionsAndCAS: a create is revision 1; an update at
// the expected revision writes the next one and keeps the old one readable;
// an update at a stale revision writes nothing; delete cascades the revisions.
func TestPG_RunTemplates_RevisionsAndCAS(t *testing.T) {
	st, pool := templatesPG(t)
	ctx := context.Background()
	created, err := st.CreateTemplate(ctx, pgTemplate(personOwner, "first"), nil)
	if err != nil || created.Revision != 1 {
		t.Fatalf("create = %+v, %v", created, err)
	}
	next := created
	next.Name, next.Document, next.UpdatedBy = "second", json.RawMessage(`{"intent":{"v":2}}`), "bob"
	updated, err := st.UpdateTemplate(ctx, next, 1, nil)
	if err != nil || updated.Revision != 2 || updated.Name != "second" {
		t.Fatalf("update = %+v, %v", updated, err)
	}
	stale := next
	stale.Name = "stale"
	var conflict *store.TemplateRevisionConflictError
	if _, err := st.UpdateTemplate(ctx, stale, 1, nil); !errors.As(err, &conflict) || conflict.Current != 2 {
		t.Fatalf("stale update err = %v, want a conflict at 2", err)
	}
	cur, err := st.GetTemplate(ctx, created.ID)
	var doc struct{ Intent struct{ V int } }
	if err != nil || cur.Name != "second" || cur.Revision != 2 || json.Unmarshal(cur.Document, &doc) != nil || doc.Intent.V != 2 {
		t.Fatalf("current = %+v (%s), %v", cur, cur.Document, err)
	}
	old, err := st.GetTemplateRevision(ctx, created.ID, 1)
	if err != nil || old.Name != "first" || old.Revision != 1 {
		t.Fatalf("revision 1 = %+v, %v", old, err)
	}
	if old.UpdatedBy != "alice" || !old.UpdatedAt.Equal(created.UpdatedAt) || cur.UpdatedBy != "bob" {
		t.Fatalf("revision 1 reads saver %q at %v, current saver %q; want revision 1's own (alice at %v)", old.UpdatedBy, old.UpdatedAt, cur.UpdatedBy, created.UpdatedAt)
	}
	if _, err := st.GetTemplateRevision(ctx, created.ID, 3); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revision 3 err = %v", err)
	}
	if _, err := st.DeleteTemplate(ctx, created.ID, nil); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM run_template_revisions WHERE template_id = $1`, created.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("revisions after delete = %d, %v", n, err)
	}
}

// TestPG_RunTemplates_NamePerAudience: a name is unique within one audience,
// case-insensitively, and free in every other audience.
func TestPG_RunTemplates_NamePerAudience(t *testing.T) {
	st, _ := templatesPG(t)
	ctx := context.Background()
	if _, err := st.CreateTemplate(ctx, pgTemplate(personOwner, "Deploy"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTemplate(ctx, pgTemplate(personOwner, "deploy"), nil); !errors.Is(err, store.ErrTemplateNameTaken) {
		t.Fatalf("same audience err = %v", err)
	}
	for _, o := range []types.TemplateOwner{
		{Scope: types.TemplateScopePerson, Person: "bob"}, {Scope: types.TemplateScopeOrg},
		{Scope: types.TemplateScopeGroup, Group: "eng"}, {Scope: types.TemplateScopeGroup, Group: "ops"},
	} {
		if _, err := st.CreateTemplate(ctx, pgTemplate(o, "deploy"), nil); err != nil {
			t.Errorf("%+v: %v", o, err)
		}
	}
	other, _ := st.CreateTemplate(ctx, pgTemplate(personOwner, "other"), nil)
	other.Name, other.UpdatedBy = "DEPLOY", "alice"
	if _, err := st.UpdateTemplate(ctx, other, 1, nil); !errors.Is(err, store.ErrTemplateNameTaken) {
		t.Fatalf("rename onto a taken name err = %v", err)
	}
}

// TestPG_RunTemplates_LastSlotRace (D25-B): with one slot left in an audience,
// sixteen creates of different names race for it. Exactly one lands, because
// the count and the insert sit under the audience's named lock
// (templateAudienceLockSQL).
func TestPG_RunTemplates_LastSlotRace(t *testing.T) {
	st, pool := templatesPG(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO run_templates (id, scope, owner_id, kind, name, revision, created_at, created_by, updated_at, updated_by)
		SELECT gen_random_uuid(), 'person', 'alice', 'template', 'seed ' || g, 1, now(), 'alice', now(), 'alice'
		FROM generate_series(1, $1) g`, store.TemplatesMaxPerPerson-1); err != nil {
		t.Fatal(err)
	}
	// Each insert waits a moment before it lands, so every racer has counted
	// before the first one commits: without the lock they would all see the
	// last slot free.
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION test_slow_template_insert() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN PERFORM pg_sleep(0.3); RETURN NEW; END $$;
		CREATE TRIGGER test_slow_template_insert BEFORE INSERT ON run_templates
		FOR EACH ROW EXECUTE FUNCTION test_slow_template_insert()`); err != nil {
		t.Fatal(err)
	}
	const racers = 16
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, racers)
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = st.CreateTemplate(ctx, pgTemplate(personOwner, "racer "+uuid.NewString()), nil)
		}()
	}
	close(start)
	wg.Wait()
	won := 0
	for _, err := range errs {
		var limit *store.TemplateLimitError
		switch {
		case err == nil:
			won++
		case !errors.As(err, &limit) || limit.Limit != store.TemplatesMaxPerPerson:
			t.Errorf("a racer failed otherwise: %v", err)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM run_templates WHERE owner_id = 'alice'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if won != 1 || n != store.TemplatesMaxPerPerson {
		t.Fatalf("%d racers won and the audience holds %d rows, want 1 and %d", won, n, store.TemplatesMaxPerPerson)
	}
}

// TestPG_RunTemplates_GrantReadInsideTheWrite: a group write re-reads the
// writer's grant FOR SHARE inside its own transaction. A revoke that holds the
// row when the write begins makes the write wait, and once it commits the
// write finds no grant and stores nothing.
func TestPG_RunTemplates_GrantReadInsideTheWrite(t *testing.T) {
	st, pool := templatesPG(t)
	ctx := context.Background()
	eng := types.TemplateOwner{Scope: types.TemplateScopeGroup, Group: "eng"}
	check := &store.TemplateGrantCheck{Group: "eng", Persons: []string{"gary", "gary@corp.example"}}
	if _, err := st.PutTemplateGroupAdmin(ctx, types.TemplateGroupAdmin{Group: "eng", Person: "gary@corp.example", GrantedBy: "root"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTemplate(ctx, pgTemplate(eng, "with the grant"), check); err != nil {
		t.Fatalf("write under a live grant: %v", err)
	}
	revoke, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer revoke.Rollback(ctx) //nolint:errcheck // a no-op after Commit
	if _, err := revoke.Exec(ctx, `DELETE FROM template_group_admins WHERE group_id = 'eng' AND person = 'gary@corp.example'`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := st.CreateTemplate(ctx, pgTemplate(eng, "racing the revoke"), check)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the write did not wait for the revoke holding the grant: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := revoke.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, store.ErrTemplateGrantGone) {
		t.Fatalf("write after the revoke err = %v, want ErrTemplateGrantGone", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM run_templates WHERE group_id = 'eng'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("eng holds %d templates, want 1", n)
	}
}

// TestPG_RunTemplates_GrantsLabelsAndVisibility: grants and labels round-trip,
// and a person sees the organisation's, their own and their groups' templates.
func TestPG_RunTemplates_GrantsLabelsAndVisibility(t *testing.T) {
	st, _ := templatesPG(t)
	ctx := context.Background()
	g, err := st.PutTemplateGroupAdmin(ctx, types.TemplateGroupAdmin{Group: "eng", Person: "gary", GrantedBy: "root", Label: "Engineering"})
	if err != nil || g.Label != "Engineering" {
		t.Fatalf("put = %+v, %v", g, err)
	}
	if g, err = st.PutTemplateGroupAdmin(ctx, types.TemplateGroupAdmin{Group: "eng", Person: "gina", GrantedBy: "root"}); err != nil || g.Label != "Engineering" {
		t.Fatalf("a second grant without a label = %+v, %v", g, err)
	}
	if groups, _ := st.TemplateGroupAdminGroups(ctx, []string{"gary", "nobody"}); len(groups) != 1 || groups[0] != "eng" {
		t.Fatalf("gary's groups = %v", groups)
	}
	if removed, _ := st.DeleteTemplateGroupAdmin(ctx, "eng", "gina"); !removed {
		t.Fatal("gina's grant was not removed")
	}
	if removed, _ := st.DeleteTemplateGroupAdmin(ctx, "eng", "gina"); removed {
		t.Fatal("a second revoke removed something")
	}
	all, _ := st.ListTemplateGroupAdmins(ctx)
	if len(all) != 1 || all[0].Person != "gary" || all[0].Label != "Engineering" {
		t.Fatalf("grants = %+v", all)
	}
	for _, o := range []types.TemplateOwner{personOwner, {Scope: types.TemplateScopePerson, Person: "bob"}, {Scope: types.TemplateScopeOrg},
		{Scope: types.TemplateScopeGroup, Group: "eng"}, {Scope: types.TemplateScopeGroup, Group: "ops"}} {
		if _, err := st.CreateTemplate(ctx, pgTemplate(o, string(o.Scope)+o.Person+o.Group), nil); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := st.ListTemplatesVisible(ctx, "alice", []string{"eng"})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range rows {
		names = append(names, r.Name)
	}
	if len(names) != 3 || names[0] != "groupeng" || names[1] != "org" || names[2] != "personalice" {
		t.Fatalf("alice in eng sees %v", names)
	}
}

// TestPG_RunTemplates_EraseRunSetups: run_setups deletes the person's own
// templates (and revisions) and grants, and blanks their name on shared rows,
// revisions and grants they wrote. Nobody else's rows move.
func TestPG_RunTemplates_EraseRunSetups(t *testing.T) {
	st, pool := templatesPG(t)
	ctx := context.Background()
	mine, _ := st.CreateTemplate(ctx, pgTemplate(personOwner, "mine"), nil)
	shared, _ := st.CreateTemplate(ctx, pgTemplate(types.TemplateOwner{Scope: types.TemplateScopeOrg}, "shared"), nil)
	bobs := pgTemplate(types.TemplateOwner{Scope: types.TemplateScopePerson, Person: "bob"}, "bob's")
	bobs.CreatedBy = "bob"
	if _, err := st.CreateTemplate(ctx, bobs, nil); err != nil {
		t.Fatal(err)
	}
	_, _ = st.PutTemplateGroupAdmin(ctx, types.TemplateGroupAdmin{Group: "eng", Person: "alice", GrantedBy: "root"})
	_, _ = st.PutTemplateGroupAdmin(ctx, types.TemplateGroupAdmin{Group: "eng", Person: "bob", GrantedBy: "alice"})
	for name, by := range map[string]string{"alices-preset": "Alice", "bobs-preset": "bob"} {
		if _, _, err := st.PutLaunchPreset(ctx, types.LaunchPreset{Name: name, Request: json.RawMessage(`{}`), CreatedBy: by}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.EraseRunSetupsOf(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	// authorship: the shared row (created_by and updated_by: one row), its one
	// revision, the grant alice wrote for bob, and the launch preset she wrote.
	if got.Templates != 1 || got.Grants != 1 || got.AuthorshipCleared != 4 {
		t.Fatalf("erased = %+v", got)
	}
	for name, want := range map[string]string{"alices-preset": "", "bobs-preset": "bob"} {
		p, err := st.GetLaunchPreset(ctx, name)
		if err != nil || p.CreatedBy != want || p.UpdatedBy != want {
			t.Errorf("preset %s = created_by %q updated_by %q (%v), want %q", name, p.CreatedBy, p.UpdatedBy, err, want)
		}
	}
	if _, err := st.GetTemplate(ctx, mine.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("alice's template err = %v", err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM run_template_revisions WHERE template_id = $1`, mine.ID).Scan(&n)
	var left int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM run_templates t JOIN run_template_revisions r ON r.template_id = t.id
		WHERE t.created_by = 'alice' OR t.updated_by = 'alice' OR r.created_by = 'alice'`).Scan(&left)
	if n != 0 || left != 0 {
		t.Fatalf("revisions of alice's template %d, rows still naming alice %d", n, left)
	}
	if cur, _ := st.GetTemplate(ctx, shared.ID); cur.CreatedBy != "" || cur.Name != "shared" {
		t.Fatalf("shared row = %+v", cur)
	}
	all, _ := st.ListTemplateGroupAdmins(ctx)
	if len(all) != 1 || all[0].Person != "bob" || all[0].GrantedBy != "" {
		t.Fatalf("grants = %+v", all)
	}
	if _, err := st.EraseRunSetupsOf(ctx, ""); err == nil {
		t.Fatal("an erase with no person ran")
	}
}
