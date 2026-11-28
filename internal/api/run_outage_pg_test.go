// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// outageDispatchStore is the dispatch fixture's store with a site config the test sets and the run's outage
// terms kept in Postgres.
type outageDispatchStore struct {
	*dispatchTestStore
	pg     store.PG
	siteMu sync.Mutex
	site   types.SiteConfig
}

func (s *outageDispatchStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	s.siteMu.Lock()
	defer s.siteMu.Unlock()
	return s.site, nil
}

func (s *outageDispatchStore) setSite(c types.SiteConfig) {
	s.siteMu.Lock()
	defer s.siteMu.Unlock()
	s.site = c
}

func (s *outageDispatchStore) SetRunOutageTerms(ctx context.Context, id uuid.UUID, t types.OutageTerms) (types.OutageTerms, error) {
	return s.pg.SetRunOutageTerms(ctx, id, t)
}

func (s *outageDispatchStore) RunOutageTerms(ctx context.Context, id uuid.UUID) (types.OutageTerms, error) {
	return s.pg.RunOutageTerms(ctx, id)
}

// outageDispatch builds a dispatch fixture whose run is also a row in Postgres, with the site's allowance at
// siteMinutes.
func outageDispatch(t *testing.T, siteMinutes int) (*Server, *outageDispatchStore, *fakeRunner, types.AgentRun) {
	t.Helper()
	h := newHarness(t)
	now := time.Now().UTC()
	run := types.AgentRun{
		ID: uuid.New(), CreatedAt: now, UpdatedAt: now, CreatedBy: "t@example.com",
		Agent: "claude-code", ConfinementClass: types.CC1, State: types.RunPending,
		RunnerTarget: "docker", Task: "do the thing",
	}
	pg := store.NewPG(throwawayPGPool(t))
	pgRun := run
	pgRun.SPIFFEID = "spiffe://wardyn.test/agent-run/" + run.ID.String()
	if _, err := pg.CreateRun(context.Background(), pgRun); err != nil {
		t.Fatalf("persist the run: %v", err)
	}
	st := &outageDispatchStore{dispatchTestStore: &dispatchTestStore{run: run, state: types.RunPending}, pg: pg,
		site: types.SiteConfig{OutageAllowanceMinutes: ptr(siteMinutes)}}
	fr := &fakeRunner{}
	cfg := baseTestConfig(h, st)
	cfg.Runner = fr
	cfg.Broker = h.broker
	cfg.Audit = &recRecorder{}
	return New(cfg), st, fr, run
}

// renderedOutage dispatches run with policy and returns the rendered proxy config's top-level keys and its
// policy's keys.
func renderedOutage(t *testing.T, srv *Server, fr *fakeRunner, run types.AgentRun, policy types.RunPolicySpec) (top, pol map[string]json.RawMessage) {
	t.Helper()
	fr.mu.Lock()
	before := fr.createCalls
	fr.lastSpec = runner.SandboxSpec{}
	fr.mu.Unlock()
	srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
		RunToken: "run-token", Image: "wardyn/claude-code:latest", Policy: policy,
	})
	fr.mu.Lock()
	spec, calls := fr.lastSpec, fr.createCalls-before
	fr.mu.Unlock()
	// Zeroed before, so a dispatch that never reached CreateSandbox cannot pass on the previous one's spec.
	if calls != 1 || spec.RunID != run.ID {
		t.Fatalf("this dispatch asked for %d sandboxes (last spec is for %s), want one for %s", calls, spec.RunID, run.ID)
	}
	raw, err := runner.BuildProxyConfig(spec.RunID, spec.ProxyConfig, 3128)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(top["policy"], &pol); err != nil {
		t.Fatal(err)
	}
	return top, pol
}

