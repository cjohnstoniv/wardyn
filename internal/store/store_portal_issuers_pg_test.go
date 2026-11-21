// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// An issuer binds to one internal address: asking again with the same one returns the binding, another is
// refused, and nothing changes.
func TestPortalIssuerBinding_OtherAddressRefused_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()
	first, err := st.BindPortalIssuerQ(ctx, pool, "https://idp.example.com", "https://a.internal", "uid", "test")
	if err != nil {
		t.Fatal(err)
	}
	if first.IssuerKey != types.PortalIssuerKey("https://idp.example.com") {
		t.Fatalf("key = %s, want the hash of the exact issuer", first.IssuerKey)
	}
	again, err := st.BindPortalIssuerQ(ctx, pool, "https://idp.example.com", "https://a.internal", "uid", "test")
	if err != nil || again.IssuerKey != first.IssuerKey {
		t.Fatalf("same binding again = %+v, %v", again, err)
	}
	if _, err := st.BindPortalIssuerQ(ctx, pool, "https://idp.example.com", "https://b.internal", "uid", "test"); !errors.Is(err, store.ErrIssuerAddressConflict) {
		t.Fatalf("another address = %v, want ErrIssuerAddressConflict", err)
	}
	// Another spelling of a bound issuer is refused too, and is not a second binding.
	if _, err := st.BindPortalIssuerQ(ctx, pool, "https://IDP.example.com/", "https://a.internal", "uid", "test"); !errors.Is(err, store.ErrIssuerSpellingConflict) {
		t.Fatalf("another spelling = %v, want ErrIssuerSpellingConflict", err)
	}
	if n := rowCount(t, pool, `SELECT count(*) FROM portal_issuers`); n != 1 {
		t.Fatalf("%d bindings, want 1", n)
	}
}

// An issuer binds to one person-id claim: the other one is refused.
func TestPortalIssuerBinding_OtherProfileRefused_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()
	if _, err := st.BindPortalIssuerQ(ctx, pool, "https://idp.example.com", "https://a.internal", "uid", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.BindPortalIssuerQ(ctx, pool, "https://idp.example.com", "https://a.internal", "sub", "test"); !errors.Is(err, store.ErrIssuerProfileConflict) {
		t.Fatalf("the other claim = %v, want ErrIssuerProfileConflict", err)
	}
}

// A retirement and a first introduction on one binding never both win: an introduction that committed first
// makes the retirement refuse, and one that waits behind the retirement finds the binding gone and writes
// nothing.
func TestPortalIssuerBinding_RetireRacesFirstIntroduction_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()

	// The introduction first.
	a := newIssuerPortal(t, st, "")
	issueFor(t, st, a, "alice")
	if _, _, err := st.RevokeDelegate(ctx, a.Delegate.ID, time.Now().UTC(), false); err != nil {
		t.Fatal(err)
	}
	if err := st.RetirePortalIssuerQ(ctx, pool, a.Key); !errors.Is(err, store.ErrIssuerInUse) {
		t.Fatalf("retire after an introduction = %v, want ErrIssuerInUse", err)
	}
	if n := rowCount(t, pool, `SELECT count(*) FROM portal_issuers WHERE issuer_key = $1`, a.Key); n != 1 {
		t.Fatal("a refused retirement removed the binding")
	}

	// The retirement first: it holds the binding while an introduction waits.
	b := newIssuerPortal(t, st, "")
	if _, _, err := st.RevokeDelegate(ctx, b.Delegate.ID, time.Now().UTC(), false); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := st.RetirePortalIssuerQ(ctx, tx, b.Key); err != nil {
		t.Fatalf("retire of an unused binding = %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := st.IssuePortalIdentity(ctx, types.PortalPrincipal(b.Key, "bob"), b.Key, "", b.Issuer, b.Delegate.ID, b.Delegate.AdmissionRev)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the introduction did not wait for the retirement: %v", err)
	case <-time.After(time.Second):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, store.ErrPortalGone) {
		t.Fatalf("introduction behind a retirement = %v, want ErrPortalGone", err)
	}
	if n := rowCount(t, pool, `SELECT count(*) FROM principal_identities WHERE issuer = $1`, types.PortalPersonPrefix+b.Key); n != 0 {
		t.Fatalf("%d identities under a retired binding, want 0", n)
	}
	if err := st.RetirePortalIssuerQ(ctx, pool, b.Key); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second retire = %v, want ErrNotFound", err)
	}
}

