// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The plan-level test for composed profiles: a running child, after its BASE is edited, must honour
// what it inherits at every door that binds a run already going, and every door must fail closed when
// the chain cannot be read or composed. Each door is the real handler (or sweep), over a store double
// that serves the child's chain the way the one recursive statement does in Postgres. A door that
// still read the child's raw row would read empty limits and open.

// composedPair is a standalone base and a child composed on it with an empty overlay: the child
// carries no policy of its own, so everything it enforces is inherited.
func composedPair(baseCeiling types.RunPolicySpec, baseLimits types.GovernanceLimits) (base, child types.GovernanceProfile) {
	base = types.GovernanceProfile{ID: uuid.New(), Name: "company-baseline-secret-name", Ceiling: baseCeiling, Limits: baseLimits}
	child = types.GovernanceProfile{ID: uuid.New(), Name: "team", BaseProfileID: &base.ID, Overlay: &types.CeilingOverlay{}}
	return base, child
}

// chainScenario is one state of the stored graph a door is asked under.
type chainScenario struct {
	name     string
	profiles func(base, child types.GovernanceProfile) []types.GovernanceProfile
	listErr  error
}

// seededBadChains are the graphs every door must refuse: a base that cannot be read, a base the
// read left out (never a partial chain), a loop, and a chain four deep.
func seededBadChains() []chainScenario {
	return []chainScenario{
		{name: "the base read fails", listErr: errors.New("conn closed by peer"),
			profiles: func(b, c types.GovernanceProfile) []types.GovernanceProfile { return []types.GovernanceProfile{b, c} }},
		{name: "the base is missing from the read",
			profiles: func(_, c types.GovernanceProfile) []types.GovernanceProfile { return []types.GovernanceProfile{c} }},
		{name: "two profiles are each other's base", profiles: func(b, c types.GovernanceProfile) []types.GovernanceProfile {
			b.BaseProfileID, b.Overlay = &c.ID, &types.CeilingOverlay{}
			return []types.GovernanceProfile{b, c}
		}},
		{name: "a chain four deep", profiles: func(b, c types.GovernanceProfile) []types.GovernanceProfile {
			mid := types.GovernanceProfile{ID: uuid.New(), Name: "division", BaseProfileID: &b.ID, Overlay: &types.CeilingOverlay{}}
			top := types.GovernanceProfile{ID: uuid.New(), Name: "org", BaseProfileID: &mid.ID, Overlay: &types.CeilingOverlay{}}
			c.BaseProfileID = &top.ID
			return []types.GovernanceProfile{b, mid, top, c}
		}},
	}
}

// the attach door

func attachClosed(t *testing.T, profiles []types.GovernanceProfile, listErr error, childID uuid.UUID) (closed bool, w doorAnswer) {
	t.Helper()
	run := types.AgentRun{ID: uuid.New(), CreatedBy: "alice", State: types.RunRunning, SandboxRef: "sbx-1",
		Task: "make test", GovernanceProfileID: &childID}
	st := &profileListStore{profiles: profiles, err: listErr, authzStore: newAuthzStore()}
	st.authzStore.runs[run.ID] = run
	h := newHarness(t)
	cfg := baseTestConfig(h, st)
	cfg.Runner = &fakeRunner{}
	srv := New(cfg)
	tok, err := mintAttachTicket(context.Background(), st, run.ID, types.ActorHuman, "alice", oidc.RoleUser, time.Now())
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	rec := do(t, srv, http.MethodGet, "/api/v1/runs/"+run.ID.String()+"/attach?ticket="+tok, "", "")
	// Past the profile door the plain GET (no WebSocket handshake) is answered 426.
	return rec.Code != http.StatusUpgradeRequired, doorAnswer{rec: rec, audit: h.audit}
}

func TestComposedChild_AttachInheritsTheBase(t *testing.T) {
	base, child := composedPair(types.RunPolicySpec{}, types.GovernanceLimits{DenyInteractive: true})
	closed, w := attachClosed(t, []types.GovernanceProfile{base, child}, nil, child.ID)
	if !closed || w.rec.Code != http.StatusForbidden || w.reason() != string(authz.ReasonGovernanceProfile) {
		t.Fatalf("attach under a child of a deny_interactive base = %d %s, want 403 governance_profile", w.rec.Code, w.rec.Body.String())
	}
	if !recHasRefusal(w.audit, authz.ReasonGovernanceProfile, "runs.attach") {
		t.Error("no authz.denied governance_profile row for runs.attach")
	}
	// The refusal names the run's own profile and nothing about its base.
	if strings.Contains(w.rec.Body.String(), base.Name) {
		t.Errorf("the refusal names the base: %s", w.rec.Body.String())
	}

	openBase, openChild := composedPair(types.RunPolicySpec{}, types.GovernanceLimits{})
	if closed, w := attachClosed(t, []types.GovernanceProfile{openBase, openChild}, nil, openChild.ID); closed {
		t.Fatalf("attach under a child of an open base = %d %s, want it to pass the profile door", w.rec.Code, w.rec.Body.String())
	}

	for _, sc := range seededBadChains() {
		t.Run(sc.name, func(t *testing.T) {
			b, c := composedPair(types.RunPolicySpec{}, types.GovernanceLimits{})
			if closed, w := attachClosed(t, sc.profiles(b, c), sc.listErr, c.ID); !closed || w.rec.Code != http.StatusInternalServerError {
				t.Fatalf("attach = %d %s, want a closed door (500)", w.rec.Code, w.rec.Body.String())
			}
		})
	}
}

