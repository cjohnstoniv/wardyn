// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

// Credential rows under principal keys (enc_version 3), against a real Postgres
// and an isolated database: one MIXED database holding v1 rows, a pointer to
// each external store, and v3 rows, held to every path the design names, with
// WARDYN_PRINCIPAL_KEYS both on and off.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
)

// memExt is a minimal secretstore.External, named as the store it stands in for.
type memExt struct {
	name string
	mu   sync.Mutex
	vals map[string][]byte
	dels int
}

func newMemExt(name string) *memExt { return &memExt{name: name, vals: map[string][]byte{}} }

func (m *memExt) key(owner, name string) string { return owner + "/" + name }
func (m *memExt) Name() string                  { return m.name }
func (m *memExt) Describe() string              { return m.name + " at test" }
func (m *memExt) Put(_ context.Context, owner, name, _ string, v []byte, _ bool) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.vals[m.key(owner, name)] = v
	return m.key(owner, name), nil
}
func (m *memExt) Get(_ context.Context, owner, name, ref string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.vals[ref]
	if ref != m.key(owner, name) || !ok {
		return nil, fmt.Errorf("refused: nothing at %s", ref)
	}
	return v, nil
}
func (m *memExt) Ref(owner, name, _ string) (string, error) { return m.key(owner, name), nil }
func (m *memExt) Check(ctx context.Context, owner, name, ref string) error {
	_, err := m.Get(ctx, owner, name, ref)
	return err
}
func (m *memExt) Delete(_ context.Context, owner, name, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dels++
	delete(m.vals, m.key(owner, name))
	return nil
}
func (m *memExt) Walk(context.Context) ([]secretstore.ExternalEntry, error) { return nil, nil }
func (m *memExt) deletes() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dels
}

// mixedStore is a store over pool under id: principal keys on or off, writing to
// ext when writeExt (store mode), else sealing locally and reading ext's pointers.
func mixedStore(t *testing.T, pool *pgxpool.Pool, id age.Identity, principal bool, ext secretstore.External, writeExt bool) *Store {
	t.Helper()
	s := &Store{pool: pool, ext: ext, writeExt: writeExt, extTimeout: 5 * time.Second}
	if err := s.setLocalKeys(id, nil); err != nil {
		t.Fatal(err)
	}
	s.principalKeys = principal
	s.initSubjects()
	return s
}

type rowInfo struct {
	version int16
	kekID   string
}

func rowOf(t *testing.T, pool *pgxpool.Pool, owner, name string) rowInfo {
	t.Helper()
	var r rowInfo
	if err := pool.QueryRow(t.Context(), `SELECT enc_version, kek_id FROM secrets WHERE owned_by=$1 AND name=$2`, owner, name).Scan(&r.version, &r.kekID); err != nil {
		t.Fatalf("row (%q, %q): %v", owner, name, err)
	}
	return r
}

func mustGetOwned(t *testing.T, s secretstore.Store, owner, name, want string) {
	t.Helper()
	got, err := s.For(owner).Get(secretstore.WithPurpose(t.Context(), secretstore.PurposeStatus), name)
	if err != nil || string(got) != want {
		t.Fatalf("Get (%q, %q) = (%q, %v); want %q", owner, name, got, err, want)
	}
}

func mustPut(t *testing.T, s secretstore.Store, owner, name, value string) {
	t.Helper()
	if err := s.For(owner).Put(t.Context(), name, []byte(value)); err != nil {
		t.Fatalf("Put (%q, %q): %v", owner, name, err)
	}
}