func capabilityGrantFor(subject string) types.CapabilityGrant {
	return types.CapabilityGrant{SubjectType: types.CapabilitySubjectUser, Subject: subject, Capability: "test_" + uuid.NewString(),
		Value: "pypi.org", Effect: types.CapabilityAllow, CreatedBy: "admin@example.com"}
}

// Once a binding is retired, its key is unknown to every allow row; while it is bound, the same row writes.
func TestPortalIssuerBinding_RetiredKeyUnknownToAllowRows_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()
	p := newIssuerPortal(t, st, "")
	person := types.PortalPrincipal(p.Key, "alice")

	if _, err := store.UpsertCapabilityGrantQ(ctx, pool, capabilityGrantFor(person)); err != nil {
		t.Fatalf("allow row under a bound key = %v", err)
	}
	if _, _, err := st.RevokeDelegate(ctx, p.Delegate.ID, time.Now().UTC(), false); err != nil {
		t.Fatal(err)
	}
	// The grant just written is a census hit, so the binding is in use; remove it, then retire.
	if err := st.RetirePortalIssuerQ(ctx, pool, p.Key); !errors.Is(err, store.ErrIssuerInUse) {
		t.Fatalf("retire with an allow row = %v, want ErrIssuerInUse", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM capability_grants WHERE subject = $1`, person); err != nil {
		t.Fatal(err)
	}
	if err := st.RetirePortalIssuerQ(ctx, pool, p.Key); err != nil {
		t.Fatalf("retire after the row went = %v", err)
	}
	if _, err := store.UpsertCapabilityGrantQ(ctx, pool, capabilityGrantFor(person)); !errors.Is(err, store.ErrDelegatePersonUnknownPortal) {
		t.Fatalf("allow row under a retired key = %v, want ErrDelegatePersonUnknownPortal", err)
	}
	if bound, err := store.IssuerKeyBoundQ(ctx, pool, p.Key); err != nil || bound {
		t.Fatalf("IssuerKeyBoundQ after retirement = %v, %v", bound, err)
	}
}

// A row that was valid when written and is now ruled invalid can still be removed: removal is never ruled.
func TestPortalSubjectRuling_OldInvalidGrantRemovable_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()
	person := types.PortalPrincipal(types.PortalIssuerKey("https://never-bound.example.com"), "alice")

	g := capabilityGrantFor(person)
	g.ID = uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO capability_grants (id, subject_type, subject, capability, value, effect, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, g.ID, g.SubjectType, g.Subject, g.Capability, g.Value, g.Effect, g.CreatedBy); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertCapabilityGrant(ctx, g); !errors.Is(err, store.ErrDelegatePersonUnknownPortal) {
		t.Fatalf("rewriting the old row = %v, want ErrDelegatePersonUnknownPortal", err)
	}
	if _, err := st.DeleteCapabilityGrant(ctx, g.ID); err != nil {
		t.Fatalf("removing the old row = %v, want it removed", err)
	}

	m, err := st.UpsertRoleMapping(ctx, types.RoleMapping{Value: "removable-" + uuid.NewString(), Role: "user", UserType: "standard", CreatedBy: "admin@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE role_mappings SET value = $2 WHERE id = $1`, m.ID, person); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertRoleMapping(ctx, types.RoleMapping{Value: person, Role: "user", UserType: "standard", CreatedBy: "admin@example.com"}); !errors.Is(err, store.ErrDelegatePersonNotAllowed) {
		t.Fatalf("writing a role mapping for a portal person = %v, want ErrDelegatePersonNotAllowed", err)
	}
	if err := st.DeleteRoleMapping(ctx, m.ID); err != nil {
		t.Fatalf("removing the old role mapping = %v", err)
	}
}

