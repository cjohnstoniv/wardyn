// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sync"
	"testing"

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

func (s *generationStore) AdvanceRunPolicyGeneration(_ context.Context, _ uuid.UUID, digest string, retired []string, fresh string) (int, error) {
	s.gmu.Lock()
	defer s.gmu.Unlock()
	if s.failAdvance > 0 {
		s.failAdvance--
		return 0, errors.New("write policy generation: connection reset")
	}
	var gen int
	s.state, gen = s.state.Advance(digest, retired, fresh)
	return gen, nil
}

func (s *generationStore) AbandonRunPolicyGeneration(_ context.Context, _ uuid.UUID, gen int) error {
	s.gmu.Lock()
	defer s.gmu.Unlock()
	s.state = s.state.Abandon(gen)
	return nil
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
