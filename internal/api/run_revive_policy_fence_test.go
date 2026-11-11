// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
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
	// dropAck makes the first advance that lands answer an error all the same, as a commit whose
	// acknowledgement was lost does; afterDroppedAck runs before that answer.
	dropAck, firedDrop bool
	afterDroppedAck    func(gen int)
	// failBegin makes every BeginProxyStart fail, as a database blip would.
	failBegin bool
	// owed is the run's agent start still owed: the claim of a revive that starts the agent leaves
	// agent_stopped_at set on the live row (store.RunAgentStartOwed), at owedAt. claims are the stamps
	// those claims returned and cleared the stamps ClearRunAgentStartOwed matched.
	owed            bool
	owedAt          time.Time
	claims, cleared []time.Time
	// failClears is how many more ClearRunAgentStartOwed calls fail, as a database blip would; clearCalls counts them all.
	failClears, clearCalls int
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

func (s *interleaveStore) AdvanceRunPolicyGeneration(ctx context.Context, id uuid.UUID, expected store.PolicySlot, digest string, retired []string, fresh string, sealed []byte) (int, error) {
	gen, err := s.readbackStore.AdvanceRunPolicyGeneration(ctx, id, expected, digest, retired, fresh, sealed)
	if err == nil {
		s.mu.Lock()
		drop := s.dropAck && !s.firedDrop
		s.firedDrop = s.firedDrop || drop
		s.mu.Unlock()
		if drop {
			if s.afterDroppedAck != nil {
				s.afterDroppedAck(gen)
			}
			return 0, errors.New("write policy generation: the commit's acknowledgement was lost")
		}
	}
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

func (s *interleaveStore) BeginProxyStart(ctx context.Context, runID uuid.UUID, gen int, jti, mode string) (store.ProxyStart, error) {
	if s.failBegin {
		return store.ProxyStart{}, errors.New("begin proxy start: connection reset")
	}
	return s.readbackStore.BeginProxyStart(ctx, runID, gen, jti, mode)
}

// MarkRunRevived also clears the agent's stop mark, as the PG claim (markRunRevived) does, and refuses a
// live claim that starts no agent while a start is owed.
func (s *interleaveStore) MarkRunRevived(ctx context.Context, id uuid.UUID, from types.LostReason, ended *store.EndedKept, limit int, startsAgent bool) (bool, error) {
	s.mu.Lock()
	refused := !startsAgent && from == "" && s.owed
	s.mu.Unlock()
	if refused {
		return false, nil
	}
	ok, err := s.readbackStore.MarkRunRevived(ctx, id, from, ended, limit, startsAgent)
	if ok {
		s.readbackStore.generationStore.mu.Lock()
		s.readbackStore.generationStore.agentStopped = false
		s.readbackStore.generationStore.mu.Unlock()
		s.mu.Lock()
		s.owed = false
		s.mu.Unlock()
	}
	return ok, err
}

var _ store.RunAgentStartOwed = (*interleaveStore)(nil)

// owe leaves an agent start owed on a fresh stamp, as an owing claim does.
func (s *interleaveStore) owe() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.owed = true
	s.owedAt = time.Unix(int64(len(s.claims)+1), 0).UTC()
	s.claims = append(s.claims, s.owedAt)
	return s.owedAt
}

// startOwed is whether the run's agent start is still owed.
func (s *interleaveStore) startOwed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.owed
}

func (s *interleaveStore) RunAgentStopped(context.Context, uuid.UUID) (bool, error) {
	return s.startOwed(), nil
}

// MarkRunRevivedOwingStart is the claim of a revive that starts the agent, which leaves the start owed.
func (s *interleaveStore) MarkRunRevivedOwingStart(ctx context.Context, id uuid.UUID, from types.LostReason, ended *store.EndedKept, limit int, expected store.PolicySlot) (time.Time, bool, error) {
	var ok bool
	var err error
	if expected.Generation > 0 {
		ok, err = s.MarkRunRevivedAtSlot(ctx, id, from, ended, limit, true, expected)
	} else {
		ok, err = s.MarkRunRevived(ctx, id, from, ended, limit, true)
	}
	if !ok || err != nil {
		return time.Time{}, false, err
	}
	return s.owe(), true, nil
}

