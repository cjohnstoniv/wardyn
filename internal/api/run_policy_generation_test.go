// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// generationStore is reviveStore holding the run's policy generations as agent_runs does.
type generationStore struct {
	*reviveStore
	gmu   sync.Mutex
	state types.RunPolicyState
	// failAdvance is how many more AdvanceRunPolicyGeneration calls fail, as a database blip would.
	failAdvance int
	// agentStoppedAt is the agent_stopped_at RunLostFate answers while the run's agent is recorded stopped.
	agentStoppedAt time.Time
	// abandonErr makes AbandonRunPolicyGeneration fail without writing, as a database blip would; onAbandonErr
	// runs once before that failure answers, outside the store's lock (a newer revive landing first).
	abandonErr   error
	onAbandonErr func()
	// beforeAbandon and afterAbandon each run once, outside the store's lock, around the first Noted
	// abandon: before it reads the generations (a newer revive landing first), and after it has (a newer
	// revive landing before the caller acts on the answer).
	beforeAbandon, afterAbandon func()
	// recorded is the run's audit log as the recorded-policy read sees it (the dispatch envelope); readErr fails that read.
	recorded []types.AuditEvent
	readErr  error
}

func (s *generationStore) QueryAuditEvents(_ context.Context, runID uuid.UUID, _ int) ([]types.AuditEvent, error) {
	if s.readErr != nil {
		return nil, s.readErr
	}
	var out []types.AuditEvent
	for _, ev := range s.recorded {
		if ev.RunID != nil && *ev.RunID == runID {
			out = append(out, ev)
		}
	}
	return out, nil
}

var _ store.RunPolicyGenerations = (*generationStore)(nil)

// AdvanceRunPolicyGeneration lands, with its config, only on a live run whose newest generation is still
// expected, as the PG write's predicate does; a config write that fails leaves the generations as they were.
func (s *generationStore) AdvanceRunPolicyGeneration(ctx context.Context, id uuid.UUID, expected store.PolicySlot, digest string, retired []string, fresh string, sealed []byte) (int, error) {
	s.gmu.Lock()
	defer s.gmu.Unlock()
	if s.failAdvance > 0 {
		s.failAdvance--
		return 0, errors.New("write policy generation: connection reset")
	}
	run, err := s.reviveStore.GetRun(ctx, id)
	if err != nil {
		return 0, err
	}
	if run.State != types.RunRunning || run.LostAt != nil || store.NewestPolicySlot(s.state) != expected {
		return 0, store.ErrRunPolicyMoved
	}
	if sealed != nil {
		if err := s.reviveStore.PutRunProxyConfig(ctx, id, sealed); err != nil {
			return 0, err
		}
	}
	var gen int
	s.state, gen = s.state.Advance(digest, retired, fresh)
	return gen, nil
}

// AbandonRunPolicyGeneration abandons gen, restoring its config, only while gen holds the newest slot
// and is not abandoned yet, as the PG write does.
func (s *generationStore) AbandonRunPolicyGeneration(ctx context.Context, id uuid.UUID, gen int, restore []byte) error {
	s.gmu.Lock()
	failure, hook := s.abandonErr, s.onAbandonErr
	s.onAbandonErr = nil
	s.gmu.Unlock()
	if failure != nil {
		if hook != nil {
			hook()
		}
		return failure
	}
	s.gmu.Lock()
	defer s.gmu.Unlock()
	n := len(s.state.Generations)
	if n == 0 || s.state.Generations[n-1].Generation != gen || s.state.Generations[n-1].Abandoned {
		return nil
	}
	if restore != nil {
		if err := s.reviveStore.PutRunProxyConfig(ctx, id, restore); err != nil {
			return err
		}
	}
	s.state = s.state.Abandon(gen)
	return nil
}

var _ store.RunPolicyAbandonNoted = (*generationStore)(nil)

