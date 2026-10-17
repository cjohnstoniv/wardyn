// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
)

func singleSecretPool(t *testing.T, base *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	cfg := base.Config()
	cfg.MaxConns, cfg.MinConns = 1, 0
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestPG_SealAndMigrateSingleConnection(t *testing.T) {
	pool := singleSecretPool(t, rekeyDatabase(t))
	ext := newMemExt("vaultkv")
	s := mixedStore(t, pool, mustIdentity(t), false, ext, false)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := s.For("alice").Put(ctx, "token", []byte("value")); err != nil {
		t.Fatal(err)
	}
	if got, err := s.SealToPrincipalKeys(ctx); err != nil || got.Moved != 1 {
		t.Fatalf("principal seal = %+v, %v", got, err)
	}
	for _, target := range []string{"vaultkv", MigrateLocal} {
		if got, err := s.Migrate(ctx, target, func(string, string) {}); err != nil || got.Moved != 1 {
			t.Fatalf("migrate to %s = %+v, %v", target, got, err)
		}
		mustGetOwned(t, s, "alice", "token", "value")
	}
}

type pausedUnwrap struct {
	kek.KEK
	once            sync.Once
	reached, resume chan struct{}
}

func (k *pausedUnwrap) Unwrap(ctx context.Context, wrapped []byte, bind map[string]string) ([]byte, error) {
	k.once.Do(func() {
		close(k.reached)
		select {
		case <-k.resume:
		case <-ctx.Done():
		}
	})
	return k.KEK.Unwrap(ctx, wrapped, bind)
}

func TestPG_SealConcurrentRowChanges(t *testing.T) {
	for _, change := range []string{"replace", "erase", "expiry"} {
		t.Run(change, func(t *testing.T) {
			base := rekeyDatabase(t)
			s := mixedStore(t, singleSecretPool(t, base), mustIdentity(t), false, nil, false)
			writer := *s
			writer.pool = base
			mustPut(t, s, "alice", "token", "old")
			gate := &pausedUnwrap{KEK: s.kek, reached: make(chan struct{}), resume: make(chan struct{})}
			s.kek = gate
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := s.SealToPrincipalKeys(ctx); done <- err }()
			select {
			case <-gate.reached:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			switch change {
			case "replace":
				mustPut(t, &writer, "alice", "token", "new")
			case "erase":
				if err := writer.For("alice").Delete(ctx, "token"); err != nil {
					t.Fatal(err)
				}
			case "expiry":
				if _, err := base.Exec(ctx, `UPDATE secrets SET expires_at=now()-interval '1 day' WHERE owned_by='alice'`); err != nil {
					t.Fatal(err)
				}
			}
			close(gate.resume)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if change == "erase" {
				if _, err := s.For("alice").Get(ctx, "token"); !errors.Is(err, secretstore.ErrNotFound) {
					t.Fatalf("erased row restored: %v", err)
				}
			} else {
				want := "old"
				if change == "replace" {
					want = "new"
				}
				mustGetOwned(t, s, "alice", "token", want)
				if change == "expiry" && count(t, base, `SELECT count(*) FROM secrets WHERE expires_at < now()`) != 1 {
					t.Fatal("expiry was lost")
				}
			}
		})
	}
}

func TestPG_MigrateConcurrentRowAndKeyChanges(t *testing.T) {
	for _, change := range []string{"replace", "erase", "expiry", "destroy-key", "migrate"} {
		t.Run(change, func(t *testing.T) {
			base := rekeyDatabase(t)
			s := mixedStore(t, singleSecretPool(t, base), mustIdentity(t), true, newMemExt("vaultkv"), false)
			writer := *s
			writer.pool = base
			mustPut(t, s, "alice", "token", "old")
			reached, resume := make(chan struct{}), make(chan struct{})
			var once sync.Once
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := s.Migrate(ctx, "vaultkv", func(string, string) {
					once.Do(func() {
						close(reached)
						select {
						case <-resume:
						case <-ctx.Done():
						}
					})
				})
				done <- err
			}()
			select {
			case <-reached:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			switch change {
			case "replace":
				mustPut(t, &writer, "alice", "token", "new")
			case "erase":
				if err := writer.For("alice").Delete(ctx, "token"); err != nil {
					t.Fatal(err)
				}
			case "expiry":
				if _, err := base.Exec(ctx, `UPDATE secrets SET expires_at=now()-interval '1 day' WHERE owned_by='alice'`); err != nil {
					t.Fatal(err)
				}
			case "destroy-key":
				if _, err := writer.DestroyCredentialKey(ctx, "alice"); err != nil {
					t.Fatal(err)
				}
			case "migrate":
				if _, err := writer.Migrate(ctx, "vaultkv", func(string, string) {}); err != nil {
					t.Fatal(err)
				}
			}
			close(resume)
			err := <-done
			if change == "destroy-key" {
				if !errors.Is(err, subjectkey.ErrDataLoss) {
					t.Fatalf("destroyed key migration = %v", err)
				}
				if s.ext.(*memExt).deletes() != 1 {
					t.Fatal("prepared external copy was not removed")
				}
				if rowOf(t, base, "alice", "token").version != pkVersion {
					t.Fatal("destroyed credential migrated")
				}
			} else if err != nil {
				t.Fatal(err)
			} else if change == "erase" {
				if _, err := s.For("alice").Get(ctx, "token"); !errors.Is(err, secretstore.ErrNotFound) {
					t.Fatalf("erased row restored: %v", err)
				}
			} else {
				want := "old"
				if change == "replace" {
					want = "new"
				}
				mustGetOwned(t, s, "alice", "token", want)
				if change == "expiry" && count(t, base, `SELECT count(*) FROM secrets WHERE expires_at < now()`) != 1 {
					t.Fatal("expiry was lost")
				}
			}
		})
	}
}

func TestPG_ExternalRowLockReleasedOrConnectionDestroyed(t *testing.T) {
	base := rekeyDatabase(t)
	pool := singleSecretPool(t, base)
	s := mixedStore(t, pool, mustIdentity(t), false, nil, false)
	for _, kill := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		conn, release, err := s.lockExternalRow(ctx, "alice", "token")
		if err != nil {
			t.Fatal(err)
		}
		pid := conn.Conn().PgConn().PID()
		if kill {
			// The independent connection represents a failover or operator termination.
			other, err := pgx.ConnectConfig(t.Context(), base.Config().ConnConfig.Copy())
			if err != nil {
				t.Fatal(err)
			}
			_, err = other.Exec(t.Context(), `SELECT pg_terminate_backend($1)`, pid)
			_ = other.Close(t.Context())
			if err != nil {
				t.Fatal(err)
			}
		}
		cancel()
		release()
		next, unlock, err := s.lockExternalRow(t.Context(), "alice", "token")
		if err != nil {
			t.Fatal(err)
		}
		if kill && next.Conn().PgConn().PID() == pid {
			t.Fatal("terminated connection returned to pool")
		}
		unlock()
		var held int
		if err := base.QueryRow(t.Context(), `SELECT count(*) FROM pg_locks WHERE pid=$1 AND locktype='advisory'`, pid).Scan(&held); err != nil || held != 0 {
			t.Fatalf("left %d locks, %v", held, err)
		}
	}
}
