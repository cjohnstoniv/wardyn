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
	if err := st.RetirePortalIssuerQ(ctx, pool, a.Key, nil); !errors.Is(err, store.ErrIssuerInUse) {
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
	if err := st.RetirePortalIssuerQ(ctx, tx, b.Key, nil); err != nil {
		t.Fatalf("retire of an unused binding = %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := st.IssuePortalIdentity(ctx, types.PortalPrincipal(b.Key, "bob"), b.Key, "", b.Issuer, b.Delegate.ID, b.Delegate.AdmissionRev)
		done <- err
	}()
	untilBlockedBy(t, pool, pidOf(t, tx), 1) // the introduction waits on the retirement's binding row
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, store.ErrPortalGone) {
		t.Fatalf("introduction behind a retirement = %v, want ErrPortalGone", err)
	}
	if n := rowCount(t, pool, `SELECT count(*) FROM principal_identities WHERE issuer = $1`, types.PortalPersonPrefix+b.Key); n != 0 {
		t.Fatalf("%d identities under a retired binding, want 0", n)
	}
	if err := st.RetirePortalIssuerQ(ctx, pool, b.Key, nil); !errors.Is(err, store.ErrNotFound) {
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
	if err := st.RetirePortalIssuerQ(ctx, pool, p.Key, nil); !errors.Is(err, store.ErrIssuerInUse) {
		t.Fatalf("retire with an allow row = %v, want ErrIssuerInUse", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM capability_grants WHERE subject = $1`, person); err != nil {
		t.Fatal(err)
	}
	if err := st.RetirePortalIssuerQ(ctx, pool, p.Key, nil); err != nil {
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
		{"PutLaunchPreset policy_overlay with upper-case keys", "launch_presets.request", func(key string) error {
			doc := strings.NewReplacer(`"ui_apps"`, `"UI_APPS"`, `"callers"`, `"Callers"`).Replace(uiAppsJSON(key))
			_, _, err := st.PutLaunchPreset(ctx, preset(`{"task":"x","policy_overlay":`+doc+`}`))
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
			go func() { retired <- st.RetirePortalIssuerQ(ctx, retire1, first.Key, nil) }()
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
			if err := st.RetirePortalIssuerQ(ctx, retire, inflight.Key, nil); err != nil {
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
			if err := st.RetirePortalIssuerQ(ctx, pool, gone.Key, nil); err != nil {
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
	if activated, err := st.ActivatePortalIssuers(ctx, "admin@example.com", nil); err != nil || !activated {
		t.Fatalf("the first activation = %v, %v; want it reported as the one that set the mark", activated, err)
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
	if activated, err := st.ActivatePortalIssuers(ctx, "admin@example.com", nil); err != nil || activated {
		t.Fatalf("a second activation = %v, %v; want it reported as not the one that set the mark", activated, err)
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
			if retireErr = st.RetirePortalIssuerQ(ctx, tx, p.Key, nil); retireErr == nil {
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

// Two activations at once: exactly one is told it set the mark, and the stored time is the one that call wrote
// (the other's statement changed nothing). The race is forced: a transaction holds the mark's insert open,
// the store call waits on it, and the row is then committed.
func TestPortalIssuersActivate_ConcurrentActivationsOneWinner_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()

	// The winner is a transaction that wrote the mark and has not committed; the store call waits behind it.
	winner, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = winner.Rollback(ctx) }()
	const stamp = "2001-02-03T04:05:06.000007Z"
	if _, err := winner.Exec(ctx, `INSERT INTO site_config (singleton, config, updated_at) VALUES (true, jsonb_build_object($1::text, $2::text), now())`,
		store.PortalIssuersActivatedKey, stamp); err != nil {
		t.Fatal(err)
	}
	type result struct {
		activated bool
		err       error
	}
	done := make(chan result, 1)
	go func() {
		activated, err := st.ActivatePortalIssuers(ctx, "admin@example.com", nil)
		done <- result{activated, err}
	}()
	untilBlockedBy(t, pool, pidOf(t, winner), 1)
	if err := winner.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.err != nil || r.activated {
		t.Fatalf("the activation that lost = %v, %v; want it reported as not the one that set the mark", r.activated, r.err)
	}
	var stored string
	if err := pool.QueryRow(ctx, `SELECT config ->> $1 FROM site_config WHERE singleton`, store.PortalIssuersActivatedKey).Scan(&stored); err != nil || stored != stamp {
		t.Fatalf("stored time = %q, %v; want the winner's %q", stored, err, stamp)
	}
}

// Two activations started together on a fresh database: exactly one is told it set the mark.
func TestPortalIssuersActivate_TwoStartedTogetherOneWinner_PG(t *testing.T) {
	st2, _ := isolatedStore(t)
	ctx := context.Background()
	type result struct {
		activated bool
		err       error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			activated, err := st2.ActivatePortalIssuers(ctx, "admin@example.com", nil)
			results <- result{activated, err}
		}()
	}
	close(start)
	wins := 0
	for range 2 {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.activated {
			wins++
		}
	}
	if wins != 1 {
		t.Errorf("%d of two concurrent activations were told they set the mark, want exactly 1", wins)
	}
}

// A portal revoked between an admission proposal and its decision takes no edit: the update is ErrStaleCompare
// and the revision does not move.
func TestUpdateDelegateAdmission_RevokedPortalIsStale_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()
	p := newIssuerPortal(t, st, "")
	was := types.PortalAdmission{Group: "", PortalManaged: true}
	// A managed-admission portal has no group; turning the flag off needs one (delegates_admission_shape), so
	// next is a valid edit that would land if the portal were live.
	next := types.PortalAdmission{Group: "portal-users", PortalManaged: false}
	rev := func() int {
		return rowCount(t, pool, `SELECT admission_rev FROM delegates WHERE id = $1`, p.Delegate.ID)
	}
	before := rev()

	if _, _, err := st.RevokeDelegate(ctx, p.Delegate.ID, time.Now().UTC(), false); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateDelegateAdmissionQ(ctx, pool, p.Delegate.ID, was, next); !errors.Is(err, store.ErrStaleCompare) {
		t.Fatalf("update on a revoked portal = %v, want ErrStaleCompare", err)
	}
	if after := rev(); after != before {
		t.Errorf("admission_rev = %d after the refused update, want %d", after, before)
	}
	if managed := rowCount(t, pool, `SELECT (portal_managed_admission)::int FROM delegates WHERE id = $1`, p.Delegate.ID); managed != 1 {
		t.Errorf("portal_managed_admission changed on a revoked portal")
	}
}

// A retirement decided on one binding record leaves a replacement alone: after the first binding is retired and
// the same issuer is registered again with a different internal address, the held decision carrying the old
// record is ErrStaleCompare and the replacement stays. A decision carrying the current record retires it.
func TestRetirePortalIssuer_StaleRecordLeavesReplacement_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()
	issuer := "https://idp-" + uuid.NewString() + ".example.com"
	old, err := st.BindPortalIssuerQ(ctx, pool, issuer, "https://internal-old-"+uuid.NewString()+".example.com", "uid", "test")
	if err != nil {
		t.Fatal(err)
	}
	held := old // the record the decision was made on

	if err := st.RetirePortalIssuerQ(ctx, pool, old.IssuerKey, &old); err != nil {
		t.Fatalf("retire with the current record = %v", err)
	}
	replacement, err := st.BindPortalIssuerQ(ctx, pool, issuer, "https://internal-new-"+uuid.NewString()+".example.com", "uid", "test")
	if err != nil {
		t.Fatal(err)
	}
	if replacement.IssuerKey != held.IssuerKey || replacement.InternalIssuerNorm == held.InternalIssuerNorm {
		t.Fatalf("replacement %+v should share the key and differ in address from %+v", replacement, held)
	}

	if err := st.RetirePortalIssuerQ(ctx, pool, held.IssuerKey, &held); !errors.Is(err, store.ErrStaleCompare) {
		t.Fatalf("retire with the old record = %v, want ErrStaleCompare", err)
	}
	if bound, err := store.IssuerKeyBoundQ(ctx, pool, replacement.IssuerKey); err != nil || !bound {
		t.Fatalf("replacement after the stale retire: bound=%v err=%v, want it kept", bound, err)
	}
	if err := st.RetirePortalIssuerQ(ctx, pool, replacement.IssuerKey, &replacement); err != nil {
		t.Fatalf("retire with the replacement's own record = %v", err)
	}
}

// Each field of the binding record the retirement was decided on is compared under the lock: the record is
// retired, the same issuer is registered again changing exactly one of created_at, the internal address or the
// person-id claim (the row's created_at is set back to the old one where the case must differ in the other
// field alone), and the held decision is ErrStaleCompare and the replacement stays. The issuer string itself
// cannot differ under one key, which is derived from its bytes.
func TestRetirePortalIssuer_EachRecordFieldPinsTheCompare_PG(t *testing.T) {
	cases := []struct {
		name       string
		sameAddr   bool
		claim      string
		keepStamp  bool // set created_at back to the held record's
		wantDiffer func(old, now types.PortalIssuer) bool
	}{
		{"created_at only", true, "uid", false, func(o, n types.PortalIssuer) bool {
			return !n.CreatedAt.Equal(o.CreatedAt) && n.InternalIssuerNorm == o.InternalIssuerNorm && n.PersonIDClaim == o.PersonIDClaim
		}},
		{"address only", false, "uid", true, func(o, n types.PortalIssuer) bool {
			return n.CreatedAt.Equal(o.CreatedAt) && n.InternalIssuerNorm != o.InternalIssuerNorm && n.PersonIDClaim == o.PersonIDClaim
		}},
		{"person-id claim only", true, "sub", true, func(o, n types.PortalIssuer) bool {
			return n.CreatedAt.Equal(o.CreatedAt) && n.InternalIssuerNorm == o.InternalIssuerNorm && n.PersonIDClaim != o.PersonIDClaim
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st, pool := isolatedStore(t)
			ctx := context.Background()
			issuer := "https://idp-" + uuid.NewString() + ".example.com"
			addr := "https://internal-" + uuid.NewString() + ".example.com"
			held, err := st.BindPortalIssuerQ(ctx, pool, issuer, addr, "uid", "test")
			if err != nil {
				t.Fatal(err)
			}
			if err := st.RetirePortalIssuerQ(ctx, pool, held.IssuerKey, &held); err != nil {
				t.Fatalf("retire with the current record = %v", err)
			}
			again := addr
			if !c.sameAddr {
				again = "https://internal-" + uuid.NewString() + ".example.com"
			}
			if _, err := st.BindPortalIssuerQ(ctx, pool, issuer, again, c.claim, "test"); err != nil {
				t.Fatal(err)
			}
			if c.keepStamp {
				if _, err := pool.Exec(ctx, `UPDATE portal_issuers SET created_at = $2 WHERE issuer_key = $1`, held.IssuerKey, held.CreatedAt); err != nil {
					t.Fatal(err)
				}
			}
			all, err := store.PortalIssuersQ(ctx, pool)
			if err != nil || len(all) != 1 {
				t.Fatalf("bindings after re-registering = %+v, %v", all, err)
			}
			if !c.wantDiffer(held, all[0]) {
				t.Fatalf("fixture: replacement %+v does not differ from %+v in %s alone", all[0], held, c.name)
			}
			if err := st.RetirePortalIssuerQ(ctx, pool, held.IssuerKey, &held); !errors.Is(err, store.ErrStaleCompare) {
				t.Fatalf("retire with the old record = %v, want ErrStaleCompare", err)
			}
			if bound, err := store.IssuerKeyBoundQ(ctx, pool, held.IssuerKey); err != nil || !bound {
				t.Fatalf("replacement after the stale retire: bound=%v err=%v, want it kept", bound, err)
			}
		})
	}
}