// AbandonRunPolicyGenerationNoted is AbandonRunPolicyGeneration that also reports a no-op for a gen with a
// newer slot after it: the generation then current and the newest number, as the PG write reads them.
func (s *generationStore) AbandonRunPolicyGenerationNoted(ctx context.Context, id uuid.UUID, gen int, restore []byte) (*store.PolicyAbandonNoop, error) {
	s.gmu.Lock()
	before, after := s.beforeAbandon, s.afterAbandon
	s.beforeAbandon, s.afterAbandon = nil, nil
	s.gmu.Unlock()
	if before != nil {
		before()
	}
	s.gmu.Lock()
	var noop *store.PolicyAbandonNoop
	if n := len(s.state.Generations); n > 0 && s.state.Generations[n-1].Generation > gen {
		noop = &store.PolicyAbandonNoop{Current: s.state.CurrentGeneration(), Newest: s.state.Generations[n-1].Generation}
	}
	s.gmu.Unlock()
	err := s.AbandonRunPolicyGeneration(ctx, id, gen, restore)
	if after != nil {
		after()
	}
	return noop, err
}

// MarkRunLostAtGeneration marks the run lost only while gen is its newest generation, as the PG write's
// predicate does.
func (s *generationStore) MarkRunLostAtGeneration(ctx context.Context, id uuid.UUID, gen int, reason types.LostReason, now time.Time, tokenLife time.Duration) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err // a write on a context that ran out fails, as the database call would
	}
	s.gmu.Lock()
	defer s.gmu.Unlock()
	if store.NewestPolicyGeneration(s.state) != gen {
		return false, nil
	}
	return s.reviveStore.MarkRunLost(ctx, id, reason, now, tokenLife)
}

// RunLostFate reads the run's lost mark, its agent stop at agentStoppedAt.
func (s *generationStore) RunLostFate(context.Context, uuid.UUID) (store.RunLostFate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := store.RunLostFate{LostReason: s.run.LostReason}
	if s.run.LostAt != nil {
		at := *s.run.LostAt
		f.LostAt = &at
	}
	if s.agentStopped {
		at := s.agentStoppedAt
		f.AgentStoppedAt = &at
	}
	return f, nil
}

