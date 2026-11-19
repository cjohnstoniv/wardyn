// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// keptStore is the kept machines, runners, pools and runs a kept-machine create and dispatch read, in memory,
// with the store's own conditions on the insert and the claim.
type keptStore struct {
	noGovernanceStore
	mu         sync.Mutex
	site       types.SiteConfig
	runners    map[uuid.UUID]types.Runner
	runs       map[uuid.UUID]types.AgentRun
	pools      map[uuid.UUID][]types.RunnerPool
	machines   map[uuid.UUID]types.KeptMachine
	runMachine map[uuid.UUID]keptRunRecord
	stops      []store.KeptMachineStop
	dropped    []uuid.UUID
	failed     []uuid.UUID
	stopped    []uuid.UUID
	// resetting is each machine's unconfirmed fresh start: the run that asked for it. unconfirmed is an
	// earlier one a later mark replaced.
	resetting   map[uuid.UUID]uuid.UUID
	unconfirmed map[uuid.UUID]bool
	// claimErr, when set, is what BeginKeptMachineUse answers: a race dispatch could not see.
	claimErr error
}

type keptRunRecord struct {
	id   *uuid.UUID
	mode string
}

func newKeptStore() *keptStore {
	on := true
	return &keptStore{
		site:    types.SiteConfig{MacOSVM: &types.SiteMacOSVM{AllowKeptMachines: &on, MaxKeptMachinesPerPerson: 2, KeptMachineIdleDays: 14}},
		runners: map[uuid.UUID]types.Runner{}, runs: map[uuid.UUID]types.AgentRun{}, pools: map[uuid.UUID][]types.RunnerPool{},
		machines: map[uuid.UUID]types.KeptMachine{}, runMachine: map[uuid.UUID]keptRunRecord{}, resetting: map[uuid.UUID]uuid.UUID{},
		unconfirmed: map[uuid.UUID]bool{},
	}
}

func (s *keptStore) GetSiteConfig(context.Context) (types.SiteConfig, error) { return s.site, nil }

func (s *keptStore) GetRunner(_ context.Context, id uuid.UUID) (types.Runner, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rn, ok := s.runners[id]; ok {
		return rn, nil
	}
	return types.Runner{}, store.ErrNotFound
}

func (s *keptStore) GetRun(_ context.Context, id uuid.UUID) (types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.runs[id]; ok {
		return r, nil
	}
	return types.AgentRun{}, store.ErrNotFound
}

// UpdateRunStateIf records a fail and answers that a concurrent change won, so failAndRevoke stops there.
func (s *keptStore) UpdateRunStateIf(_ context.Context, id uuid.UUID, _, to types.RunState) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if to == types.RunFailed {
		s.failed = append(s.failed, id)
	}
	return false, nil
}

func (s *keptStore) ListRunnerPoolsWithMember(_ context.Context, id uuid.UUID) ([]types.RunnerPool, error) {
	return s.pools[id], nil
}

// starting is the store's run lock: an insert or claim for a run no longer STARTING changes nothing.
func (s *keptStore) starting(runID uuid.UUID) bool {
	r, ok := s.runs[runID]
	return ok && r.State == types.RunStarting
}

func (s *keptStore) CreateKeptMachineForRun(_ context.Context, m types.KeptMachine, limit int) (types.KeptMachine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.starting(*m.RunID) {
		return types.KeptMachine{}, store.ErrRunNotDispatching
	}
	m, err := s.createLocked(m, limit)
	if err == nil {
		s.runMachine[*m.RunID] = keptRunRecord{id: &m.ID, mode: keptModeNew}
	}
	return m, err
}

func (s *keptStore) ClaimKeptMachineForRun(_ context.Context, id uuid.UUID, owner string, runID uuid.UUID, mode string) (types.KeptMachine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.starting(runID) {
		return types.KeptMachine{}, store.ErrRunNotDispatching
	}
	// The store's compare-and-swap: a resume of a machine with an unconfirmed fresh start is refused.
	if m, ok := s.machines[id]; ok && m.OwnerSubject == owner && m.State != types.KeptMachineDeleting && mode != keptModeFresh && s.pendingLocked(id) {
		return types.KeptMachine{}, store.ErrKeptMachineResetUnconfirmed
	}
	m, err := s.beginLocked(id, owner, runID)
	if err == nil {
		s.runMachine[runID] = keptRunRecord{id: &m.ID, mode: mode}
	}
	return m, err
}

func (s *keptStore) createLocked(m types.KeptMachine, limit int) (types.KeptMachine, error) {
	n := 0
	for _, o := range s.machines {
		if o.OwnerSubject != m.OwnerSubject {
			continue
		}
		n++
		if strings.EqualFold(o.Name, m.Name) {
			return types.KeptMachine{}, store.ErrKeptMachineNameTaken
		}
	}
	if n >= limit {
		return types.KeptMachine{}, store.ErrKeptMachineLimit
	}
	m.State = types.KeptMachineCreating
	s.machines[m.ID] = m
	return m, nil
}

func (s *keptStore) GetKeptMachine(_ context.Context, id uuid.UUID) (types.KeptMachine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.machines[id]; ok {
		return m, nil
	}
	return types.KeptMachine{}, store.ErrNotFound
}

func (s *keptStore) FindKeptMachineOf(_ context.Context, owner, ref string) (types.KeptMachine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.machines {
		if m.OwnerSubject == owner && (m.ID.String() == ref || strings.EqualFold(m.Name, ref)) {
			return m, nil
		}
	}
	return types.KeptMachine{}, store.ErrNotFound
}

func (s *keptStore) ListKeptMachinesOf(_ context.Context, owner string) ([]types.KeptMachine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []types.KeptMachine
	for _, m := range s.machines {
		if m.OwnerSubject == owner {
			out = append(out, m)
		}
	}
	return out, nil
}

func (s *keptStore) BeginKeptMachineUse(_ context.Context, id uuid.UUID, owner string, runID uuid.UUID) (types.KeptMachine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.beginLocked(id, owner, runID)
}

// beginLocked is the store's condition: the owner's, not being deleted, and kept or already this run's.
func (s *keptStore) beginLocked(id uuid.UUID, owner string, runID uuid.UUID) (types.KeptMachine, error) {
	if s.claimErr != nil {
		return types.KeptMachine{}, s.claimErr
	}
	m, ok := s.machines[id]
	switch {
	case !ok || m.OwnerSubject != owner:
		return types.KeptMachine{}, store.ErrNotFound
	case m.State == types.KeptMachineDeleting:
		return types.KeptMachine{}, store.ErrKeptMachineDeleting
	case m.State != types.KeptMachineKept && (m.RunID == nil || *m.RunID != runID):
		return types.KeptMachine{}, store.ErrKeptMachineInUse
	}
	m.State, m.RunID = types.KeptMachineInUse, &runID
	s.machines[id] = m
	return m, nil
}

func (s *keptStore) RecordKeptMachineStop(_ context.Context, st store.KeptMachineStop) (types.KeptMachine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stops = append(s.stops, st)
	m := s.machines[st.ID]
	m.State, m.Scrubbed, m.DiskBytes, m.ExpiresAt = types.KeptMachineKept, st.Scrubbed, st.DiskBytes, st.ExpiresAt
	s.machines[st.ID] = m
	return m, nil
}