func count(t *testing.T, pool *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// seedMixed fills a database with every row format: v1 rows (a boot key, an
// operator credential, a person's), a vaultkv pointer and an azurekv pointer,
// and v3 rows. The seeding is the same whatever the flag under test says.
func seedMixed(t *testing.T, pool *pgxpool.Pool, id age.Identity, vault, azure *memExt) {
	t.Helper()
	off := mixedStore(t, pool, id, false, nil, false)
	on := mixedStore(t, pool, id, true, nil, false)
	mustPut(t, off, "", "wardyn-signing-key", "boot-key-value")
	mustPut(t, off, "", "operator-token", "operator-value")
	mustPut(t, off, "bob", "v1-cred", "bob-v1")
	mustPut(t, mixedStore(t, pool, id, false, vault, true), "bob", "vault-cred", "bob-vault")
	mustPut(t, mixedStore(t, pool, id, false, azure, true), "zoe", "azure-cred", "zoe-azure")
	mustPut(t, on, "alice", "pk-a", "alice-v3")
	mustPut(t, on, "bob", "pk-b", "bob-v3")
	if got := rowOf(t, pool, "alice", "pk-a"); got.version != 3 || got.kekID != "pk:v1" {
		t.Fatalf("a person's row written with principal keys on = %+v; want enc_version 3, kek_id pk:v1", got)
	}
}

// The mixed-database test: every path in the design's table, with the flag on
// and with it off.
func TestMixedDatabase_EveryPathBehavesAsListed(t *testing.T) {
	for _, principal := range []bool{true, false} {
		t.Run(fmt.Sprintf("flag_%v", principal), func(t *testing.T) {
			pool := rekeyDatabase(t)
			id := mustIdentity(t)
			vault, azure := newMemExt("vaultkv"), newMemExt("azurekv")
			seedMixed(t, pool, id, vault, azure)
			s := mixedStore(t, pool, id, principal, vault, false)
			sAz := mixedStore(t, pool, id, principal, azure, false)

			// open: every format reads, whatever the flag says.
			mustGetOwned(t, s, "", "wardyn-signing-key", "boot-key-value")
			mustGetOwned(t, s, "", "operator-token", "operator-value")
			mustGetOwned(t, s, "bob", "v1-cred", "bob-v1")
			mustGetOwned(t, s, "bob", "vault-cred", "bob-vault")
			mustGetOwned(t, sAz, "zoe", "azure-cred", "zoe-azure")
			mustGetOwned(t, s, "alice", "pk-a", "alice-v3")
			mustGetOwned(t, s, "bob", "pk-b", "bob-v3")

			// Put: v3 for a person's row only with the flag on; every owner="" row
			// (boot key, operator namespace) stays v1; store mode writes pointers.
			mustPut(t, s, "dave", "fresh", "dave-value")
			wantVersion := int16(1)
			if principal {
				wantVersion = 3
			}
			if got := rowOf(t, pool, "dave", "fresh"); got.version != wantVersion {
				t.Fatalf("a person's Put with the flag %v wrote enc_version %d; want %d", principal, got.version, wantVersion)
			}
			mustPut(t, s, "", "operator-new", "x")
			mustPut(t, s, "", "wardyn-session-key", "y")
			for _, name := range []string{"operator-new", "wardyn-session-key"} {
				if got := rowOf(t, pool, "", name); got.version != 1 || strings.HasPrefix(got.kekID, "pk:") {
					t.Fatalf("an owner=\"\" row %q = %+v; want v1 under a KEK", name, got)
				}
			}
			storeMode := mixedStore(t, pool, id, principal, vault, true)
			mustPut(t, storeMode, "erin", "ptr", "erin-value")
			if got := rowOf(t, pool, "erin", "ptr"); got.version != 2 || !strings.HasPrefix(got.kekID, "vaultkv:") {
				t.Fatalf("store mode with the flag %v wrote %+v; want a v2 pointer (the flag is inert)", principal, got)
			}

			// A replaced value re-keys the row into the format the flag now says.
			mustPut(t, s, "alice", "pk-a", "alice-v3-replaced")
			wantReplaced := int16(1)
			if principal {
				wantReplaced = 3
			}
			if got := rowOf(t, pool, "alice", "pk-a"); got.version != wantReplaced {
				t.Fatalf("a replaced v3 row with the flag %v is enc_version %d; want %d", principal, got.version, wantReplaced)
			}
			mustGetOwned(t, s, "alice", "pk-a", "alice-v3-replaced")

			// metadata: pk is the principal-key format, not a store.
			metas, err := s.MetadataEverywhere(t.Context(), []string{"pk-b", "v1-cred", "vault-cred"})
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]secretstore.Meta{}
			for _, m := range metas {
				got[m.Name] = m
			}
			if m := got["pk-b"]; m.Store != "pg" || !m.PrincipalKey {
				t.Fatalf("metadata of a v3 row = %+v; want Store pg, PrincipalKey true", m)
			}
			if m := got["v1-cred"]; m.Store != "pg" || m.PrincipalKey {
				t.Fatalf("metadata of a v1 row = %+v", m)
			}
			if m := got["vault-cred"]; m.Store != "vaultkv" || m.PrincipalKey {
				t.Fatalf("metadata of a pointer row = %+v", m)
			}

			// LocalRows counts the principal keys a local KEK wraps.
			if n, err := s.LocalRows(t.Context()); err != nil || n < 3 {
				t.Fatalf("LocalRows = (%d, %v); want the v1 rows plus the principal keys", n, err)
			}

			// Reconcile: a v3 row is no pointer.
			rep, err := s.Reconcile(t.Context())
			// bob/vault-cred and erin/ptr are checked; the azurekv pointer is one this
			// store cannot reach, which is dangling here and nothing else.
			if err != nil || rep.Checked != 2 || len(rep.Dangling) != 1 || !strings.Contains(rep.Dangling[0], "azurekv") {
				t.Fatalf("Reconcile = (%+v, %v); want the 2 vaultkv pointers checked and only the azurekv one dangling", rep, err)
			}

			// Delete and DeleteEverywhere: a v3 row is a plain row delete, no external call.
			before := vault.deletes()
			if err := s.For("bob").Delete(t.Context(), "pk-b"); err != nil {
				t.Fatal(err)
			}
			if vault.deletes() != before {
				t.Fatal("deleting a v3 row called the external store")
			}
			if n := count(t, pool, `SELECT count(*) FROM secrets WHERE owned_by='bob' AND name='pk-b'`); n != 0 {
				t.Fatal("the v3 row survived Delete")
			}
			mustPut(t, mixedStore(t, pool, id, true, nil, false), "bob", "pk-b", "bob-v3")
			mustPut(t, mixedStore(t, pool, id, true, nil, false), "frank", "pk-b", "frank-v3")
			n, err := s.DeleteEverywhere(t.Context(), []string{"pk-b"})
			if err != nil || n != 2 {
				t.Fatalf("DeleteEverywhere over two v3 rows = (%d, %v); want 2", n, err)
			}
			if vault.deletes() != before {
				t.Fatal("DeleteEverywhere of v3 rows called the external store")
			}

			// DeleteExpired: a v3 row is swept by row and reported as deleted.
			expiring := secretstore.WithExpiry(t.Context(), time.Now().Add(-time.Minute))
			if err := mixedStore(t, pool, id, true, nil, false).For("gina").Put(expiring, "signin", []byte("expired")); err != nil {
				t.Fatal(err)
			}
			if r := rowOf(t, pool, "gina", "signin"); r.version != 3 {
				t.Fatalf("setup: %+v", r)
			}
			gone, err := s.DeleteExpired(t.Context())
			if err != nil || len(gone) != 1 || gone[0].Owner != "gina" {
				t.Fatalf("DeleteExpired = (%+v, %v); want the one v3 row", gone, err)
			}

			// Migrate: a v3 row moves to the external store as a v2 pointer and
			// opens through its principal key on the way; back to local it lands
			// in the format the flag says.
			var read []string
			// The azurekv pointer sorts last and is not this store's to read, so the
			// run moves every row before it and aborts naming it.
			migrate := func(target string, onRead func(owner, name string)) MigrateResult {
				t.Helper()
				res, err := s.Migrate(t.Context(), target, onRead)
				if err != nil && !strings.Contains(err.Error(), `name="azure-cred"`) {
					t.Fatalf("Migrate to %s: %v", target, err)
				}
				return res
			}
			res := migrate("vaultkv", func(owner, name string) { read = append(read, owner+"/"+name) })
			if got := rowOf(t, pool, "alice", "pk-a"); got.version != 2 || !strings.HasPrefix(got.kekID, "vaultkv:") {
				t.Fatalf("a v3 row migrated to the store is %+v; want a v2 pointer", got)
			}
			mustGetOwned(t, s, "alice", "pk-a", "alice-v3-replaced")
			if res.Moved == 0 || len(read) != res.Moved {
				t.Fatalf("Migrate moved %d rows and reported %d reads", res.Moved, len(read))
			}
			// azure's pointer is not this store's to move: only the rows it can reach.
			if got := rowOf(t, pool, "zoe", "azure-cred"); got.version != 2 || !strings.HasPrefix(got.kekID, "azurekv:") {
				t.Fatalf("an azurekv pointer = %+v after a migration to vaultkv", got)
			}
			migrate(MigrateLocal, func(string, string) {})
			if got := rowOf(t, pool, "alice", "pk-a"); got.version != wantReplaced {
				t.Fatalf("a row migrated to local with the flag %v is enc_version %d; want %d", principal, got.version, wantReplaced)
			}
			mustGetOwned(t, s, "alice", "pk-a", "alice-v3-replaced")

			// No v3 row is ever owned by the operator namespace.
			if n := count(t, pool, `SELECT count(*) FROM secrets WHERE enc_version=3 AND owned_by=''`); n != 0 {
				t.Fatalf("%d v3 rows are owned by the operator namespace", n)
			}
		})
	}
}

