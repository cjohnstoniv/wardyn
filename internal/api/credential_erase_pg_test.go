// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"filippo.io/age"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// The erase route against the real pg store: a person's rows under their
// principal key (written with WARDYN_PRINCIPAL_KEYS on) are reported
// crypto-erased, and a row written while it was off is reported only deleted.
// The credential.erase row carries both counts, never a name or a key. Guarded
// by WARDYN_TEST_PG (throwawayPGPool); skipped cleanly when unset.
func TestPG_ErasePersonCredentials_ReportsCryptoErasedAndDeleted(t *testing.T) {
	pool := throwawayPGPool(t)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	open := func(principal bool) secretstore.Store {
		s, err := secretstore.New("pg", secretstore.Deps{Pool: pool, AgeIdentity: id, PrincipalKeys: principal})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	on, off := open(true), open(false)
	// eraseFixture seeds through on: bob's two rows are v3. One of them is then
	// rewritten with principal keys off, which leaves it v1.
	h, srv := eraseFixture(t, secretstore.Audited(on, &memAudit{}))
	if err := off.For("bob").Put(context.Background(), "wardyn-harness-aws-oauth", []byte("rewritten-while-off")); err != nil {
		t.Fatal(err)
	}
	var v3, v1 int
	for _, q := range []struct {
		dst *int
		sql string
	}{
		{&v3, `SELECT count(*) FROM secrets WHERE owned_by='bob' AND enc_version=3`},
		{&v1, `SELECT count(*) FROM secrets WHERE owned_by='bob' AND enc_version=1`},
	} {
		if err := pool.QueryRow(context.Background(), q.sql).Scan(q.dst); err != nil {
			t.Fatal(err)
		}
	}
	if v3 != 1 || v1 != 1 {
		t.Fatalf("setup: bob holds %d v3 and %d v1 rows; want one of each", v3, v1)
	}

	admin := ssoSession(t, "admin-1", "admin@corp.example", oidc.RoleAdmin)
	w := doSSO(t, srv, http.MethodDelete, "/api/v1/people/bob/credentials", admin, "")
	if w.Code != http.StatusOK {
		t.Fatalf("erase = %d %s, want 200", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["count"] != float64(2) || body["crypto_erased"] != float64(1) || body["deleted"] != float64(1) {
		t.Fatalf("erase body = %s; want 2 erased, 1 crypto-erased and 1 deleted", w.Body.String())
	}
	ev := lastAuditEvent(t, h.audit.events, "credential.erase")
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatal(err)
	}
	if ev.Outcome != "success" || data["crypto_erased"] != float64(1) || data["deleted"] != float64(1) {
		t.Fatalf("credential.erase row = %s %s; want success with crypto_erased 1 and deleted 1", ev.Outcome, ev.Data)
	}
	var live int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM principal_keys WHERE owner='bob' AND destroyed_at IS NULL`).Scan(&live); err != nil || live != 0 {
		t.Fatalf("live principal keys of bob after the erase = (%d, %v); want none", live, err)
	}
	// alice is untouched: her row still reads under her key.
	if got, err := on.For("alice").Get(secretstore.WithPurpose(context.Background(), secretstore.PurposeStatus), "anthropic-api-key"); err != nil || string(got) != "seeded-credential-value-0000" {
		t.Fatalf("alice's credential after bob's erase = (%q, %v)", got, err)
	}
	// Reconnecting writes a fresh generation.
	if err := on.For("bob").Put(context.Background(), "anthropic-api-key", []byte("again")); err != nil {
		t.Fatal(err)
	}
	var kekID string
	if err := pool.QueryRow(context.Background(), `SELECT kek_id FROM secrets WHERE owned_by='bob' AND name='anthropic-api-key'`).Scan(&kekID); err != nil || kekID != "pk:v2" {
		t.Fatalf("a reconnect after an erase = (%q, %v); want generation pk:v2", kekID, err)
	}
}
