// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// digestStore is dispatchTestStore with the agent_runs.policy_digest column.
type digestStore struct {
	*dispatchTestStore
	mu     sync.Mutex
	digest map[uuid.UUID]string
}

var _ store.RunPolicyDigests = (*digestStore)(nil)

func (s *digestStore) SetRunPolicyDigest(_ context.Context, id uuid.UUID, d string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old := s.digest[id]; old != "" && old != d {
		return store.ErrPolicyDigestSet
	}
	s.digest[id] = d
	return nil
}

func (s *digestStore) lookup(_ context.Context, id uuid.UUID) (types.RunPolicyState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return types.RunPolicyState{Digest: s.digest[id]}, nil
}

// TestDispatch_ResolveRowCarriesItsOwnDigest: dispatch writes the digest of the policy it records
// BEFORE it records run.policy.resolve, so the resolve row is itself stamped, and the stamp equals the
// digest recomputed from that row's own stored data.
func TestDispatch_ResolveRowCarriesItsOwnDigest(t *testing.T) {
	rn := &killRaceRunner{fakeRunner: &fakeRunner{}}
	srv, st, rec, run := dispatchTeardownFixture(t, rn, types.RunPending)
	ds := &digestStore{dispatchTestStore: st, digest: map[uuid.UUID]string{}}
	srv.cfg.Store = ds
	srv.cfg.Audit = audit.PolicyStampRecorder{Inner: rec, Lookup: ds.lookup}

	srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
		RunToken: "run-token", Image: "wardyn/claude-code:latest",
		Policy: types.RunPolicySpec{MinConfinementClass: types.CC1, AllowedDomains: []string{"api.example.com"}},
	})

	ev := findAudit(rec.snapshot(), run.ID, "run.policy.resolve", "success")
	if ev == nil {
		t.Fatal("no run.policy.resolve row")
	}
	var spec types.RunPolicySpec
	if err := json.Unmarshal(ev.Data, &spec); err != nil {
		t.Fatal(err)
	}
	var stamped struct {
		PolicyDigest string `json:"policy_digest"`
	}
	if err := json.Unmarshal(ev.Data, &stamped); err != nil {
		t.Fatal(err)
	}
	if stamped.PolicyDigest == "" || stamped.PolicyDigest != spec.Digest() {
		t.Errorf("resolve row policy_digest = %q, digest recomputed from its data = %q; want them equal and non-empty", stamped.PolicyDigest, spec.Digest())
	}
	if got, _ := ds.lookup(context.Background(), run.ID); got.Digest != stamped.PolicyDigest {
		t.Errorf("stored digest = %q, want %q", got.Digest, stamped.PolicyDigest)
	}
}

// TestEgressDecision_CarriesRunPolicyDigest: an action row for a run whose digest is stored carries it.
func TestEgressDecision_CarriesRunPolicyDigest(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	ds := &digestStore{digest: map[uuid.UUID]string{runID: "digest-of-the-run"}}
	h.srv.cfg.Audit = audit.PolicyStampRecorder{Inner: h.srv.cfg.Audit, Lookup: ds.lookup}
	tok := h.mintRunToken(t, runID)
	body := `{"request":{"host":"api.example.com","method":"CONNECT"},"decision":"allow","rule_source":"policy"}`
	if w := do(t, h.srv, http.MethodPost, "/api/v1/internal/decisions", tok, body); w.Code != http.StatusAccepted {
		t.Fatalf("decision code = %d, want 202", w.Code)
	}
	for _, ev := range h.audit.snapshot() {
		if ev.Action != "egress.allow" {
			continue
		}
		var d struct {
			PolicyDigest string `json:"policy_digest"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil || d.PolicyDigest != "digest-of-the-run" {
			t.Errorf("egress.allow data = %s, want policy_digest digest-of-the-run", ev.Data)
		}
		return
	}
	t.Fatal("no egress.allow audit event recorded")
}