// Rekey and RewrapKeys leave v3 rows alone and move the principal keys, so a
// root rotation never strands a credential row under a principal key.
func TestMixedDatabase_RootRotationKeepsV3Readable(t *testing.T) {
	pool := rekeyDatabase(t)
	oldID, newID := mustIdentity(t), mustIdentity(t)
	vault, azure := newMemExt("vaultkv"), newMemExt("azurekv")
	seedMixed(t, pool, oldID, vault, azure)
	if n, err := Rekey(t.Context(), pool, oldID, newID, nil); err != nil {
		t.Fatalf("Rekey with v3 rows present = (%d, %v)", n, err)
	}
	// A restarted wardynd on the new key (no warm cache) reads every format: v1,
	// both pointer kinds (each through its own store) and v3.
	cold := mixedStore(t, pool, newID, false, vault, false)
	mustGetOwned(t, cold, "alice", "pk-a", "alice-v3")
	mustGetOwned(t, cold, "bob", "pk-b", "bob-v3")
	mustGetOwned(t, cold, "bob", "v1-cred", "bob-v1")
	mustGetOwned(t, cold, "bob", "vault-cred", "bob-vault")
	mustGetOwned(t, cold, "", "wardyn-signing-key", "boot-key-value")
	mustGetOwned(t, mixedStore(t, pool, newID, false, azure, false), "zoe", "azure-cred", "zoe-azure")
	if _, _, err := mixedStore(t, pool, oldID, false, vault, false).SubjectKeys().Current(t.Context(), "alice", subjectkey.PurposeCred); err == nil {
		t.Fatal("the OLD age key still opens alice's principal key after the rotation")
	}
}