// doorAnswer is a door's answer beside the audit it wrote.
type doorAnswer struct {
	rec   *httptest.ResponseRecorder
	audit *recRecorder
}

func (a doorAnswer) reason() string {
	var body errorBody
	_ = json.Unmarshal(a.rec.Body.Bytes(), &body)
	return body.Reason
}

// the unsatisfiable composition: the deployment-borne base narrowed under an overlay

func TestComposedChild_UnsatisfiableRefusesAttachWithTheAuthzReason(t *testing.T) {
	base, child := composedPair(types.RunPolicySpec{AllowedMethods: []string{"GET"}}, types.GovernanceLimits{})
	post := []string{"POST"}
	child.Overlay = &types.CeilingOverlay{AllowedMethods: &post}
	closed, w := attachClosed(t, []types.GovernanceProfile{base, child}, nil, child.ID)
	if !closed || w.rec.Code != http.StatusForbidden || w.reason() != string(authz.ReasonGovernanceOverlayUnsatisfiable) {
		t.Fatalf("attach = %d %s, want 403 governance_overlay_unsatisfiable", w.rec.Code, w.rec.Body.String())
	}
	if !recHasRefusal(w.audit, authz.ReasonGovernanceOverlayUnsatisfiable, "runs.attach") {
		t.Error("the refusal was not audited as authz.denied")
	}
	var refusal errorBody
	_ = json.Unmarshal(w.rec.Body.Bytes(), &refusal)
	if strings.Contains(refusal.Error, base.Name) || !strings.Contains(refusal.Error, `"team"`) {
		t.Errorf("refusal body = %s, want the run's own profile named and the base not", w.rec.Body.String())
	}
}

// the SSH gateway

func TestComposedChild_SSHInheritsTheBase(t *testing.T) {
	dial := func(t *testing.T, profiles []types.GovernanceProfile, listErr error, childID uuid.UUID) error {
		st := newSSHMemStore()
		st.profiles, st.profilesErr = profiles, listErr
		runID := uuid.New()
		st.putRun(types.AgentRun{ID: runID, CreatedBy: "alice@example.com", State: types.RunRunning, SandboxRef: "sbx-1", GovernanceProfileID: &childID})
		priv, pub := mustSSHKeypair(t)
		st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: "alice@example.com",
			PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
		h := newSSHTestHarness(t, st, &sshFakeRunner{})
		client, err := sshDial(t, h, runID.String(), priv)
		if err == nil {
			_ = client.Close()
		}
		return err
	}
	base, child := composedPair(types.RunPolicySpec{}, types.GovernanceLimits{DenyInteractive: true})
	if err := dial(t, []types.GovernanceProfile{base, child}, nil, child.ID); err == nil {
		t.Fatal("ssh into a child of a deny_interactive base succeeded, want refused")
	}
	openBase, openChild := composedPair(types.RunPolicySpec{}, types.GovernanceLimits{})
	if err := dial(t, []types.GovernanceProfile{openBase, openChild}, nil, openChild.ID); err != nil {
		t.Fatalf("ssh into a child of an open base refused: %v", err)
	}
	for _, sc := range seededBadChains() {
		t.Run(sc.name, func(t *testing.T) {
			b, c := composedPair(types.RunPolicySpec{}, types.GovernanceLimits{})
			if err := dial(t, sc.profiles(b, c), sc.listErr, c.ID); err == nil {
				t.Fatal("ssh with an unresolvable chain succeeded, want refused")
			}
		})
	}
}

// the UI gateway

