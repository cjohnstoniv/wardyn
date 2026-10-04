// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

// The run token in Postgres (migration 0121, internal/adorunpat): sealed under the owner's key,
// readable by a second wardynd, deleted with the person, and never written back after the
// erasure.

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/adorunpat"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey/subjectkeytest"
)

func TestPG_ADORunPATState(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	keys := subjectkeytest.Manager(pool, localKEK(t))
	a, b := adorunpat.New(pool, keys), adorunpat.New(pool, keys)
	const owner, token = "alice", "the-run-token-value-0123456789abcdefghij"
	run := manifestRun(t, pool, owner)
	other := manifestRun(t, pool, "bob")

	if _, found, err := b.Load(ctx, run); err != nil || found {
		t.Fatalf("an empty state read found=%v err=%v", found, err)
	}
	validTo := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	rec := adorunpat.Record{Owner: owner, AuthorizationID: "auth-1", Token: token, Scope: "vso.code", ValidTo: validTo,
		Capabilities: []string{"code.read", "project.read"}, Paused: false, ForcedAt: validTo.Add(-time.Minute)}
	if err := a.Save(ctx, run, owner, rec); err != nil {
		t.Fatal(err)
	}

	// A second process reads what the first wrote.
	got, found, err := b.Load(ctx, run)
	if err != nil || !found || got.Token != token || got.AuthorizationID != "auth-1" || got.Scope != "vso.code" ||
		!got.ValidTo.Equal(validTo) || got.Owner != owner || len(got.Capabilities) != 2 || got.Capabilities[1] != "project.read" ||
		!got.ForcedAt.Equal(rec.ForcedAt) {
		t.Fatalf("B read %+v found=%v err=%v, want what A saved", got, found, err)
	}

	// The table holds the sealed value, never the token.
	var sealed []byte
	var version int
	if err := pool.QueryRow(ctx, `SELECT sealed, key_version FROM ado_run_pat_state WHERE run_id = $1`, run).Scan(&sealed, &version); err != nil {
		t.Fatal(err)
	}
	if len(sealed) == 0 || bytes.Contains(sealed, []byte(token)) {
		t.Fatalf("the stored value is %d bytes and contains the token: %v", len(sealed), bytes.Contains(sealed, []byte(token)))
	}
	var dump string
	_ = pool.QueryRow(ctx, `SELECT row(s.*)::text FROM ado_run_pat_state s WHERE run_id = $1`, run).Scan(&dump)
	if bytes.Contains([]byte(dump), []byte(token)) {
		t.Fatal("the token is in the row's text form")
	}

	// A state with no token (paused) keeps its flags and no value.
	if err := a.Save(ctx, run, owner, adorunpat.Record{Owner: owner, Paused: true}); err != nil {
		t.Fatal(err)
	}
	if got, _, _ = b.Load(ctx, run); got.Token != "" || !got.Paused {
		t.Fatalf("paused state = %+v, want no token and paused", got)
	}
	if err := a.Save(ctx, run, owner, rec); err != nil {
		t.Fatal(err)
	}
	if err := a.Save(ctx, other, "bob", adorunpat.Record{Owner: "bob", Token: "bobs-token-value-0123456789abcdefghijklmn"}); err != nil {
		t.Fatal(err)
	}

	// An erasure fences the person's manifests and deletes their state; a write after it, from
	// any replica, is refused and recreates nothing.
	rep := newReplica(t, pool, localKEK(t))
	if err := rep.m.Start(ctx, run, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := rep.m.FenceSubject(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if n, err := b.DeleteOwner(ctx, owner); err != nil || n != 1 {
		t.Fatalf("DeleteOwner = %d, %v, want 1 and nil", n, err)
	}
	if err := a.Save(ctx, run, owner, rec); !errors.Is(err, adorunpat.ErrFenced) {
		t.Fatalf("a save after the erasure = %v, want ErrFenced", err)
	}
	var rows int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM ado_run_pat_state WHERE owner = $1`, owner).Scan(&rows)
	if rows != 0 {
		t.Fatalf("%d of the person's token rows remain after the erasure and a late write", rows)
	}
	if _, found, _ := a.Load(ctx, other); !found {
		t.Error("another person's state went with the erasure")
	}

	// Without the owner's key the value cannot be opened.
	if _, err := keys.Destroy(ctx, "bob", subjectkey.PurposeCred); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Load(ctx, other); err == nil {
		t.Fatal("a value opened after its owner's key was destroyed")
	}
}
