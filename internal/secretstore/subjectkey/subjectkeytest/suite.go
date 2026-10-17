// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package subjectkeytest is the contract suite every KEK behind
// subjectkey.Manager is held to, against a real Postgres: the local key, and
// Vault Transit and Azure Key Vault against their fakes.
package subjectkeytest

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek/kektest"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
)

// ThrowawayDB creates a migrated database on the WARDYN_TEST_PG server and
// returns a pool for it, dropped on cleanup. It skips when WARDYN_TEST_PG is
// unset; the lane's check fails on that message, so a skip cannot pass for a run.
func ThrowawayDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping Postgres-backed subject-key test")
	}
	ctx := context.Background()
	admin, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	name := "wardyn_sk_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		admin.Close()
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1 AND pid <> pg_backend_pid()`, name)
		_, _ = admin.Exec(context.Background(), `DROP DATABASE IF EXISTS `+name)
		admin.Close()
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	pool, err := db.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

// Manager is a subjectkey.Manager over pool whose every key is wrapped, and
// read, under k: a store instance with its own cache.
func Manager(pool *pgxpool.Pool, k kek.KEK) *subjectkey.Manager {
	return subjectkey.New(pool, subjectkey.Resolver{
		Writer: func(string) (kek.KEK, error) { return k, nil },
		Reader: func(_, kekID string) (kek.KEK, error) {
			if kekID != k.ID() {
				return nil, fmt.Errorf("test KEK %s does not open %s", k.ID(), kekID)
			}
			return k, nil
		},
	})
}

// Run holds subjectkey to its contract over the KEK newKEK returns. newKEK is
// called more than once, and each call must return a KEK over the same key: a
// second instance is a second wardynd.
func Run(t *testing.T, pool *pgxpool.Pool, newKEK func(t *testing.T) kek.KEK) {
	t.Run("round_trip", func(t *testing.T) { roundTrip(t, pool, newKEK) })
	t.Run("cross_person_and_purpose", func(t *testing.T) { crossBinding(t, pool, newKEK) })
	t.Run("destroy_reaches_a_warm_second_instance", func(t *testing.T) { warmSecondInstance(t, pool, newKEK) })
	t.Run("postgres_down_fails_the_use", func(t *testing.T) { postgresDown(t, pool, newKEK) })
	t.Run("no_version_reused", func(t *testing.T) { noReuse(t, pool, newKEK) })
	t.Run("operator_owner_refused", func(t *testing.T) { operatorRefused(t, pool, newKEK) })
	t.Run("concurrent_first_writes", func(t *testing.T) { concurrentFirstWrites(t, pool, newKEK) })
	t.Run("destroy_event_holds_no_key_material", func(t *testing.T) { destroyEvent(t, pool, newKEK) })
}

// who is a fresh owner, so subtests sharing a database never meet.
func who() string { return "person-" + uuid.NewString() + "@example.com" }

func current(t *testing.T, m *subjectkey.Manager, owner, purpose string) (int, []byte) {
	t.Helper()
	v, key, err := m.Current(t.Context(), owner, purpose)
	if err != nil {
		t.Fatalf("Current(%s, %s): %v", owner, purpose, err)
	}
	if len(key) != kek.DEKSize {
		t.Fatalf("Current key is %d bytes, want %d", len(key), kek.DEKSize)
	}
	return v, key
}

func roundTrip(t *testing.T, pool *pgxpool.Pool, newKEK func(*testing.T) kek.KEK) {
	m, other := Manager(pool, newKEK(t)), Manager(pool, newKEK(t))
	owner := who()
	v, key := current(t, m, owner, subjectkey.PurposeCred)
	if v != 1 {
		t.Fatalf("first generation is %d, want 1", v)
	}
	// A value sealed under the key opens on a second instance with a cold cache.
	aad := kek.Encode("test", owner)
	sealed, err := kek.Seal(key, []byte("the value"), aad)
	if err != nil {
		t.Fatal(err)
	}
	got, err := other.Key(t.Context(), owner, subjectkey.PurposeCred, v)
	if err != nil {
		t.Fatalf("Key on a second instance: %v", err)
	}
	if plain := kektest.Open(t, got, sealed, aad); string(plain) != "the value" {
		t.Fatalf("Open under the second instance's key = %q", plain)
	}
	if v2, key2 := current(t, other, owner, subjectkey.PurposeCred); v2 != v || !bytes.Equal(key2, key) {
		t.Fatalf("Current again = (%d, same key %v); want generation %d and the same key", v2, bytes.Equal(key2, key), v)
	}
	// Each purpose has a key of its own.
	av, akey := current(t, m, owner, subjectkey.PurposeAuditSeal)
	if av != 1 || bytes.Equal(akey, key) {
		t.Fatalf("audit-seal generation %d, same key as cred %v; want generation 1 and an unrelated key", av, bytes.Equal(akey, key))
	}
}

func crossBinding(t *testing.T, pool *pgxpool.Pool, newKEK func(*testing.T) kek.KEK) {
	k := newKEK(t)
	m := Manager(pool, k)
	alice, bob := who(), who()
	current(t, m, alice, subjectkey.PurposeCred)
	var kekID string
	var wrapped []byte
	if err := pool.QueryRow(t.Context(),
		`SELECT kek_id, wrapped_key FROM principal_keys WHERE owner=$1 AND purpose='cred' AND version=1`, alice).Scan(&kekID, &wrapped); err != nil {
		t.Fatal(err)
	}
	// The KEK itself refuses a principal-key wrap under another subject's, another
	// purpose's, another version's or another domain's binding, and under a
	// credential row's binding: the labels differ.
	for name, bind := range map[string]map[string]string{
		"another person":   kek.PrincipalBind(bob, "cred", 1, "default"),
		"another purpose":  kek.PrincipalBind(alice, "audit-seal", 1, "default"),
		"another version":  kek.PrincipalBind(alice, "cred", 2, "default"),
		"another domain":   kek.PrincipalBind(alice, "cred", 1, "other"),
		"a credential row": kek.Bind(alice, "cred"),
	} {
		kektest.UnwrapRefused(t, k, wrapped, bind, "Unwrap under "+name)
	}
	// And a wrap copied onto another subject's or another purpose's row in SQL
	// does not open, whatever the row says.
	for _, to := range []struct{ owner, purpose string }{{bob, "cred"}, {alice, "audit-seal"}} {
		if _, err := pool.Exec(t.Context(),
			`INSERT INTO principal_keys (owner, purpose, version, domain, kek_id, wrapped_key) VALUES ($1, $2, 1, 'default', $3, $4)
			 ON CONFLICT DO NOTHING`, to.owner, to.purpose, kekID, wrapped); err != nil {
			t.Fatal(err)
		}
		key, err := Manager(pool, newKEK(t)).Key(t.Context(), to.owner, to.purpose, 1)
		if err == nil || errors.Is(err, subjectkey.ErrDataLoss) || errors.Is(err, secretstore.ErrUnavailable) || key != nil {
			t.Fatalf("Key of a wrap copied to (%s, %s) = (%x, %v); want a definitive refusal", to.owner, to.purpose, key, err)
		}
	}
}

func warmSecondInstance(t *testing.T, pool *pgxpool.Pool, newKEK func(*testing.T) kek.KEK) {
	a, b := Manager(pool, newKEK(t)), Manager(pool, newKEK(t))
	owner := who()
	v, _ := current(t, a, owner, subjectkey.PurposeCred)
	if _, err := b.Key(t.Context(), owner, subjectkey.PurposeCred, v); err != nil {
		t.Fatalf("warm the second instance: %v", err)
	}
	gens, err := a.Destroy(t.Context(), owner, subjectkey.PurposeCred)
	if err != nil || !slices.Equal(gens, []int{1}) {
		t.Fatalf("Destroy = (%v, %v); want [1]", gens, err)
	}
	// No notification reached b: its next use reads destroyed_at.
	key, err := b.Key(t.Context(), owner, subjectkey.PurposeCred, v)
	if !errors.Is(err, subjectkey.ErrDataLoss) || key != nil {
		t.Fatalf("Key on the warm second instance after Destroy = (%x, %v); want data loss and no key", key, err)
	}
	if _, err := a.Key(t.Context(), owner, subjectkey.PurposeCred, v); !errors.Is(err, subjectkey.ErrDataLoss) {
		t.Fatalf("Key on the destroying instance = %v; want data loss", err)
	}
	var live, wrappedRows int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FILTER (WHERE destroyed_at IS NULL), count(*) FILTER (WHERE wrapped_key IS NOT NULL) FROM principal_keys WHERE owner=$1`, owner).Scan(&live, &wrappedRows); err != nil {
		t.Fatal(err)
	}
	if live != 0 || wrappedRows != 0 {
		t.Fatalf("after Destroy: %d live rows and %d rows still holding a wrapped key; want none", live, wrappedRows)
	}
	if gens, err := a.Destroy(t.Context(), owner, subjectkey.PurposeCred); err != nil || len(gens) != 0 {
		t.Fatalf("a second Destroy = (%v, %v); want nothing to destroy", gens, err)
	}
}