// uiWriter is one durable write of a stored document that can name a UI-app caller, driven with the caller of
// a person under the issuer key it is given, and the census column that must then find the stored row.
type uiWriter struct {
	name, census string
	write        func(key string) error
}

// uiCallerJSON is a UI-app caller of a portal person under key, spelled with JSON escapes the way a client
// that escapes every character spells it: "user" and "delegate" as \u escapes, which a text search for the
// prefix does not see and the decoder reads back.
func uiCallerJSON(key string) string {
	caller := "user:" + types.PortalPrincipal(key, "alice")
	return strings.Replace(caller, "user:delegate", `\u0075ser:\u0064elegate`, 1)
}

func uiAppsJSON(key string) string {
	return `{"ui_apps":[{"name":"app","port":8080,"inbound":true,"callers":["` + uiCallerJSON(key) + `"]}]}`
}

// uiWriters are the durable writers of the policy documents the ruling reads: a policy's spec on create and
// on update, a preset's inline policy and its policy overlay, a template's intent inline policy, a standalone
// unit's ceiling, and a composed unit's overlay (on the Default unit, as the schema wants a parent with every
// overlay). The typed documents carry a real portal principal; the preset and template documents are raw
// JSON with the prefix itself escaped, which is where an escape changes what a text search finds.
func uiWriters(st store.PG, pool *pgxpool.Pool) []uiWriter {
	ctx := context.Background()
	now := time.Now().UTC()
	callerOf := func(key string) string { return "user:" + types.PortalPrincipal(key, "alice") }
	parent := store.DefaultUnitID
	specWith := func(caller string) types.RunPolicySpec {
		return types.RunPolicySpec{UIApps: []types.UIApp{{Name: "app", Port: 8080, Inbound: true, Callers: []string{caller}}}}
	}
	preset := func(request string) types.LaunchPreset {
		return types.LaunchPreset{Name: "preset-" + uuid.NewString(), UserTypes: []string{}, Request: json.RawMessage(request), CreatedBy: "admin@example.com"}
	}
	template := func(document string) types.RunTemplate {
		return types.RunTemplate{ID: uuid.New(), Owner: types.TemplateOwner{Scope: types.TemplateScopePerson, Person: "alice"},
			Kind: types.TemplateKindTemplate, Name: "t-" + uuid.NewString(), Document: json.RawMessage(document), CreatedBy: "alice"}
	}
	unit := func(u types.GovernanceUnit) error {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		u.Name, u.CreatedBy = "unit-"+uuid.NewString(), "admin@example.com"
		if _, err := store.WriteGovernanceUnitQ(ctx, tx, uuid.New(), func(_ []types.GovernanceUnit, _ []types.GovernanceMembership) (types.GovernanceUnit, error) {
			return u, nil
		}); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	return []uiWriter{
		{"CreatePolicy", "run_policies.spec", func(key string) error {
			_, err := st.CreatePolicy(ctx, types.RunPolicy{ID: uuid.New(), Name: "p-" + uuid.NewString(), CreatedAt: now, UpdatedAt: now, Spec: specWith(callerOf(key))})
			return err
		}},
		{"UpdatePolicy", "run_policies.spec", func(key string) error {
			base, err := st.CreatePolicy(ctx, types.RunPolicy{ID: uuid.New(), Name: "p-" + uuid.NewString(), CreatedAt: now, UpdatedAt: now})
			if err != nil {
				return err
			}
			_, err = st.UpdatePolicy(ctx, base.ID, base.UpdatedAt, base.Name, specWith(callerOf(key)))
			return err
		}},
		{"PutLaunchPreset inline_policy", "launch_presets.request", func(key string) error {
			_, _, err := st.PutLaunchPreset(ctx, preset(`{"task":"x","inline_policy":`+uiAppsJSON(key)+`}`))
			return err
		}},
		{"PutLaunchPreset policy_overlay", "launch_presets.request", func(key string) error {
			_, _, err := st.PutLaunchPreset(ctx, preset(`{"task":"x","policy_overlay":`+uiAppsJSON(key)+`}`))
			return err
		}},
		{"CreateTemplate intent.inline_policy", "run_template_revisions.document", func(key string) error {
			_, err := st.CreateTemplate(ctx, template(`{"kind":"template","intent":{"inline_policy":`+uiAppsJSON(key)+`}}`), nil)
			return err
		}},
		{"WriteGovernanceUnitQ ceiling", "governance_units.ceiling", func(key string) error {
			return unit(types.GovernanceUnit{Ceiling: specWith(callerOf(key))})
		}},
		{"WriteGovernanceUnitQ composed overlay", "governance_units.overlay", func(key string) error {
			return unit(types.GovernanceUnit{ParentID: &parent, Overlay: &types.CeilingOverlay{UIApps: &[]types.UIApp{{Name: "app", Port: 8080, Inbound: true, Callers: []string{callerOf(key)}}}}})
		}},
	}
}

// Every durable write of a stored document that can name a UI-app caller rules the callers inside it: a caller
// of a portal person whose binding does not exist is refused, one whose binding exists is stored. Against a
// retirement of that binding each write is driven in the orders that matter, with two transactions and explicit
// sequencing: the retirement commits first and the write is refused; the retirement is in flight when the
// write starts, the write waits on the binding row and is refused when the retirement commits; the write
// commits first and the retirement's census finds its row (ErrIssuerInUse).
func TestPortalSubjectRuling_UICallersRuledAtEverySpecWrite_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()
	unbound := types.PortalIssuerKey("https://never-bound.example.com")
	revokedPortal := func() issuerPortal {
		p := newIssuerPortal(t, st, "")
		if _, _, err := st.RevokeDelegate(ctx, p.Delegate.ID, time.Now().UTC(), false); err != nil {
			t.Fatal(err)
		}
		return p
	}
	installWriteGate(t, pool)
	for _, w := range uiWriters(st, pool) {
		t.Run(w.name, func(t *testing.T) {
			if err := w.write(unbound); !errors.Is(err, store.ErrDelegatePersonUnknownPortal) {
				t.Errorf("a caller of an unbound issuer = %v, want ErrDelegatePersonUnknownPortal", err)
			}

			// The write is in flight first: it holds the binding FOR KEY SHARE and is parked in its insert, the
			// retirement starts and waits for it, and when the write commits the retirement's census finds it.
			first := revokedPortal()
			gate := holdWriteGate(t, pool)
			wrote := make(chan error, 1)
			go func() { wrote <- w.write(first.Key) }()
			untilBlockedBy(t, pool, pidOf(t, gate), 1)
			writer := sessionBlockedBy(t, pool, pidOf(t, gate))
			retire1, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = retire1.Rollback(ctx) })
			retired := make(chan error, 1)
			go func() { retired <- st.RetirePortalIssuerQ(ctx, retire1, first.Key) }()
			untilBlockedBy(t, pool, writer, 1)
			if err := gate.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-wrote; err != nil {
				t.Fatalf("a caller of a bound issuer = %v", err)
			}
			if err := <-retired; !errors.Is(err, store.ErrIssuerInUse) {
				t.Errorf("retirement behind the write = %v, want ErrIssuerInUse", err)
			}
			_ = retire1.Rollback(ctx)
			if hits, err := store.NamespaceCensusQ(ctx, pool, first.Key); err != nil || hits[w.census] < 1 {
				t.Errorf("census after the write = %v, %v; want %s found", hits, err, w.census)
			}

			// The retirement is in flight when the write starts: the write waits for its commit.
			inflight := revokedPortal()
			retire, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = retire.Rollback(ctx) })
			if err := st.RetirePortalIssuerQ(ctx, retire, inflight.Key); err != nil {
				t.Fatalf("retirement with no row: %v", err)
			}
			done := make(chan error, 1)
			go func() { done <- w.write(inflight.Key) }()
			untilBlockedBy(t, pool, pidOf(t, retire), 1)
			if err := retire.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-done; !errors.Is(err, store.ErrDelegatePersonUnknownPortal) {
				t.Errorf("a write behind a retirement = %v, want ErrDelegatePersonUnknownPortal", err)
			}
			if hits, err := store.NamespaceCensusQ(ctx, pool, inflight.Key); err != nil || hits[w.census] != 0 {
				t.Errorf("census after the refused write = %v, %v; want %s empty", hits, err, w.census)
			}

			// The retirement commits first: the write is refused.
			gone := revokedPortal()
			if err := st.RetirePortalIssuerQ(ctx, pool, gone.Key); err != nil {
				t.Fatalf("retirement with no row: %v", err)
			}
			if err := w.write(gone.Key); !errors.Is(err, store.ErrDelegatePersonUnknownPortal) {
				t.Errorf("a write after the retirement = %v, want ErrDelegatePersonUnknownPortal", err)
			}
		})
	}
}

