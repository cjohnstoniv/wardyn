// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/keydomain"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
)

type preparedPrincipalKey struct {
	kek.KEK
	once            sync.Once
	reached, resume chan struct{}
}

func (k *preparedPrincipalKey) Unwrap(ctx context.Context, wrapped []byte, bind map[string]string) ([]byte, error) {
	key, err := k.KEK.Unwrap(ctx, wrapped, bind)
	if err == nil && bind[kek.BindPurpose] == subjectkey.PurposeCred {
		k.once.Do(func() {
			close(k.reached)
			select {
			case <-k.resume:
			case <-ctx.Done():
			}
		})
	}
	return key, err
}

func TestPG_PreparedPrincipalGenerationRevalidated(t *testing.T) {
	for _, operation := range []string{"seal", "migrate"} {
		for _, change := range []string{"supersede", "destroy", "erase", "erase-and-sign-in"} {
			t.Run(operation+"/"+change, func(t *testing.T) {
				base := rekeyDatabase(t)
				s := mixedStore(t, singleSecretPool(t, base), mustIdentity(t), false, newMemExt("vaultkv"), operation == "migrate")
				s.domains = map[string]kek.KEK{"next": s.kek}
				s.initSubjects()
				mustPut(t, s, "alice", "token", "value")
				s.principalKeys = true
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				if _, key, err := s.subjects.Current(ctx, "alice", subjectkey.PurposeCred); err != nil {
					t.Fatal(err)
				} else {
					clear(key)
				}
				writer := *s
				writer.pool = base
				writer.initSubjects()
				gate := &preparedPrincipalKey{KEK: s.kek, reached: make(chan struct{}), resume: make(chan struct{})}
				s.kek = gate
				s.initSubjects() // a cold key read exposes the post-check preparation window
				done := make(chan error, 1)
				go func() {
					var err error
					if operation == "seal" {
						_, err = s.SealToPrincipalKeys(ctx)
					} else {
						_, err = s.Migrate(ctx, MigrateLocal, func(string, string) {})
					}
					done <- err
				}()
				select {
				case <-gate.reached:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				if n := s.pool.Stat().AcquiredConns(); n != 0 {
					t.Fatalf("key preparation holds %d pooled connections", n)
				}
				switch change {
				case "supersede":
					svc := keydomain.NewService(base, []string{"next"})
					if _, err := svc.Set(ctx, keydomain.Assignment{SubjectType: "user", Subject: "alice", Domain: "next", SetBy: "admin"}); err != nil {
						t.Fatal(err)
					}
				case "destroy":
					// This v1/external source was never sealed under the destroyed
					// principal key. Owner erasure must also delete it, below.
					if _, err := writer.DestroyCredentialKey(ctx, "alice"); err != nil {
						t.Fatal(err)
					}
				default:
					if rep, err := secretstore.EraseOwner(ctx, &writer, "alice"); err != nil || rep.Count != 1 {
						t.Fatalf("owner erasure = %+v, %v", rep, err)
					}
					if change == "erase-and-sign-in" {
						writer.writeExt = false
						mustPut(t, &writer, "alice", "token", "new-sign-in")
					}
				}
				version, key, err := writer.subjects.Current(ctx, "alice", subjectkey.PurposeCred)
				clear(key)
				if err != nil || version != 2 {
					t.Fatalf("next generation = %d, %v", version, err)
				}
				close(gate.resume)
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				if change == "erase" {
					if _, err := s.For("alice").Get(ctx, "token"); !errors.Is(err, secretstore.ErrNotFound) {
						t.Fatalf("erased source restored under new key: %v", err)
					}
					return
				}
				if got := rowOf(t, base, "alice", "token"); got.version != pkVersion || got.kekID != pkKekID(2) {
					t.Fatalf("committed obsolete generation: %+v", got)
				}
				want := "value"
				if change == "erase-and-sign-in" {
					want = "new-sign-in"
				}
				mustGetOwned(t, s, "alice", "token", want)
			})
		}
	}
}