func postgresDown(t *testing.T, pool *pgxpool.Pool, newKEK func(*testing.T) kek.KEK) {
	own, err := pgxpool.New(t.Context(), pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	m := Manager(own, newKEK(t))
	owner := who()
	v, _ := current(t, m, owner, subjectkey.PurposeCred)
	if _, err := m.Key(t.Context(), owner, subjectkey.PurposeCred, v); err != nil {
		t.Fatal(err) // warm
	}
	own.Close()
	key, err := m.Key(t.Context(), owner, subjectkey.PurposeCred, v)
	if !errors.Is(err, secretstore.ErrUnavailable) || key != nil {
		t.Fatalf("Key with Postgres down and a warm cache = (%x, %v); want ErrUnavailable and no key", key, err)
	}
	if _, _, err := m.Current(t.Context(), owner, subjectkey.PurposeCred); !errors.Is(err, secretstore.ErrUnavailable) {
		t.Fatalf("Current with Postgres down = %v; want ErrUnavailable", err)
	}
}

func noReuse(t *testing.T, pool *pgxpool.Pool, newKEK func(*testing.T) kek.KEK) {
	m := Manager(pool, newKEK(t))
	owner := who()
	var keys [][]byte
	for want := 1; want <= 3; want++ {
		v, key := current(t, m, owner, subjectkey.PurposeCred)
		if v != want {
			t.Fatalf("generation %d after %d Destroys, want %d", v, want-1, want)
		}
		for _, old := range keys {
			if bytes.Equal(old, key) {
				t.Fatal("a new generation drew a key an earlier generation held")
			}
		}
		keys = append(keys, key)
		if _, err := m.Destroy(t.Context(), owner, subjectkey.PurposeCred); err != nil {
			t.Fatal(err)
		}
	}
	for v := 1; v <= 3; v++ {
		if _, err := m.Key(t.Context(), owner, subjectkey.PurposeCred, v); !errors.Is(err, subjectkey.ErrDataLoss) {
			t.Fatalf("Key of destroyed generation %d = %v; want data loss", v, err)
		}
	}
	var rows int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM principal_keys WHERE owner=$1`, owner).Scan(&rows); err != nil || rows != 3 {
		t.Fatalf("rows = (%d, %v); want the three tombstones kept, none deleted", rows, err)
	}
}

func operatorRefused(t *testing.T, pool *pgxpool.Pool, newKEK func(*testing.T) kek.KEK) {
	m := Manager(pool, newKEK(t))
	if _, _, err := m.Current(t.Context(), "", subjectkey.PurposeCred); !errors.Is(err, subjectkey.ErrOperatorOwner) {
		t.Fatalf("Current(owner=\"\") = %v; want ErrOperatorOwner", err)
	}
	if _, err := m.Key(t.Context(), "", subjectkey.PurposeCred, 1); !errors.Is(err, subjectkey.ErrOperatorOwner) {
		t.Fatalf("Key(owner=\"\") = %v; want ErrOperatorOwner", err)
	}
	if _, err := m.Destroy(t.Context(), "", subjectkey.PurposeCred); !errors.Is(err, subjectkey.ErrOperatorOwner) {
		t.Fatalf("Destroy(owner=\"\") = %v; want ErrOperatorOwner", err)
	}
	if _, _, err := m.Current(t.Context(), who(), "other"); err == nil {
		t.Fatal("Current under an unknown purpose succeeded")
	}
	// The table says the same, for a writer that is not this package.
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO principal_keys (owner, purpose, version, domain, kek_id, wrapped_key) VALUES ('', 'cred', 1, 'default', 'x', '\x00')`); err == nil {
		t.Fatal("the table accepted owner=\"\"")
	}
}