func (s *keptStore) DropUnstartedKeptMachine(_ context.Context, id, _ uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropped = append(s.dropped, id)
	delete(s.machines, id)
	return nil
}

func (s *keptStore) SetRunKeptMachine(_ context.Context, runID uuid.UUID, id *uuid.UUID, mode string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runMachine[runID] = keptRunRecord{id: id, mode: mode}
	return nil
}

func (s *keptStore) RunKeptMachine(_ context.Context, runID uuid.UUID) (*uuid.UUID, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.runMachine[runID]
	return r.id, r.mode, nil
}

func (s *keptStore) MarkKeptMachineResetting(_ context.Context, id, runID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.machines[id]; !ok || m.RunID == nil || *m.RunID != runID || m.State != types.KeptMachineInUse {
		return store.ErrNotFound
	}
	if _, earlier := s.resetting[id]; earlier {
		s.unconfirmed[id] = true
	}
	s.resetting[id] = runID
	return nil
}

func (s *keptStore) ClearKeptMachineResetting(_ context.Context, id, runID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resetting[id] != runID {
		return store.ErrNotFound
	}
	delete(s.resetting, id)
	return nil
}

// ResetKeptMachineBase writes the base and its age, as the store does, only for the run that marked it.
func (s *keptStore) ResetKeptMachineBase(_ context.Context, id, runID uuid.UUID, base string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.machines[id]
	if !ok || s.resetting[id] != runID || m.RunID == nil || *m.RunID != runID {
		return store.ErrNotFound
	}
	m.Base, m.BasePreparedAt = base, at
	s.machines[id] = m
	delete(s.resetting, id)
	delete(s.unconfirmed, id)
	return nil
}

func (s *keptStore) KeptMachineResetPending(_ context.Context, id uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pendingLocked(id), nil
}

func (s *keptStore) pendingLocked(id uuid.UUID) bool {
	_, ok := s.resetting[id]
	return ok || s.unconfirmed[id]
}

// The run leaser: StopKeptRunIf records the end a resume asks for and answers that it lost, so the test
// stops short of the run's teardown.
func (s *keptStore) StopKeptRunIf(_ context.Context, id uuid.UUID, to types.RunState, _ *time.Time, _ types.LostReason, _ *time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if to == types.RunStopped {
		s.stopped = append(s.stopped, id)
	}
	return false, nil
}
func (s *keptStore) ListLeasedRuns(context.Context) ([]types.AgentRun, error) { return nil, nil }
func (s *keptStore) MarkRunEnded(context.Context, uuid.UUID, time.Time) (bool, error) {
	return false, nil
}
func (s *keptStore) MarkRunEndingSoon(context.Context, uuid.UUID, time.Time, int) (bool, error) {
	return false, nil
}
func (s *keptStore) SetRunEndAndWait(context.Context, uuid.UUID, types.RunLimits, *time.Time, int, *time.Time, int, *store.EndedKept) (bool, error) {
	return false, nil
}
func (s *keptStore) SetRunContainmentError(context.Context, uuid.UUID, string) error {
	return nil
}
func (s *keptStore) ClearRunContainmentError(context.Context, uuid.UUID) (bool, error) {
	return false, nil
}
func (s *keptStore) MarkRunAgentStopped(context.Context, uuid.UUID, time.Time) error { return nil }

var (
	_ keptMachineStore    = (*keptStore)(nil)
	_ runKeptMachineStore = (*keptStore)(nil)
	_ store.RunLeaser     = (*keptStore)(nil)
)

// keptRig is a server holding Alice's own Mac, online, keeping machines, in her own pool.
type keptRig struct {
	srv   *Server
	st    *keptStore
	audit *recRecorder
	mac   uuid.UUID
	owner string
}

const keptMacName = "Alice's Mac Studio"

func newKeptRig(t *testing.T, caps *runnerwire.MacVMCaps) *keptRig {
	t.Helper()
	st := newKeptStore()
	rec := &recRecorder{}
	srv := New(Config{BaseCtx: context.Background(), Store: st, Audit: rec, RunnerTarget: "docker"})
	mac := uuid.New()
	st.runners[mac] = types.Runner{ID: mac, Name: keptMacName, Owner: aliceSub, State: types.RunnerClaimed, OwnerKind: types.RunnerOwnerPerson}
	st.pools[mac] = []types.RunnerPool{{ID: uuid.New(), Name: "Mine", Scope: types.RunnerPoolUser, Owner: aliceSub}}
	if caps != nil {
		sub := remote.New(mac.String(), &capsTransport{online: true}, remote.Options{})
		data, err := json.Marshal(runnerwire.Caps{MacOSVM: caps})
		if err != nil {
			t.Fatal(err)
		}
		sub.HandleEvent(runnerwire.Event{Kind: runnerwire.EventCaps, Data: data})
		srv.runnerStreams.subsMu.Lock()
		if srv.runnerStreams.subs == nil {
			srv.runnerStreams.subs = map[uuid.UUID]runnerEndpoint{}
		}
		srv.runnerStreams.subs[mac] = runnerEndpoint{sub: sub, link: &remote.Link{}}
		srv.runnerStreams.subsMu.Unlock()
	}
	return &keptRig{srv: srv, st: st, audit: rec, mac: mac, owner: types.CanonicalUserSubject(aliceSub)}
}

// keepingCaps is a Mac that keeps machines, with room for several, and a ready Standard base.
func keepingCaps() *runnerwire.MacVMCaps {
	return &runnerwire.MacVMCaps{KeptMachines: true, Max: 2,
		Base: runnerwire.MacVMBase{State: runnerwire.MacVMBaseReady, CreatedAt: time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)},
		Kept: &runnerwire.MacVMKept{LimitBytes: 500 << 30}}
}

// ask is a create on Alice's own Mac asking for m.
func (r *keptRig) ask(m client.RunMachine) keptMachineAsk {
	mac := r.mac
	return keptMachineAsk{
		req:   createRunRequest{Agent: "claude-code", MachineKind: string(types.MachineMacOSVM), Interactive: true, Machine: &m},
		place: runPlacement{pooled: true, placement: types.PlacementLocal, eligible: []poolTarget{{runnerID: &mac}}},
	}
}

// startingRun is a run being dispatched.
func (r *keptRig) startingRun() types.AgentRun {
	run := types.AgentRun{ID: uuid.New(), State: types.RunStarting, MachineKind: types.MachineMacOSVM}
	r.st.runs[run.ID] = run
	return run
}

func (r *keptRig) decide(in keptMachineAsk) (*keptMachinePlan, *runRefusal) {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(personCtx(aliceSub))
	return r.srv.keptMachineDecision(req, in)
}

// kept puts a machine of Alice's, kept on her Mac, and returns it.
func (r *keptRig) kept(name string, f func(m *types.KeptMachine)) types.KeptMachine {
	m := types.KeptMachine{ID: uuid.New(), Name: name, OwnerSubject: r.owner, RunnerID: r.mac, Base: types.MacOSStandardBase,
		BasePreparedAt: time.Now().Add(-72 * time.Hour), State: types.KeptMachineKept, Scrubbed: true, DiskBytes: 30 << 30,
		ExpiresAt: time.Now().Add(7 * 24 * time.Hour)}
	if f != nil {
		f(&m)
	}
	r.st.machines[m.ID] = m
	return m
}

