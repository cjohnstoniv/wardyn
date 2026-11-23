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
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runWallsPattern is route B.
const runWallsPattern = "/api/v1/runs/{id}/walls"

// runWallsStore is pvStore with a decision row reader over decisions, one hosting runner, and a unit chain read
// that fails with chainErr when it is set.
type runWallsStore struct {
	*pvStore
	decisions  []store.DecisionRow
	runner     *types.Runner
	chainErr   error
	chainReads atomic.Int32
}

var _ store.WallsAuditReader = (*runWallsStore)(nil)

// AuditWallsRows serves decisions as the accessor does: by action, recorded_at, run and seq, oldest first.
func (s *runWallsStore) AuditWallsRows(_ context.Context, q store.DecisionQuery) ([]store.DecisionRow, store.DecisionCursor, error) {
	var out []store.DecisionRow
	next := q.After
	for _, r := range s.decisions {
		if r.Seq <= q.After.Seq || !slices.Contains(q.Actions, r.Action) || r.RecordedAt.Before(q.Since) ||
			len(q.RunIDs) > 0 && !slices.Contains(q.RunIDs, r.RunID) {
			continue
		}
		if len(out) == q.Limit {
			break
		}
		out, next = append(out, r), store.DecisionCursor{Seq: r.Seq}
	}
	return out, next, nil
}

func (s *runWallsStore) GetRunner(_ context.Context, id uuid.UUID) (types.Runner, error) {
	if s.runner == nil || s.runner.ID != id {
		return types.Runner{}, store.ErrNotFound
	}
	return *s.runner, nil
}

func (s *runWallsStore) GetGovernanceUnitChain(ctx context.Context, id uuid.UUID) ([]types.GovernanceUnit, error) {
	s.chainReads.Add(1)
	if s.chainErr != nil {
		return nil, s.chainErr
	}
	return s.pvStore.GetGovernanceUnitChain(ctx, id)
}

// runWallsFixture is the policy view's fixture over a runWallsStore.
func runWallsFixture(t *testing.T) (*Server, *runWallsStore) {
	t.Helper()
	srv, st := pvFixture(t, &capStore{})
	ws := &runWallsStore{pvStore: st}
	srv.cfg.Store = ws
	return srv, ws
}

// runWallsSeed is a running run of sub's, changed by mut.
func runWallsSeed(st *runWallsStore, sub string, mut func(*types.AgentRun)) types.AgentRun {
	run := st.seedRun(sub, types.RunRunning)
	if mut != nil {
		mut(&run)
		st.mu.Lock()
		st.runs[run.ID] = run
		st.mu.Unlock()
	}
	return run
}

// runWallsBase is a bounded launch spec: the served spec below struck drop.example from it and denied
// deny.example, both cause limits.
func runWallsBase() *policySourceRecord {
	return pvSource(policyKindInline, nil, "", true, types.RunPolicySpec{AllowedDomains: []string{"keep.example", "drop.example"}})
}

func runWallsServed() types.RunPolicySpec {
	return types.RunPolicySpec{AllowedDomains: []string{"keep.example"}, DeniedDomains: []string{"deny.example"}}
}

// runWallsPolicy records run's create row from src and its envelope: served, set as prov records.
func runWallsPolicy(st *runWallsStore, runID uuid.UUID, src *policySourceRecord, served types.RunPolicySpec, prov knobProvenance) {
	st.rows(runID, pvCreate(nil, src), pvRow{action: "run.policy.resolve", data: effectivePolicyDatum{RunPolicySpec: served, Provenance: prov}})
}

// runWallsGet is GET route B for runID with query, as c's caller; body is decoded on a 200.
func runWallsGet(t *testing.T, srv *Server, c *http.Cookie, runID uuid.UUID, query string) (*httptest.ResponseRecorder, RunWalls) {
	t.Helper()
	path := buildPath(runWallsPattern, runID.String())
	if query != "" {
		path += "?" + query
	}
	w := doSSO(t, srv, http.MethodGet, path, c, "")
	var body RunWalls
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode run walls: %v (%s)", err, w.Body.String())
		}
	}
	return w, body
}