// The activation mark is a key of the site config document that only ActivatePortalIssuers writes. Whole
// document saves, from this binary or an older one, keep it; a second activation keeps the first time; and
// with no stored row the activation creates one.
func TestPortalIssuersActivate_MarkSurvivesWholeDocumentSaves_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()
	markOf := func() string {
		t.Helper()
		var v *string
		if err := pool.QueryRow(ctx, `SELECT config ->> $1 FROM site_config WHERE singleton`, store.PortalIssuersActivatedKey).Scan(&v); err != nil {
			t.Fatalf("read the mark: %v", err)
		}
		if v == nil {
			return ""
		}
		return *v
	}

	if on, err := st.PortalIssuersActivated(ctx); err != nil || on {
		t.Fatalf("before any row: activated = %v, %v", on, err)
	}
	if n := rowCount(t, pool, `SELECT count(*) FROM site_config`); n != 0 {
		t.Fatalf("%d site_config rows on a fresh database, want 0", n)
	}
	if err := st.ActivatePortalIssuers(ctx, "admin@example.com", nil); err != nil {
		t.Fatal(err)
	}
	first := markOf()
	if first == "" {
		t.Fatal("activation with no stored row left no mark")
	}
	if on, err := st.PortalIssuersActivated(ctx); err != nil || !on {
		t.Fatalf("after activation: activated = %v, %v", on, err)
	}

	for range 2 {
		if _, err := st.PutSiteConfig(ctx, types.SiteConfig{}); err != nil {
			t.Fatal(err)
		}
		if got := markOf(); got != first {
			t.Fatalf("a whole-document save left the mark %q, want %q", got, first)
		}
	}
	// An older binary's save: its statement replaces the keys it declares and keeps the rest.
	if _, err := pool.Exec(ctx, `
		INSERT INTO site_config (singleton, config, updated_at) VALUES (true, '{"unrelated":1}'::jsonb, now())
		ON CONFLICT (singleton) DO UPDATE SET config = (site_config.config - ARRAY['unrelated']::text[]) || EXCLUDED.config, updated_at = now()`); err != nil {
		t.Fatal(err)
	}
	if got := markOf(); got != first {
		t.Fatalf("an older whole-document save left the mark %q, want %q", got, first)
	}

	time.Sleep(20 * time.Millisecond)
	if err := st.ActivatePortalIssuers(ctx, "admin@example.com", nil); err != nil {
		t.Fatal(err)
	}
	if got := markOf(); got != first {
		t.Fatalf("a second activation moved the mark to %q, want %q", got, first)
	}
}

