// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Integration test for creating a SCIM group with its initial members. Guarded by WARDYN_TEST_PG; skipped
// cleanly when unset.
package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// The group and its initial members commit together. A member write that fails leaves no group, so the
// identity provider's retry with the same externalId succeeds; a group that really exists is still
// ErrScimGroupExists.
func TestPG_ScimGroupCreateWithMembersIsOneTransaction(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	now := time.Now()
	var ann uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO principal_identities (principal, issuer, email_lower) VALUES ('sub-ann', 'https://idp.example', 'ann@corp.example') RETURNING id`).Scan(&ann); err != nil {
		t.Fatal(err)
	}
	// A membership insert that fails, as a lost connection or a lock timeout would.
	for _, q := range []string{
		`CREATE FUNCTION test_refuse_member() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'member write refused'; END $$`,
		`CREATE TRIGGER test_refuse_member BEFORE INSERT ON scim_group_members FOR EACH ROW EXECUTE FUNCTION test_refuse_member()`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := st.CreateScimGroupWithMembers(ctx, "eng", "Engineering", []uuid.UUID{ann}, now); err == nil || errors.Is(err, store.ErrScimGroupExists) {
		t.Fatalf("create with a failing member write = %v, want a plain error", err)
	}
	var groups int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM scim_groups WHERE external_id = 'eng'`).Scan(&groups); err != nil || groups != 0 {
		t.Fatalf("groups after the failed create = %d, %v; want none", groups, err)
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER test_refuse_member ON scim_group_members`); err != nil {
		t.Fatal(err)
	}
	g, members, err := st.CreateScimGroupWithMembers(ctx, "eng", "Engineering", []uuid.UUID{ann, ann, uuid.New()}, now)
	if err != nil {
		t.Fatalf("the retry with the same externalId: %v, want it to succeed", err)
	}
	if len(members) != 1 || members[0] != ann {
		t.Fatalf("members the create returned = %v, want the one identity once", members)
	}
	if got, err := st.ScimGroupMembers(ctx, g.ID); err != nil || len(got) != 1 || got[0] != ann {
		t.Fatalf("members after the retry = %v, %v; want the one identity", got, err)
	}
	if _, _, err := st.CreateScimGroupWithMembers(ctx, " ENG ", "Again", nil, now); !errors.Is(err, store.ErrScimGroupExists) {
		t.Fatalf("create over a group that exists = %v, want ErrScimGroupExists", err)
	}
}

// A displayName lookup compares the whole name, ignoring case only: a group created with a trailing space is
// found by that name, and the near-miss without the space finds only its own group. externalId still ignores
// outer spaces.
func TestPG_SearchScimGroupsDisplayNameIsExact(t *testing.T) {
	st := store.NewPG(runsPGPoolIsolated(t))
	ctx := context.Background()
	now := time.Now()
	padded, _, err := st.CreateScimGroupWithMembers(ctx, "g-padded", "Platform ", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	plain, _, err := st.CreateScimGroupWithMembers(ctx, "g-plain", "Platform", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		value string
		want  uuid.UUID
	}{{"Platform ", padded.ID}, {"PLATFORM ", padded.ID}, {"platform", plain.ID}} {
		got, err := st.SearchScimGroups(ctx, store.SearchDisplayName, c.value, 10)
		if err != nil || len(got) != 1 || got[0].ID != c.want {
			t.Errorf("displayName eq %q = %+v, %v; want only %s", c.value, got, err, c.want)
		}
	}
	if got, err := st.SearchScimGroups(ctx, store.SearchExternalID, " G-PLAIN ", 10); err != nil || len(got) != 1 || got[0].ID != plain.ID {
		t.Errorf("externalId lookup = %+v, %v; want the plain group", got, err)
	}
}