// ClearRunAgentStartOwed clears the mark only on the claim's own stamp, on a live RUNNING run, while gen
// is the newest generation (0 binds nothing), as the PG write does.
func (s *interleaveStore) ClearRunAgentStartOwed(ctx context.Context, id uuid.UUID, gen int, owedAt time.Time) error {
	s.mu.Lock()
	s.clearCalls++
	fail := s.failClears > 0
	if fail {
		s.failClears--
	}
	s.mu.Unlock()
	if fail {
		return errors.New("clear run agent start owed: connection reset")
	}
	run, err := s.GetRun(ctx, id)
	if err != nil {
		return err
	}
	gs := s.readbackStore.generationStore
	gs.gmu.Lock()
	newest := store.NewestPolicyGeneration(gs.state)
	gs.gmu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.owed && s.owedAt.Equal(owedAt) && run.State == types.RunRunning && run.LostAt == nil && (gen == 0 || newest == gen) {
		s.owed = false
		s.cleared = append(s.cleared, owedAt)
	}
	return nil
}

// RestoreRunLostFateAtGeneration writes the lost row's fate back, whose stop mark replaces the owed start.
func (s *interleaveStore) RestoreRunLostFateAtGeneration(ctx context.Context, id uuid.UUID, gen int, f store.RunLostFate) (bool, error) {
	applied, err := s.readbackStore.RestoreRunLostFateAtGeneration(ctx, id, gen, f)
	if applied {
		s.mu.Lock()
		s.owed = false
		s.mu.Unlock()
	}
	return applied, err
}

