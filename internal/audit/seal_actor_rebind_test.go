// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// An erased person's subject that is unbound and bound to someone else keeps the rows written before
// the erasure erased; the new holder's own later rows read as them.
func TestSealFullErasedSubjectStaysErasedAfterARebind(t *testing.T) {
	s, _, dir := newFullSealer(t, "alice")
	id := dir.ids["alice"]
	before, _, err := s.Seal(t.Context(), humanEvent("alice", "run.kill"))
	if err != nil {
		t.Fatal(err)
	}
	erasedAt := time.Now().UTC().Add(time.Second)
	dir.destroyed["alice"] = erasedAt
	dir.destroyed[SubjectActorPrefix+id] = erasedAt
	delete(dir.ids, "alice")
	dir.ids["bob"] = id
	after := before
	after.Time = erasedAt.Add(time.Minute)

	back, err := s.Unseal(t.Context(), []types.AuditEvent{before, after})
	if err != nil {
		t.Fatal(err)
	}
	if back[0].Actor != ErasedValue {
		t.Errorf("a row written before the erasure reads %q after the rebind, want %q", back[0].Actor, ErasedValue)
	}
	if back[1].Actor != "bob" {
		t.Errorf("the new holder's later row reads %q, want bob", back[1].Actor)
	}
}
