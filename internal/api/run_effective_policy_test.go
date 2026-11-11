// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/ghscope"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// genViewStore is pvStore read the way Postgres serves the effective-policy readers: a plain read of a
// run's events stops at the limit asked, oldest first (ORDER BY seq ASC LIMIT); a filtered read keeps
// every matching row; and the run's policy generations live beside it, as agent_runs holds them. Pager
// methods the readers never call are the nil embedded ones.
type genViewStore struct {
	*pvStore
	store.Pager
	gmu   sync.Mutex
	state types.RunPolicyState
}

func (s *genViewStore) QueryAuditEvents(ctx context.Context, runID uuid.UUID, limit int) ([]types.AuditEvent, error) {
	all, err := s.pvStore.QueryAuditEvents(ctx, runID, 0)
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all, err
}

func (s *genViewStore) QueryAuditEventsFilteredPage(ctx context.Context, runID *uuid.UUID, f store.AuditFilter, p store.Page) ([]types.AuditEvent, error) {
	all, err := s.pvStore.QueryAuditEvents(ctx, *runID, 0)
	out := f.Keep(all)
	if p.Newest {
		slices.Reverse(out)
	}
	if p.Limit > 0 && len(out) > p.Limit {
		out = out[:p.Limit]
	}
	return out, err
}

func (s *genViewStore) AdvanceRunPolicyGeneration(_ context.Context, _ uuid.UUID, expected store.PolicySlot, digest string, retired []string, fresh string, _ []byte) (int, error) {
	s.gmu.Lock()
	defer s.gmu.Unlock()
	if store.NewestPolicySlot(s.state) != expected {
		return 0, store.ErrRunPolicyMoved
	}
	var gen int
	s.state, gen = s.state.Advance(digest, retired, fresh)
	return gen, nil
}

func (s *genViewStore) AbandonRunPolicyGeneration(_ context.Context, _ uuid.UUID, gen int, _ []byte) error {
	s.gmu.Lock()
	defer s.gmu.Unlock()
	s.state = s.state.Abandon(gen)
	return nil
}

func (s *genViewStore) RunPolicyState(context.Context, uuid.UUID) (types.RunPolicyState, bool, error) {
	s.gmu.Lock()
	defer s.gmu.Unlock()
	return s.state, true, nil
}

// The policy a run was dispatched under, and the narrower one a revive proposes after the owner's
// profile drops a.example and code_write.
var (
	genViewDispatched = types.RunPolicySpec{AllowedDomains: []string{"a.example", "b.example"},
		GitHubCapabilities: []ghscope.Capability{ghscope.CapCodeRead, ghscope.CapCodeWrite}}
	genViewRevived = types.RunPolicySpec{AllowedDomains: []string{"b.example"},
		GitHubCapabilities: []ghscope.Capability{ghscope.CapCodeRead}}
)

// newGenViewFixture is a running run dispatched under genViewDispatched, followed by filler more of
// the run's own rows.
func newGenViewFixture(t *testing.T, filler int) (*Server, *genViewStore, types.AgentRun) {
	t.Helper()
	srv, st := pvFixture(t, &capStore{})
	gs := &genViewStore{pvStore: st, state: types.RunPolicyState{Digest: genViewDispatched.Digest()}}
	srv.cfg.Store = gs
	run := st.seedRun(pvOwner, types.RunRunning)
	rows := []pvRow{pvResolve(genViewDispatched, false)}
	for range filler {
		rows = append(rows, pvRow{action: "egress.allow", data: map[string]any{"host": "b.example"}})
	}
	st.rows(run.ID, rows...)
	return srv, gs, run
}

// reviseGenView starts the revive's generation as reviveRunProxy does, before the new proxy starts.
func reviseGenView(t *testing.T, srv *Server, run types.AgentRun) int {
	t.Helper()
	gen, err := srv.advanceRunPolicyGeneration(context.Background(), run, genViewRevived, "", renewedTokens{}, "jti-revived", store.PolicySlot{Generation: 1}, nil)
	if err != nil || gen != 2 {
		t.Fatalf("advance = %d, %v; want generation 2", gen, err)
	}
	return gen
}

// wantEffectivePolicy checks two of the readers that share the run's effective policy: the run policy
// view's spec and the capabilities approval rules match against.
func wantEffectivePolicy(t *testing.T, srv *Server, run types.AgentRun, want types.RunPolicySpec) {
	t.Helper()
	w, resp := pvReader{}.get(t, srv, run.ID)
	if w.Code != http.StatusOK {
		t.Fatalf("run policy view: status %d: %s", w.Code, w.Body.String())
	}
	if resp.Spec == nil || !slices.Equal(resp.Spec.AllowedDomains, want.AllowedDomains) {
		t.Errorf("run policy view spec = %+v, want allowed domains %v", resp.Spec, want.AllowedDomains)
	}
	var wantCaps []string
	for _, c := range want.GitHubCapabilities {
		wantCaps = append(wantCaps, string(c))
	}
	caps, err := srv.runScopeCapabilities(context.Background(), run)
	if err != nil || !slices.Equal(caps, wantCaps) {
		t.Errorf("capabilities = %v (err %v), want %v", caps, err, wantCaps)
	}
}

// A revive that left the run's old proxy running abandons its generation. The proxy still enforces the
// dispatch policy, so every reader keeps it: the abandoned proposal's narrower row is not the policy.
func TestEffectivePolicy_AbandonedGenerationKeepsTheDispatchPolicy(t *testing.T) {
	srv, _, run := newGenViewFixture(t, 0)
	gen := reviseGenView(t, srv, run)
	srv.abandonRunPolicyGeneration(context.Background(), run, gen, nil)
	wantEffectivePolicy(t, srv, run, genViewDispatched)
}

// A revive that replaced the proxy keeps its generation current, so the readers take its policy.
func TestEffectivePolicy_CurrentGenerationIsThePolicy(t *testing.T) {
	srv, _, run := newGenViewFixture(t, 0)
	reviseGenView(t, srv, run)
	wantEffectivePolicy(t, srv, run, genViewRevived)
}

// A run with more of its own rows than one plain read returns before the revive still reads the revived
// policy: the current generation's row is looked up by its number, not found in the run's first rows.
func TestEffectivePolicy_CurrentGenerationPastTheFirstRows(t *testing.T) {
	srv, _, run := newGenViewFixture(t, effectivePolicyAuditScan+50)
	reviseGenView(t, srv, run)
	wantEffectivePolicy(t, srv, run, genViewRevived)
}