func wantKeptRefusal(t *testing.T, rf *runRefusal, status int, reason, sentence string) {
	t.Helper()
	if rf == nil {
		t.Fatalf("got no refusal, want %d %s", status, reason)
	}
	got := macWindowRefusalReason(rf)
	if rf.status != status || got != reason {
		t.Fatalf("refusal = %d %s (%q), want %d %s", rf.status, got, rf.body.Error, status, reason)
	}
	if sentence != "" && rf.body.Error != sentence {
		t.Fatalf("sentence = %q, want %q", rf.body.Error, sentence)
	}
	if strings.Contains(rf.body.Error, "`") {
		t.Fatalf("a server sentence carries a backtick: %q", rf.body.Error)
	}
}

func TestKeptMachineNameRuleIsTheSentencesRule(t *testing.T) {
	for _, ok := range []string{"a", "Build box", "web-2_old", strings.Repeat("x", 64), "0"} {
		if rf := keptMachineShapeRefusal(client.RunMachine{Keep: true, Name: ok}); rf != nil {
			t.Errorf("%q refused: %s", ok, rf.body.Error)
		}
	}
	for _, bad := range []string{strings.Repeat("x", 65), "café", "a.b", "a/b", "tab\there", "new\nline", "a`b"} {
		wantKeptRefusal(t, keptMachineShapeRefusal(client.RunMachine{Keep: true, Name: bad}), http.StatusBadRequest, reasonKeptMachineInvalid,
			"Kept machine names are 1 to 64 letters, digits, spaces, hyphens or underscores.")
	}
	for name, m := range map[string]client.RunMachine{
		"keep and resume":     {Keep: true, Resume: "x"},
		"fresh without one":   {Keep: true, Fresh: true},
		"a name on a resume":  {Resume: "x", Name: "y"},
		"nothing asked":       {},
		"a blank resume only": {Resume: "  "},
	} {
		if rf := keptMachineShapeRefusal(m); rf == nil || rf.status != http.StatusBadRequest {
			t.Errorf("%s: %+v not refused 400", name, m)
		}
	}
}

func TestKeptMachineRefusedWhenTheSiteTurnsItOff(t *testing.T) {
	r := newKeptRig(t, keepingCaps())
	r.st.site.MacOSVM.AllowKeptMachines = nil
	_, rf := r.decide(r.ask(client.RunMachine{Keep: true}))
	wantKeptRefusal(t, rf, http.StatusForbidden, reasonKeptMachineSiteOff, keptMachineSiteOffMsg)
}

func TestKeptMachineOnlyForAnInteractiveMacOSRun(t *testing.T) {
	r := newKeptRig(t, keepingCaps())
	in := r.ask(client.RunMachine{Keep: true})
	in.req.Interactive, in.req.Task = false, "fix the build"
	_, rf := r.decide(in)
	wantKeptRefusal(t, rf, http.StatusUnprocessableEntity, reasonKeptMachineInteractiveOnly, keptMachineInteractiveMsg)
	in = r.ask(client.RunMachine{Keep: true})
	in.req.MachineKind = string(types.MachineLinuxVM)
	_, rf = r.decide(in)
	wantKeptRefusal(t, rf, http.StatusUnprocessableEntity, reasonKeptMachineInteractiveOnly, "")
}

func TestKeptMachineGovernanceDenyRefusesWithTheProfileSentence(t *testing.T) {
	r := newKeptRig(t, keepingCaps())
	in := r.ask(client.RunMachine{Keep: true})
	in.ceiling.Limits.DenyKeptMachines = true
	_, rf := r.decide(in)
	if rf == nil || rf.decision == nil || macWindowRefusalReason(rf) != "governance_unit" {
		t.Fatalf("refusal = %+v, want a governance decision", rf)
	}
	if rf.decision.Sentence != "Your organisation's governance profile doesn't allow kept machines." {
		t.Fatalf("sentence = %q", rf.decision.Sentence)
	}
}

func TestKeptMachineOnlyOnTheCallersOwnMacInTheirOwnPool(t *testing.T) {
	r := newKeptRig(t, keepingCaps())
	in := r.ask(client.RunMachine{Keep: true})
	in.place.pooled = false
	_, rf := r.decide(in)
	wantKeptRefusal(t, rf, http.StatusForbidden, reasonKeptMachineNotYourMac, keptRunNotYourMacMsg(""))

	other := uuid.New()
	in = r.ask(client.RunMachine{Keep: true})
	in.place.eligible = append(in.place.eligible, poolTarget{runnerID: &other})
	_, rf = r.decide(in)
	wantKeptRefusal(t, rf, http.StatusForbidden, reasonKeptMachineNotYourMac, keptMachinePoolMsg)

	rn := r.st.runners[r.mac]
	rn.Owner = bobSub
	r.st.runners[r.mac] = rn
	_, rf = r.decide(r.ask(client.RunMachine{Keep: true}))
	wantKeptRefusal(t, rf, http.StatusForbidden, reasonKeptMachineNotYourMac, keptRunNotYourMacMsg(keptMacName))
	rn.Owner = aliceSub
	r.st.runners[r.mac] = rn

	r.st.pools[r.mac] = append(r.st.pools[r.mac], types.RunnerPool{ID: uuid.New(), Name: "Builds", Scope: types.RunnerPoolOrganization})
	_, rf = r.decide(r.ask(client.RunMachine{Keep: true}))
	wantKeptRefusal(t, rf, http.StatusForbidden, reasonKeptMachineNotYourMac, keptMachinePoolMsg)

	r.st.pools[r.mac][1].State = types.RunnerPoolDeleted
	if plan, rf := r.decide(r.ask(client.RunMachine{Keep: true})); rf != nil || plan.mode != keptModeNew || plan.runner != r.mac {
		t.Fatalf("own Mac, own pool: %+v, %+v", plan, rf)
	}
}

func TestKeptMachinePerPersonLimit(t *testing.T) {
	r := newKeptRig(t, keepingCaps())
	r.st.site.MacOSVM.MaxKeptMachinesPerPerson = 1
	r.kept("first", nil)
	_, rf := r.decide(r.ask(client.RunMachine{Keep: true}))
	wantKeptRefusal(t, rf, http.StatusUnprocessableEntity, reasonKeptMachinePersonLimit, "1 of 1 kept machine used")
}

func TestKeptMachineMacDiskLimit(t *testing.T) {
	caps := keepingCaps()
	caps.Kept = &runnerwire.MacVMKept{LimitBytes: 0}
	r := newKeptRig(t, caps)
	_, rf := r.decide(r.ask(client.RunMachine{Keep: true}))
	wantKeptRefusal(t, rf, http.StatusUnprocessableEntity, reasonKeptMachineMacLimit, keptMacName+" doesn't keep machines.")

	caps = keepingCaps()
	caps.Kept = &runnerwire.MacVMKept{LimitBytes: 100 << 30, WorstBytes: 60 << 30}
	r = newKeptRig(t, caps)
	_, rf = r.decide(r.ask(client.RunMachine{Keep: true}))
	wantKeptRefusal(t, rf, http.StatusUnprocessableEntity, reasonKeptMachineMacLimit, keptMachineMacLimitMsg(keptMacName, 100, 11))
}

