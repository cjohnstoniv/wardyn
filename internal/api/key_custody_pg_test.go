// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretstore/keydomain"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func setupRows(t *testing.T, srv *Server) map[string]SetupCheck {
	t.Helper()
	code, st := decodeSetup(t, srv, adminToken)
	if code != http.StatusOK {
		t.Fatalf("GET /setup/status = %d", code)
	}
	out := map[string]SetupCheck{}
	for _, c := range st.Checks {
		out[c.ID] = c
	}
	return out
}

// The three key-custody rows come up on /setup/status, and an assignment change
// in the audit log moves the 30-day row from ok to warn, naming the latest.
func TestPG_SetupStatus_KeyCustodyRows(t *testing.T) {
	pool := throwawayPGPool(t)
	svc := keydomain.NewService(pool, []string{"finance", "research"})
	h := newHarness(t)
	h.srv.cfg.KeyDomains = svc
	h.srv.cfg.PrincipalKeys = true
	h.srv.router = h.srv.routes()

	rows := setupRows(t, h.srv)
	if c := rows["key_domains"]; c.Status != "ok" || c.Detail != "2 key domains, each proven at boot: finance, research." {
		t.Errorf("key_domains = %+v", c)
	}
	if c := rows["principal_keys"]; c.Status != "ok" || c.Detail != "On. Every stored credential is sealed under its owner's key." {
		t.Errorf("principal_keys = %+v", c)
	}
	if c := rows["key_domain_changes"]; c.Status != "ok" || c.Detail != "No key-domain assignment changed in the last 30 days." {
		t.Errorf("key_domain_changes with none = %+v", c)
	}

	// A credential still under the credential key, and two audited assignment
	// writes (one old enough to fall outside the 30 days).
	if _, err := pool.Exec(t.Context(), `INSERT INTO secrets (owned_by, name, enc_version, kek_id, wrapped_dek, ciphertext) VALUES ('bob','k',1,'local:x','\x00','\x00')`); err != nil {
		t.Fatalf("seed a v1 credential: %v", err)
	}
	now := time.Now().UTC()
	for _, ev := range []struct {
		action string
		at     time.Time
		by     string
	}{
		{"key_domain.assignment.set", now.Add(-40 * 24 * time.Hour), "old-admin"},
		{"key_domain.assignment.set", now.Add(-49 * time.Hour), "sec-1"},
		{"key_domain.assignment.delete", now.Add(-2 * time.Hour), "sec-2"},
	} {
		e := types.AuditEvent{ID: uuid.New(), Time: ev.at, ActorType: types.ActorHuman, Actor: ev.by, Action: ev.action, Target: "user:bob", Outcome: "success", Data: json.RawMessage(`{}`)}
		if err := store.InsertAuditEvent(t.Context(), pool, &e); err != nil {
			t.Fatalf("insert audit row: %v", err)
		}
	}

	rows = setupRows(t, h.srv)
	if c := rows["key_domain_changes"]; c.Status != "warn" ||
		c.Detail != "2 key-domain assignment changes in the last 30 days, the latest today by sec-2. Each one moves where that person's next keys are made." {
		t.Errorf("key_domain_changes after changes = %+v", c)
	}
	if c := rows["principal_keys"]; c.Detail != "On. 1 stored credentials still use this deployment's key." || c.Fix == "" {
		t.Errorf("principal_keys with a v1 row = %+v", c)
	}
}

// The credential inventory says which domain each person's next key goes to,
// and why, and marks a conflict.
func TestPG_CredentialInventory_KeyDomains(t *testing.T) {
	pool := throwawayPGPool(t)
	svc := keydomain.NewService(pool, []string{"finance", "research"})
	ctx := t.Context()
	h := newHarness(t)
	h.srv.cfg.KeyDomains = svc
	for _, a := range []keydomain.Assignment{
		{SubjectType: "user", Subject: "ann", Domain: "research", SetBy: "x"},
		{SubjectType: "group", Subject: "eng-finance", Domain: "finance", SetBy: "x"},
		{SubjectType: "group", Subject: "eng-research", Domain: "research", SetBy: "x"},
	} {
		if _, err := svc.Set(ctx, a); err != nil {
			t.Fatalf("assign %+v: %v", a, err)
		}
	}
	for p, groups := range map[string][]string{"ann": {"eng-finance"}, "bob": {"eng-finance"}, "cat": {"eng-finance", "eng-research"}} {
		if err := svc.RecordLoginGroups(ctx, p, groups, false); err != nil {
			t.Fatalf("record groups of %s: %v", p, err)
		}
	}
	inv := credentialInventory{Credentials: []credentialInventoryRow{{Person: "ann"}, {Person: "ann"}, {Person: "bob"}, {Person: "cat"}, {Person: "dan"}}}
	h.srv.annotateKeyDomains(ctx, &inv)
	want := []struct{ domain, source, group string }{
		{"research", "user", ""}, {"research", "user", ""}, {"finance", "group", "eng-finance"}, {"", "conflict", ""}, {"default", "default", ""},
	}
	for i, w := range want {
		r := inv.Credentials[i]
		if r.KeyDomain != w.domain || r.KeyDomainSource != w.source || r.KeyDomainGroup != w.group {
			t.Errorf("row %d (%s) = %q/%q/%q, want %q/%q/%q", i, r.Person, r.KeyDomain, r.KeyDomainSource, r.KeyDomainGroup, w.domain, w.source, w.group)
		}
	}
}

// GET /key-domains says where each domain's key is and whether it was proven at boot.
func TestPG_KeyDomains_ListCarriesKeyAndProof(t *testing.T) {
	_, srv, svc := keyDomainFixture(t)
	srv.cfg.KeyDomainKeys = map[string]string{"a": "Transit key finance"}
	srv.cfg.PrincipalKeys = true
	if _, err := svc.Set(t.Context(), keydomain.Assignment{SubjectType: "user", Subject: "bob", Domain: "a", SetBy: "x"}); err != nil {
		t.Fatal(err)
	}
	w := do(t, srv, http.MethodGet, "/api/v1/key-domains", adminToken, "")
	var got keyDomainsResponse
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil {
		t.Fatalf("GET = %d %s", w.Code, w.Body.String())
	}
	if !got.PrincipalKeys || len(got.Domains) != 3 {
		t.Fatalf("body = %s", w.Body.String())
	}
	if d := got.Domains[1]; d.Domain != "a" || d.Key != "Transit key finance" || !d.Proven || !d.Declared {
		t.Errorf("domain a = %+v", d)
	}
	if d := got.Domains[0]; d.Domain != keydomain.Default || d.Key != "Credential key" || !d.Proven {
		t.Errorf("default = %+v", d)
	}
}