func TestDispatchOutageTerms_PG(t *testing.T) {
	ctx := context.Background()
	want := types.OutageTerms{AllowanceSec: 1800, Strict: true}

	srv, st, fr, run := outageDispatch(t, 120)
	top, pol := renderedOutage(t, srv, fr, run, types.RunPolicySpec{MinConfinementClass: types.CC1,
		Outage: &types.OutageControls{AllowanceMinutes: 30, Strict: true}})
	if stored, err := st.pg.RunOutageTerms(ctx, run.ID); err != nil || stored != want {
		t.Fatalf("stored terms = %+v %v, want %+v", stored, err, want)
	}
	if got := string(top["outage"]); got != `{"allowance_s":1800,"strict":true}` {
		t.Fatalf("rendered outage = %s, want 1800 s strict", got)
	}
	if _, ok := pol["outage"]; ok {
		t.Fatalf("the proxy's policy carries outage: %s", top["policy"])
	}

	// Second dispatch of the same run: the site is lowered to the default and the policy says nothing, and
	// the run keeps the terms it was first dispatched with.
	st.setSite(types.SiteConfig{OutageAllowanceMinutes: ptr(60)})
	st.mu.Lock()
	st.state = types.RunPending
	st.mu.Unlock()
	top, _ = renderedOutage(t, srv, fr, run, types.RunPolicySpec{MinConfinementClass: types.CC1})
	if stored, err := st.pg.RunOutageTerms(ctx, run.ID); err != nil || stored != want {
		t.Fatalf("stored terms after the second dispatch = %+v %v, want the first %+v", stored, err, want)
	}
	if got := string(top["outage"]); got != `{"allowance_s":1800,"strict":true}` {
		t.Fatalf("second rendered outage = %s, want the recorded 1800 s strict", got)
	}

	// A default run writes no outage key at all.
	srv, _, fr, run = outageDispatch(t, 0)
	top, pol = renderedOutage(t, srv, fr, run, types.RunPolicySpec{MinConfinementClass: types.CC1})
	if _, ok := top["outage"]; ok {
		t.Fatalf("a default run's proxy config carries outage: %s", top["outage"])
	}
	if _, ok := pol["outage"]; ok {
		t.Fatalf("a default run's proxy policy carries outage: %s", top["policy"])
	}

	// A runner run's lease carries its recorded terms; a default run's lease marshals without either key.
	f := newRunnerLeaseFixture(t)
	first := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	termed, plain := f.localRun(t, 0), f.localRun(t, 0)
	if _, err := f.st.SetRunOutageTerms(f.ctx, termed.ID, want); err != nil {
		t.Fatal(err)
	}
	// Only the leases of the placed generations are checked: the runner's session can become READY after
	// the runs exist but before their terms are recorded, and its re-lease of every live run then carries
	// no generation and the terms of that moment.
	placedGen := map[uuid.UUID]uuid.UUID{}
	for _, r := range []*types.AgentRun{&termed, &plain} {
		f.running(t, r, &first)
		placedGen[r.ID] = f.placed(t, r).generation
		if got := f.leaseEnd(r.ID, first); !got.Equal(first) {
			t.Fatalf("the runner holds a lease ending %s for a run placed with the end %s", got, first)
		}
	}
	f.leaseMu.Lock()
	frames := append([]runnerwire.Lease(nil), f.leaseFrames...)
	f.leaseMu.Unlock()
	var sawTermed, sawPlain bool
	for _, l := range frames {
		if l.ExecutionGeneration != placedGen[l.RunID] {
			continue
		}
		switch l.RunID {
		case termed.ID:
			sawTermed = true
			if l.OutageAllowanceS != 1800 || !l.OutageStrict {
				t.Errorf("the termed run's lease = %+v, want 1800 s strict", l)
			}
		case plain.ID:
			sawPlain = true
			b, err := runnerwire.Marshal(l)
			if err != nil || strings.Contains(string(b), "outage") {
				t.Errorf("a default run's lease marshals outage keys: %s %v", b, err)
			}
		}
	}
	if !sawTermed || !sawPlain {
		t.Fatalf("leases seen: termed %v, default %v", sawTermed, sawPlain)
	}
}