// A policy write that names a portal person as a UI-app caller and the retirement of that person's issuer
// binding never both succeed: the write takes the binding FOR KEY SHARE before it writes, the retirement takes
// it FOR UPDATE and then runs its census.
func TestPortalSubjectRuling_RetireRacesPolicyCaller_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()
	for round := range 15 {
		p := newIssuerPortal(t, st, "")
		if _, _, err := st.RevokeDelegate(ctx, p.Delegate.ID, time.Now().UTC(), false); err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		spec := types.RunPolicySpec{UIApps: []types.UIApp{{Name: "app", Port: 8080, Inbound: true,
			Callers: []string{"user:" + types.PortalPrincipal(p.Key, "alice")}}}}
		var wg sync.WaitGroup
		var writeErr, retireErr error
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, writeErr = st.CreatePolicy(ctx, types.RunPolicy{ID: uuid.New(), Name: "race-" + uuid.NewString(), CreatedAt: now, UpdatedAt: now, Spec: spec})
		}()
		go func() {
			defer wg.Done()
			<-start
			tx, err := pool.Begin(ctx)
			if err != nil {
				retireErr = err
				return
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if retireErr = st.RetirePortalIssuerQ(ctx, tx, p.Key); retireErr == nil {
				retireErr = tx.Commit(ctx)
			}
		}()
		close(start)
		wg.Wait()
		switch {
		case writeErr == nil && errors.Is(retireErr, store.ErrIssuerInUse):
		case errors.Is(writeErr, store.ErrDelegatePersonUnknownPortal) && retireErr == nil:
		default:
			t.Fatalf("round %d: the policy write returned %v and the retirement %v; exactly one must succeed", round, writeErr, retireErr)
		}
	}
}

