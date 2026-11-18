// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
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

// An in-place Kubernetes egress edit records its proxy's policy as the next generation, and that policy
// never carries data_out: the run keeps the stricter of its resolved rule and the generation's.
func TestEffectivePolicy_GenerationKeepsTheStricterDataOut(t *testing.T) {
	const a, r, b = types.TransferAllowed, types.TransferReview, types.TransferBlocked
	for _, c := range []struct{ resolved, gen, want types.TransferRule }{
		{a, "", a}, {a, a, a}, {a, r, r}, {a, b, b},
		{r, "", r}, {r, a, r}, {r, r, r}, {r, b, b},
		{b, "", b}, {b, a, b}, {b, r, b}, {b, b, b},
		{"", "", a},
	} {
		srv, st := pvFixture(t, &capStore{})
		dispatched, edited := genViewDispatched, genViewRevived
		dispatched.DataOut, edited.DataOut = c.resolved, c.gen
		srv.cfg.Store = &genViewStore{pvStore: st, state: types.RunPolicyState{Digest: dispatched.Digest()}}
		run := st.seedRun(pvOwner, types.RunRunning)
		st.rows(run.ID, pvResolve(dispatched, false))
		if g, err := srv.advanceRunPolicyGeneration(context.Background(), run, edited, "", renewedTokens{}, "jti-edited", store.PolicySlot{Generation: 1}, nil); err != nil || g != 2 {
			t.Fatalf("advance = %d, %v; want generation 2", g, err)
		}
		if rule, found, err := srv.runDataOutRule(context.Background(), run.ID); err != nil || !found || rule != c.want {
			t.Errorf("resolved %q, generation %q: data_out = %q (found %v, err %v), want %q", c.resolved, c.gen, rule, found, err, c.want)
		}
	}
}

// dataOutGenFixture is a running run dispatched with data_out allowed, whose current policy generation is 2.
// withRow records the generation's row as a revive does (advanceRunPolicyGeneration), with the rule edited;
// without it the store advanced and the audit row did not land, which that write allows.
func dataOutGenFixture(t *testing.T, withRow bool, edited types.TransferRule) (*Server, types.AgentRun) {
	t.Helper()
	srv, st := pvFixture(t, &capStore{})
	dispatched := genViewDispatched
	dispatched.DataOut = types.TransferAllowed
	gs := &genViewStore{pvStore: st, state: types.RunPolicyState{Digest: dispatched.Digest()}}
	srv.cfg.Store = gs
	run := st.seedRun(pvOwner, types.RunRunning)
	st.rows(run.ID, pvResolve(dispatched, false))
	if withRow {
		revised := genViewRevived
		revised.DataOut = edited
		if g, err := srv.advanceRunPolicyGeneration(context.Background(), run, revised, "", renewedTokens{}, "jti-revived", store.PolicySlot{Generation: 1}, nil); err != nil || g != 2 {
			t.Fatalf("advance = %d, %v; want generation 2", g, err)
		}
		return srv, run
	}
	gs.state, _ = gs.state.Advance(genViewRevived.Digest(), nil, "jti-revived")
	if gen := gs.state.CurrentGeneration(); gen != 2 {
		t.Fatalf("current generation = %d, want 2", gen)
	}
	return srv, run
}

// A run whose current generation's row did not land has an unknown data_out rule, and the dispatch rule may
// be looser than the one it enforces: it is unreadable, never a rule. A door answers 503 data_out_unreadable,
// and a revive or an edit is refused rather than carrying "" (allowed).
func TestDataOutRule_UnreadableGenerationRowIsUnreadable(t *testing.T) {
	srv, run := dataOutGenFixture(t, false, "")
	ctx := context.Background()
	if rule, found, err := srv.runDataOutRule(ctx, run.ID); !errors.Is(err, errDataOutGenerationUnread) || found || rule != types.TransferAllowed {
		t.Errorf("data_out with no generation row = %q (found %v, err %v), want errDataOutGenerationUnread", rule, found, err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+run.ID.String()+"/output", nil)
	if !srv.refuseDataOutReader(w, r, run, "output", dataOutKeepsOutputMsg, false) || w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), reasonDataOutUnreadable) {
		t.Errorf("output with no generation row: %d %s, want 503 %s", w.Code, w.Body.String(), reasonDataOutUnreadable)
	}
	var policy types.RunPolicySpec
	re := srv.reviveCarryRecordedRules(ctx, run.ID, &policy)
	if re == nil || re.status != http.StatusServiceUnavailable || re.reason != reasonReviveConfigUnreadable || !strings.Contains(re.msg, "policy generation") {
		t.Fatalf("revive carry with no generation row = %+v, want 503 %s naming the generation", re, reasonReviveConfigUnreadable)
	}
	if policy.DataOut != "" {
		t.Errorf("refused revive still carried data_out %q", policy.DataOut)
	}
}

// genUnreadFolderStore is a computer-folder store whose run is on policy generation 2 with no generation row.
type genUnreadFolderStore struct {
	*computerFolderStore
	store.RunPolicyGenerations
}

func (genUnreadFolderStore) RunPolicyState(context.Context, uuid.UUID) (types.RunPolicyState, bool, error) {
	st, _ := types.RunPolicyState{}.Advance("d2", nil, "jti")
	return st, true, nil
}

