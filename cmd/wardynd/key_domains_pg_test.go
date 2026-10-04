// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/keydomain"
	secretstorepg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
)

// domainTransit is one fake Transit key: versioned, rotatable, and bound to its
// row. Each domain has its own id and its own key material.
type domainTransit struct {
	id   string
	keys map[int][]byte
	cur  int
}

func newDomainTransit(name string) *domainTransit {
	d := &domainTransit{id: kek.TransitIDPrefix + "transit/" + name, keys: map[int][]byte{}}
	d.rotate()
	return d
}

func (d *domainTransit) rotate() {
	d.cur++
	k := make([]byte, kek.DEKSize)
	_, _ = rand.Read(k)
	d.keys[d.cur] = k
}

func (d *domainTransit) ID() string { return d.id }

func (d *domainTransit) Wrap(_ context.Context, dek []byte, bind map[string]string) ([]byte, error) {
	aad, err := kek.WrapAAD(bind, d.id)
	if err != nil {
		return nil, err
	}
	w, err := kek.Seal(d.keys[d.cur], dek, aad)
	return append([]byte(strconv.Itoa(d.cur)+":"), w...), err
}

func (d *domainTransit) Unwrap(_ context.Context, wrapped []byte, bind map[string]string) ([]byte, error) {
	v, w, _ := bytes.Cut(wrapped, []byte(":"))
	n, _ := strconv.Atoi(string(v))
	key, ok := d.keys[n]
	if !ok {
		return nil, errors.New("version retired")
	}
	aad, err := kek.WrapAAD(bind, d.id)
	if err != nil {
		return nil, err
	}
	return kek.Open(key, w, aad)
}

func (d *domainTransit) WrapVersion(wrapped []byte) (string, error) {
	v, _, _ := bytes.Cut(wrapped, []byte(":"))
	return string(v), nil
}

func (d *domainTransit) LatestVersion(context.Context) (string, error) {
	return strconv.Itoa(d.cur), nil
}

// domainRig is two declared domains beside a credential key, over one database.
type domainRig struct {
	pool      *pgxpool.Pool
	cred      *memKEK
	a, b      *domainTransit
	svc       *keydomain.Service
	rec       *capturingRecorder
	clientsOn storeClients
}

func newDomainRig(t *testing.T) *domainRig {
	t.Helper()
	r := &domainRig{pool: envelopeDB(t), cred: newMemKEK(), a: newDomainTransit("domain-a"), b: newDomainTransit("domain-b"), rec: &capturingRecorder{}}
	r.svc = keydomain.NewService(r.pool, []string{"a", "b"})
	r.clientsOn = storeClients{kek: r.cred, kekWrites: true, principalKeys: true, keyDomains: secretstore.KeyDomains{"a": r.a, "b": r.b}}
	return r
}

func (r *domainRig) assign(t *testing.T, owner, domain string) {
	t.Helper()
	if _, err := r.svc.Set(t.Context(), keydomain.Assignment{SubjectType: "user", Subject: owner, Domain: domain, SetBy: "admin"}); err != nil {
		t.Fatal(err)
	}
}

func (r *domainRig) boot(t *testing.T, c storeClients) (secretstore.Store, error) {
	t.Helper()
	s, err := newSecretStore(t.Context(), r.pool, "", nil, "", c, r.rec)
	if err != nil {
		return nil, err
	}
	if err := verifyKeyDomains(t.Context(), s); err != nil {
		return nil, err
	}
	return secretstore.Audited(s, r.rec), nil
}

type pkRow struct {
	domain, kekID string
	wrapped       []byte
}