// RestoreRunLostFateAtGeneration puts fate back only while gen is the newest generation and the run is
// RUNNING and not lost, as the PG write's predicate does.
func (s *generationStore) RestoreRunLostFateAtGeneration(_ context.Context, _ uuid.UUID, gen int, f store.RunLostFate) (bool, error) {
	s.gmu.Lock()
	defer s.gmu.Unlock()
	if store.NewestPolicyGeneration(s.state) != gen || f.LostAt == nil {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reviveStore.state != types.RunRunning || s.run.LostAt != nil {
		return false, nil
	}
	at := *f.LostAt
	s.run.LostAt, s.run.LostReason = &at, f.LostReason
	s.agentStopped = f.AgentStoppedAt != nil
	if f.AgentStoppedAt != nil {
		s.agentStoppedAt = *f.AgentStoppedAt
	}
	return true, nil
}

// TouchRun is the idle-clock bump a decision makes; the embedded store.Store is nil.
func (s *generationStore) TouchRun(context.Context, uuid.UUID) error { return nil }

func (s *generationStore) RunPolicyState(ctx context.Context, id uuid.UUID) (types.RunPolicyState, bool, error) {
	st, err := s.lookup(ctx, id)
	return st, true, err
}

// lookup is store.RunPolicyStateOf over the fake: the generations plus the live token lineage.
func (s *generationStore) lookup(context.Context, uuid.UUID) (types.RunPolicyState, error) {
	s.gmu.Lock()
	st := s.state
	s.gmu.Unlock()
	s.mu.Lock()
	st.LiveTokenIDs = slices.DeleteFunc(slices.Clone(s.jtis), func(j string) bool { return j == "" })
	s.mu.Unlock()
	return st, nil
}

// newGenerationFixture is newReviveFixture whose run was dispatched under d1, on a real token, with
// the policy stamp in front of the audit recorder as cmd/wardynd puts it. It returns the old token.
func newGenerationFixture(t *testing.T) (*reviveFixture, *generationStore, string) {
	t.Helper()
	f := newReviveFixture(t)
	gs := &generationStore{reviveStore: f.rs, state: types.RunPolicyState{Digest: "d1"}}
	f.srv.cfg.Store = gs
	f.srv.cfg.Audit = audit.PolicyStampRecorder{Inner: f.audit, Lookup: gs.lookup}
	id, err := f.srv.cfg.Identity.MintRunIdentity(context.Background(), f.run.ID, f.run.CreatedBy, f.run.CreatedBy, internalAudience, false)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := runner.BuildProxyConfig(f.run.ID, runner.ProxyConfig{
		RunToken:        id.Token,
		ControlPlaneURL: "http://127.0.0.1:8081",
		Policy:          types.RunPolicySpec{AllowedDomains: []string{"api.openai.com", "api.anthropic.com"}},
		MITMCACertPEM:   "ca-cert",
		MITMCAKeyPEM:    "ca-key",
	}, runner.ProxyListenPort)
	if err != nil {
		t.Fatal(err)
	}
	f.rs.cfg = cfg
	return f, gs, id.Token
}

// postGenerationDecision sends a proxy decision where the proxy sends it: the internal listener. The
// fixture's hop is TLS, so the console listener answers every /api/v1/internal/ path with a 404.
func postGenerationDecision(t *testing.T, f *reviveFixture, token, host string) {
	t.Helper()
	body, _ := json.Marshal(egress.DecisionLog{Decision: egress.Allow, Request: egress.Request{RunID: f.run.ID, Host: host, Port: 443}, RuleSource: "policy:allowlist"})
	internal := func(tb testing.TB, _ http.Handler) http.Handler { return panicFails(tb, f.srv.InternalHandler()) }
	if w := doVia(t, internal, f.srv, http.MethodPost, "/api/v1/internal/decisions", token, string(body)); w.Code >= 300 {
		t.Fatalf("decision for %s: code %d %s", host, w.Code, w.Body.String())
	}
}

func generationStampOf(t *testing.T, f *reviveFixture, action, target string) (string, int) {
	t.Helper()
	for _, ev := range f.audit.eventsFor(f.run.ID, action) {
		if target != "" && ev.Target != target {
			continue
		}
		var d struct {
			Digest     string `json:"policy_digest"`
			Generation int    `json:"policy_generation"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatal(err)
		}
		return d.Digest, d.Generation
	}
	t.Fatalf("no %s row for %q", action, target)
	return "", 0
}

// Tighten, revive, act: the owner's profile now denies two hosts, so the revive starts generation 2
// under the tightened policy's digest and records that policy as run.policy.generation, a row whose
// data reads back to the same digest. A decision the new proxy makes on its fresh token carries
// generation 2.
func TestRevive_TightenThenActionStampsTheNewGeneration(t *testing.T) {
	f, gs, _ := newGenerationFixture(t)
	if code := f.revive(t); code != http.StatusOK {
		t.Fatalf("revive: code %d, want 200", code)
	}
	cfg := f.newConfig(t)
	d2 := auditablePolicy(cfg.Policy).Digest()
	if d2 == "d1" || gs.state.Digest != d2 {
		t.Fatalf("the run's digest after the revive = %q, want the tightened policy's %q", gs.state.Digest, d2)
	}
	gens := f.audit.eventsFor(f.run.ID, "run.policy.generation")
	if len(gens) != 1 {
		t.Fatalf("run.policy.generation rows = %d, want 1", len(gens))
	}
	var rec struct {
		Generation int                 `json:"generation"`
		Policy     types.RunPolicySpec `json:"policy"`
	}
	if err := json.Unmarshal(gens[0].Data, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Generation != 2 || rec.Policy.Digest() != d2 {
		t.Errorf("run.policy.generation: generation %d, recomputed digest %q; want 2 and %q", rec.Generation, rec.Policy.Digest(), d2)
	}
	if d, g := generationStampOf(t, f, "run.policy.generation", ""); d != d2 || g != 2 {
		t.Errorf("the generation row is stamped %q/%d, want its own generation %q/2", d, g, d2)
	}
	postGenerationDecision(t, f, cfg.RunToken, "api.anthropic.com")
	if d, g := generationStampOf(t, f, "egress.allow", "api.anthropic.com"); d != d2 || g != 2 {
		t.Errorf("the new proxy's decision is stamped %q/%d, want %q/2", d, g, d2)
	}
}

// A decision the old proxy made before the revive and delivered while the revive replaces it presents
// the old token: it keeps generation 1, and the new proxy's next decision is generation 2.
func TestRevive_DelayedPreReviveDecisionKeepsItsGeneration(t *testing.T) {
	f, _, oldToken := newGenerationFixture(t)
	f.rr.onReplace = func([]byte) { postGenerationDecision(t, f, oldToken, "late.example") }
	if code := f.revive(t); code != http.StatusOK {
		t.Fatalf("revive: code %d, want 200", code)
	}
	if d, g := generationStampOf(t, f, "egress.allow", "late.example"); d != "d1" || g != 1 {
		t.Errorf("the delayed pre-revive decision is stamped %q/%d, want d1/1", d, g)
	}
	cfg := f.newConfig(t)
	postGenerationDecision(t, f, cfg.RunToken, "api.anthropic.com")
	if _, g := generationStampOf(t, f, "egress.allow", "api.anthropic.com"); g != 2 {
		t.Errorf("the new proxy's decision is generation %d, want 2", g)
	}
}

// A revive that never touched the old proxy abandons its generation: the old proxy goes on under
// generation 1, and the control plane's rows say so.
func TestRevive_UntouchedProxyAbandonsTheGeneration(t *testing.T) {
	f, gs, _ := newGenerationFixture(t)
	f.st.run.LostAt, f.st.run.LostReason = nil, "" // a live run, whose untouched proxy keeps serving
	f.rr.replaceErr = errors.New("inspect the proxy: daemon busy")
	if res := f.restart(t); res.OK || res.Reason != reasonReviveProxyKeptCurrent {
		t.Fatalf("restart with an untouched failed replace: %+v, want reason %s", res, reasonReviveProxyKeptCurrent)
	}
	if st, _ := gs.lookup(context.Background(), f.run.ID); st.Stamp(false, "") != (types.PolicyStamp{Digest: "d1", Generation: 1}) {
		t.Errorf("after an untouched revive the current stamp is %+v, want d1 generation 1", st.Stamp(false, ""))
	}
}

// The revived config meets an operator's pinned previous-release proxy too: the generation rides the
// token, never a new config key, so a default revived config carries no key that proxy refuses.
func TestRevivedConfigLoadsOnPreviousProxy(t *testing.T) {
	f, _, _ := newGenerationFixture(t)
	if code := f.revive(t); code != http.StatusOK {
		t.Fatalf("revive: code %d, want 200", code)
	}
	f.newConfig(t)
	if k := refusedByPreviousProxy(t, f.rr.replaced[0]); k != "" {
		t.Errorf("the previous release's proxy refuses the revived config's key %q", k)
	}
}

// replacedDecisionStamp posts a decision from every proxy the revive started, on the token its config
// carries, and returns the stamps they got.
func replacedDecisionStamps(t *testing.T, f *reviveFixture, host string) [][2]any {
	t.Helper()
	var out [][2]any
	for _, raw := range f.rr.replaced {
		cfg, err := proxy.LoadConfigBytes(raw)
		if err != nil {
			t.Fatalf("a replacement config does not load: %v", err)
		}
		postGenerationDecision(t, f, cfg.RunToken, host)
		d, g := generationStampOf(t, f, "egress.allow", host)
		out = append(out, [2]any{d, g})
	}
	return out
}

// A revive whose generation write fails never leaves a new proxy's rows stamped with the old policy:
// the new proxy enforces the tightened policy, so a decision it makes must never claim d1. Once the
// store recovers, the next revive stamps the new proxy's decisions with generation 2.
func TestRevive_FailedGenerationWriteNeverStampsTheOldPolicy(t *testing.T) {
	for _, live := range []bool{false, true} {
		f, gs, _ := newGenerationFixture(t)
		attempt := func() bool {
			if live {
				return f.restart(t).OK
			}
			return f.revive(t) == http.StatusOK
		}
		if live {
			f.st.run.LostAt, f.st.run.LostReason = nil, ""
		}
		gs.failAdvance = 1
		ok := attempt()
		for _, st := range replacedDecisionStamps(t, f, "during.example") {
			if st[0] == "d1" || st[1] == 1 {
				t.Errorf("live=%v: a proxy started by the revive whose generation write failed is stamped %v/%v, the old policy", live, st[0], st[1])
			}
		}
		if ok || len(f.rr.replaced) > 0 {
			continue // the failure already shows above; a second revive of a replaced proxy proves nothing more
		}
		if !attempt() {
			t.Fatalf("live=%v: the revive after the store recovered failed", live)
		}
		cfg := f.newConfig(t)
		postGenerationDecision(t, f, cfg.RunToken, "after.example")
		if d, g := generationStampOf(t, f, "egress.allow", "after.example"); d == "d1" || g != 2 {
			t.Errorf("live=%v: after recovery the new proxy's decision is stamped %q/%d, want generation 2", live, d, g)
		}
	}
}

// A revive whose new generation cannot be undone never answers that the run keeps its current proxy: that
// generation stays current, narrower than the kept proxy, so the run is lost again and its proxy stopped,
// fenced on the generation. When a newer revive took the generation over before the lost mark, the mark
// misses and the newer revive's proxy, config and generation stand untouched.
func TestRevive_AnUndoThatFailsNeverAnswersKeptCurrent(t *testing.T) {
	undoFails := errors.New("abandon policy generation: connection reset")
	cases := map[string]func(f *fenceFixture){
		"the commit's acknowledgement was lost":  func(f *fenceFixture) { f.is.dropAck = true },
		"the old proxy was never touched":        func(f *fenceFixture) { f.rr.replaceErr = errors.New("inspect the proxy: daemon busy") },
		"the proxy start could not be recorded":  func(f *fenceFixture) { f.is.failBegin = true },
		"the run's lock was lost before replace": func(f *fenceFixture) { f.is.afterAdvance = func(int) { loseLock(f.srv) } },
	}
	for name, arrange := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFenceFixture(t)
			f.is.abandonErr = undoFails
			arrange(f)
			stops := f.lr.proxyStopCount()
			a := f.reviveOnA(t)
			if errorReason(a) == reasonReviveProxyKeptCurrent {
				t.Fatalf("A: %d %s, a revive whose generation could not be undone answered kept-current", a.Code, a.Body.String())
			}
			if a.Code < 400 {
				t.Fatalf("A: %d %s, want a refusal", a.Code, a.Body.String())
			}
			if n := f.lr.proxyStopCount() - stops; n != 1 {
				t.Errorf("StopProxy calls = %d, want 1", n)
			}
			if lostAt, _ := f.st.lost(); lostAt == nil {
				t.Error("the run is not lost, though its proxy is narrower than the generation that stays current")
			}
			f.ls.mu.Lock()
			jtis := slices.Clone(f.ls.jtis)
			f.ls.mu.Unlock()
			if len(jtis) != 2 || jtis[0] == "" || jtis[1] != "" {
				t.Errorf("token lineage %v, want the fresh token alone: the old lineage must not be restored under a kept-current answer", jtis)
			}
			if st := f.policyState(t); st.CurrentGeneration() != 2 {
				t.Errorf("current generation = %d, want A's 2: it stays, and the run is contained", st.CurrentGeneration())
			}
		})
	}
	newer := map[string]func(f *fenceFixture){
		"the commit's acknowledgement was lost": func(f *fenceFixture) { f.is.dropAck = true },
		"the old proxy was never touched": func(f *fenceFixture) {
			f.rr.replaceErr = errors.New("inspect the proxy: daemon busy")
			prev := f.is.onAbandonErr
			f.is.onAbandonErr = func() { // B's replace must work
				f.rr.mu.Lock()
				f.rr.replaceErr = nil
				f.rr.mu.Unlock()
				prev()
			}
		},
		"the run's lock was lost before replace": func(f *fenceFixture) { f.is.afterAdvance = func(int) { loseLock(f.srv) } },
	}
	for name, arrange := range newer {
		t.Run("a newer revive took the generation over: "+name, func(t *testing.T) {
			f := newFenceFixture(t)
			f.is.abandonErr = undoFails
			var b *httptest.ResponseRecorder
			f.is.onAbandonErr = func() { b = f.editOnB(t) }
			arrange(f)
			stops := f.lr.proxyStopCount()
			a := f.reviveOnA(t)
			if b == nil || b.Code != http.StatusOK {
				t.Fatalf("B never ran or did not confirm: %v", b)
			}
			if errorReason(a) == reasonReviveProxyKeptCurrent {
				t.Errorf("A: %d %s, a revive whose generation could not be undone answered kept-current", a.Code, a.Body.String())
			}
			if f.lr.proxyStopCount() != stops {
				t.Error("A must stop no proxy once B owns the generation")
			}
			if lostAt, _ := f.st.lost(); lostAt != nil {
				t.Error("A's lost mark landed over B's generation")
			}
			if p := f.active(t); p.State != types.ProxyStartEnforced || p.Generation != 3 {
				t.Errorf("start = %d %s, want B's generation 3 enforced", p.Generation, p.State)
			}
			if st := f.policyState(t); st.CurrentGeneration() != 3 {
				t.Errorf("current generation = %d, want B's 3", st.CurrentGeneration())
			}
			denied(t, "the stored config", f.storedPolicy(t))
		})
	}
	t.Run("the claim's bound ran out before the undo failed", func(t *testing.T) {
		old := reviveClaimedTimeout
		reviveClaimedTimeout = 300 * time.Millisecond
		t.Cleanup(func() { reviveClaimedTimeout = old })
		f := newFenceFixture(t)
		f.rr.replaceErr = errors.New("inspect the proxy: daemon busy")
		f.is.abandonErr = undoFails
		f.is.onAbandonErr = func() { time.Sleep(450 * time.Millisecond) }
		stops := f.lr.proxyStopCount()
		a := f.reviveOnA(t)
		if errorReason(a) == reasonReviveProxyKeptCurrent || a.Code < 400 {
			t.Fatalf("A: %d %s, want a refusal that is not kept-current", a.Code, a.Body.String())
		}
		if strings.Contains(a.Body.String(), "credentials were revoked") {
			t.Errorf("A's answer claims revoked credentials: %s", a.Body.String())
		}
		if n := f.lr.proxyStopCount() - stops; n != 1 {
			t.Errorf("StopProxy calls = %d, want 1: the containment must not run on the expired bound", n)
		}
		if lostAt, _ := f.st.lost(); lostAt == nil {
			t.Error("the run is not lost: the containment ran on the claim's expired bound")
		}
	})
}

// TestReviveKept_UndoNoopContainsFencedOnNewest: A advances policy 2 and loses its lock; B advances policy 3,
// fails before touching the proxy and abandons it, so policy 2 is current again over the untouched proxy that
// never enforced it, and A's own undo changes nothing (a newer slot exists). A must not answer kept-current: it
// loses the run, fenced on the newest slot. When B's policy 3 is the current one instead, A's undo changes
// nothing and stops nothing, as before.
func TestReviveKept_UndoNoopContainsFencedOnNewest(t *testing.T) {
	for _, tc := range []struct {
		name        string
		bFails      bool
		wantLost    bool
		wantKeptCur bool
	}{
		{"the newer edit was undone too", true, true, false},
		{"the newer edit is current", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFenceFixture(t)
			var b *httptest.ResponseRecorder
			f.is.afterAdvance = func(int) {
				loseLock(f.srv)
				if tc.bFails {
					f.rr.mu.Lock()
					f.rr.replaceErr = errors.New("inspect the proxy: daemon busy")
					f.rr.mu.Unlock()
				}
				b = f.editOnB(t)
			}
			stops := f.lr.proxyStopCount()
			a := f.reviveOnA(t)
			if b == nil {
				t.Fatal("B never ran")
			}
			if kept := errorReason(a) == reasonReviveProxyKeptCurrent; kept != tc.wantKeptCur {
				t.Errorf("A: %d %s, kept-current = %v, want %v", a.Code, a.Body.String(), kept, tc.wantKeptCur)
			}
			if lostAt, _ := f.st.lost(); (lostAt != nil) != tc.wantLost {
				t.Errorf("lost = %v, want lost %v", lostAt, tc.wantLost)
			}
			wantStops := 0
			if tc.wantLost {
				wantStops = 1
			}
			if n := f.lr.proxyStopCount() - stops; n != wantStops {
				t.Errorf("StopProxy calls = %d, want %d", n, wantStops)
			}
		})
	}
}

// containedNotKept fails unless B's generation 3 stands abandoned over A's 2, A's answer is a refusal that
// is not kept-current, the run is lost with its proxy stopped once since stops, and A's generation 2 is the
// current one (nothing undid it).
func (f *fenceFixture) containedNotKept(t *testing.T, a *httptest.ResponseRecorder, stops int) {
	t.Helper()
	if st := f.policyState(t); len(st.Generations) != 3 || !st.Generations[2].Abandoned || st.Generations[1].Abandoned {
		t.Fatalf("generations %+v, want B's 3 abandoned over A's 2 (the arrangement)", st.Generations)
	}
	if errorReason(a) == reasonReviveProxyKeptCurrent || a.Code < 400 {
		t.Errorf("A: %d %s, want a refusal that is not kept-current", a.Code, a.Body.String())
	}
	if lostAt, _ := f.st.lost(); lostAt == nil {
		t.Error("the run is not lost: A's lost mark did not land on the newest slot")
	}
	if n := f.lr.proxyStopCount() - stops; n != 1 {
		t.Errorf("StopProxy calls = %d, want 1", n)
	}
	if st := f.policyState(t); st.CurrentGeneration() != 2 {
		t.Errorf("current generation = %d, want A's 2: nothing undid it", st.CurrentGeneration())
	}
}

// proxyBusy is the error B's replace fails with in the tests below: before it touches the proxy, so B undoes
// its generation and the run keeps its proxy.
const proxyBusy = "inspect the proxy: daemon busy"

// A's proxy start cannot be recorded after B advanced policy 3 and undid it: A's own undo changes nothing
// and generation 2 is current again, so A's answer carries errPolicyNotAbandoned and its lost mark is fenced
// on 3, the newest slot. A wrapper that drops the no-op answers kept-current instead.
func TestFailedStart_UndoNoopContains(t *testing.T) {
	f := newFenceFixture(t)
	f.rr.replaceErr = errors.New(proxyBusy)
	var b *httptest.ResponseRecorder
	f.is.afterAdvance = func(int) {
		b = f.editOnB(t)
		f.is.failBegin = true // B's start was recorded; A's cannot be
	}
	stops := f.lr.proxyStopCount()
	a := f.reviveOnA(t)
	if b == nil {
		t.Fatal("B never ran")
	}
	f.containedNotKept(t, a, stops)
}

// The same through reconcileLostAdvance: A's advance commits and answers an error, and B advances policy 3
// and undoes it after A read generation 2 back and before A's undo runs.
func TestLostAck_UndoNoopContains(t *testing.T) {
	f := newFenceFixture(t)
	f.rr.replaceErr = errors.New(proxyBusy)
	f.is.dropAck = true
	var b *httptest.ResponseRecorder
	f.rb.beforeAbandon = func() { b = f.editOnB(t) }
	stops := f.lr.proxyStopCount()
	a := f.reviveOnA(t)
	if b == nil {
		t.Fatal("B never ran")
	}
	f.containedNotKept(t, a, stops)
}

// B's whole advance and undo at policy 3 finishes before A reads its lost-ack advance back, so A's
// generation 2 is not the last entry: A must still find it and contain on its undo that changed nothing. A
// check on the last entry alone finds B's, takes the advance for someone else's and answers kept-current.
func TestLostAck_BCycleBeforeReadStillUndoes(t *testing.T) {
	f := newFenceFixture(t)
	f.rr.replaceErr = errors.New(proxyBusy)
	f.is.dropAck = true
	var b *httptest.ResponseRecorder
	f.is.afterDroppedAck = func(int) { b = f.editOnB(t) }
	stops := f.lr.proxyStopCount()
	a := f.reviveOnA(t)
	if b == nil {
		t.Fatal("B never ran")
	}
	f.containedNotKept(t, a, stops)
}

// A's undo changed nothing and its lost mark is fenced on the newest slot; a newer revive (4) took the run
// over between the undo and the mark, so the mark misses and nothing is stopped: no proxy, no sandbox
// stop, no end, and the run is not lost.
func TestUndoNoop_FencedMissDoesNotStop(t *testing.T) {
	f := newFenceFixture(t)
	f.rr.replaceErr = errors.New(proxyBusy)
	var c int
	f.is.afterAdvance = func(int) {
		loseLock(f.srv)
		f.editOnB(t)
		// Armed once B has run, so that it fires on A's undo and not on B's.
		f.rb.afterAbandon = func() {
			var err error
			c, err = f.rb.AdvanceRunPolicyGeneration(context.Background(), f.run.ID, store.NewestPolicySlot(f.policyState(t)), "d-c", nil, "jti-c", nil)
			if err != nil {
				t.Errorf("the newer revive's advance: %v", err)
			}
		}
	}
	stops, sandboxStops, ends := f.lr.proxyStopCount(), f.rn.stopCount(), f.rn.endCount()
	a := f.reviveOnA(t)
	if c != 4 {
		t.Fatalf("the newer revive's generation = %d, want 4 (the arrangement)", c)
	}
	if errorReason(a) == reasonReviveProxyKeptCurrent || a.Code < 400 {
		t.Errorf("A: %d %s, want a refusal that is not kept-current", a.Code, a.Body.String())
	}
	if lostAt, _ := f.st.lost(); lostAt != nil {
		t.Errorf("lost at %v: A's mark landed over the newer revive's generation", lostAt)
	}
	if n, s, e := f.lr.proxyStopCount()-stops, f.rn.stopCount()-sandboxStops, f.rn.endCount()-ends; n != 0 || s != 0 || e != 0 {
		t.Errorf("StopProxy %d, StopSandbox %d, EndSandbox %d; want none: the fenced mark missed", n, s, e)
	}
}

// A lost run whose revive loses its lock after its advance gets its lost mark back (reviveLostHoldRestore);
// when its undo changed nothing because a newer edit was undone first, nothing changes in what it does, and
// the failure row records that.
func TestReviveLostHoldRestore_RecordsTheNoop(t *testing.T) {
	for _, tc := range []struct {
		name       string
		newerEdit  bool
		wantRecord bool
	}{
		{"a newer edit came and was undone", true, true},
		{"no newer edit", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFenceFixture(t)
			sr := &startingHashedRunner{hashedReviveRunner: f.hr}
			f.srv.cfg.Runner = sr
			lostAt := f.now.Add(-time.Minute)
			f.st.mu.Lock()
			f.st.run.LostAt, f.st.run.LostReason = &lostAt, types.LostReboot
			f.st.run.Interactive = true
			f.st.agentStopped = true
			f.st.mu.Unlock()
			f.rb.generationStore.agentStoppedAt = lostAt.Add(time.Second)
			f.run = f.st.run
			f.is.afterAdvance = func(gen int) {
				loseLock(f.srv)
				if !tc.newerEdit {
					return
				}
				ctx := context.Background()
				g, err := f.rb.AdvanceRunPolicyGeneration(ctx, f.run.ID, store.PolicySlot{Generation: gen}, "d-b", nil, "jti-b", nil)
				if err == nil {
					err = f.rb.AbandonRunPolicyGeneration(ctx, f.run.ID, g, nil)
				}
				if err != nil {
					t.Errorf("the newer edit and its undo: %v", err)
				}
			}
			stops := f.lr.proxyStopCount()
			a := f.reviveOnA(t)
			if a.Code != http.StatusConflict || errorReason(a) != reasonReviveRunChanged {
				t.Fatalf("A: %d %s, want 409 %s", a.Code, a.Body.String(), reasonReviveRunChanged)
			}
			if len(f.rr.replaced) != 0 || sr.started() != 0 || f.lr.proxyStopCount() != stops {
				t.Errorf("replaces %d, agent starts %d, proxy stops %d (was %d): want none", len(f.rr.replaced), sr.started(), f.lr.proxyStopCount(), stops)
			}
			ev := f.audit.eventsFor(f.run.ID, "run.revive")
			if len(ev) != 1 || ev[0].Outcome != "failure" {
				t.Fatalf("run.revive rows = %+v, want one failure", ev)
			}
			if got := leaseAuditData(t, ev[0])["abandon_noop"] == true; got != tc.wantRecord {
				t.Errorf("abandon_noop recorded = %v, want %v", got, tc.wantRecord)
			}
		})
	}
}