func TestKeptMachineNameClashRefused(t *testing.T) {
	r := newKeptRig(t, keepingCaps())
	r.kept("Build Box", nil)
	_, rf := r.decide(r.ask(client.RunMachine{Keep: true, Name: "build box"}))
	wantKeptRefusal(t, rf, http.StatusConflict, reasonKeptMachineNameTaken, "You already have a kept machine named build box.")
}

func TestKeptMachineResumeChecksItsOwnRecordNotTheMacsBase(t *testing.T) {
	caps := keepingCaps()
	caps.Base = runnerwire.MacVMBase{State: runnerwire.MacVMBasePreparing}
	r := newKeptRig(t, caps)
	m := r.kept("web", func(m *types.KeptMachine) { m.Base = "Xcode 17" })
	plan, rf := r.decide(r.ask(client.RunMachine{Resume: "WEB"}))
	if rf != nil || plan.mode != keptModeResume || plan.id != m.ID || plan.base != "Xcode 17" || plan.name != "web" {
		t.Fatalf("resume: %+v, %+v", plan, rf)
	}

	stale := []macOSBaseLimit{{MaxDays: 30, Now: time.Now(), Base: "Xcode 17", Kept: true}}
	rn := types.Runner{ID: r.mac, Name: keptMacName, TrustClass: types.RunnerTrustManagedDevice,
		Posture: types.RunnerPosture{MDMManaged: true, Evidence: types.PostureEvidenceMDMFile}}
	for name, base := range map[string]runnerwire.MacVMBase{
		"preparing": {State: runnerwire.MacVMBasePreparing},
		"replaced":  {State: runnerwire.MacVMBaseReady, CreatedAt: time.Now().Format(time.RFC3339)},
		"stale":     {State: runnerwire.MacVMBaseReady, CreatedAt: time.Now().AddDate(0, 0, -90).Format(time.RFC3339)},
	} {
		c := runnerwire.Caps{MacOSVM: &runnerwire.MacVMCaps{KeptMachines: true, Max: 2, Base: base}}
		if offer := macOSVMOffer(rn, c, true, 0, false, stale...); offer.Refusal != nil {
			t.Errorf("%s base: a resume was refused: %+v", name, offer.Refusal)
		}
		fresh := stale[0]
		fresh.Kept = false
		if offer := macOSVMOffer(rn, c, true, 0, false, fresh); offer.Refusal == nil {
			t.Errorf("%s base: a new run was not refused", name)
		}
	}
}

func TestKeptMachineResumeRefusals(t *testing.T) {
	r := newKeptRig(t, keepingCaps())
	_, rf := r.decide(r.ask(client.RunMachine{Resume: "nope"}))
	wantKeptRefusal(t, rf, http.StatusNotFound, reasonKeptMachineNotFound, "You have no kept machine nope.")

	holder := types.AgentRun{ID: uuid.New(), Title: "Fix login"}
	r.st.runs[holder.ID] = holder
	r.kept("busy", func(m *types.KeptMachine) { m.State, m.RunID = types.KeptMachineInUse, &holder.ID })
	_, rf = r.decide(r.ask(client.RunMachine{Resume: "busy"}))
	wantKeptRefusal(t, rf, http.StatusConflict, reasonKeptMachineInUse, "Your machine busy is in use by run Fix login. Stop that run first.")

	untitled := uuid.New()
	r.kept("busy2", func(m *types.KeptMachine) { m.State, m.RunID = types.KeptMachineInUse, &untitled })
	_, rf = r.decide(r.ask(client.RunMachine{Resume: "busy2"}))
	wantKeptRefusal(t, rf, http.StatusConflict, reasonKeptMachineInUse, keptMachineInUseMsg("busy2", untitled.String()[:8]))

	r.kept("going", func(m *types.KeptMachine) { m.State = types.KeptMachineDeleting })
	_, rf = r.decide(r.ask(client.RunMachine{Resume: "going"}))
	wantKeptRefusal(t, rf, http.StatusConflict, reasonKeptMachineDeleting, "Your machine going is being deleted.")

	r.kept("old", func(m *types.KeptMachine) { m.ExpiresAt = time.Now().Add(-time.Minute) })
	_, rf = r.decide(r.ask(client.RunMachine{Resume: "old"}))
	wantKeptRefusal(t, rf, http.StatusGone, reasonKeptMachineExpired, "This machine expired and will be deleted.")

	r.st.site.MacOSVM.MaxBaseAgeDays = 30
	r.kept("aged", func(m *types.KeptMachine) { m.BasePreparedAt = time.Now().AddDate(0, 0, -40) })
	_, rf = r.decide(r.ask(client.RunMachine{Resume: "aged"}))
	wantKeptRefusal(t, rf, http.StatusConflict, reasonKeptMachineBaseTooOld, "")
	if plan, rf := r.decide(r.ask(client.RunMachine{Resume: "aged", Fresh: true})); rf != nil || plan.mode != keptModeFresh {
		t.Fatalf("a fresh start of an aged machine: %+v, %+v", plan, rf)
	}

	elsewhere := uuid.New()
	r.st.runners[elsewhere] = types.Runner{ID: elsewhere, Name: "Office Mac mini", Owner: aliceSub, State: types.RunnerClaimed, OwnerKind: types.RunnerOwnerPerson}
	r.kept("there", func(m *types.KeptMachine) { m.RunnerID = elsewhere })
	_, rf = r.decide(r.ask(client.RunMachine{Resume: "there"}))
	wantKeptRefusal(t, rf, http.StatusConflict, reasonKeptMachineOffline,
		"This run would resume your kept machine there on Office Mac mini, which isn't available right now.")
}

func TestKeptMachineRunnerMustBeOnlineAndKeep(t *testing.T) {
	r := newKeptRig(t, nil)
	_, rf := r.decide(r.ask(client.RunMachine{Keep: true}))
	wantKeptRefusal(t, rf, http.StatusConflict, reasonKeptMachineOffline, keptMachineOfflineMsg(keptMacName))
	in := r.ask(client.RunMachine{Keep: true})
	in.queued = true
	if _, rf := r.decide(in); rf != nil {
		t.Fatalf("a queued run that waits for its Mac: %+v", rf)
	}
	caps := keepingCaps()
	caps.KeptMachines = false
	r = newKeptRig(t, caps)
	_, rf = r.decide(r.ask(client.RunMachine{Keep: true}))
	wantKeptRefusal(t, rf, http.StatusConflict, reasonKeptMachineRunnerTooOld, "")
}