func (r *domainRig) keyRows(t *testing.T) map[string]pkRow {
	t.Helper()
	rows, err := r.pool.Query(t.Context(), `SELECT owner || '/' || version, domain, kek_id, wrapped_key FROM principal_keys ORDER BY owner, version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]pkRow{}
	for rows.Next() {
		var n string
		var p pkRow
		if err := rows.Scan(&n, &p.domain, &p.kekID, &p.wrapped); err != nil {
			t.Fatal(err)
		}
		out[n] = p
	}
	return out
}

func (r *domainRig) read(t *testing.T, s secretstore.Store, owner, name string) string {
	t.Helper()
	v, err := s.For(owner).Get(secretstore.WithPurpose(t.Context(), secretstore.PurposeStatus), name)
	if err != nil {
		t.Fatalf("read %s/%s: %v", owner, name, err)
	}
	return string(v)
}

// Two domains: each person's key is wrapped under their domain's key and no
// other; the boot checks pass.
func TestKeyDomains_BootWrapsEachPersonUnderTheirDomain(t *testing.T) {
	r := newDomainRig(t)
	r.assign(t, "alice", "a")
	r.assign(t, "bob", "b")
	s, err := r.boot(t, r.clientsOn)
	if err != nil {
		t.Fatal(err)
	}
	for owner, v := range map[string]string{"alice": "alice-secret", "bob": "bob-secret", "carol": "carol-secret"} {
		if err := s.For(owner).Put(t.Context(), "cred", []byte(v)); err != nil {
			t.Fatal(err)
		}
	}
	got := r.keyRows(t)
	want := map[string]pkRow{"alice/1": {"a", r.a.id, nil}, "bob/1": {"b", r.b.id, nil}, "carol/1": {"default", r.cred.ID(), nil}}
	for k, w := range want {
		if g := got[k]; g.domain != w.domain || g.kekID != w.kekID {
			t.Errorf("%s = (%s, %s), want (%s, %s)", k, g.domain, g.kekID, w.domain, w.kekID)
		}
	}
	// A restart reads every row.
	again, err := r.boot(t, r.clientsOn)
	if err != nil {
		t.Fatal(err)
	}
	for owner, v := range map[string]string{"alice": "alice-secret", "bob": "bob-secret", "carol": "carol-secret"} {
		if got := r.read(t, again, owner, "cred"); got != v {
			t.Errorf("%s after a restart = %q, want %q", owner, got, v)
		}
	}
}

// A live row naming an undeclared domain, and one naming a key its domain no
// longer reaches, each refuse boot; the first counts the rows and names the
// remedy, which never deletes rows.
func TestKeyDomains_BootRefusals(t *testing.T) {
	r := newDomainRig(t)
	r.assign(t, "alice", "a")
	r.assign(t, "alice2", "a")
	r.assign(t, "bob", "b")
	s, err := r.boot(t, r.clientsOn)
	if err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"alice", "alice2", "bob"} {
		if err := s.For(owner).Put(t.Context(), "cred", []byte("v")); err != nil {
			t.Fatal(err)
		}
	}

	onlyB := r.clientsOn
	onlyB.keyDomains = secretstore.KeyDomains{"b": r.b}
	_, err = r.boot(t, onlyB)
	if err == nil || !strings.Contains(err.Error(), "refusing to start") ||
		!strings.Contains(err.Error(), `2 live principal keys name key domain "a", which WARDYN_KEY_DOMAINS_FILE does not declare`) ||
		!strings.Contains(err.Error(), "erase or destroy") || !strings.Contains(err.Error(), "DELETE /api/v1/people/{principal}/credentials") ||
		!strings.Contains(err.Error(), "Never delete the rows") {
		t.Fatalf("boot with domain a removed = %v; want the live-row count and the remedy", err)
	}
	none := r.clientsOn
	none.keyDomains = nil
	if _, err = r.boot(t, none); err == nil || !strings.Contains(err.Error(), `key domain "a"`) || !strings.Contains(err.Error(), `key domain "b"`) {
		t.Fatalf("boot with no domains declared = %v; want both named", err)
	}

	moved := r.clientsOn
	moved.keyDomains = secretstore.KeyDomains{"a": newDomainTransit("domain-a-elsewhere"), "b": r.b}
	if _, err = r.boot(t, moved); err == nil || !strings.Contains(err.Error(), `2 live principal keys in domain "a"`) ||
		!strings.Contains(err.Error(), "no longer reaches") {
		t.Fatalf("boot with domain a pointing at another key = %v; want the unreachable key refused", err)
	}

	// Erasing the subjects through the store clears the refusal; the remedy works.
	cs, ok := s.(interface {
		For(string) secretstore.Store
	})
	if !ok {
		t.Fatal("store has no For")
	}
	_ = cs
	ps := bareStore(t, r, r.clientsOn)
	for _, owner := range []string{"alice", "alice2"} {
		if _, err := ps.DestroyCredentialKey(t.Context(), owner); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.boot(t, onlyB); err != nil {
		t.Fatalf("boot with domain a removed after its subjects were destroyed: %v", err)
	}
}

func bareStore(t *testing.T, r *domainRig, c storeClients) *secretstorepg.Store {
	t.Helper()
	s, err := newSecretStore(t.Context(), r.pool, "", nil, "", c, r.rec)
	if err != nil {
		t.Fatal(err)
	}
	return s.(*secretstorepg.Store)
}

// Rotating A's key and running -rewrap moves only A's principal keys onto A's
// new version; B's rows keep their domain, kek_id and bytes; a retirable version
// is reported per domain; a restart reads every row.
func TestRewrapKeys_RotatesOneDomainAndLeavesTheOther(t *testing.T) {
	r := newDomainRig(t)
	r.assign(t, "alice", "a")
	r.assign(t, "bob", "b")
	s, err := r.boot(t, r.clientsOn)
	if err != nil {
		t.Fatal(err)
	}
	for owner, v := range map[string]string{"alice": "alice-secret", "bob": "bob-secret", "carol": "carol-secret"} {
		if err := s.For(owner).Put(t.Context(), "cred", []byte(v)); err != nil {
			t.Fatal(err)
		}
	}
	before := r.keyRows(t)

	r.a.rotate() // domain A's key is now at version 2; B's stays at 1
	d := secretstore.Deps{Pool: r.pool, KEK: r.cred, KEKWrites: true, KeyDomains: r.clientsOn.keyDomains}
	rec := &capturingRecorder{}
	out := captureStdout(t, func() {
		if err := rewrapKeys(t.Context(), rec, d); err != nil {
			t.Fatalf("-rewrap: %v", err)
		}
	})
	after := r.keyRows(t)

	if after["bob/1"].domain != before["bob/1"].domain || after["bob/1"].kekID != before["bob/1"].kekID || !bytes.Equal(after["bob/1"].wrapped, before["bob/1"].wrapped) {
		t.Fatalf("domain B's row changed: before %+v, after %+v", before["bob/1"], after["bob/1"])
	}
	if !bytes.Equal(after["carol/1"].wrapped, before["carol/1"].wrapped) {
		t.Fatal("the default domain's row changed though its key did not rotate")
	}
	if a, b := after["alice/1"], before["alice/1"]; a.domain != "a" || a.kekID != b.kekID || bytes.Equal(a.wrapped, b.wrapped) {
		t.Fatalf("domain A's row = %+v (was %+v); want the same domain and kek_id under a new wrap", a, b)
	}
	if v, _ := r.a.WrapVersion(after["alice/1"].wrapped); v != "2" {
		t.Fatalf("alice's key is wrapped under version %q, want 2", v)
	}
	if v, _ := r.b.WrapVersion(after["bob/1"].wrapped); v != "1" {
		t.Fatalf("bob's key is wrapped under version %q, want 1", v)
	}

	// A retirable version per domain, in the output and the audit row.
	for _, want := range []string{`key domain "a"`, `key domain "b"`, r.a.id + " version 2", r.b.id + " version 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("-rewrap printed %q; want it to contain %q", out, want)
		}
	}
	var data map[string]any
	if err := json.Unmarshal(rec.got[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	if dv, _ := data["domain_key_versions"].(map[string]any); dv["a"] != "2" || dv["b"] != "1" || data["principal_keys"] != float64(1) {
		t.Fatalf("audit fields = %s; want domain_key_versions a=2 b=1 and one principal key moved", rec.got[0].Data)
	}

	// The retired version is gone from A: a restart still reads every row.
	delete(r.a.keys, 1)
	again, err := r.boot(t, r.clientsOn)
	if err != nil {
		t.Fatal(err)
	}
	for owner, v := range map[string]string{"alice": "alice-secret", "bob": "bob-secret", "carol": "carol-secret"} {
		if got := r.read(t, again, owner, "cred"); got != v {
			t.Errorf("%s after the rewrap and a restart = %q, want %q", owner, got, v)
		}
	}
}

// A rewrap refuses a live row naming an undeclared domain, with nothing changed.
func TestRewrapKeys_RefusesAnUndeclaredDomain(t *testing.T) {
	r := newDomainRig(t)
	r.assign(t, "alice", "a")
	s, err := r.boot(t, r.clientsOn)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.For("alice").Put(t.Context(), "cred", []byte("v")); err != nil {
		t.Fatal(err)
	}
	before := r.keyRows(t)
	r.a.rotate()
	d := secretstore.Deps{Pool: r.pool, KEK: r.cred, KEKWrites: true} // no domain declared
	err = rewrapKeys(t.Context(), &capturingRecorder{}, d)
	if err == nil || !strings.Contains(err.Error(), "REFUSED") || !strings.Contains(err.Error(), `key domain "a"`) {
		t.Fatalf("-rewrap with domain a undeclared = %v; want a refusal naming it", err)
	}
	if after := r.keyRows(t); !bytes.Equal(after["alice/1"].wrapped, before["alice/1"].wrapped) {
		t.Fatal("a refused -rewrap changed a row")
	}
}

// A reassignment applies to the next generation: the credential written before
// stays under the old generation, a write after it lands in the new domain, and
// -rewrap-principal-keys re-seals the old row into the new generation without
// moving either key.
func TestKeyDomains_ReassignmentAndResealing(t *testing.T) {
	r := newDomainRig(t)
	r.assign(t, "alice", "a")
	s, err := r.boot(t, r.clientsOn)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.For("alice").Put(t.Context(), "old", []byte("written-in-a")); err != nil {
		t.Fatal(err)
	}
	r.assign(t, "alice", "b")
	if err := s.For("alice").Put(t.Context(), "new", []byte("written-in-b")); err != nil {
		t.Fatal(err)
	}
	keys := r.keyRows(t)
	if keys["alice/1"].domain != "a" || keys["alice/2"].domain != "b" {
		t.Fatalf("generations = %+v; want 1 in a and 2 in b", keys)
	}
	rowKEK := func(name string) string {
		var id string
		if err := r.pool.QueryRow(t.Context(), `SELECT kek_id FROM secrets WHERE owned_by='alice' AND name=$1`, name).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	if rowKEK("old") != "pk:v1" || rowKEK("new") != "pk:v2" {
		t.Fatalf("rows under %s and %s; want the old row under generation 1 and the new under 2", rowKEK("old"), rowKEK("new"))
	}
	if got := r.read(t, s, "alice", "old"); got != "written-in-a" {
		t.Fatalf("the old row after the reassignment = %q", got)
	}

	rec := &recAudit{}
	if err := sealToPrincipalKeys(t.Context(), bareStore(t, r, r.clientsOn), rec); err != nil {
		t.Fatalf("-rewrap-principal-keys: %v", err)
	}
	if rowKEK("old") != "pk:v2" || rowKEK("new") != "pk:v2" {
		t.Fatalf("after -rewrap-principal-keys the rows are under %s and %s; want both under generation 2", rowKEK("old"), rowKEK("new"))
	}
	after := r.keyRows(t)
	if after["alice/1"].domain != "a" || !bytes.Equal(after["alice/1"].wrapped, keys["alice/1"].wrapped) ||
		after["alice/2"].domain != "b" || !bytes.Equal(after["alice/2"].wrapped, keys["alice/2"].wrapped) {
		t.Fatalf("a principal key moved: before %+v, after %+v", keys, after)
	}
	if got := r.read(t, s, "alice", "old"); got != "written-in-a" {
		t.Fatalf("the old row after re-sealing = %q", got)
	}
	var data map[string]any
	_ = json.Unmarshal(rec.evs[0].Data, &data)
	if data["secrets"] != float64(1) || data["remaining"] != float64(0) {
		t.Fatalf("secret.rewrap data = %s; want 1 moved, 0 remaining", rec.evs[0].Data)
	}
	// A second run moves nothing.
	rec2 := &recAudit{}
	if err := sealToPrincipalKeys(t.Context(), bareStore(t, r, r.clientsOn), rec2); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(rec2.evs[0].Data, &data)
	if data["secrets"] != float64(0) {
		t.Fatalf("a second -rewrap-principal-keys moved %v rows", data["secrets"])
	}
}

// An ambiguous membership refuses the write by name and stores nothing.
func TestKeyDomains_AmbiguousMembershipRefusesTheWrite(t *testing.T) {
	r := newDomainRig(t)
	if err := r.svc.RecordLoginGroups(t.Context(), "gina", []string{"eng", "ops"}, false); err != nil {
		t.Fatal(err)
	}
	for g, d := range map[string]string{"eng": "a", "ops": "b"} {
		if _, err := r.svc.Set(t.Context(), keydomain.Assignment{SubjectType: "group", Subject: g, Domain: d, SetBy: "admin"}); err != nil {
			t.Fatal(err)
		}
	}
	s, err := r.boot(t, r.clientsOn)
	if err != nil {
		t.Fatal(err)
	}
	err = s.For("gina").Put(t.Context(), "cred", []byte("v"))
	if !errors.Is(err, keydomain.ErrAmbiguous) || !strings.Contains(err.Error(), "eng") || !strings.Contains(err.Error(), "ops") {
		t.Fatalf("Put for an ambiguous member = %v; want ErrAmbiguous naming both groups", err)
	}
	if n := len(r.keyRows(t)); n != 0 {
		t.Fatalf("a refused write left %d principal keys", n)
	}
}