func TestComposedChild_UIAppsInheritTheBase(t *testing.T) {
	enter := func(profiles []types.GovernanceProfile, listErr error, childID uuid.UUID) (int, string) {
		h := newUIHarness(t, okBackend())
		h.store.profiles, h.store.profilesErr = profiles, listErr
		h.run.GovernanceProfileID = &childID
		h.store.putRun(h.run)
		rec := h.enter(url.Values{"run": {h.run.ID.String()}, "app": {"code"}, "ticket": {h.ticket(h.run.ID, h.owner, oidc.RoleUser)}})
		return rec.Code, rec.Body.String()
	}
	base, child := composedPair(types.RunPolicySpec{}, types.GovernanceLimits{DenyUIApps: true})
	if code, body := enter([]types.GovernanceProfile{base, child}, nil, child.ID); code != http.StatusForbidden || strings.Contains(body, base.Name) {
		t.Fatalf("UI gateway under a child of a deny_ui_apps base = %d %s, want 403 naming only the child", code, body)
	}
	openBase, openChild := composedPair(types.RunPolicySpec{}, types.GovernanceLimits{})
	if code, body := enter([]types.GovernanceProfile{openBase, openChild}, nil, openChild.ID); code != http.StatusFound {
		t.Fatalf("UI gateway under a child of an open base = %d %s, want 302", code, body)
	}
	for _, sc := range seededBadChains() {
		t.Run(sc.name, func(t *testing.T) {
			b, c := composedPair(types.RunPolicySpec{}, types.GovernanceLimits{})
			if code, body := enter(sc.profiles(b, c), sc.listErr, c.ID); code != http.StatusInternalServerError {
				t.Fatalf("UI gateway = %d %s, want a closed door (500)", code, body)
			}
		})
	}
}

// the limits re-clamp

func TestComposedChild_ReclampTightensToTheBase(t *testing.T) {
	captured := types.RunLimits{MaxEndAheadSec: 30 * 86400, MaxWaitSec: 3600, UserChangesLimits: true}
	setup := func(t *testing.T, base, child types.GovernanceProfile, extra ...types.GovernanceProfile) (*endWaitFixture, *reclampStore) {
		f, st, _ := newReclampFixture(t, captured, 20*day, 3600)
		id := child.ID
		st.run.GovernanceProfileID = &id
		st.profiles = append([]types.GovernanceProfile{base, child}, extra...)
		return f, st
	}
	base, child := composedPair(types.RunPolicySpec{}, types.GovernanceLimits{RunLimits: types.RunLimits{MaxEndAheadSec: 2 * 86400, MaxWaitSec: 3600}})
	f, st := setup(t, base, child)
	sweepLimits(t, f)
	if got := st.current().RunLimits; got.MaxEndAheadSec != 2*86400 {
		t.Fatalf("captured limits after the sweep = %+v, want the BASE's 2-day max inherited by the child", got)
	}

	// A base the read cannot reach changes nothing, and the sweep says so rather than guess.
	f, st = setup(t, base, child)
	f.srv.cfg.Store = profileErrStore{reclampStore: st, err: errors.New("conn closed by peer")}
	if err := f.srv.sweepRunLimits(context.Background()); err == nil {
		t.Error("a sweep whose profile read failed reported success")
	}
	if got := st.current().RunLimits; got != captured {
		t.Errorf("limits after a failed read = %+v, want the captured ones untouched", got)
	}

	// A chain that cannot be composed leaves that profile's runs on what they captured.
	b2, c2 := composedPair(types.RunPolicySpec{}, types.GovernanceLimits{RunLimits: types.RunLimits{MaxEndAheadSec: 86400}})
	b2.BaseProfileID, b2.Overlay = &c2.ID, &types.CeilingOverlay{}
	f, st = setup(t, b2, c2)
	sweepLimits(t, f)
	if got := st.current().RunLimits; got != captured {
		t.Errorf("limits under a loop = %+v, want the captured ones untouched", got)
	}
}

// end extension