// An ended run with a seeded both-ways folder, whose rule allows content out, and whose current generation's row
// cannot be read: every run-end path reads that as a rule it cannot read, which keeps the sandbox for the copy
// back, and never as a blocked rule, which stops the sandbox with the folder's changes owed.
func TestRunEnd_UnreadableGenerationRowKeepsTheFolderSandbox(t *testing.T) {
	s, cs, _, run := giveUpFixture(t)
	cs.dataOut = types.TransferAllowed
	s.cfg.Store = genUnreadFolderStore{computerFolderStore: cs}
	ctx := context.Background()
	if got, err := s.folderBackEligible(ctx, run.ID); !errors.Is(err, errDataOutGenerationUnread) || got {
		t.Errorf("folderBackEligible = %v, %v; want an error, not false", got, err)
	}
	if !s.endMayPass(ctx, run.ID) {
		t.Error("endMayPass = false; want true: a rule that cannot be read counts as a pass that can happen")
	}
	if folder, unreadable := s.forcedFolder(ctx, run.ID); folder != nil || !unreadable {
		t.Errorf("forcedFolder = %v, unreadable %v; want unreadable", folder, unreadable)
	}
}

// With the generation's row present the rule is that row's, stricter than the resolved one, and a revive carries it.
func TestDataOutRule_GenerationRowStillRead(t *testing.T) {
	srv, run := dataOutGenFixture(t, true, types.TransferReview)
	ctx := context.Background()
	if rule, found, err := srv.runDataOutRule(ctx, run.ID); err != nil || !found || rule != types.TransferReview {
		t.Errorf("data_out with the generation row = %q (found %v, err %v), want review, found", rule, found, err)
	}
	var policy types.RunPolicySpec
	if re := srv.reviveCarryRecordedRules(ctx, run.ID, &policy); re != nil || policy.DataOut != types.TransferReview {
		t.Errorf("revive carry = %+v, data_out %q; want no refusal and review", re, policy.DataOut)
	}
}

// A run whose audit rows were pruned has no envelope, so no rule: allowed, not found, whatever its generation.
func TestDataOutRule_PrunedRunIsAllowed(t *testing.T) {
	srv, st := pvFixture(t, &capStore{})
	gs := &genViewStore{pvStore: st}
	gs.state, _ = gs.state.Advance(genViewRevived.Digest(), nil, "jti-revived")
	srv.cfg.Store = gs
	run := st.seedRun(pvOwner, types.RunRunning)
	ctx := context.Background()
	if rule, found, err := srv.runDataOutRule(ctx, run.ID); err != nil || found || rule != types.TransferAllowed {
		t.Errorf("data_out of a pruned run = %q (found %v, err %v), want allowed, not found", rule, found, err)
	}
	if re := srv.reviveCarryRecordedRules(ctx, run.ID, &types.RunPolicySpec{}); re != nil {
		t.Errorf("revive carry of a pruned run = %+v, want nil", re)
	}
}

// dataOutGenViewColumnStore is genViewStore with the agent_runs.data_out column (store.RunDataOuts): every run
// it reads carries row, nil for none. The generation view (RunPolicyState, the filtered audit reads) is
// genViewStore's, so a run on generation 2 with no generation row is unreadable here as it is there.
type dataOutGenViewColumnStore struct {
	*genViewStore
	row *types.TransferRule
}

var _ store.RunDataOuts = (*dataOutGenViewColumnStore)(nil)

func (s *dataOutGenViewColumnStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	run, err := s.genViewStore.GetRun(ctx, id)
	run.DataOut = s.row
	return run, err
}

func (*dataOutGenViewColumnStore) RaiseRunDataOut(context.Context, uuid.UUID, types.TransferRule) error {
	return nil
}

// dataOutGenViewColumnFixture is dataOutGenFixture's run (resolve row present, generation 2, no generation
// row) on a store that keeps the column with row as the run's rule.
func dataOutGenViewColumnFixture(t *testing.T, row *types.TransferRule) (*Server, types.AgentRun) {
	t.Helper()
	srv, run := dataOutGenFixture(t, false, "")
	srv.cfg.Store = &dataOutGenViewColumnStore{genViewStore: srv.cfg.Store.(*genViewStore), row: row}
	return srv, run
}

// A rule on the run's row is the answer whatever its current generation's row: the row is raised before a
// revive's generation is current, so the generation is never read and its unreadable row is no sentinel.
func TestDataOutRule_StoredRowWinsOverAnUnreadableGeneration(t *testing.T) {
	for _, want := range []types.TransferRule{types.TransferReview, types.TransferBlocked} {
		srv, run := dataOutGenViewColumnFixture(t, &want)
		rule, found, err := srv.runDataOutRule(context.Background(), run.ID)
		if errors.Is(err, errDataOutGenerationUnread) {
			t.Errorf("row %q with an unreadable generation row: err %v, want the row's rule and no sentinel", want, err)
		}
		if err != nil || !found || rule != want {
			t.Errorf("row %q with an unreadable generation row = %q (found %v, err %v), want %q, found, no error", want, rule, found, err, want)
		}
	}
}

// A run with no rule on its row reads its envelope, and on a column store whose current generation's row
// cannot be read it has no rule to give: the sentinel, never allowed and never found.
func TestDataOutRule_NullRowWithAnUnreadableGenerationIsUnreadable(t *testing.T) {
	srv, run := dataOutGenViewColumnFixture(t, nil)
	rule, found, err := srv.runDataOutRule(context.Background(), run.ID)
	if !errors.Is(err, errDataOutGenerationUnread) || !strings.Contains(err.Error(), "(generation 2)") || found || rule != types.TransferAllowed {
		t.Errorf("NULL row with an unreadable generation row = %q (found %v, err %v), want allowed, not found, errDataOutGenerationUnread (generation 2)", rule, found, err)
	}
}
