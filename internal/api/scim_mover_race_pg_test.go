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
	"github.com/cjohnstoniv/wardyn/internal/testutil"
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

// The identity provider's add is in flight when the sweeper resumes the mover's removal: the add's foreign-key
// check holds the identity row, so the resume waits for it, and once the add commits the resume sees the
// membership and drops the candidate.
func TestSCIMGroupRemovalSweeperWaitsForAnAddInFlight(t *testing.T) {
	w := newMoverWorld(t)
	e := w.e
	ctx := context.Background()
	failGroupRemovalAndAge(t, w)
	tok, raw := e.seedToken("sub-mover", moverEmail)
	if _, err := e.pool.Exec(ctx, `UPDATE api_tokens SET groups = $2::jsonb WHERE id = $1`, tok, `["`+groupExternalID+`"]`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE deprovision_jobs SET state = 'pending' WHERE identity_id = $1 AND kind = $2`,
		w.moverID, store.JobKindGroupRemove); err != nil {
		t.Fatal(err)
	}

	add, err := testutil.PGConn(t, e.pool).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = add.Rollback(ctx) }()
	if _, err := add.Exec(ctx, `INSERT INTO scim_group_members (group_id, identity_id, added_at) VALUES ($1, $2, now())`, w.group.ID, w.moverID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- e.b.srv.SweepSCIMPurge(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("the sweep finished without waiting for the add in flight: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the sweep never waited on the identity row")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := add.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	if len(e.rows(e.b, "scim.group.member_remove")) != 0 {
		t.Error("the sweeper recorded a removal of a person added back")
	}
	if e.tokenRevoked(tok) {
		t.Error("the sweeper revoked the member's token")
	}
	for name, node := range map[string]*scimNode{"a": e.a, "b": e.b} {
		if !e.tokenWorks(node, raw) {
			t.Errorf("instance %s: the member's token stopped authenticating", name)
		}
	}
}