// concurrentFirstWrites: several instances create a subject's first key at
// once. One generation is live, every writer seals under it, and every sealed
// value opens under that single generation.
func concurrentFirstWrites(t *testing.T, pool *pgxpool.Pool, newKEK func(*testing.T) kek.KEK) {
	const writers, subjects = 4, 6
	ms := make([]*subjectkey.Manager, writers)
	for i := range ms {
		ms[i] = Manager(pool, newKEK(t))
	}
	for range subjects {
		owner := who()
		type sealed struct {
			version int
			ct      []byte
			err     error
		}
		out := make([]sealed, writers)
		aad := kek.Encode("test", owner)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range ms {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				v, key, err := ms[i].Current(context.Background(), owner, subjectkey.PurposeCred)
				if err != nil {
					out[i].err = err
					return
				}
				defer clear(key)
				out[i].version = v
				out[i].ct, out[i].err = kek.Seal(key, fmt.Appendf(nil, "writer %d", i), aad)
			}()
		}
		close(start)
		wg.Wait()
		var live int
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM principal_keys WHERE owner=$1 AND destroyed_at IS NULL`, owner).Scan(&live); err != nil || live != 1 {
			t.Fatalf("live generations = (%d, %v); want exactly one", live, err)
		}
		reader := Manager(pool, newKEK(t))
		for i, s := range out {
			if s.err != nil {
				t.Fatalf("writer %d: %v", i, s.err)
			}
			key, err := reader.Key(t.Context(), owner, subjectkey.PurposeCred, s.version)
			if err != nil {
				t.Fatalf("writer %d sealed under generation %d, which does not open: %v", i, s.version, err)
			}
			if plain := kektest.Open(t, key, s.ct, aad); string(plain) != fmt.Sprintf("writer %d", i) {
				t.Fatalf("writer %d's value under generation %d = %q", i, s.version, plain)
			}
			if s.version != out[0].version {
				t.Fatalf("writers sealed under generations %d and %d; want one", out[0].version, s.version)
			}
		}
	}
}

func destroyEvent(t *testing.T, pool *pgxpool.Pool, newKEK func(*testing.T) kek.KEK) {
	m := Manager(pool, newKEK(t))
	owner := who()
	current(t, m, owner, subjectkey.PurposeAuditSeal)
	var kekID string
	var wrapped []byte
	if err := pool.QueryRow(t.Context(),
		`SELECT kek_id, wrapped_key FROM principal_keys WHERE owner=$1 AND purpose='audit-seal'`, owner).Scan(&kekID, &wrapped); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Destroy(t.Context(), owner, subjectkey.PurposeAuditSeal); err != nil {
		t.Fatal(err)
	}
	var raw string
	var n int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*), max(data::text) FROM audit_events WHERE action='principal_key.destroyed' AND data->>'owner'=$1`, owner).Scan(&n, &raw); err != nil || n != 1 {
		t.Fatalf("principal_key.destroyed rows = (%d, %v); want one", n, err)
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"generations", "owner", "purpose"}) || data["owner"] != owner || data["purpose"] != "audit-seal" ||
		fmt.Sprint(data["generations"]) != "[1]" {
		t.Fatalf("principal_key.destroyed data = %s; want exactly owner, purpose and generations [1]", raw)
	}
	for _, secret := range []string{kekID, string(wrapped), hex.EncodeToString(wrapped), base64.StdEncoding.EncodeToString(wrapped), base64.RawURLEncoding.EncodeToString(wrapped)} {
		if strings.Contains(raw, secret) {
			t.Fatalf("principal_key.destroyed data %s holds a wrapped key or kek_id", raw)
		}
	}
}
