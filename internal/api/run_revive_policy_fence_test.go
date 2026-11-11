// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Two replicas over one store, the second with its own run locks: a revive (A) on the first whose lock
// is lost, and an edit (B) on the second that takes the lock and narrows the run. Each test lets B run at
// one of the real calls A makes, as a lost lock lets it.

// fenceHost is the host B removes from the run's allowlist and denies.
const fenceHost = "api.anthropic.com"

// interleaveStore is readbackStore with a hook at the store calls a revive makes, each run once, at its
// first call: before the run's generation read (RunPolicyState), before its source config read after that
// generation read (GetRunProxyConfig), before its lineage retire (after the claim), and after a
// generation advance lands.
type interleaveStore struct {
	*readbackStore
	mu                                               sync.Mutex
	genRead                                          bool
	beforeGenRead, beforeSourceRead, beforeRetire    func()
	afterAdvance                                     func(gen int)
	firedGen, firedSource, firedRetire, firedAdvance bool
}

// take returns the hook to run now, once: nil when it ran already or is not set.
func (s *interleaveStore) take(fired *bool, hook func()) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if *fired || hook == nil {
		return nil
	}
	*fired = true
	return hook
}

func (s *interleaveStore) RunPolicyState(ctx context.Context, id uuid.UUID) (types.RunPolicyState, bool, error) {
	if h := s.take(&s.firedGen, s.beforeGenRead); h != nil {
		h()
	}
	s.mu.Lock()
	s.genRead = true
	s.mu.Unlock()
	return s.readbackStore.RunPolicyState(ctx, id)
}

func (s *interleaveStore) GetRunProxyConfig(ctx context.Context, id uuid.UUID) ([]byte, error) {
	s.mu.Lock()
	read := s.genRead
	s.mu.Unlock()
	if read {
		if h := s.take(&s.firedSource, s.beforeSourceRead); h != nil {
			h()
		}
	}
	return s.readbackStore.GetRunProxyConfig(ctx, id)
}

func (s *interleaveStore) RetireRunTokenLineage(ctx context.Context, id uuid.UUID, fresh string) (string, string, error) {
	if h := s.take(&s.firedRetire, s.beforeRetire); h != nil {
		h()
	}
	return s.readbackStore.RetireRunTokenLineage(ctx, id, fresh)
}

func (s *interleaveStore) AdvanceRunPolicyGeneration(ctx context.Context, id uuid.UUID, expected int, digest string, retired []string, fresh string, sealed []byte) (int, error) {
	gen, err := s.readbackStore.AdvanceRunPolicyGeneration(ctx, id, expected, digest, retired, fresh, sealed)
	if err == nil {
		var hook func()
		if s.afterAdvance != nil {
			hook = s.take(&s.firedAdvance, func() { s.afterAdvance(gen) })
		}
		if hook != nil {
			hook()
		}
	}
	return gen, err
}

// fenceFixture is newEgressEditFixture on an interleaveStore, with a second replica (b) over the same
// store, runner, identity and audit, whose run locks are its own.
type fenceFixture struct {
	*readbackFixture
	is *interleaveStore
	b  *Server
}

func newFenceFixture(t *testing.T) *fenceFixture {
	t.Helper()
	f := newEgressEditFixture(t)
	is := &interleaveStore{readbackStore: f.rb}
	f.srv.cfg.Store = is
	f.confirmEachReplace(t)
	return &fenceFixture{readbackFixture: f, is: is, b: New(f.srv.cfg)}
}

// editOnB is B: the edit that removes and denies fenceHost, on the second replica.
func (f *fenceFixture) editOnB(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, f.b, http.MethodPatch, "/api/v1/runs/"+f.run.ID.String()+"/egress", adminToken,
		`{"remove_hosts":["`+fenceHost+`"],"deny_hosts":["`+fenceHost+`"]}`)
}

// reviveOnA is A: an ordinary revive on the first replica, from a source config that still allows fenceHost.
func (f *fenceFixture) reviveOnA(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, f.srv, http.MethodPost, "/api/v1/runs/"+f.run.ID.String()+"/revive", adminToken, "")
}

