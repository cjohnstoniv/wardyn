// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package secretmask_test

import (
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/maskstore"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey/subjectkeytest"
)

type delayedGlobalReturn struct {
	secretmask.Backend
	committed, release chan struct{}
}

func (b *delayedGlobalReturn) PutGlobal(gen int64, owner, name string, values []secretmask.GlobalPut, merge bool, now time.Time) error {
	err := b.Backend.PutGlobal(gen, owner, name, values, merge, now)
	close(b.committed)
	<-b.release
	return err
}

func TestPG_MaskErasureConcurrentBackendReturnCannotRestoreCache(t *testing.T) {
	pool := subjectkeytest.ThrowawayDB(t)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	k, err := kek.NewLocalPurpose(id, kek.PurposeCred)
	if err != nil {
		t.Fatal(err)
	}
	reg := secretmask.NewRegistry()
	st := maskstore.New(pool, subjectkeytest.Manager(pool, k), reg)
	delayed := &delayedGlobalReturn{Backend: st, committed: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-delayed.release:
		default:
			close(delayed.release)
		}
	})
	reg.SetBackend(delayed)
	gen, err := reg.GlobalGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- reg.MergeGlobal(gen, "alice", "sso", []byte("erased-before-backend-return")) }()
	select {
	case <-delayed.committed:
	case <-time.After(10 * time.Second):
		t.Fatal("global write did not complete")
	}
	if len(reg.Snapshot(uuid.Nil)) != 1 {
		t.Fatal("successful production backend did not cache the committed row")
	}
	if left, err := reg.EraseOwner(t.Context(), "alice"); err != nil || left != 0 {
		t.Fatalf("erase=%d, %v", left, err)
	}
	close(delayed.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("delayed writer did not return")
	}
	if len(reg.Snapshot(uuid.Nil)) != 0 {
		t.Fatal("Registry re-applied erased value after backend returned")
	}
	if err := st.Fresh(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(reg.Snapshot(uuid.Nil)) != 0 {
		t.Fatal("late note caused erased value to survive another read")
	}
}
