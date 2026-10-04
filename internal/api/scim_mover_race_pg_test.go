// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/test/entrafake"
)

// afterCandidates runs hook once, after the sweeper has selected its pending group removals.
type afterCandidates struct {
	store.PG
	hook func()
}

func (s *afterCandidates) PendingGroupRemovals(ctx context.Context, idle time.Duration, limit int) ([]store.PendingGroupRemoval, error) {
	rows, err := s.PG.PendingGroupRemovals(ctx, idle, limit)
	if hook := s.hook; err == nil && len(rows) > 0 && hook != nil {
		s.hook = nil
		hook()
	}
	return rows, err
}

// The identity provider adds a mover back after the sweeper selected their failed removal and before it
// resumed it. The sweeper drops the candidate: the membership stays, and a token minted after the re-add
// keeps authenticating.
func TestSCIMGroupRemovalSweeperDropsACandidateAddedBack(t *testing.T) {
	w := newMoverWorld(t)
	e := w.e
	failGroupRemovalAndAge(t, w)
	var raw string
	wrapped := &afterCandidates{PG: e.st, hook: func() {
		if r := e.patchGroup(e.a, w.group.ID, addMembersPatch(w.moverID)); r.Code != 200 {
			t.Fatalf("re-add = %d %s", r.Code, r.Body.String())
		}
		_, _, raw = e.mintTokenAs(e.a, entrafake.Identity{Username: moverEmail, Subject: "sub-mover", Groups: []string{groupClaim}})
	}}
	n := e.node(func(c *Config) { c.Store = wrapped })

	if err := n.srv.SweepSCIMPurge(context.Background()); err != nil {
		t.Fatal(err)
	}
	if raw == "" {
		t.Fatal("the sweeper selected no candidate")
	}
	members, err := e.st.ScimGroupMembers(context.Background(), uuid.MustParse(w.group.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(members, uuid.MustParse(w.moverID)) {
		t.Error("the sweeper deleted the membership the identity provider added back")
	}
	for name, node := range map[string]*scimNode{"a": e.a, "b": e.b} {
		if !e.tokenWorks(node, raw) {
			t.Errorf("instance %s: the token minted after the re-add stopped authenticating", name)
		}
	}
	if len(e.rows(n, "scim.group.member_remove")) != 0 {
		t.Error("the sweeper recorded a removal of a person added back")
	}
}