// loseLock makes srv's every hold read as lost from now on, as a lock whose connection died does.
func loseLock(srv *Server) {
	srv.locks.held = func(context.Context) error { return db.ErrLockLost }
}

// storedPolicy is the policy of the run's stored config, the one every later revive starts from.
func (f *fenceFixture) storedPolicy(t *testing.T) types.RunPolicySpec {
	t.Helper()
	f.rs.mu.Lock()
	raw := slices.Clone(f.rs.cfg)
	f.rs.mu.Unlock()
	cfg, err := proxy.LoadConfigBytes(raw)
	if err != nil {
		t.Fatalf("the stored config does not load: %v", err)
	}
	return cfg.Policy
}

// denied fails unless policy keeps fenceHost off its allowlist and denies it.
func denied(t *testing.T, what string, policy types.RunPolicySpec) {
	t.Helper()
	if slices.Contains(policy.AllowedDomains, fenceHost) || !slices.Contains(policy.DeniedDomains, fenceHost) {
		t.Errorf("%s: allowed %v denied %v; want %s removed and denied", what, policy.AllowedDomains, policy.DeniedDomains, fenceHost)
	}
}

// policyState is the run's policy generations as the store holds them.
func (f *fenceFixture) policyState(t *testing.T) types.RunPolicyState {
	t.Helper()
	st, _, err := f.rb.RunPolicyState(context.Background(), f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// A loses its lock after its claim; B narrows and confirms; then A resumes. A's advance expects the
// generation it read, which B moved on, so A answers that the run keeps its current proxy, issues no
// replace and no stop, and B's confirmed start and narrowed config stand.
func TestReviveFence_LostLockAfterClaimKeepsTheNewerEdit(t *testing.T) {
	f := newFenceFixture(t)
	stops := f.lr.proxyStopCount()
	var b *httptest.ResponseRecorder
	f.is.beforeRetire = func() { b = f.editOnB(t) }
	a := f.reviveOnA(t)
	if b == nil || b.Code != http.StatusOK {
		t.Fatalf("B never ran or did not confirm: %v", b)
	}
	if a.Code != http.StatusBadGateway || errorReason(a) != reasonReviveProxyKeptCurrent {
		t.Fatalf("A: %d %s, want 502 %s", a.Code, a.Body.String(), reasonReviveProxyKeptCurrent)
	}
	if n := len(f.rr.replaced); n != 1 {
		t.Errorf("replaces = %d, want B's one: A must issue none", n)
	}
	if f.lr.proxyStopCount() != stops {
		t.Error("A must stop no proxy")
	}
	if p := f.active(t); p.State != types.ProxyStartEnforced || p.Generation != 2 {
		t.Errorf("start = %d %s, want B's generation 2 enforced", p.Generation, p.State)
	}
	denied(t, "the running proxy", f.handedPolicy(t))
	denied(t, "the stored config", f.storedPolicy(t))
}

// A reads the run's generation; B then narrows and confirms (its config and generation in one write);
// A reads the source config B stored. A's generation is stale, so its advance changes nothing: A answers
// kept current and issues no replace.
func TestReviveFence_EditBetweenTheGenerationAndSourceReads(t *testing.T) {
	f := newFenceFixture(t)
	var b *httptest.ResponseRecorder
	f.is.beforeSourceRead = func() { b = f.editOnB(t) }
	a := f.reviveOnA(t)
	if b == nil || b.Code != http.StatusOK {
		t.Fatalf("B never ran or did not confirm: %v", b)
	}
	if a.Code != http.StatusBadGateway || errorReason(a) != reasonReviveProxyKeptCurrent {
		t.Fatalf("A: %d %s, want 502 %s", a.Code, a.Body.String(), reasonReviveProxyKeptCurrent)
	}
	if n := len(f.rr.replaced); n != 1 {
		t.Errorf("replaces = %d, want B's one", n)
	}
	denied(t, "the running proxy", f.handedPolicy(t))
	denied(t, "the stored config", f.storedPolicy(t))
}

// B's generation lands and B pauses right after it; A then reads the generation and the source. B's
// config was stored in the same write as its generation, so A reads B's narrowed config, never the one
// before it: A wins with that config and fenceHost stays denied. B resumes with its lock lost, replaces
// nothing, and its late abandon leaves A's newer generation alone.
func TestReviveFence_ReviveAfterTheEditsGenerationReadsTheEditsConfig(t *testing.T) {
	f := newFenceFixture(t)
	bAdvanced, releaseB := make(chan struct{}), make(chan struct{})
	bDone := make(chan *httptest.ResponseRecorder, 1)
	f.is.afterAdvance = func(int) {
		close(bAdvanced)
		<-releaseB
	}
	f.is.beforeGenRead = func() {
		go func() { bDone <- f.editOnB(t) }()
		<-bAdvanced
	}
	a := f.reviveOnA(t)
	if a.Code != http.StatusOK {
		t.Fatalf("A: %d %s, want 200", a.Code, a.Body.String())
	}
	denied(t, "A's proxy", f.handedPolicy(t))
	loseLock(f.b)
	close(releaseB)
	b := <-bDone
	if b.Code != http.StatusBadGateway || errorReason(b) != reasonReviveProxyKeptCurrent {
		t.Errorf("B after its lock was lost: %d %s, want 502 %s", b.Code, b.Body.String(), reasonReviveProxyKeptCurrent)
	}
	if n := len(f.rr.replaced); n != 1 {
		t.Errorf("replaces = %d, want A's one: B must issue none once its lock is lost", n)
	}
	if st := f.policyState(t); st.CurrentGeneration() != 3 {
		t.Errorf("current generation = %d, want A's 3: B's late abandon must leave it", st.CurrentGeneration())
	}
	denied(t, "the stored config", f.storedPolicy(t))
}

// A wins its advance and pauses before the replace; B, on the lock A lost, narrows from A's stored
// config, wins the next generation and confirms; A resumes with its hold lost. A issues no replace, so
// B's proxy keeps running, and A's late abandon leaves B's generation and config untouched.
func TestReviveFence_LostLockAfterTheAdvanceIssuesNoReplace(t *testing.T) {
	f := newFenceFixture(t)
	stops := f.lr.proxyStopCount()
	var b *httptest.ResponseRecorder
	var afterB types.RunPolicyState
	var storedAfterB string
	f.is.afterAdvance = func(int) {
		b = f.editOnB(t)
		afterB = f.policyState(t)
		f.rs.mu.Lock()
		storedAfterB = string(f.rs.cfg)
		f.rs.mu.Unlock()
		loseLock(f.srv)
	}
	a := f.reviveOnA(t)
	if b == nil || b.Code != http.StatusOK {
		t.Fatalf("B never ran or did not confirm: %v", b)
	}
	if a.Code != http.StatusBadGateway || errorReason(a) != reasonReviveProxyKeptCurrent {
		t.Fatalf("A: %d %s, want 502 %s", a.Code, a.Body.String(), reasonReviveProxyKeptCurrent)
	}
	if n := len(f.rr.replaced); n != 1 {
		t.Fatalf("replaces = %d, want B's one: A must issue no replace once its lock is lost", n)
	}
	if f.lr.proxyStopCount() != stops {
		t.Error("A must stop no proxy")
	}
	if p := f.active(t); p.State != types.ProxyStartEnforced || p.Generation != 3 {
		t.Errorf("start = %d %s, want B's generation 3 enforced", p.Generation, p.State)
	}
	st := f.policyState(t)
	if st.CurrentGeneration() != 3 || st.Digest != afterB.Digest || len(st.Generations) != len(afterB.Generations) ||
		st.Generations[len(st.Generations)-1].Abandoned {
		t.Errorf("after A's late abandon: %+v, want B's generation untouched (%+v)", st, afterB)
	}
	f.rs.mu.Lock()
	stored := string(f.rs.cfg)
	f.rs.mu.Unlock()
	if stored != storedAfterB {
		t.Error("A's late abandon overwrote B's stored config")
	}
	denied(t, "the running proxy", f.handedPolicy(t))
	denied(t, "the stored config", f.storedPolicy(t))
}
