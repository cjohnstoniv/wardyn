// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package subjectkey

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
)

// downManager is a Manager whose Postgres refuses every connection (pgxpool
// dials lazily, so it builds) and whose KEK resolver must never be reached.
func downManager(t *testing.T) *Manager {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://wardyn@127.0.0.1:1/wardyn?connect_timeout=1&sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	refuse := func(string) (kek.KEK, error) { t.Error("the KEK was resolved"); return nil, errors.New("unexpected") }
	return New(pool, Resolver{
		Writer: refuse,
		Reader: func(string, string) (kek.KEK, error) { return refuse("") },
	})
}

func TestOwnerAndPurposeAreRefusedBeforePostgresIsAsked(t *testing.T) {
	m := downManager(t)
	ctx := context.Background()

	if _, _, err := m.Current(ctx, "", PurposeCred); !errors.Is(err, ErrOperatorOwner) {
		t.Errorf("Current with no owner = %v, want ErrOperatorOwner", err)
	}
	if _, err := m.Key(ctx, "", PurposeCred, 1); !errors.Is(err, ErrOperatorOwner) {
		t.Errorf("Key with no owner = %v, want ErrOperatorOwner", err)
	}
	if _, err := m.Destroy(ctx, "", PurposeAuditSeal); !errors.Is(err, ErrOperatorOwner) {
		t.Errorf("Destroy with no owner = %v, want ErrOperatorOwner", err)
	}
	for name, call := range map[string]func() error{
		"Current": func() error { _, _, err := m.Current(ctx, "alice", "tls"); return err },
		"Key":     func() error { _, err := m.Key(ctx, "alice", "tls", 1); return err },
		"Destroy": func() error { _, err := m.Destroy(ctx, "alice", "tls"); return err },
	} {
		err := call()
		if err == nil || errors.Is(err, secretstore.ErrUnavailable) {
			t.Errorf("%s with an unknown purpose = %v, want a definitive refusal, not an outage", name, err)
		}
	}
}

func TestPostgresDownFailsEveryUseAsUnavailable(t *testing.T) {
	m := downManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, key, err := m.Current(ctx, "alice", PurposeCred); !errors.Is(err, secretstore.ErrUnavailable) || key != nil {
		t.Errorf("Current = %v, key %v; want ErrUnavailable and no key", err, key)
	}
	if key, err := m.Key(ctx, "alice", PurposeCred, 1); !errors.Is(err, secretstore.ErrUnavailable) || key != nil {
		t.Errorf("Key (cold) = %v, key %v; want ErrUnavailable and no key", err, key)
	}
	if gens, err := m.Destroy(ctx, "alice", PurposeCred); !errors.Is(err, secretstore.ErrUnavailable) || gens != nil {
		t.Errorf("Destroy = %v, gens %v; want ErrUnavailable and none", err, gens)
	}
}

// SECURITY: a warm cache saves the KEK round trip, never the revocation check,
// so a Postgres that does not answer must not be served from the cache.
func TestAWarmCacheIsNotServedWhilePostgresCannotAnswer(t *testing.T) {
	m := downManager(t)
	id := keyID{"alice", PurposeCred, 1}
	m.cache.put(id, key(7))

	got, err := m.Key(context.Background(), "alice", PurposeCred, 1)
	if !errors.Is(err, secretstore.ErrUnavailable) || got != nil {
		t.Fatalf("Key (warm) = %v, %v; want ErrUnavailable and no key", got, err)
	}
}

func TestCacheEvictDropsOneGenerationOnly(t *testing.T) {
	c := newCache(cacheMax, cacheTTL)
	a, b := keyID{"alice", PurposeCred, 1}, keyID{"alice", PurposeCred, 2}
	c.put(a, key(1))
	c.put(b, key(2))

	held := c.m[a].Value.(*cacheEntry).key
	c.evict(a)
	c.evict(keyID{"nobody", PurposeCred, 9}) // absent: a no-op

	if _, ok := c.get(a); ok {
		t.Error("the evicted generation is still served")
	}
	if _, ok := c.get(b); !ok {
		t.Error("evicting one generation dropped its sibling")
	}
	for _, x := range held {
		if x != 0 {
			t.Fatal("the evicted key's bytes were not cleared")
		}
	}
}