func TestKeptMachineRefusesResidentSecretsAtCreate(t *testing.T) {
	r := newKeptRig(t, keepingCaps())
	in := r.ask(client.RunMachine{Keep: true})
	in.spec.EligibleGrants = []types.GrantSpec{{Kind: types.GrantSSHKey}, {Kind: types.GrantEnvSecret}, {Kind: types.GrantGitPAT}}
	_, rf := r.decide(in)
	wantKeptRefusal(t, rf, http.StatusUnprocessableEntity, reasonKeptMachineResidentSecrets,
		keptMachineResidentSecretsMsg([]string{"environment secrets", "SSH keys"}))
	if got := keptResidentKinds(in.spec, runComponents{}, true); strings.Join(got, ",") != "environment secrets,SSH keys,git tokens" {
		t.Fatalf("with the git broker off: %v", got)
	}
}

func TestKeptMachineRefusesResidentSecretsAtDispatch(t *testing.T) {
	spec := runner.SandboxSpec{
		SecretEnv:    map[string]string{"ANTHROPIC_API_KEY": "minted", "DB_PASSWORD": "stored"},
		ManagedFiles: []runner.ManagedFile{{Path: runner.ComponentSecretDir + "/deploy_key"}, {Path: "/etc/wardyn/ca.pem"}},
	}
	origins := map[string]placement.CredentialOrigin{
		"SandboxSpec.SecretEnv[ANTHROPIC_API_KEY]":              {Class: placement.ClassPlatform},
		"SandboxSpec.SecretEnv[DB_PASSWORD]":                    {Class: placement.ClassOwn, GrantKind: types.GrantEnvSecret},
		placementCredentialIndex("SandboxSpec.ManagedFiles", 0): {Class: placement.ClassOwn, GrantKind: types.GrantSSHKey},
	}
	if got := keptSpecSecretKinds(spec, origins); strings.Join(got, ",") != "environment secrets,SSH keys" {
		t.Fatalf("kinds = %v", got)
	}
	delete(spec.SecretEnv, "DB_PASSWORD")
	spec.ManagedFiles = spec.ManagedFiles[1:]
	if got := keptSpecSecretKinds(spec, origins); len(got) != 0 {
		t.Fatalf("platform credentials alone: %v", got)
	}

	r := newKeptRig(t, keepingCaps())
	run := r.startingRun()
	spec.SecretEnv["DB_PASSWORD"] = "stored"
	if !r.srv.keptSecretRefusal(context.Background(), run, spec, origins) {
		t.Fatal("dispatch did not refuse a stored secret")
	}
	if len(r.st.failed) != 1 || r.st.failed[0] != run.ID {
		t.Fatalf("failed runs = %v", r.st.failed)
	}
	wantKeptAudit(t, r.audit, "run.dispatch", "failure", reasonKeptMachineResidentSecrets)
}

func wantKeptAudit(t *testing.T, rec *recRecorder, action, outcome, reason string) map[string]any {
	t.Helper()
	for _, ev := range rec.snapshot() {
		if ev.Action != action || ev.Outcome != outcome {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatal(err)
		}
		if reason == "" || data["reason"] == reason {
			return data
		}
	}
	t.Fatalf("no %s %s row with reason %q in %+v", action, outcome, reason, rec.snapshot())
	return nil
}

func TestKeptMachineCreateDeniedWritesItsRow(t *testing.T) {
	r := newKeptRig(t, keepingCaps())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(personCtx(aliceSub))
	rf := runError(http.StatusConflict, reasonKeptMachineNameTaken, keptMachineNameTakenMsg("x"))
	r.srv.auditKeptMachineDenied(req, createRunRequest{Machine: &client.RunMachine{Keep: true, Name: "x"}}, rf, false)
	if len(r.audit.snapshot()) != 0 {
		t.Fatal("a dry door wrote a row")
	}
	r.srv.auditKeptMachineDenied(req, createRunRequest{Machine: &client.RunMachine{Keep: true, Name: "x"}}, rf, true)
	wantKeptAudit(t, r.audit, "machine.create", "denied", reasonKeptMachineNameTaken)
	r.srv.auditKeptMachineDenied(req, createRunRequest{Machine: &client.RunMachine{Resume: "x"}},
		runError(http.StatusNotFound, reasonKeptMachineNotFound, keptMachineNotFoundMsg("x")), true)
	wantKeptAudit(t, r.audit, "machine.resume", "denied", reasonKeptMachineNotFound)
}

func TestKeptMachineDispatchInsertsAndDropsAnUnstartedNewMachine(t *testing.T) {
	r := newKeptRig(t, keepingCaps())
	plan, rf := r.decide(r.ask(client.RunMachine{Keep: true}))
	if rf != nil {
		t.Fatal(rf.body.Error)
	}
	r.kept("machine-1", nil)
	run := r.startingRun()
	kd, ok := r.srv.keptMachineAtDispatch(context.Background(), run, plan)
	if !ok || kd.machine.Name != "machine-2" || kd.machine.State != types.KeptMachineCreating || *kd.machine.RunID != run.ID {
		t.Fatalf("insert: %+v, %v", kd.machine, ok)
	}
	if got := r.st.runMachine[run.ID]; got.id == nil || *got.id != kd.machine.ID || got.mode != keptModeNew {
		t.Fatalf("the run records %+v", got)
	}
	sp := kd.spec()
	if sp.Mode != keptModeNew || sp.Name != "machine-2" || sp.IdleDays != 14 || sp.OwnerHash != types.DriveSubjectHash(r.owner) || sp.ID != kd.machine.ID {
		t.Fatalf("spec = %+v", sp)
	}
	data := wantKeptAudit(t, r.audit, "machine.create", "success", "")
	if data["name"] != "machine-2" || data["run_id"] != run.ID.String() {
		t.Fatalf("machine.create = %v", data)
	}

	created := true
	r.srv.releaseUnstartedKeptMachine(context.Background(), run, kd, &created)
	if len(r.st.dropped) != 0 {
		t.Fatal("a started machine was dropped")
	}
	created = false
	r.srv.releaseUnstartedKeptMachine(context.Background(), run, kd, &created)
	if len(r.st.dropped) != 1 || r.st.dropped[0] != kd.machine.ID {
		t.Fatalf("dropped = %v", r.st.dropped)
	}
	if got := r.st.runMachine[run.ID]; got.id != nil {
		t.Fatalf("the run still names a dropped machine: %+v", got)
	}
}

func TestKeptMachineUnstartedResumeIsNeverMarkedCleaned(t *testing.T) {
	r := newKeptRig(t, keepingCaps())
	m := r.kept("web", nil) // last stop reported it cleaned
	plan, rf := r.decide(r.ask(client.RunMachine{Resume: "web"}))
	if rf != nil {
		t.Fatal(rf.body.Error)
	}
	run := r.startingRun()
	kd, ok := r.srv.keptMachineAtDispatch(context.Background(), run, plan)
	if !ok || kd.machine.State != types.KeptMachineInUse {
		t.Fatalf("claim: %+v, %v", kd.machine, ok)
	}
	data := wantKeptAudit(t, r.audit, "machine.resume", "success", "")
	if data["last_scrubbed"] != true || data["fresh"] != false {
		t.Fatalf("machine.resume = %v", data)
	}
	created := false
	r.srv.releaseUnstartedKeptMachine(context.Background(), run, kd, &created)
	if len(r.st.stops) != 1 || r.st.stops[0].Scrubbed || r.st.stops[0].ID != m.ID || r.st.stops[0].DiskBytes != m.DiskBytes {
		t.Fatalf("stops = %+v", r.st.stops)
	}
	if r.st.machines[m.ID].Scrubbed || r.st.machines[m.ID].State != types.KeptMachineKept {
		t.Fatalf("machine after a run that never started: %+v", r.st.machines[m.ID])
	}
	if keptUnstartedStop(types.KeptMachine{Scrubbed: true}, run.ID).Scrubbed {
		t.Fatal("a missing stop report set scrubbed")
	}
}

