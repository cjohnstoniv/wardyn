// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	secretstorepg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
)

func TestParsePrincipalKeys(t *testing.T) {
	for in, want := range map[string]bool{"": false, "off": false, "OFF": false, " on ": true, "on": true, "On": true} {
		if got, err := parsePrincipalKeys(in); err != nil || got != want {
			t.Errorf("parsePrincipalKeys(%q) = (%v, %v); want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"true", "1", "yes", "enabled"} {
		if _, err := parsePrincipalKeys(bad); err == nil || !strings.Contains(err.Error(), "WARDYN_PRINCIPAL_KEYS") {
			t.Errorf("parsePrincipalKeys(%q) = %v; want a refusal naming the setting", bad, err)
		}
	}
}

// With the setting on, a boot under a key service mints and reads every boot
// key as v1 under that service, and a person's credential is v3. `-rewrap-principal-keys`
// then moves a v1 person row, writes one secret.rewrap row that names no
// secret, and leaves the boot keys and the operator namespace alone.
func TestSealToPrincipalKeysMode_BootKeysStayV1AndRowsMove(t *testing.T) {
	pool := envelopeDB(t)
	ctx := t.Context()
	k := newMemKEK()
	on := storeClients{kek: k, kekWrites: true, principalKeys: true}
	off := storeClients{kek: k, kekWrites: true}

	offStore, err := buildSecretStore(ctx, pool, "", nil, "", off, &capturingRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	if err := offStore.For("bob").Put(ctx, "v1-cred", []byte("bob-v1")); err != nil {
		t.Fatal(err)
	}
	if err := offStore.Put(ctx, "operator-token", []byte("operator-value")); err != nil {
		t.Fatal(err)
	}
	onStore, err := buildSecretStore(ctx, pool, "", nil, "", on, &capturingRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	if err := onStore.For("alice").Put(ctx, "v3-cred", []byte("alice-v3")); err != nil {
		t.Fatal(err)
	}
	orig, err := loadOrCreateSigningKey(ctx, unlocked(onStore))
	if err != nil {
		t.Fatalf("boot with the setting on: %v", err)
	}
	if got, err := loadOrCreateSigningKey(ctx, unlocked(onStore)); err != nil || !got.Equal(orig) {
		t.Fatalf("a second boot read a different signing key (%v)", err)
	}
	versions := func() map[string]int {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT owned_by || '/' || name, enc_version FROM secrets`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]int{}
		for rows.Next() {
			var n string
			var v int
			if err := rows.Scan(&n, &v); err != nil {
				t.Fatal(err)
			}
			out[n] = v
		}
		return out
	}
	got := versions()
	if got["/"+secretSigningKey] != 1 || got["alice/v3-cred"] != 3 || got["bob/v1-cred"] != 1 {
		t.Fatalf("row versions after a boot with the setting on = %v; want the boot key v1, alice v3, bob v1", got)
	}

	bare, err := newSecretStore(ctx, pool, "", nil, "", on, &capturingRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	rec := &recAudit{}
	if err := sealToPrincipalKeys(ctx, bare.(*secretstorepg.Store), rec); err != nil {
		t.Fatalf("-rewrap-principal-keys: %v", err)
	}
	got = versions()
	if got["bob/v1-cred"] != 3 || got["/"+secretSigningKey] != 1 || got["/operator-token"] != 1 {
		t.Fatalf("row versions after -rewrap-principal-keys = %v; want bob moved and the operator rows left at v1", got)
	}
	if v, err := onStore.For("bob").Get(secretstore.WithPurpose(ctx, secretstore.PurposeStatus), "v1-cred"); err != nil || string(v) != "bob-v1" {
		t.Fatalf("bob's credential after the move = (%q, %v)", v, err)
	}
	if len(rec.evs) != 1 || rec.evs[0].Action != "secret.rewrap" || rec.evs[0].Actor != rewrapPrincipalKeysActor || rec.evs[0].Outcome != "success" {
		t.Fatalf("audit = %+v; want one successful secret.rewrap by %s", rec.evs, rewrapPrincipalKeysActor)
	}
	var data map[string]any
	_ = json.Unmarshal(rec.evs[0].Data, &data)
	if data["mode"] != "principal_keys" || data["secrets"] != float64(1) || data["remaining"] != float64(0) ||
		strings.Contains(string(rec.evs[0].Data), "v1-cred") || strings.Contains(string(rec.evs[0].Data), "bob") {
		t.Fatalf("secret.rewrap data = %s", rec.evs[0].Data)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM secrets WHERE enc_version=3 AND owned_by=''`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("v3 rows owned by the operator namespace = (%d, %v); want none", n, err)
	}
}