// writeGateKey is the advisory lock the test gate trigger waits on.
const writeGateKey = 7770001

// installWriteGate puts a trigger on the four tables that store a policy document: a row that names a portal
// person waits for the advisory lock writeGateKey before it is written. A test that holds the lock (holdWriteGate)
// parks a real write inside its transaction, after the write took the issuer binding FOR KEY SHARE and before it
// commits; with the lock free the trigger passes at once.
func installWriteGate(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	stmts := []string{`CREATE FUNCTION wardyn_test_write_gate() RETURNS trigger LANGUAGE plpgsql AS $g$
		BEGIN
			IF NEW::text ILIKE '%delegate:person:%' THEN PERFORM pg_advisory_xact_lock(7770001); END IF;
			RETURN NEW;
		END $g$`}
	for _, table := range []string{"run_policies", "launch_presets", "run_template_revisions", "governance_units"} {
		stmts = append(stmts, `CREATE TRIGGER wardyn_test_write_gate BEFORE INSERT OR UPDATE ON `+table+` FOR EACH ROW EXECUTE FUNCTION wardyn_test_write_gate()`)
	}
	for _, stmt := range stmts {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("install the write gate: %v", err)
		}
	}
}

// holdWriteGate opens a transaction that holds the gate's lock until the test commits it.
func holdWriteGate(t *testing.T, pool *pgxpool.Pool) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(writeGateKey)); err != nil {
		t.Fatal(err)
	}
	return tx
}

// sessionBlockedBy is the process id of the one session waiting on holder.
func sessionBlockedBy(t *testing.T, pool *pgxpool.Pool, holder int) int {
	t.Helper()
	var pid int
	if err := pool.QueryRow(context.Background(), `SELECT pid FROM pg_stat_activity WHERE datname = current_database() AND $1 = ANY(pg_blocking_pids(pid))`, holder).Scan(&pid); err != nil {
		t.Fatalf("the session waiting on %d: %v", holder, err)
	}
	return pid
}
