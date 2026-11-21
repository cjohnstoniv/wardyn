// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

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

	m, err := st.UpsertRoleMapping(ctx, types.RoleMapping{Value: "removable-" + uuid.NewString(), Role: "member", CreatedBy: "admin@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE role_mappings SET value = $2 WHERE id = $1`, m.ID, person); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertRoleMapping(ctx, types.RoleMapping{Value: person, Role: "member", CreatedBy: "admin@example.com"}); !errors.Is(err, store.ErrDelegatePersonNotAllowed) {
		t.Fatalf("writing a role mapping for a portal person = %v, want ErrDelegatePersonNotAllowed", err)
	}
	if err := st.DeleteRoleMapping(ctx, m.ID); err != nil {
		t.Fatalf("removing the old role mapping = %v", err)
	}
}

// Every store write of a stored document that can name a UI-app caller rules the callers inside it: a caller
// of a portal person whose binding does not exist is refused, one whose binding exists is stored.
func TestPortalSubjectRuling_UICallersRuledAtEverySpecWrite_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()
	p := newIssuerPortal(t, st, "")
	unbound := types.PortalIssuerKey("https://never-bound.example.com")
	callerOf := func(key string) string { return "user:" + types.PortalPrincipal(key, "alice") }
	specWith := func(caller string) types.RunPolicySpec {
		return types.RunPolicySpec{UIApps: []types.UIApp{{Name: "app", Port: 8080, Inbound: true, Callers: []string{caller}}}}
	}
	refused := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, store.ErrDelegatePersonUnknownPortal) {
			t.Errorf("%s with an unbound caller = %v, want ErrDelegatePersonUnknownPortal", what, err)
		}
	}
	now := time.Now().UTC()

	// run_policies: create and update.
	_, err := st.CreatePolicy(ctx, types.RunPolicy{ID: uuid.New(), Name: "bad-" + uuid.NewString(), CreatedAt: now, UpdatedAt: now, Spec: specWith(callerOf(unbound))})
	refused("CreatePolicy", err)
	good, err := st.CreatePolicy(ctx, types.RunPolicy{ID: uuid.New(), Name: "good-" + uuid.NewString(), CreatedAt: now, UpdatedAt: now, Spec: specWith(callerOf(p.Key))})
	if err != nil {
		t.Fatalf("CreatePolicy with a bound caller = %v", err)
	}
	_, err = st.UpdatePolicy(ctx, good.ID, good.UpdatedAt, good.Name, specWith(callerOf(unbound)))
	refused("UpdatePolicy", err)
	if _, err = st.UpdatePolicy(ctx, good.ID, good.UpdatedAt, good.Name, specWith(callerOf(p.Key))); err != nil {
		t.Fatalf("UpdatePolicy with a bound caller = %v", err)
	}

	// launch_presets.
	preset := func(caller string) types.LaunchPreset {
		req, _ := json.Marshal(map[string]any{"policy": specWith(caller)})
		return types.LaunchPreset{Name: "preset-" + uuid.NewString(), UserTypes: []string{}, Request: req, CreatedBy: "admin@example.com"}
	}
	_, _, err = st.PutLaunchPreset(ctx, preset(callerOf(unbound)))
	refused("PutLaunchPreset", err)
	if _, _, err = st.PutLaunchPreset(ctx, preset(callerOf(p.Key))); err != nil {
		t.Fatalf("PutLaunchPreset with a bound caller = %v", err)
	}

	// run_template_revisions: create and update.
	tmpl := func(caller string) types.RunTemplate {
		doc, _ := json.Marshal(map[string]any{"policy": specWith(caller)})
		return types.RunTemplate{ID: uuid.New(), Owner: types.TemplateOwner{Scope: types.TemplateScopePerson, Person: "alice"},
			Kind: types.TemplateKindTemplate, Name: "t-" + uuid.NewString(), Document: doc, CreatedBy: "alice"}
	}
	_, err = st.CreateTemplate(ctx, tmpl(callerOf(unbound)), nil)
	refused("CreateTemplate", err)
	okTmpl, err := st.CreateTemplate(ctx, tmpl(callerOf(p.Key)), nil)
	if err != nil {
		t.Fatalf("CreateTemplate with a bound caller = %v", err)
	}
	upd := okTmpl
	upd.Document = tmpl(callerOf(unbound)).Document
	_, err = st.UpdateTemplate(ctx, upd, okTmpl.Revision, nil)
	refused("UpdateTemplate", err)
	if n := rowCount(t, pool, `SELECT count(*) FROM run_template_revisions WHERE template_id = $1`, okTmpl.ID); n != 1 {
		t.Errorf("a refused update left %d revisions, want 1", n)
	}

	// governance_units: the standalone ceiling.
	writeUnit := func(caller string) error {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := store.WriteGovernanceUnitQ(ctx, tx, uuid.New(), func(_ []types.GovernanceUnit, _ []types.GovernanceMembership) (types.GovernanceUnit, error) {
			return types.GovernanceUnit{Name: "unit-" + uuid.NewString(), Ceiling: specWith(caller), CreatedBy: "admin@example.com"}, nil
		}); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	refused("WriteGovernanceUnitQ", writeUnit(callerOf(unbound)))
	if err := writeUnit(callerOf(p.Key)); err != nil {
		t.Fatalf("WriteGovernanceUnitQ with a bound caller = %v", err)
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