func TestComposedChild_EndExtensionReadsTheChain(t *testing.T) {
	captured := types.RunLimits{MaxEndAheadSec: 2 * 86400, MaxWaitSec: 3600, UserChangesLimits: true}
	run := func(t *testing.T, profiles []types.GovernanceProfile, err error, childID uuid.UUID, body func(*endWaitFixture) string) (int, map[string]any, string, *endWaitFixture) {
		f, st, _ := newReclampFixture(t, captured, day, 1800)
		st.run.GovernanceProfileID = &childID
		st.profiles = profiles
		f.srv.cfg.Store = profileErrStore{reclampStore: st, err: err}
		code, m, raw := f.patchRaw(t, ownerSession(t), body(f))
		return code, m, raw, f
	}
	week := func(f *endWaitFixture) string { return endsAtBody(f.now.Add(10 * day)) }

	// The cap flag compares against the child's EFFECTIVE max (the base's one day), so it is not
	// "loosened"; the child's own raw limits are empty, which would read as no limit and say it was.
	base, child := composedPair(types.RunPolicySpec{}, types.GovernanceLimits{RunLimits: types.RunLimits{MaxEndAheadSec: 86400}})
	code, m, raw, _ := run(t, []types.GovernanceProfile{base, child}, nil, child.ID, week)
	if code != http.StatusOK {
		t.Fatalf("extend under a composed profile = %d %s, want 200 capped", code, raw)
	}
	if _, present := m["ends_cap_loosened"]; present {
		t.Errorf("ends_cap_loosened set although the base's cap is tighter than the captured one: %s", raw)
	}
	looseBase, looseChild := composedPair(types.RunPolicySpec{}, types.GovernanceLimits{RunLimits: types.RunLimits{MaxEndAheadSec: 30 * 86400}})
	if _, m, raw, _ := run(t, []types.GovernanceProfile{looseBase, looseChild}, nil, looseChild.ID, week); m["ends_cap_loosened"] != true {
		t.Errorf("ends_cap_loosened missing although the base's cap is looser than the captured one: %s", raw)
	}

	// An extension re-reads the owner's profile through the chain and refuses when it cannot.
	for _, sc := range seededBadChains() {
		t.Run(sc.name, func(t *testing.T) {
			b, c := composedPair(types.RunPolicySpec{}, types.GovernanceLimits{})
			code, _, raw, f := run(t, sc.profiles(b, c), sc.listErr, c.ID, week)
			if code != http.StatusServiceUnavailable && code != http.StatusConflict {
				t.Fatalf("extend = %d %s, want a refusal (503 or 409)", code, raw)
			}
			if end, _ := f.stored(); end == nil || !end.Equal(f.now.Add(day)) {
				t.Errorf("stored end = %v; a refused extension moved it", end)
			}
		})
	}

	// An unsatisfiable composition is the authz reason, not an unreadable profile.
	ub, uc := composedPair(types.RunPolicySpec{AllowedMethods: []string{"GET"}}, types.GovernanceLimits{})
	post := []string{"POST"}
	uc.Overlay = &types.CeilingOverlay{AllowedMethods: &post}
	code, _, raw, f := run(t, []types.GovernanceProfile{ub, uc}, nil, uc.ID, week)
	if code != http.StatusForbidden {
		t.Fatalf("extend = %d %s, want 403", code, raw)
	}
	rows := f.rows(t, "run.end.set")
	if len(rows) != 1 || rows[0].Outcome != "denied" || leaseAuditData(t, rows[0])["reason"] != "governance_overlay_unsatisfiable" {
		t.Errorf("run.end.set rows = %+v, want one denied row with reason governance_overlay_unsatisfiable", rows)
	}
}

// revive

func TestComposedChild_ReviveReassertsTheBaseDenies(t *testing.T) {
	arrange := func(f *reviveFixture, profiles []types.GovernanceProfile, childID uuid.UUID) {
		f.st.run.GovernanceProfileID = &childID
		f.rs.profiles = profiles
		var cfg map[string]any
		_ = json.Unmarshal(f.rs.cfg, &cfg)
		cfg["git_grants"] = map[string]string{"acme/app": uuid.NewString()}
		f.rs.cfg, _ = json.Marshal(cfg)
	}
	// The BASE denies github.com while the run holds a brokered git grant for it: the same refusal a
	// standalone profile gives, reached through the child.
	f := newReviveFixture(t)
	base, child := composedPair(types.RunPolicySpec{DeniedDomains: []string{"github.com"}}, types.GovernanceLimits{})
	arrange(f, []types.GovernanceProfile{base, child}, child.ID)
	if code := f.revive(t); code != http.StatusConflict || len(f.rr.replaced) != 0 {
		t.Fatalf("revive under a child of a base that denies github.com = %d (replaced %d), want 409 and no proxy", code, len(f.rr.replaced))
	}

	// A chain that cannot be read or composed refuses: 503 when the read fails, 409 when it was cut.
	for _, sc := range seededBadChains() {
		t.Run(sc.name, func(t *testing.T) {
			f := newReviveFixture(t)
			b, c := composedPair(types.RunPolicySpec{}, types.GovernanceLimits{})
			arrange(f, sc.profiles(b, c), c.ID)
			if sc.listErr != nil {
				f.srv.cfg.Store = &reviveErrStore{reviveStore: f.rs, err: sc.listErr}
			}
			if code := f.revive(t); code != http.StatusServiceUnavailable && code != http.StatusConflict {
				t.Fatalf("revive = %d, want a refusal (503 or 409)", code)
			}
			if len(f.rr.replaced) != 0 {
				t.Error("a refused revive replaced the proxy")
			}
		})
	}
}

// reviveErrStore is the revive store whose profile read fails.
type reviveErrStore struct {
	*reviveStore
	err error
}

func (s *reviveErrStore) GetGovernanceProfileChain(context.Context, uuid.UUID) ([]types.GovernanceProfile, error) {
	return nil, s.err
}