// A v3 row is bound to its owner, its name and its key generation: moved or
// swapped, it is refused, never read as another's.
func TestPrincipalRow_BindingHolds(t *testing.T) {
	pool := rekeyDatabase(t)
	id := mustIdentity(t)
	s := mixedStore(t, pool, id, true, nil, false)
	mustPut(t, s, "alice", "a", "alice-secret")
	mustPut(t, s, "bob", "b", "bob-secret")
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(t.Context(), q, args...); err != nil {
			t.Fatal(err)
		}
	}
	read := func(owner, name string) error {
		_, err := mixedStore(t, pool, id, true, nil, false).For(owner).Get(secretstore.WithPurpose(t.Context(), secretstore.PurposeStatus), name)
		return err
	}
	expectRefused := func(label string, err error) {
		t.Helper()
		if err == nil || errors.Is(err, secretstore.ErrNotFound) || errors.Is(err, secretstore.ErrUnavailable) || strings.Contains(err.Error(), "-secret") {
			t.Fatalf("%s: err = %v; want a definitive refusal that carries no value", label, err)
		}
	}
	// alice's wrap moved onto bob's row.
	exec(`UPDATE secrets SET wrapped_dek=(SELECT wrapped_dek FROM secrets WHERE owned_by='alice' AND name='a') WHERE owned_by='bob' AND name='b'`)
	expectRefused("a wrap moved to another owner", read("bob", "b"))
	// alice's row renamed.
	exec(`UPDATE secrets SET name='a2' WHERE owned_by='alice' AND name='a'`)
	expectRefused("a row renamed", read("alice", "a2"))
	// a v3 row with no owner is refused outright.
	exec(`INSERT INTO secrets (owned_by, name, enc_version, kek_id, wrapped_dek, ciphertext) VALUES ('', 'op', 3, 'pk:v1', '\x01', '\x02')`)
	expectRefused("a v3 operator row", read("", "op"))
	// a kek_id that names no generation.
	exec(`UPDATE secrets SET kek_id='pk:vx' WHERE owned_by='alice' AND name='a2'`)
	expectRefused("a malformed pk kek_id", read("alice", "a2"))
}