// MarkRunRevivedAtSlot is the claim bound to the policy slot the revive read, as the PG claim's UPDATE
// compares it: a moved slot claims nothing.
func (s *interleaveStore) MarkRunRevivedAtSlot(ctx context.Context, id uuid.UUID, from types.LostReason, ended *store.EndedKept, limit int, startsAgent bool, expected store.PolicySlot) (bool, error) {
	gs := s.readbackStore.generationStore
	gs.gmu.Lock()
	defer gs.gmu.Unlock()
	if store.NewestPolicySlot(gs.state) != expected {
		return false, nil
	}
	return s.MarkRunRevived(ctx, id, from, ended, limit, startsAgent)
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

// storedConfig is the run's stored config as every later revive opens it.
func (f *fenceFixture) storedConfig() []byte {
	f.rs.mu.Lock()
	defer f.rs.mu.Unlock()
	return slices.Clone(f.rs.cfg)
}

// An edit on A whose lock is lost after its advance stored the edited config abandons its generation
// and stores back, byte for byte, the source config it read before it changed anything: not the edited
// config, and not nothing.
func TestReviveFence_AbandonStoresBackTheExactSourceConfig(t *testing.T) {
	f := newFenceFixture(t)
	source := f.storedConfig()
	f.is.afterAdvance = func(int) {
		if bytes.Equal(f.storedConfig(), source) {
			t.Error("the advance did not store the edit's config")
		}
		loseLock(f.srv)
	}
	w := f.editEgress(t, `{"remove_hosts":["`+fenceHost+`"],"deny_hosts":["`+fenceHost+`"]}`)
	if w.Code != http.StatusBadGateway || errorReason(w) != reasonReviveProxyKeptCurrent {
		t.Fatalf("edit: %d %s, want 502 %s", w.Code, w.Body.String(), reasonReviveProxyKeptCurrent)
	}
	if len(f.rr.replaced) != 0 {
		t.Fatal("the edit issued a replace after its lock was lost")
	}
	if got := f.storedConfig(); !bytes.Equal(got, source) {
		t.Errorf("stored config after the abandon:\n%s\nwant the source config:\n%s", got, source)
	}
	if st := f.policyState(t); !st.Generations[len(st.Generations)-1].Abandoned || st.CurrentGeneration() != 1 {
		t.Errorf("policy state %+v, want the edit's generation abandoned and generation 1 current", st)
	}
}

// A revive whose lock is lost while its replace runs, the replace answering that the old proxy may be
// gone, with no newer revive: its lost mark is fenced on its own generation, which is still the newest,
// so the run is lost with its proxy stopped, as before the fence.
func TestReviveFence_LostLockMidReplaceWithNoNewerReviveIsLost(t *testing.T) {
	f := newFenceFixture(t)
	stops := f.lr.proxyStopCount()
	f.rr.onReplace = func([]byte) {
		loseLock(f.srv)
		f.rr.mu.Lock()
		f.rr.replaceErr = runner.ErrProxyReplaceFailed
		f.rr.mu.Unlock()
	}
	a := f.reviveOnA(t)
	if a.Code != http.StatusBadGateway || errorReason(a) != reasonReviveProxyReplaceFailedLost {
		t.Fatalf("A: %d %s, want 502 %s", a.Code, a.Body.String(), reasonReviveProxyReplaceFailedLost)
	}
	if lostAt, reason := f.st.lost(); lostAt == nil || reason != types.LostOutage {
		t.Errorf("lost = %v %q, want lost (outage): the fence must match the revive's own generation", lostAt, reason)
	}
	if f.lr.proxyStopCount() == stops {
		t.Error("the lost run's proxy was not stopped")
	}
}

// Containment of A's failed start, paused after its generation read while B narrows the run and
// confirms on the second replica (A's lock lost, so B took it), stops nothing and loses nothing: its lost
// mark is fenced on A's generation, which B moved on. B's start and narrowed config stand.
func TestReviveFence_ContainmentAcrossANewerConfirmedEditStopsNothing(t *testing.T) {
	f := newFenceFixture(t)
	if code := f.revive(t); code != http.StatusOK {
		t.Fatalf("A's revive: %d, want 200", code)
	}
	f.srv.liveRevived.Delete(f.run.ID)
	gen := f.active(t).Generation
	f.rb.pmu.Lock()
	f.rb.starts[f.run.ID][gen].State = types.ProxyStartFailed
	f.rb.pmu.Unlock()
	stops := f.lr.proxyStopCount()
	var b *httptest.ResponseRecorder
	old := proxyContainPaused
	proxyContainPaused = func() {
		proxyContainPaused = func() {}
		b = f.editOnB(t)
	}
	t.Cleanup(func() { proxyContainPaused = old })
	c := f.srv.containProxyStart(context.Background(), f.run.ID, gen)
	if b == nil || b.Code != http.StatusOK {
		t.Fatalf("B never ran or did not confirm: %v", b)
	}
	if c.Contained {
		t.Errorf("containment of start %d = %+v, want not contained: it stopped nothing", gen, c)
	}
	if f.lr.proxyStopCount() != stops {
		t.Error("A's containment stopped the proxy of B's newer, confirmed start")
	}
	if lostAt, _ := f.st.lost(); lostAt != nil {
		t.Errorf("lost at %v: A's containment lost a run B had taken over", lostAt)
	}
	if p := f.active(t); p.Generation <= gen || p.State != types.ProxyStartEnforced {
		t.Errorf("start = %d %s, want B's newer start enforced", p.Generation, p.State)
	}
	denied(t, "the stored config", f.storedPolicy(t))
}

// startingHashedRunner is the readback runner that can also start a stopped agent.
type startingHashedRunner struct {
	*hashedReviveRunner
	smu    sync.Mutex
	starts int
}

func (r *startingHashedRunner) StartSandbox(context.Context, string) error {
	r.smu.Lock()
	defer r.smu.Unlock()
	r.starts++
	return nil
}

func (r *startingHashedRunner) started() int {
	r.smu.Lock()
	defer r.smu.Unlock()
	return r.starts
}

// A run lost to a reboot, or ended and kept, whose revive loses its lock after its advance with no newer
// revive: nothing is replaced or started, and the run gets back exactly the lost mark the claim cleared
// (lost_at, lost_reason, agent_stopped_at). So the next revive takes the lost path and starts the agent,
// where one that found the run live would leave its stopped agent unstarted.
func TestReviveFence_LostLockAfterTheAdvancePutsBackALostRunsFate(t *testing.T) {
	for _, reason := range []types.LostReason{types.LostReboot, types.LostEnded} {
		t.Run(string(reason), func(t *testing.T) {
			f := newFenceFixture(t)
			sr := &startingHashedRunner{hashedReviveRunner: f.hr}
			f.srv.cfg.Runner = sr
			lostAt := f.now.Add(-time.Minute)
			f.st.mu.Lock()
			f.st.run.LostAt, f.st.run.LostReason = &lostAt, reason
			f.st.run.Interactive = true
			if reason == types.LostEnded {
				end := f.now.Add(48 * time.Hour) // extended past its end, so it may be revived
				f.st.run.EndsAt = &end
			}
			f.st.agentStopped = true
			f.st.mu.Unlock()
			f.rb.generationStore.agentStoppedAt = lostAt.Add(time.Second)
			f.run = f.st.run
			ctx := context.Background()
			before, err := f.rb.RunLostFate(ctx, f.run.ID)
			if err != nil || before.LostAt == nil || before.AgentStoppedAt == nil {
				t.Fatalf("fate before = %+v, %v; want lost with its agent stopped", before, err)
			}
			stops := f.lr.proxyStopCount()
			f.is.afterAdvance = func(int) { loseLock(f.srv) }
			a := f.reviveOnA(t)
			if a.Code != http.StatusConflict || errorReason(a) != reasonReviveRunChanged {
				t.Fatalf("A: %d %s, want 409 %s", a.Code, a.Body.String(), reasonReviveRunChanged)
			}
			if len(f.rr.replaced) != 0 || sr.started() != 0 || f.lr.proxyStopCount() != stops {
				t.Fatalf("replaces %d, agent starts %d, proxy stops %d (was %d): want none", len(f.rr.replaced), sr.started(), f.lr.proxyStopCount(), stops)
			}
			if after, err := f.rb.RunLostFate(ctx, f.run.ID); err != nil || !reflect.DeepEqual(after, before) {
				t.Fatalf("fate after = %+v, %v; want exactly %+v", after, err, before)
			}
			f.srv.locks.held = nil
			if b := f.reviveOnA(t); b.Code != http.StatusOK {
				t.Fatalf("the next revive: %d %s, want 200", b.Code, b.Body.String())
			}
			if sr.started() != 1 {
				t.Errorf("agent starts = %d, want 1: the next revive must take the lost path", sr.started())
			}
		})
	}
}

// An advance that commits but answers an error (its acknowledgement lost) is read back: the newest
// generation is this attempt's (its fresh token), so it is recorded and abandoned with the source config
// stored back, and the run keeps its current proxy.
func TestReviveFence_LostCommitAcknowledgementIsAbandoned(t *testing.T) {
	f := newFenceFixture(t)
	source := f.storedConfig()
	f.is.dropAck = true
	a := f.reviveOnA(t)
	if a.Code != http.StatusBadGateway || errorReason(a) != reasonReviveProxyKeptCurrent {
		t.Fatalf("A: %d %s, want 502 %s", a.Code, a.Body.String(), reasonReviveProxyKeptCurrent)
	}
	if len(f.rr.replaced) != 0 {
		t.Fatal("A issued a replace after its advance answered an error")
	}
	st := f.policyState(t)
	last := st.Generations[len(st.Generations)-1]
	if last.Generation != 2 || !last.Abandoned || st.CurrentGeneration() != 1 {
		t.Errorf("policy state %+v, want generation 2 written and abandoned, 1 current", st)
	}
	if got := f.storedConfig(); !bytes.Equal(got, source) {
		t.Error("the source config was not stored back")
	}
	if rows := f.audit.eventsFor(f.run.ID, "run.policy.generation"); len(rows) == 0 {
		t.Error("the written generation has no run.policy.generation row")
	}
}

// The same lost acknowledgement, when B advances and confirms before A reads back: the newest
// generation is B's, not A's, so A undoes nothing and B's generation, start and config stand.
func TestReviveFence_LostCommitAcknowledgementNeverUndoesANewerRevive(t *testing.T) {
	f := newFenceFixture(t)
	var b *httptest.ResponseRecorder
	f.is.dropAck = true
	f.is.afterDroppedAck = func(int) { b = f.editOnB(t) }
	a := f.reviveOnA(t)
	if b == nil || b.Code != http.StatusOK {
		t.Fatalf("B never ran or did not confirm: %v", b)
	}
	if a.Code != http.StatusBadGateway || errorReason(a) != reasonReviveProxyKeptCurrent {
		t.Fatalf("A: %d %s, want 502 %s", a.Code, a.Body.String(), reasonReviveProxyKeptCurrent)
	}
	if n := len(f.rr.replaced); n != 1 {
		t.Fatalf("replaces = %d, want B's one", n)
	}
	st := f.policyState(t)
	if last := st.Generations[len(st.Generations)-1]; last.Abandoned || st.CurrentGeneration() != last.Generation {
		t.Errorf("policy state %+v, want B's newest generation current", st)
	}
	if p := f.active(t); p.State != types.ProxyStartEnforced {
		t.Errorf("start = %d %s, want B's enforced", p.Generation, p.State)
	}
	denied(t, "the stored config", f.storedPolicy(t))
}

// A caller that disconnects while a revive's replace runs does not stop it: the consistency work
// outlives the request, and only the lock's loss ends it (see the Postgres pin,
// TestWhileLockHeld_LockLostAfterTheCallerEndedCancelsTheReplace).
func TestWhileLockHeld_CallerCancelAloneDoesNotEndTheReplace(t *testing.T) {
	hctx, release, err := db.NewLocalLocker().Lock(context.Background(), db.NewLockKey(db.RunOpLockClass, uuid.NewString()), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	caller, cancelCaller := context.WithCancel(hctx)
	replaceCtx, end := whileLockHeld(context.WithoutCancel(caller), caller)
	defer end()
	cancelCaller()
	select {
	case <-replaceCtx.Done():
		t.Fatalf("the caller's cancel ended the replace: %v", context.Cause(replaceCtx))
	case <-time.After(100 * time.Millisecond):
	}
	end()
	if !errors.Is(context.Cause(replaceCtx), context.Canceled) {
		t.Errorf("after stop: cause %v, want context.Canceled", context.Cause(replaceCtx))
	}
}

// A stale revive (A) of a run lost to a reboot reads its policy slot and source config, then pauses
// before its claim. B, on the second replica, claims, advances, fails its replace and loses the run again
// (reboot). A's claim is bound to the slot it read, which B moved, so it claims nothing: B's lost mark
// stands exactly, and A starts and stops nothing. Unbound, A's claim would clear B's mark and its
// advance would then miss, leaving the run looking live with its agent stopped.
func TestReviveFence_StaleClaimAfterANewerReviveReLostTheRunClearsNothing(t *testing.T) {
	f := newFenceFixture(t)
	sr := &startingHashedRunner{hashedReviveRunner: f.hr}
	f.srv.cfg.Runner, f.b.cfg.Runner = sr, sr
	lostAt := f.now.Add(-time.Minute)
	f.st.mu.Lock()
	f.st.run.LostAt, f.st.run.LostReason = &lostAt, types.LostReboot
	f.st.run.Interactive = true
	f.st.mu.Unlock()
	f.run = f.st.run
	ctx := context.Background()
	var b *httptest.ResponseRecorder
	var afterB store.RunLostFate
	var stopsAfterB int
	f.is.beforeSourceRead = func() {
		f.rr.mu.Lock()
		f.rr.replaceErr = runner.ErrProxyReplaceFailed
		f.rr.mu.Unlock()
		b = do(t, f.b, http.MethodPost, "/api/v1/runs/"+f.run.ID.String()+"/revive", adminToken, "")
		var err error
		if afterB, err = f.rb.RunLostFate(ctx, f.run.ID); err != nil {
			t.Error(err)
		}
		stopsAfterB = f.lr.proxyStopCount()
	}
	a := f.reviveOnA(t)
	if b == nil || b.Code != http.StatusBadGateway || errorReason(b) != reasonReviveProxyReplaceFailedLost {
		t.Fatalf("B: %v, want 502 %s", b, reasonReviveProxyReplaceFailedLost)
	}
	if afterB.LostAt == nil || afterB.LostReason != types.LostReboot {
		t.Fatalf("after B: %+v, want lost again (reboot)", afterB)
	}
	if a.Code != http.StatusConflict || errorReason(a) != reasonReviveRunChanged {
		t.Errorf("A: %d %s, want 409 %s", a.Code, a.Body.String(), reasonReviveRunChanged)
	}
	if got, err := f.rb.RunLostFate(ctx, f.run.ID); err != nil || !reflect.DeepEqual(got, afterB) {
		t.Errorf("fate after A = %+v, %v; want B's relose exactly %+v", got, err, afterB)
	}
	if sr.started() != 0 || f.lr.proxyStopCount() != stopsAfterB {
		t.Errorf("agent starts %d, proxy stops %d (after B %d): A must start and stop nothing", sr.started(), f.lr.proxyStopCount(), stopsAfterB)
	}
}

// rebootLost makes the fixture's run one lost to a reboot, on runners that can start its agent again.
func (f *fenceFixture) rebootLost() *startingHashedRunner {
	sr := &startingHashedRunner{hashedReviveRunner: f.hr}
	f.srv.cfg.Runner, f.b.cfg.Runner = sr, sr
	lostAt := f.now.Add(-time.Minute)
	f.st.mu.Lock()
	f.st.run.LostAt, f.st.run.LostReason = &lostAt, types.LostReboot
	f.st.run.Interactive = true
	f.st.mu.Unlock()
	f.run = f.st.run
	return sr
}

// A claims a run lost to a reboot, which clears its lost mark and owes the agent's start, and pauses before
// its advance. A second revive then reads the run live. From the run's page it sees the owed start, starts
// the agent behind its own proxy and clears the mark on the stamp of its own claim; A, whose slot that
// moved, answers kept-current and starts nothing. An egress edit cannot start an agent, so it is refused
// 409 with the mark still set, and A, resuming, starts the agent itself.
func TestRevive_ANewerLiveReviveAfterAStrandedClaimStartsTheAgent(t *testing.T) {
	t.Run("a revive from the run's page", func(t *testing.T) {
		f := newFenceFixture(t)
		sr := f.rebootLost()
		var b *httptest.ResponseRecorder
		var owedAtB bool
		f.is.beforeRetire = func() {
			owedAtB = f.is.startOwed()
			b = do(t, f.b, http.MethodPost, "/api/v1/runs/"+f.run.ID.String()+"/revive", adminToken, "")
		}
		a := f.reviveOnA(t)
		if !owedAtB {
			t.Fatal("A's claim left no owed start for the second revive to read")
		}
		if b == nil || b.Code != http.StatusOK || !strings.Contains(b.Body.String(), `"agent_started":true`) {
			t.Fatalf("B: %v, want 200 with the agent started", b)
		}
		if a.Code != http.StatusBadGateway || errorReason(a) != reasonReviveProxyKeptCurrent {
			t.Errorf("A: %d %s, want 502 %s", a.Code, a.Body.String(), reasonReviveProxyKeptCurrent)
		}
		if sr.started() != 1 {
			t.Errorf("StartSandbox calls = %d, want 1: B's", sr.started())
		}
		if f.is.startOwed() {
			t.Error("the agent start is still owed after B started it")
		}
		if len(f.is.claims) != 2 || !slices.Equal(f.is.cleared, f.is.claims[1:]) {
			t.Errorf("claims %v, clears %v; want B's clear on B's own stamp alone", f.is.claims, f.is.cleared)
		}
	})
	t.Run("an egress edit", func(t *testing.T) {
		f := newFenceFixture(t)
		sr := f.rebootLost()
		var b *httptest.ResponseRecorder
		var owedAfterB bool
		var replacedByB int
		f.is.beforeRetire = func() {
			b = f.editOnB(t)
			owedAfterB, replacedByB = f.is.startOwed(), len(f.rr.replaced)
		}
		a := f.reviveOnA(t)
		if b == nil || b.Code != http.StatusConflict || errorReason(b) != reasonReviveBulkCannotStartAgent {
			t.Fatalf("B: %v, want 409 %s", b, reasonReviveBulkCannotStartAgent)
		}
		if !owedAfterB || replacedByB != 0 {
			t.Errorf("after B: start owed %v, replaces %d; want the mark kept and no replace by B", owedAfterB, replacedByB)
		}
		if a.Code != http.StatusOK || sr.started() != 1 || f.is.startOwed() {
			t.Errorf("A: %d, agent starts %d, still owed %v; want A to finish, start the agent and clear its mark", a.Code, sr.started(), f.is.startOwed())
		}
	})
}

// A revive that read the run live with no start owed (an edit or a restart, which cannot start an agent)
// meets a start-owing claim before its own claim lands: its claim is refused and the owed mark stays, so
// nothing is replaced and the agent is never left unstarted behind a proxy the revive replaced.
func TestRevive_AStaleReviveThatStartsNoAgentNeverErasesAnOwedStart(t *testing.T) {
	f := newFenceFixture(t)
	sr := &startingHashedRunner{hashedReviveRunner: f.hr}
	f.srv.cfg.Runner = sr
	f.is.beforeSourceRead = func() { f.is.owe() }
	a := f.reviveOnA(t)
	if a.Code != http.StatusConflict || errorReason(a) != reasonReviveRunChanged {
		t.Fatalf("A: %d %s, want 409 %s", a.Code, a.Body.String(), reasonReviveRunChanged)
	}
	if !f.is.startOwed() || len(f.rr.replaced) != 0 || sr.started() != 0 {
		t.Errorf("start owed %v, replaces %d, agent starts %d; want the mark kept and nothing replaced or started", f.is.startOwed(), len(f.rr.replaced), sr.started())
	}
}

// A live run whose agent start is owed (its agent_stopped_at set) reads live to every other reader of that
// column: it is not kept, its agent is not stopped, its desktop pause is not over, and it is still a run the
// lease may lose. Each of them checks the lost mark before the stop mark.
func TestStartOwedLiveRunReadsLiveToEveryOtherReader(t *testing.T) {
	run := types.AgentRun{ID: uuid.New(), State: types.RunRunning, Interactive: true, SandboxRef: "wardyn-agent-x"}
	srv := New(Config{BaseCtx: testBaseCtx(t), Store: agentStoppedStore{runFilesStore{run: run}, true}, Runner: &sshFakeRunner{}, Audit: &recRecorder{}})
	ctx := context.Background()
	if runIsKept(run) {
		t.Error("runIsKept: a live run with its start owed reads kept")
	}
	if srv.keptAgentStopped(ctx, run) {
		t.Error("keptAgentStopped: the run's files read as an agent stopped on purpose")
	}
	if srv.desktopPauseOver(ctx, run) {
		t.Error("desktopPauseOver: the run's desktop pause reads over")
	}
	if !srv.lostRunKeepable(run, time.Now()) {
		t.Error("lostRunKeepable: the live run is not one the lease may lose")
	}
}

// The clear of the owed start takes a bound of its own and one retry: a first clear that fails and a second
// that lands leaves the mark cleared, and two that fail leave it for the next live revive.
func TestRevive_AFailedClearOfTheOwedStartIsRetriedOnce(t *testing.T) {
	for _, tc := range []struct {
		name       string
		fails      int
		wantOwed   bool
		wantClears int
	}{
		{"the retry lands", 1, false, 2},
		{"both tries fail", 2, true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFenceFixture(t)
			sr := f.rebootLost()
			f.is.failClears = tc.fails
			a := f.reviveOnA(t)
			if a.Code != http.StatusOK || sr.started() != 1 {
				t.Fatalf("A: %d %s, agent starts %d; want 200 and one start", a.Code, a.Body.String(), sr.started())
			}
			if f.is.startOwed() != tc.wantOwed || f.is.clearCalls != tc.wantClears {
				t.Errorf("start owed %v after %d clear calls; want owed %v after %d", f.is.startOwed(), f.is.clearCalls, tc.wantOwed, tc.wantClears)
			}
		})
	}
}