// runWallsRunKeys is the keys of the answer's run item.
func runWallsRunKeys(t *testing.T, w *httptest.ResponseRecorder) map[string]json.RawMessage {
	t.Helper()
	var top struct {
		Run map[string]json.RawMessage `json:"run"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &top); err != nil {
		t.Fatalf("decode run walls: %v", err)
	}
	return top.Run
}

// TestRunWallsOwnerSeesNoRestrictionName: the owner reads a restriction as an unnamed "policy" layer, and neither
// the restriction's name nor its id appears in their answer; a security admin's answer has both.
func TestRunWallsOwnerSeesNoRestrictionName(t *testing.T) {
	srv, st := runWallsFixture(t)
	rid := uuid.New()
	run := runWallsSeed(st, pvOwner, func(r *types.AgentRun) { r.RestrictionIDs = []uuid.UUID{rid} })
	rec := policyref.Provenance{Layer: policyref.LayerRestriction, Name: "planted-restriction"}
	runWallsPolicy(st, run.ID, runWallsBase(), runWallsServed(), knobProvenance{"allowed_domains": rec, "denied_domains": rec})
	for _, tc := range []struct {
		who   string
		c     *http.Cookie
		named bool
	}{{"owner", pvMember(t, pvOwner).cookie, false}, {"security admin", pvSecurity(t).cookie, true}} {
		w, body := runWallsGet(t, srv, tc.c, run.ID, "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", tc.who, w.Code, w.Body.String())
		}
		for _, planted := range []string{"planted-restriction", rid.String()} {
			if got := strings.Contains(w.Body.String(), planted); got != tc.named {
				t.Errorf("%s: answer names %q = %v, want %v", tc.who, planted, got, tc.named)
			}
		}
		want := RunWallsLayer{Layer: policyref.LayerRestriction, Name: "planted-restriction"}
		if !tc.named {
			want = RunWallsLayer{Layer: runWallsPolicyLayer}
		}
		if !slices.ContainsFunc(body.Layers, func(l RunWallsLayer) bool { return l.Layer == want.Layer && l.Name == want.Name }) {
			t.Errorf("%s: layers %+v, want one %+v", tc.who, body.Layers, want)
		}
	}
}

// TestRunWallsRateLimit: the run limiter lets a burst of 10 through and answers the 11th with 429
// walls_rate_limited and a Retry-After.
func TestRunWallsRateLimit(t *testing.T) {
	srv, st := runWallsFixture(t)
	now := time.Now()
	srv.cfg.Now = func() time.Time { return now }
	run := runWallsSeed(st, pvOwner, nil)
	c := pvMember(t, pvOwner).cookie
	for i := range 10 {
		if w, _ := runWallsGet(t, srv, c, run.ID, ""); w.Code != http.StatusOK {
			t.Fatalf("call %d: %d %s", i+1, w.Code, w.Body.String())
		}
	}
	w, _ := runWallsGet(t, srv, c, run.ID, "")
	if w.Code != http.StatusTooManyRequests || errorReason(w) != reasonWallsRateLimited {
		t.Fatalf("call 11: %d %s, want 429 %s", w.Code, w.Body.String(), reasonWallsRateLimited)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("call 11: no Retry-After")
	}
}

// TestRunWallsComposeErrorIsUnverifiedNever500: when the re-fold cannot read the run's unit chain the answer is
// still 200, no strike has a removed_by, and no refusal row is written.
func TestRunWallsComposeErrorIsUnverifiedNever500(t *testing.T) {
	srv, st := runWallsFixture(t)
	st.chainErr = errors.New("unit chain read failed")
	unit := uuid.New()
	run := runWallsSeed(st, pvOwner, func(r *types.AgentRun) { r.UnitID = &unit })
	setter := policyref.Provenance{Layer: policyref.LayerUnit, Name: "walls-unit"}
	runWallsPolicy(st, run.ID, runWallsBase(), runWallsServed(), knobProvenance{"allowed_domains": setter, "denied_domains": setter})
	before := len(st.audit.snapshot())
	w, body := runWallsGet(t, srv, pvSecurity(t).cookie, run.ID, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", w.Code, w.Body.String())
	}
	if st.chainReads.Load() == 0 {
		t.Fatal("the re-fold never read the unit chain")
	}
	if len(body.Struck) != 2 {
		t.Fatalf("struck = %+v, want drop.example and deny.example", body.Struck)
	}
	if strings.Contains(w.Body.String(), `"removed_by"`) {
		t.Errorf("a removed_by after a failed chain read: %s", w.Body.String())
	}
	for _, ev := range st.audit.snapshot()[before:] {
		if ev.Action == authz.AuditAction || ev.Outcome == "denied" {
			t.Errorf("a refusal row was written: %s %s", ev.Action, ev.Outcome)
		}
	}
}

// TestRunWallsHidesAPersonalPresetFromOthers: a person-scope preset's name and the entries it removed are its
// owner's: a security admin who is not the owner gets no wall_preset strike and neither planted string.
func TestRunWallsHidesAPersonalPresetFromOthers(t *testing.T) {
	srv, st := runWallsFixture(t)
	run := runWallsSeed(st, pvOwner, nil)
	src := pvSource(policyKindInline, nil, "", false, types.RunPolicySpec{AllowedDomains: []string{"keep.example", "planted-preset-host.example"}})
	src.WallPreset = &wallPresetSourceRecord{ID: uuid.NewString(), Name: "planted-preset-name", Scope: types.WallPresetScopePerson, Version: 1,
		WallsSourceRecord: &WallsSourceRecord{Narrowed: []string{}, Removed: map[string][]string{fieldAllowed: {"planted-preset-host.example"}}}}
	served := types.RunPolicySpec{AllowedDomains: []string{"keep.example"}}
	runWallsPolicy(st, run.ID, src, served, knobProvenance{"allowed_domains": {Layer: policyref.LayerWallPreset}})
	for _, tc := range []struct {
		who   string
		c     *http.Cookie
		shown bool
	}{{"owner", pvMember(t, pvOwner).cookie, true}, {"security admin", pvSecurity(t).cookie, false}} {
		w, body := runWallsGet(t, srv, tc.c, run.ID, "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", tc.who, w.Code, w.Body.String())
		}
		for _, planted := range []string{"planted-preset-name", "planted-preset-host.example"} {
			if got := strings.Contains(w.Body.String(), planted); got != tc.shown {
				t.Errorf("%s: answer names %q = %v, want %v", tc.who, planted, got, tc.shown)
			}
		}
		struck := slices.ContainsFunc(body.Struck, func(s RunWallsStruck) bool { return s.Cause == causeWallPreset })
		if struck != tc.shown {
			t.Errorf("%s: a wall_preset strike = %v, want %v (%+v)", tc.who, struck, tc.shown, body.Struck)
		}
		if !slices.ContainsFunc(body.Layers, func(l RunWallsLayer) bool { return l.Layer == policyref.LayerWallPreset }) {
			t.Errorf("%s: no wall_preset layer in %+v", tc.who, body.Layers)
		}
	}
}

// TestRunWallsOwnerRunItemFields: the owner's run item has no restriction_ids, attention or owner_kind; a
// security admin's has the two this fixture can give.
func TestRunWallsOwnerRunItemFields(t *testing.T) {
	srv, st := runWallsFixture(t)
	runnerID := uuid.New()
	st.runner = &types.Runner{ID: runnerID, State: types.RunnerClaimed, OwnerKind: types.RunnerOwnerPerson}
	run := runWallsSeed(st, pvOwner, func(r *types.AgentRun) {
		r.RestrictionIDs, r.RunnerID = []uuid.UUID{uuid.New()}, &runnerID
	})
	runWallsPolicy(st, run.ID, runWallsBase(), runWallsServed(), nil)

	w, _ := runWallsGet(t, srv, pvMember(t, pvOwner).cookie, run.ID, "")
	if w.Code != http.StatusOK {
		t.Fatalf("owner: %d %s", w.Code, w.Body.String())
	}
	for _, key := range []string{"restriction_ids", "attention", "owner_kind"} {
		if _, ok := runWallsRunKeys(t, w)[key]; ok {
			t.Errorf("owner: run item has %q", key)
		}
	}
	w, _ = runWallsGet(t, srv, pvSecurity(t).cookie, run.ID, "")
	if w.Code != http.StatusOK {
		t.Fatalf("security admin: %d %s", w.Code, w.Body.String())
	}
	for _, key := range []string{"restriction_ids", "owner_kind"} {
		if _, ok := runWallsRunKeys(t, w)[key]; !ok {
			t.Errorf("security admin: run item has no %q", key)
		}
	}
}

// TestRunWallsUnavailableWithoutAReader: a store with no decision row reader answers the owner 501
// walls_view_unavailable, after the gate: a non-owner still gets the missing run's 404.
func TestRunWallsUnavailableWithoutAReader(t *testing.T) {
	srv, st := pvFixture(t, &capStore{})
	run := st.seedRun(pvOwner, types.RunRunning)
	w := doSSO(t, srv, http.MethodGet, buildPath(runWallsPattern, run.ID.String()), pvMember(t, pvOwner).cookie, "")
	if w.Code != http.StatusNotImplemented || errorReason(w) != reasonWallsViewUnavailable {
		t.Fatalf("owner: %d %s, want 501 %s", w.Code, w.Body.String(), reasonWallsViewUnavailable)
	}
	other := pvMember(t, "sub-walls-other").cookie
	w = doSSO(t, srv, http.MethodGet, buildPath(runWallsPattern, run.ID.String()), other, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("non-owner: %d %s, want 404", w.Code, w.Body.String())
	}
	assertSameAsMissing404(t, srv, http.MethodGet, runWallsPattern, other, "", w, "a non-owner")
}

// TestRunWallsCounterCap: a run with more rows than the reads may take is partial; one under the cap is not.
func TestRunWallsCounterCap(t *testing.T) {
	srv, st := runWallsFixture(t)
	srv.wallsCaps.readLimit = 5
	now := time.Now().UTC()
	busy, quiet := runWallsSeed(st, pvOwner, nil), runWallsSeed(st, pvOwner, nil)
	add := func(runID uuid.UUID, n int) {
		for range n {
			st.decisions = append(st.decisions, store.DecisionRow{ID: uuid.New(), RunID: runID, Seq: int64(len(st.decisions) + 1),
				RecordedAt: now, Time: now, DecidedAt: now, Action: "egress.allow", Host: "pypi.org", Port: 443,
				RuleSource: "policy:allowlist", Weight: 1})
		}
	}
	add(busy.ID, 11)
	add(quiet.ID, 4)
	for _, tc := range []struct {
		run     types.AgentRun
		partial bool
		allowed int64
	}{{busy, true, 10}, {quiet, false, 4}} {
		w, body := runWallsGet(t, srv, pvMember(t, pvOwner).cookie, tc.run.ID, "")
		if w.Code != http.StatusOK {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
		if body.Partial != tc.partial || body.Counters.Allowed != tc.allowed {
			t.Errorf("partial %v allowed %d, want %v %d", body.Partial, body.Counters.Allowed, tc.partial, tc.allowed)
		}
	}
}