// -rewrap-principal-keys: v1 person rows move into v3; boot keys, the operator
// namespace and pointers do not; a partial failure leaves earlier rows moved and
// the run resumes; a concurrent writer is not lost.
func TestSealToPrincipalKeys_MovesOnlyPersonRowsAndResumes(t *testing.T) {
	pool := rekeyDatabase(t)
	id := mustIdentity(t)
	vault, azure := newMemExt("vaultkv"), newMemExt("azurekv")
	seedMixed(t, pool, id, vault, azure)
	s := mixedStore(t, pool, id, false, nil, false)

	// One person row that will not open sorts after the rest: the run moves
	// the rows before it, aborts naming it, and commits what it moved.
	mustPut(t, s, "zed", "broken", "zed-value")
	if _, err := pool.Exec(t.Context(), `UPDATE secrets SET wrapped_dek='\x0102' WHERE owned_by='zed' AND name='broken'`); err != nil {
		t.Fatal(err)
	}
	res, err := s.SealToPrincipalKeys(t.Context())
	if err == nil || !strings.Contains(err.Error(), `name="broken"`) {
		t.Fatalf("SealToPrincipalKeys over a broken row = (%+v, %v); want an abort naming it", res, err)
	}
	if got := rowOf(t, pool, "bob", "v1-cred"); got.version != 3 {
		t.Fatalf("a row before the failure is %+v; want it moved and committed", got)
	}
	// Operator rows, boot keys and pointers are untouched.
	for _, c := range []struct {
		owner, name string
		version     int16
	}{{"", "wardyn-signing-key", 1}, {"", "operator-token", 1}, {"bob", "vault-cred", 2}, {"zoe", "azure-cred", 2}} {
		if got := rowOf(t, pool, c.owner, c.name); got.version != c.version {
			t.Fatalf("(%q, %q) is enc_version %d after the move; want it left at %d", c.owner, c.name, got.version, c.version)
		}
	}
	// Fix the row and re-run: it resumes, and a third run moves nothing.
	if _, err := pool.Exec(t.Context(), `DELETE FROM secrets WHERE owned_by='zed'`); err != nil {
		t.Fatal(err)
	}
	if res, err = s.SealToPrincipalKeys(t.Context()); err != nil || res.Remaining != 0 {
		t.Fatalf("resumed SealToPrincipalKeys = (%+v, %v); want none remaining", res, err)
	}
	if res, err = s.SealToPrincipalKeys(t.Context()); err != nil || res.Moved != 0 {
		t.Fatalf("a third SealToPrincipalKeys = (%+v, %v); want a no-op", res, err)
	}
	read := mixedStore(t, pool, id, false, vault, false)
	mustGetOwned(t, read, "bob", "v1-cred", "bob-v1")
	mustGetOwned(t, read, "bob", "vault-cred", "bob-vault")
	mustGetOwned(t, read, "alice", "pk-a", "alice-v3")
	if n := count(t, pool, `SELECT count(*) FROM secrets WHERE enc_version=1 AND owned_by<>''`); n != 0 {
		t.Fatalf("%d person v1 rows remain", n)
	}
}

