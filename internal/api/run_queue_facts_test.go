// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

type countingPools struct {
	store.RunnerPoolStore
	reads int
}

func (c *countingPools) ListRunnerPoolCatalogue(context.Context, string, bool) ([]store.RunnerPoolCatalogueRow, error) {
	c.reads++
	return nil, nil
}

// TestQueuePass_ReadsAnOwnersPoolsOncePerPass: the waiting runs of one owner under one authority share
// one read of the owner's pools within a pass; another authority, or a judgement outside any pass,
// reads again.
func TestQueuePass_ReadsAnOwnersPoolsOncePerPass(t *testing.T) {
	srv, _, _ := newRunnerRegistrationServer(t)
	ps := &countingPools{}
	read := func(ctx context.Context) {
		t.Helper()
		if _, refusal := srv.queuePermittedPools(ctx, ps); refusal != nil {
			t.Fatalf("queuePermittedPools: %+v", refusal)
		}
	}
	alice := withOIDCHuman(context.Background(), "alice")

	pass := withQueuePass(alice)
	read(withQueueOwner(pass, "alice", []byte(`{"sub":"alice"}`)))
	read(withQueueOwner(pass, "alice", []byte(`{"sub":"alice"}`)))
	if ps.reads != 1 {
		t.Fatalf("two runs under one authority read the pools %d times, want 1", ps.reads)
	}
	read(withQueueOwner(pass, "alice", []byte(`{"sub":"alice","user_type":"contractor"}`)))
	if ps.reads != 2 {
		t.Fatalf("a run under another authority read the pools %d times in all, want 2", ps.reads)
	}
	read(withQueueOwner(withQueuePass(alice), "alice", []byte(`{"sub":"alice"}`)))
	if ps.reads != 3 {
		t.Fatalf("a new pass read the pools %d times in all, want 3 (no fact outlives its pass)", ps.reads)
	}
	read(alice)
	read(alice)
	if ps.reads != 5 {
		t.Fatalf("two judgements outside a pass read the pools %d times in all, want 5", ps.reads)
	}
}

// TestQueueHoldKey_OnePersonAcrossSessionsAndTokens: one person's runs queued from two sessions, or
// through two delegated tokens of one portal, share a hold key, so one owner-wide wait passes over both
// in a pass; another group snapshot, or a launch with no portal, is a key of its own.
func TestQueueHoldKey_OnePersonAcrossSessionsAndTokens(t *testing.T) {
	key := func(authority string) string {
		return queueHoldKey(store.RunQueueEntry{RunID: uuid.New(), Owner: "alice", Authority: []byte(authority), Launch: []byte(`{"request":{}}`)})
	}
	const portal = `"delegate":"6f1c0a52-2b0e-4d7e-9a43-0d6a3b1f2c11"`
	one := key(`{"sub":"alice","groups":["eng"],"epoch":1,"rseq":4,` + portal +
		`,"grant":"0b8e6d0e-5a51-4f0c-8f0e-3f4b3a1c9d01","actor":{"sub":"bot","iss":"https://a.example"}}`)
	two := key(`{"sub":"alice","groups":["eng"],"epoch":2,"rseq":9,` + portal +
		`,"grant":"a3d1f0c4-7b6e-4c39-b1f2-58e2a9d4c702","actor":{"sub":"other","iss":"https://b.example"}}`)
	if one != two {
		t.Errorf("two sessions and tokens of one person have two hold keys:\n%q\n%q", one, two)
	}
	for name, other := range map[string]string{
		"another group snapshot": `{"sub":"alice","groups":["ops"],"epoch":1,"rseq":4,` + portal + `,"grant":"0b8e6d0e-5a51-4f0c-8f0e-3f4b3a1c9d01"}`,
		"no portal":              `{"sub":"alice","groups":["eng"],"epoch":1,"rseq":4}`,
	} {
		if key(other) == one {
			t.Errorf("a run with %s shares the hold key", name)
		}
	}
}