func TestKeptMachineDispatchRaceFailsTheRunWithTheCreateSentence(t *testing.T) {
	r := newKeptRig(t, keepingCaps())
	holder := uuid.New()
	m := r.kept("web", nil)
	plan, rf := r.decide(r.ask(client.RunMachine{Resume: "web"}))
	if rf != nil {
		t.Fatal(rf.body.Error)
	}
	other := r.st.machines[m.ID]
	other.State, other.RunID = types.KeptMachineInUse, &holder
	r.st.machines[m.ID] = other
	run := r.startingRun()
	if _, ok := r.srv.keptMachineAtDispatch(context.Background(), run, plan); ok {
		t.Fatal("a claimed machine was claimed again")
	}
	if len(r.st.failed) != 1 || r.st.failed[0] != run.ID {
		t.Fatalf("failed = %v", r.st.failed)
	}
	if _, ok := r.st.runMachine[run.ID]; ok {
		t.Fatal("a failed claim was recorded on the run")
	}
	wantKeptAudit(t, r.audit, "machine.resume", "denied", reasonKeptMachineInUse)

	r = newKeptRig(t, keepingCaps())
	r.st.site.MacOSVM.MaxKeptMachinesPerPerson = 1
	plan, rf = r.decide(r.ask(client.RunMachine{Keep: true, Name: "a"}))
	if rf != nil {
		t.Fatal(rf.body.Error)
	}
	r.kept("b", nil) // made by another create between the fold and dispatch
	run = r.startingRun()
	if _, ok := r.srv.keptMachineAtDispatch(context.Background(), run, plan); ok {
		t.Fatal("dispatch passed the per-person limit")
	}
	wantKeptAudit(t, r.audit, "machine.create", "denied", reasonKeptMachinePersonLimit)
}

func TestResumeDuringGraceEndsTheOldRun(t *testing.T) {
	r := newKeptRig(t, keepingCaps())
	ended := time.Now().Add(-time.Hour)
	old := types.AgentRun{ID: uuid.New(), State: types.RunRunning, LostAt: &ended, LostReason: types.LostEnded}
	r.st.runs[old.ID] = old
	r.kept("web", func(m *types.KeptMachine) { m.RunID = &old.ID })
	plan, rf := r.decide(r.ask(client.RunMachine{Resume: "web", Fresh: true}))
	if rf != nil {
		t.Fatal(rf.body.Error)
	}
	run := r.startingRun()
	if _, ok := r.srv.keptMachineAtDispatch(context.Background(), run, plan); !ok {
		t.Fatal("claim failed")
	}
	if len(r.st.stopped) != 1 || r.st.stopped[0] != old.ID {
		t.Fatalf("stopped = %v, want the run in its grace", r.st.stopped)
	}
	if got := r.st.runMachine[run.ID]; got.mode != keptModeFresh {
		t.Fatalf("mode = %q", got.mode)
	}
	live := types.AgentRun{ID: uuid.New(), State: types.RunRunning}
	r.st.runs[live.ID] = live
	r.srv.endKeptGraceRun(context.Background(), live.ID)
	if len(r.st.stopped) != 1 {
		t.Fatal("a run not in its grace was ended")
	}
}

func TestKeptPlacementPinHoldsAResumeToItsMac(t *testing.T) {
	mac, other := uuid.New(), uuid.New()
	for name, tc := range map[string]struct {
		drives      bool
		drive, kept *uuid.UUID
		want        bool
		wantRunner  *uuid.UUID
	}{
		"no resume":            {drives: true, drive: &other, want: true, wantRunner: &other},
		"resume alone":         {kept: &mac, want: true, wantRunner: &mac},
		"drive on the same":    {drives: true, drive: &mac, kept: &mac, want: true, wantRunner: &mac},
		"drive on another Mac": {drives: true, drive: &other, kept: &mac, want: true},
		"nothing":              {},
	} {
		got, pinned := keptPlacementPin(tc.drives, tc.drive, tc.kept)
		if got != tc.want || (pinned == nil) != (tc.wantRunner == nil) || (pinned != nil && *pinned != *tc.wantRunner) {
			t.Errorf("%s: (%v, %v), want (%v, %v)", name, got, pinned, tc.want, tc.wantRunner)
		}
	}
}

func TestRunReadSaysAKeptMachineWasDeleted(t *testing.T) {
	r := newKeptRig(t, keepingCaps())
	m := r.kept("web", func(m *types.KeptMachine) { m.Scrubbed = false })
	run := types.AgentRun{ID: uuid.New()}
	r.st.runMachine[run.ID] = keptRunRecord{id: &m.ID, mode: keptModeResume}
	v := r.srv.runMachineOf(context.Background(), run)
	if v == nil || v.Name != "web" || v.Mode != "resumed" || !v.KeepAtEnd || v.Scrubbed || v.RunnerName != keptMacName || v.Deleted {
		t.Fatalf("view = %+v", v)
	}
	delete(r.st.machines, m.ID)
	v = r.srv.runMachineOf(context.Background(), run)
	if v == nil || !v.Deleted || v.ID != m.ID || v.Name != "" {
		t.Fatalf("deleted view = %+v", v)
	}
	if r.srv.runMachineOf(context.Background(), types.AgentRun{ID: uuid.New()}) != nil {
		t.Fatal("a run that keeps no machine has a view")
	}
}

// TestRunReadNamesWhichRunHoldsTheMachine: the machine on a run's read is the machine's row as it is now, shared by
// every run that used it, so the read names the run that holds it (or held it last) and the console says what a stop
// did to the machine only on that run's page. The row's base, size and expiry ride with it; last_used_at does not.
func TestRunReadNamesWhichRunHoldsTheMachine(t *testing.T) {
	r := newKeptRig(t, keepingCaps())
	runA, runB := types.AgentRun{ID: uuid.New()}, types.AgentRun{ID: uuid.New()}
	m := r.kept("dev", func(m *types.KeptMachine) { m.RunID = &runB.ID; m.Base = "Xcode 26"; m.DiskBytes = 12 << 30 })
	r.st.runMachine[runA.ID] = keptRunRecord{id: &m.ID, mode: "new"}
	r.st.runMachine[runB.ID] = keptRunRecord{id: &m.ID, mode: keptModeResume}
	for _, run := range []types.AgentRun{runA, runB} {
		v := r.srv.runMachineOf(context.Background(), run)
		if v == nil || v.HolderRunID == nil || *v.HolderRunID != runB.ID {
			t.Fatalf("run %s: holder = %+v, want run B", run.ID, v)
		}
		if v.Base != "Xcode 26" || v.DiskBytes != 12<<30 || v.ExpiresAt == nil || !v.ExpiresAt.Equal(m.ExpiresAt) {
			t.Fatalf("run %s: row facts = %+v", run.ID, v)
		}
	}
	if v := r.srv.runMachineOf(context.Background(), runA); v.Mode != "new" {
		t.Fatalf("run A's mode = %q, want its own", v.Mode)
	}
	body, err := json.Marshal(r.srv.runMachineOf(context.Background(), runB))
	if err != nil || strings.Contains(string(body), "last_used_at") {
		t.Fatalf("view JSON = %s, %v; last_used_at is overwritten by a claim and is not sent", body, err)
	}
	cleared := r.st.machines[m.ID]
	cleared.RunID, cleared.ExpiresAt = nil, time.Time{}
	r.st.machines[m.ID] = cleared
	if v := r.srv.runMachineOf(context.Background(), runA); v.HolderRunID != nil || v.ExpiresAt != nil {
		t.Fatalf("no holder and no expiry are left out: %+v", v)
	}
}