// A writer racing the move loses nothing: every row reads back as its last
// write, and once the writers stop, a final run leaves no person v1 row.
func TestSealToPrincipalKeys_ConcurrentWriter(t *testing.T) {
	pool := rekeyDatabase(t)
	id := mustIdentity(t)
	off := mixedStore(t, pool, id, false, nil, false)
	const rows = 40
	for i := range rows {
		mustPut(t, off, fmt.Sprintf("p%02d", i%8), fmt.Sprintf("n%02d", i), "first")
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	last := make([]string, rows)
	for i := range last {
		last[i] = "first"
	}
	wg.Go(func() {
		for round := 0; ; round++ {
			select {
			case <-stop:
				return
			default:
			}
			for i := range rows {
				v := fmt.Sprintf("round-%d", round)
				if err := off.For(fmt.Sprintf("p%02d", i%8)).Put(t.Context(), fmt.Sprintf("n%02d", i), []byte(v)); err != nil {
					t.Errorf("concurrent Put: %v", err)
					return
				}
				last[i] = v
			}
		}
	})
	for range 3 {
		if _, err := off.SealToPrincipalKeys(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	if _, err := off.SealToPrincipalKeys(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := count(t, pool, `SELECT count(*) FROM secrets WHERE enc_version=1 AND owned_by<>''`); n != 0 {
		t.Fatalf("%d person v1 rows remain after the writers stopped", n)
	}
	read := mixedStore(t, pool, id, false, nil, false)
	for i := range rows {
		mustGetOwned(t, read, fmt.Sprintf("p%02d", i%8), fmt.Sprintf("n%02d", i), last[i])
	}
}

// Erasing a person's credentials destroys their cred key after the rows: v3
// rows are reported crypto-erased, v1 and external rows only deleted. A v3 row
// restored from a backup then refuses, and a reconnect makes a new generation.
func TestEraseOwner_ReportsCryptoErasedForV3AndDeletedForTheRest(t *testing.T) {
	pool := rekeyDatabase(t)
	id := mustIdentity(t)
	vault := newMemExt("vaultkv")
	off := mixedStore(t, pool, id, false, nil, false)
	on := mixedStore(t, pool, id, true, nil, false)
	mustPut(t, off, "bob", "v1-cred", "bob-v1")
	mustPut(t, mixedStore(t, pool, id, false, vault, true), "bob", "vault-cred", "bob-vault")
	mustPut(t, on, "bob", "pk-1", "bob-v3-1")
	mustPut(t, on, "bob", "pk-2", "bob-v3-2")
	mustPut(t, on, "alice", "pk-a", "alice-v3")

	// A copy of one row as a backup would hold it.
	var wrapped, ct []byte
	if err := pool.QueryRow(t.Context(), `SELECT wrapped_dek, ciphertext FROM secrets WHERE owned_by='bob' AND name='pk-1'`).Scan(&wrapped, &ct); err != nil {
		t.Fatal(err)
	}

	rep, err := secretstore.EraseOwner(t.Context(), mixedStore(t, pool, id, true, vault, false), "bob")
	if err != nil {
		t.Fatalf("EraseOwner: %v", err)
	}
	if rep.Count != 4 || rep.CryptoErased != 2 {
		t.Fatalf("EraseOwner = %+v; want 4 credentials, 2 of them crypto-erased (the v1 and the pointer row were only deleted)", rep)
	}
	// alice is untouched, and so is her key.
	mustGetOwned(t, on, "alice", "pk-a", "alice-v3")

	if n := count(t, pool, `SELECT count(*) FROM principal_keys WHERE owner='bob' AND destroyed_at IS NULL`); n != 0 {
		t.Fatalf("%d live principal keys of bob remain after the erase", n)
	}
	// Restored from a backup, the row's key is gone: a definitive data-loss refusal.
	if _, err := pool.Exec(t.Context(), `INSERT INTO secrets (owned_by, name, enc_version, kek_id, wrapped_dek, ciphertext) VALUES ('bob', 'pk-1', 3, 'pk:v1', $1, $2)`, wrapped, ct); err != nil {
		t.Fatal(err)
	}
	_, err = mixedStore(t, pool, id, true, nil, false).For("bob").Get(secretstore.WithPurpose(t.Context(), secretstore.PurposeStatus), "pk-1")
	if !errors.Is(err, subjectkey.ErrDataLoss) || errors.Is(err, secretstore.ErrNotFound) || errors.Is(err, secretstore.ErrUnavailable) {
		t.Fatalf("a restored row under a destroyed key = %v; want ErrDataLoss", err)
	}
	// Reconnecting makes a fresh generation; the restored row stays unreadable.
	if _, err := pool.Exec(t.Context(), `DELETE FROM secrets WHERE owned_by='bob'`); err != nil {
		t.Fatal(err)
	}
	mustPut(t, mixedStore(t, pool, id, true, nil, false), "bob", "pk-new", "bob-again")
	if got := rowOf(t, pool, "bob", "pk-new"); got.version != 3 || got.kekID != "pk:v2" {
		t.Fatalf("a reconnect after an erase wrote %+v; want a v3 row under generation pk:v2", got)
	}
	mustGetOwned(t, mixedStore(t, pool, id, true, nil, false), "bob", "pk-new", "bob-again")

	// Erasing a person who never had a key reports nothing crypto-erased.
	mustPut(t, off, "carol", "v1-only", "carol-v1")
	if rep, err := secretstore.EraseOwner(t.Context(), mixedStore(t, pool, id, false, nil, false), "carol"); err != nil || rep.Count != 1 || rep.CryptoErased != 0 {
		t.Fatalf("EraseOwner of a v1-only person = (%+v, %v); want 1 deleted, 0 crypto-erased", rep, err)
	}
}

// ConvertV0 is unchanged: a v0 row converts to v1 under the platform or
// credential KEK even with the flag on, and a v3 row beside it is left alone.
func TestConvertV0_StaysV1WithTheFlagOn(t *testing.T) {
	pool := rekeyDatabase(t)
	id := mustIdentity(t)
	on := mixedStore(t, pool, id, true, nil, false)
	mustPut(t, on, "alice", "pk-a", "alice-v3")
	before := rowOf(t, pool, "alice", "pk-a")
	if rows, err := on.ConvertV0(t.Context(), id); err != nil || len(rows) != 0 {
		t.Fatalf("ConvertV0 over a v3 row = (%v, %v); want nothing to convert", rows, err)
	}
	if after := rowOf(t, pool, "alice", "pk-a"); after != before {
		t.Fatalf("ConvertV0 changed a v3 row: %+v -> %+v", before, after)
	}
}

func TestPkVersionOf(t *testing.T) {
	for kek, want := range map[string]int{"pk:v1": 1, "pk:v12": 12} {
		if got, ok := pkVersionOf(kek); !ok || got != want {
			t.Errorf("pkVersionOf(%q) = (%d, %v)", kek, got, ok)
		}
	}
	for _, bad := range []string{"", "pk:v", "pk:v0", "pk:v-1", "pk:vx", "local/cred:abcd", "vaultkv:pk:v1"} {
		if _, ok := pkVersionOf(bad); ok {
			t.Errorf("pkVersionOf(%q) accepted", bad)
		}
	}
}