func TestNextKeptMachineNameSkipsTakenNamesWithoutCase(t *testing.T) {
	got := nextKeptMachineName([]types.KeptMachine{{Name: "Machine-1"}, {Name: "machine-3"}})
	if got != "machine-2" {
		t.Fatalf("got %q", got)
	}
}

// keptFreshClaim is a fresh start of Alice's machine name, claimed at dispatch.
func (r *keptRig) keptFreshClaim(t *testing.T, name string) (types.AgentRun, keptDispatch) {
	t.Helper()
	plan, rf := r.decide(r.ask(client.RunMachine{Resume: name, Fresh: true}))
	if rf != nil {
		t.Fatal(rf.body.Error)
	}
	run := r.startingRun()
	kd, ok := r.srv.keptMachineAtDispatch(context.Background(), run, plan)
	if !ok {
		t.Fatal("claim failed")
	}
	return run, kd
}

func TestKeptMachineFreshStartFailedBeforeTheMacKeepsTheOldAge(t *testing.T) {
	r := newKeptRig(t, keepingCaps())
	r.st.site.MacOSVM.MaxBaseAgeDays = 30
	aged := time.Now().AddDate(0, 0, -40)
	m := r.kept("web", func(m *types.KeptMachine) { m.BasePreparedAt = aged })
	run, kd := r.keptFreshClaim(t, "web")
	// The proxy config, the masking manifest or the placement check fails: the Mac is never asked.
	created := false
	r.srv.releaseUnstartedKeptMachine(context.Background(), run, kd, &created)
	if got := r.st.machines[m.ID]; !got.BasePreparedAt.Equal(aged) || got.Base != m.Base {
		t.Fatalf("a fresh start that never reached the Mac moved the base: %+v", got)
	}
	_, rf := r.decide(r.ask(client.RunMachine{Resume: "web"}))
	wantKeptRefusal(t, rf, http.StatusConflict, reasonKeptMachineBaseTooOld, "")
}

func TestKeptMachineUnconfirmedFreshStartBlocksANormalResume(t *testing.T) {
	r := newKeptRig(t, keepingCaps())
	m := r.kept("web", nil)
	run, kd := r.keptFreshClaim(t, "web")
	if !r.srv.keptMachineResetting(context.Background(), run, kd) {
		t.Fatal("mark failed")
	}
	// The create was asked and failed: the Mac may or may not have reset the disk.
	created := false
	r.srv.releaseUnstartedKeptMachine(context.Background(), run, kd, &created)
	if !r.st.machines[m.ID].BasePreparedAt.Equal(m.BasePreparedAt) {
		t.Fatal("an unconfirmed fresh start moved the base's age")
	}
	_, rf := r.decide(r.ask(client.RunMachine{Resume: "web"}))
	wantKeptRefusal(t, rf, http.StatusConflict, reasonKeptMachineResetUnconfirmed, keptMachineResetUnconfirmedMsg("web"))
	if _, rf := r.decide(r.ask(client.RunMachine{Resume: "web", Fresh: true})); rf != nil {
		t.Fatalf("another fresh start was refused: %s", rf.body.Error)
	}

	// A later fresh start the Mac never saw (the resident values were refused) takes back only its own
	// mark: the earlier one's uncertainty stays.
	run, kd = r.keptFreshClaim(t, "web")
	if !r.srv.keptMachineResetting(context.Background(), run, kd) {
		t.Fatal("mark failed")
	}
	r.srv.keptResetNotAsked(context.Background(), run, kd)
	r.srv.releaseUnstartedKeptMachine(context.Background(), run, kd, &created)
	_, rf = r.decide(r.ask(client.RunMachine{Resume: "web"}))
	wantKeptRefusal(t, rf, http.StatusConflict, reasonKeptMachineResetUnconfirmed, keptMachineResetUnconfirmedMsg("web"))
}

func TestKeptMachineFreshStartTheMacNeverSawLeavesNoMark(t *testing.T) {
	r := newKeptRig(t, keepingCaps())
	r.kept("web", nil)
	run, kd := r.keptFreshClaim(t, "web")
	if !r.srv.keptMachineResetting(context.Background(), run, kd) {
		t.Fatal("mark failed")
	}
	r.srv.keptResetNotAsked(context.Background(), run, kd)
	created := false
	r.srv.releaseUnstartedKeptMachine(context.Background(), run, kd, &created)
	if _, rf := r.decide(r.ask(client.RunMachine{Resume: "web"})); rf != nil {
		t.Fatalf("a fresh start the Mac never saw blocks: %s", rf.body.Error)
	}
}

// A resume that passed the create's checks before a fresh start's uncertain end is refused by its claim at
// dispatch, not let through by the check it already passed.
func TestKeptMachineResumePlannedBeforeAnUncertainFreshStartIsRefusedAtDispatch(t *testing.T) {
	r := newKeptRig(t, keepingCaps())
	m := r.kept("web", nil)
	plan, rf := r.decide(r.ask(client.RunMachine{Resume: "web"}))
	if rf != nil {
		t.Fatal(rf.body.Error)
	}
	fresh, kd := r.keptFreshClaim(t, "web")
	if !r.srv.keptMachineResetting(context.Background(), fresh, kd) {
		t.Fatal("mark failed")
	}
	created := false // asked of the Mac, and the create failed: the reset may or may not have happened
	r.srv.releaseUnstartedKeptMachine(context.Background(), fresh, kd, &created)
	run := r.startingRun()
	if _, ok := r.srv.keptMachineAtDispatch(context.Background(), run, plan); ok {
		t.Fatal("the earlier resume claimed a machine whose fresh start is unconfirmed")
	}
	wantKeptAudit(t, r.audit, "machine.resume", "denied", reasonKeptMachineResetUnconfirmed)
	if got := r.st.machines[m.ID]; got.State != types.KeptMachineKept || len(r.st.failed) != 1 || r.st.failed[0] != run.ID {
		t.Fatalf("machine %+v, failed %v", got, r.st.failed)
	}
}

func TestKeptMachineConfirmedFreshStartRecordsTheMacsBase(t *testing.T) {
	caps := keepingCaps()
	r := newKeptRig(t, caps)
	r.st.site.MacOSVM.MaxBaseAgeDays = 30
	m := r.kept("web", func(m *types.KeptMachine) { m.BasePreparedAt = time.Now().AddDate(0, 0, -40) })
	run, kd := r.keptFreshClaim(t, "web")
	if !r.srv.keptMachineResetting(context.Background(), run, kd) {
		t.Fatal("mark failed")
	}
	// A Mac that reports no cloned base keeps the mark: the runner's current base is not the clone's.
	r.srv.keptMachineUp(context.Background(), run, kd, runner.Sandbox{})
	if pending, _ := r.st.KeptMachineResetPending(context.Background(), m.ID); !pending {
		t.Fatal("a fresh start with no reported base cleared its mark")
	}
	current, _ := time.Parse(time.RFC3339, caps.Base.CreatedAt)
	want := current.Add(-36 * time.Hour) // the base was prepared again after the clone
	r.srv.keptMachineUp(context.Background(), run, kd, runner.Sandbox{KeptBase: &runner.KeptBase{Name: types.MacOSStandardBase, PreparedAt: want}})
	if got := r.st.machines[m.ID]; !got.BasePreparedAt.Equal(want) || got.Base != types.MacOSStandardBase {
		t.Fatalf("after a confirmed fresh start: %+v, want the clone's own prepared time %v", got, want)
	}
	if pending, _ := r.st.KeptMachineResetPending(context.Background(), m.ID); pending {
		t.Fatal("a confirmed fresh start left its mark")
	}
}

func TestKeptMachineFreshStartKeepsTheMachinesOwnBase(t *testing.T) {
	r := newKeptRig(t, keepingCaps())
	km := r.kept("ios", func(m *types.KeptMachine) { m.Base = "xcode-17" })
	req := r.ask(client.RunMachine{Resume: "ios", Fresh: true}).req
	if rf := r.srv.keptFreshBase(&req, &km); rf != nil {
		t.Fatal(rf.body.Error)
	}
	// What dispatch puts in the sandbox spec (runs_dispatch.go: SandboxSpec.MacOSBase = macOSBaseOf(run)),
	// from the run the create records (runs.go: VMMachine = macOSBaseMachine(req)).
	run := types.AgentRun{MachineKind: types.MachineMacOSVM, VMMachine: macOSBaseMachine(req)}
	if got := macOSBaseOf(run); got != "xcode-17" {
		t.Fatalf("an omitted base dispatches %q, want the machine's xcode-17", got)
	}
	same := r.ask(client.RunMachine{Resume: "ios", Fresh: true}).req
	same.MacOSBase = "xcode-17"
	if rf := r.srv.keptFreshBase(&same, &km); rf != nil || same.MacOSBase != "xcode-17" {
		t.Fatalf("the machine's own base: %v, %q", rf, same.MacOSBase)
	}
	other := r.ask(client.RunMachine{Resume: "ios", Fresh: true}).req
	other.MacOSBase = types.MacOSStandardBase
	wantKeptRefusal(t, r.srv.keptFreshBase(&other, &km), http.StatusUnprocessableEntity, reasonKeptMachineFreshBase,
		"A fresh start keeps your kept machine's base, xcode-17. Start a new kept machine to use a different base.")
	plain := r.ask(client.RunMachine{Resume: "ios"}).req
	if rf := r.srv.keptFreshBase(&plain, &km); rf != nil || plain.MacOSBase != "" {
		t.Fatal("a normal resume's base was changed")
	}
}

func TestKeptMachineDispatchForAKilledRunMakesNoMachine(t *testing.T) {
	r := newKeptRig(t, keepingCaps())
	plan, rf := r.decide(r.ask(client.RunMachine{Keep: true}))
	if rf != nil {
		t.Fatal(rf.body.Error)
	}
	run := r.startingRun()
	run.State = types.RunKilled
	r.st.runs[run.ID] = run
	if _, ok := r.srv.keptMachineAtDispatch(context.Background(), run, plan); ok {
		t.Fatal("a killed run got a machine")
	}
	if len(r.st.machines) != 0 || len(r.st.failed) != 0 || len(r.audit.snapshot()) != 0 {
		t.Fatalf("machines %v, failed %v, audit %v", r.st.machines, r.st.failed, r.audit.snapshot())
	}
	m := r.kept("web", nil)
	plan, _ = r.decide(r.ask(client.RunMachine{Resume: "web"}))
	if _, ok := r.srv.keptMachineAtDispatch(context.Background(), run, plan); ok {
		t.Fatal("a killed run claimed a machine")
	}
	if got := r.st.machines[m.ID]; got.State != types.KeptMachineKept || got.RunID != nil {
		t.Fatalf("a killed run's claim changed the machine: %+v", got)
	}
}

// Nothing but its owner's own session can resume a kept machine: not the system, an admin token or
// another person.
func TestKeptMachineResumesOnlyFromItsOwnersSession(t *testing.T) {
	r := newKeptRig(t, keepingCaps())
	r.kept("web", func(m *types.KeptMachine) { m.Scrubbed = false })
	in := r.ask(client.RunMachine{Resume: "web"})
	for name, ctx := range map[string]context.Context{
		"the system, or an admin token": context.Background(),
		"another person":                personCtx(bobSub),
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(ctx)
		plan, rf := r.srv.keptMachineDecision(req, in)
		if rf == nil || plan != nil {
			t.Errorf("%s resumed the machine: %+v", name, plan)
		}
	}
	if plan, rf := r.decide(in); rf != nil || plan.mode != keptModeResume {
		t.Fatalf("its owner's own session: %+v, %+v", plan, rf)
	}
}

func TestKeptMachineBootScrubWritesItsRow(t *testing.T) {
	r := newKeptRig(t, keepingCaps())
	r.kept("web", nil)
	plan, rf := r.decide(r.ask(client.RunMachine{Resume: "web"}))
	if rf != nil {
		t.Fatal(rf.body.Error)
	}
	run := r.startingRun()
	kd, ok := r.srv.keptMachineAtDispatch(context.Background(), run, plan)
	if !ok {
		t.Fatal("claim failed")
	}
	r.srv.keptMachineUp(context.Background(), run, kd, runner.Sandbox{})
	for _, ev := range r.audit.snapshot() {
		if ev.Action == "machine.scrub" {
			t.Fatal("a boot with no scrub report wrote machine.scrub")
		}
	}
	r.srv.keptMachineUp(context.Background(), run, kd, runner.Sandbox{KeptScrub: &runner.KeptScrub{When: "boot", Outcome: keptScrubDone,
		Removed: map[string]int{"keychain": 2}}})
	data := wantKeptAudit(t, r.audit, "machine.scrub", "success", "")
	if data["when"] != "boot" || data["outcome"] != keptScrubDone || data["machine_id"] != kd.machine.ID.String() {
		t.Fatalf("machine.scrub = %v", data)
	}
	r.srv.keptMachineUp(context.Background(), run, kd, runner.Sandbox{KeptScrub: &runner.KeptScrub{When: "boot", Outcome: keptScrubFailed}})
	wantKeptAudit(t, r.audit, "machine.scrub", "failure", "")
}
