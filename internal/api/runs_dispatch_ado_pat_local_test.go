// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The minted-PAT lane on a run placed on a self-hosted runner (option L): the grants are recorded
// runner_resident, the runner's proxy fetches the per-run token through the relay exactly as the cluster
// sidecar does, and every relayed resolve is judged again against the delivery policy, the runner's trust
// class and posture and the connection's opt-in as they are then.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// adoResidentPolicy delivers ado_minted_pat runner_resident on every trust class.
const adoResidentPolicy = `{"classes":{"ado_minted_pat":{"mode":"runner_resident"}}}`

// adoLocalStore is the minted-PAT fixture's store plus the run's self-hosted runner and the stored
// delivery policy, both changeable mid-test.
type adoLocalStore struct {
	*adoPATStore
	mu     sync.Mutex
	runner types.Runner
	policy string
}

func (s *adoLocalStore) GetRunner(_ context.Context, id uuid.UUID) (types.Runner, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id != s.runner.ID {
		return types.Runner{}, store.ErrNotFound
	}
	return s.runner, nil
}

func (s *adoLocalStore) GetCredentialDeliveryPolicy(context.Context) (types.CredentialDeliveryPolicyDoc, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw := s.policy
	if raw == "" {
		raw = `{"classes":{}}`
	}
	return types.CredentialDeliveryPolicyDoc{Policy: json.RawMessage(raw)}, nil
}

func (s *adoLocalStore) set(f func(*adoLocalStore)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

// adoLocalFixture is the minted-PAT lane on a run bound to a personal runner that reported its posture.
type adoLocalFixture struct {
	*adoPATFixture
	ls     *adoLocalStore
	runner uuid.UUID
	env    map[string]string
}

// newADOLocalLane is everything up to dispatch, with the row opted in when optIn and policy stored.
func newADOLocalLane(t *testing.T, policy string, optIn bool) *adoLocalFixture {
	t.Helper()
	fx := newADOPATLane(t)
	id := uuid.New()
	reported := adoTestNow
	ls := &adoLocalStore{adoPATStore: fx.st, policy: policy,
		runner: types.Runner{ID: id, TrustClass: types.RunnerTrustPersonal, OwnerKind: types.RunnerOwnerPerson, PostureReportedAt: &reported, Posture: types.RunnerPosture{OS: "linux"}}}
	fx.srv.cfg.Store = ls
	fx.st.edit(fx.run.ID, func(r *types.AgentRun) { r.Placement, r.RunnerID = types.PlacementLocal, &id })
	fx.run, _ = fx.st.GetRun(context.Background(), fx.run.ID)
	fx.st.site.WorkspaceProviders.Git[0].Entra.RunnerResident = optIn
	return &adoLocalFixture{adoPATFixture: fx, ls: ls, runner: id, env: map[string]string{}}
}

// dispatchLocal authors the lane into lf.env, the sandbox's environment.
func (lf *adoLocalFixture) dispatchLocal(t *testing.T) bool {
	t.Helper()
	_, _, ok := lf.srv.authorADOEntraInjection(context.Background(), lf.run, lf.ado, "CERT", "KEY", &types.RunPolicySpec{}, lf.env, nil)
	for _, g := range lf.st.grants {
		var sc struct{ Host string }
		_ = json.Unmarshal(g.Spec.Scope, &sc)
		lf.grants[sc.Host] = g.ID
	}
	return ok
}

func newADOLocalFixture(t *testing.T) *adoLocalFixture {
	t.Helper()
	lf := newADOLocalLane(t, adoResidentPolicy, true)
	if !lf.dispatchLocal(t) {
		t.Fatalf("dispatch refused: %q", lf.st.hint)
	}
	return lf
}

// relayed is one resolve for host as the runner's proxy makes it: through the relay from runner, past the
// relayed delivery rule, then the lane's resolve. injection false is the mint route instead.
func (lf *adoLocalFixture) relayed(t *testing.T, runner uuid.UUID, host string, q url.Values, injection bool) (*httptest.ResponseRecorder, types.ResolvedInjection) {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/internal/injection/"+lf.grants[host].String()+"?"+q.Encode(), nil)
	r = r.WithContext(relayConnContext(r.Context(), relayedConn{runner: runner}))
	claims := &identity.Claims{RunID: lf.run.ID, Sub: lf.subject, SPIFFEID: "spiffe://wardyn.local/run"}
	var resp types.ResolvedInjection
	if lf.srv.refuseRelayedDelivery(w, r, claims, lf.grants[host], injection) {
		return w, resp
	}
	lf.srv.resolveADOInjection(w, r, claims,
		broker.Minted{JTI: "jti-" + host, Injection: &egress.InjectionRule{Host: host, SecretName: types.ADOEntraAccessTokenSecret}}, lf.grants[host])
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return w, resp
}

func (lf *adoLocalFixture) relayedOK(t *testing.T, host string, q url.Values) types.ResolvedInjection {
	t.Helper()
	w, resp := lf.relayed(t, lf.runner, host, q, true)
	if w.Code != http.StatusOK {
		t.Fatalf("relayed resolve %s: status %d body %q", host, w.Code, w.Body.String())
	}
	return resp
}

func assertRelayRefused(t *testing.T, w *httptest.ResponseRecorder, what string) {
	t.Helper()
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), string(placement.ReasonDeliveryRunnerResident)) {
		t.Fatalf("%s: status %d body %q, want 403 %s", what, w.Code, w.Body.String(), placement.ReasonDeliveryRunnerResident)
	}
}

// With the policy and the opt-in, dispatch records every lane grant runner_resident, the runner's relayed
// resolve is served the per-run token, and the sandbox's environment holds only the inert placeholder. The
// mint route is not a way to the grant.
func TestMintedPATLocal_ResidentIsServedThroughTheRelay(t *testing.T) {
	lf := newADOLocalFixture(t)
	if len(lf.st.grants) != len(adoEntraHosts("contoso")) {
		t.Fatalf("%d grants, want one per host", len(lf.st.grants))
	}
	for _, g := range lf.st.grants {
		if g.Delivery != types.GrantDeliveryResident {
			t.Fatalf("grant %s delivery %q, want %q", g.ID, g.Delivery, types.GrantDeliveryResident)
		}
	}
	got := lf.relayedOK(t, "dev.azure.com", nil)
	if lf.pats.createCount() != 1 || got.JTI == "" || got.Value != adoRunPATValue(lf.pats.tokens[0]) {
		t.Fatalf("relayed resolve = %+v (creates %d), want the run's one token", got, lf.pats.createCount())
	}
	if lf.env[adoEntraPlaceholderEnv] != adoEntraPlaceholderValue {
		t.Errorf("sandbox %s = %q, want the inert placeholder", adoEntraPlaceholderEnv, lf.env[adoEntraPlaceholderEnv])
	}
	for k, v := range lf.env {
		for _, tok := range lf.pats.tokens {
			if strings.Contains(v, tok) {
				t.Errorf("sandbox env %s holds the token", k)
			}
		}
	}
	w, _ := lf.relayed(t, lf.runner, "dev.azure.com", nil, false)
	assertRelayRefused(t, w, "the mint route")
}

// Every relayed resolve is judged again: a policy flipped to refuse, a posture requirement the runner does
// not meet, a trust class the policy does not deliver it to, an organization's runner, an opt-in withdrawn,
// or another runner each refuses the next resolve. A refusal the judgement gave (not a failed read)
// revokes the run's tokens at Azure DevOps with reason drift; restored, a fresh token is served.
func TestMintedPATLocal_RelayRejudgesEveryResolve(t *testing.T) {
	lf := newADOLocalFixture(t)
	lf.relayedOK(t, "dev.azure.com", nil)
	managedOnly := `{"classes":{"ado_minted_pat":{"mode":"refuse"}},"by_trust":{"managed_device":{"classes":{"ado_minted_pat":{"mode":"runner_resident"}}}}}`
	for name, tc := range map[string]struct {
		flip    func(*adoLocalStore)
		revokes bool
	}{
		"policy refuse": {func(s *adoLocalStore) { s.policy = `{"classes":{"ado_minted_pat":{"mode":"refuse"}}}` }, true},
		"posture unmet": {func(s *adoLocalStore) {
			s.policy = `{"classes":{"ado_minted_pat":{"mode":"runner_resident","require_posture":{"disk_encrypted":true}}}}`
		}, true},
		"personal, managed-only":  {func(s *adoLocalStore) { s.policy = managedOnly }, true},
		"organization's runner":   {func(s *adoLocalStore) { s.runner.OwnerKind = types.RunnerOwnerOrg }, true},
		"unknown trust class":     {func(s *adoLocalStore) { s.runner.TrustClass = "" }, false}, // the plan's facts cannot be read
		"opt-in withdrawn":        {func(*adoLocalStore) { lf.st.site.WorkspaceProviders.Git[0].Entra.RunnerResident = false }, true},
		"row no longer minted":    {func(*adoLocalStore) { lf.st.site.WorkspaceProviders.Git[0].Entra.TokenMode = types.ADOTokenModeBearer }, true},
		"connection row disabled": {func(*adoLocalStore) { lf.st.site.WorkspaceProviders.Git[0].Disabled = true }, true},
	} {
		t.Run(name, func(t *testing.T) {
			var policy string
			var rn types.Runner
			lf.ls.set(func(s *adoLocalStore) { policy, rn = s.policy, s.runner })
			site, disabled := *lf.st.site.WorkspaceProviders.Git[0].Entra, lf.st.site.WorkspaceProviders.Git[0].Disabled
			lf.ls.set(tc.flip)
			lf.audit.rows = nil
			w, _ := lf.relayed(t, lf.runner, "dev.azure.com", nil, true)
			assertRelayRefused(t, w, name)
			if live := lf.st.unrevoked(t); tc.revokes != (len(live) == 0) {
				t.Fatalf("live tokens after the refusal = %d, want revoked %v", len(live), tc.revokes)
			}
			if got := lf.auditReasons(adoPATAuditRevoke); tc.revokes && (len(got) == 0 || slices.ContainsFunc(got, func(r string) bool { return r != adoPATRevokeDrift })) {
				t.Fatalf("revoke reasons = %v, want drift", got)
			}
			lf.ls.set(func(s *adoLocalStore) { s.policy, s.runner = policy, rn })
			*lf.st.site.WorkspaceProviders.Git[0].Entra = site
			lf.st.site.WorkspaceProviders.Git[0].Disabled = disabled
			lf.relayedOK(t, "dev.azure.com", nil)
		})
	}
	w, _ := lf.relayed(t, uuid.New(), "dev.azure.com", nil, true)
	assertRelayRefused(t, w, "another runner")
}

// A connection's opt-in withdrawn mid-run: the next relayed resolve refuses, and every token the run held
// is revoked at Azure DevOps with reason drift, so the one the proxy still holds is dead at the provider.
func TestMintedPATLocal_WithdrawnOptInRevokesWithDrift(t *testing.T) {
	lf := newADOLocalFixture(t)
	held := lf.relayedOK(t, "dev.azure.com", nil)
	lf.st.site.WorkspaceProviders.Git[0].Entra.RunnerResident = false
	lf.audit.rows = nil
	w, _ := lf.relayed(t, lf.runner, "dev.azure.com", nil, true)
	assertRelayRefused(t, w, "opt-in withdrawn")
	if live := lf.st.unrevoked(t); len(live) != 0 {
		t.Fatalf("live tokens = %+v, want none", live)
	}
	if !slices.Contains(lf.pats.revokedIDs(), held.JTI) {
		t.Fatalf("revoked at Azure DevOps %v, want the held token %s", lf.pats.revokedIDs(), held.JTI)
	}
	if got := lf.auditReasons(adoPATAuditRevoke); !slices.Equal(got, []string{adoPATRevokeDrift}) {
		t.Fatalf("revoke reasons = %v, want [drift]", got)
	}
}

// Off by default: with no policy row, with no opt-in, on a trust class the policy does not deliver it
// to, on an organization's runner of the same trust class, or for a bearer row, the grant write refuses
// and the run fails with no grant recorded.
func TestMintedPATLocal_OffByDefault(t *testing.T) {
	for name, tc := range map[string]struct {
		policy string
		optIn  bool
		bearer bool
		org    bool
	}{
		"no policy row":         {"", true, false, false},
		"no opt-in":             {adoResidentPolicy, false, false, false},
		"managed_device only":   {`{"classes":{"ado_minted_pat":{"mode":"refuse"}},"by_trust":{"managed_device":{"classes":{"ado_minted_pat":{"mode":"runner_resident"}}}}}`, true, false, false},
		"bearer, opted in":      {adoResidentPolicy, true, true, false},
		"bearer, no policy row": {"", true, true, false},
		"organization's runner": {adoResidentPolicy, true, false, true},
	} {
		t.Run(name, func(t *testing.T) {
			lf := newADOLocalLane(t, tc.policy, tc.optIn)
			lf.st.edit(lf.run.ID, func(r *types.AgentRun) { r.State = types.RunStarting })
			if tc.org {
				lf.ls.set(func(s *adoLocalStore) { s.runner.OwnerKind = types.RunnerOwnerOrg })
			}
			if tc.bearer {
				lf.st.site.WorkspaceProviders.Git[0].Entra.TokenMode = types.ADOTokenModeBearer
				lf.ado.tokenMode = types.ADOTokenModeBearer
			}
			if lf.dispatchLocal(t) {
				t.Fatal("dispatch admitted the lane")
			}
			if len(lf.st.grants) != 0 {
				t.Fatalf("%d grants recorded, want none", len(lf.st.grants))
			}
			if !strings.Contains(lf.st.hint, "could not record the Azure DevOps credential grant") {
				t.Fatalf("hint %q, want the grant_write refusal", lf.st.hint)
			}
		})
	}
}

// The relay serves no other runner_resident grant: a resident env_secret or operator api_key still answers
// delivery_runner_resident.
func TestMintedPATLocal_OtherResidentGrantsStayRefused(t *testing.T) {
	lf := newADOLocalFixture(t)
	for kind, scope := range map[types.GrantKind]string{
		types.GrantEnvSecret: `{"name":"TOKEN","secret":"ops-token"}`,
		types.GrantAPIKey:    `{"host":"api.example.com","header":"Authorization","secret_name":"ops-key"}`,
	} {
		g := types.CredentialGrant{ID: uuid.New(), RunID: lf.run.ID, Delivery: types.GrantDeliveryResident,
			Spec: types.GrantSpec{Kind: kind, Scope: json.RawMessage(scope)}}
		if _, err := lf.st.CreateGrant(context.Background(), g); err != nil {
			t.Fatal(err)
		}
		lf.grants[string(kind)] = g.ID
		w, _ := lf.relayed(t, lf.runner, string(kind), nil, true)
		assertRelayRefused(t, w, string(kind))
	}
}

// Through the relay the lane keeps the cluster's lifecycle, its answers expiring within residentRejudgeEvery:
// a re-resolve in between gets the same token, renewal in the window, pause revokes and refuses,
// resume mints afresh, and the run's end revokes every token with reason run_end and refuses the next resolve.
func TestMintedPATLocal_RenewalPauseResumeAndRunEnd(t *testing.T) {
	ctx := context.Background()
	lf := newADOLocalFixture(t)
	first := lf.relayedOK(t, "dev.azure.com", nil)
	live := lf.st.unrevoked(t)
	if len(live) != 1 {
		t.Fatalf("live tokens = %d, want 1", len(live))
	}
	// The relayed answer expires within residentRejudgeEvery, long before the token does, so the proxy
	// re-resolves and is judged again; inside the window it is handed the same token.
	if limit := lf.clock().Add(residentRejudgeEvery); time.UnixMilli(first.ExpiresAt).After(limit) || !live[0].ValidTo.After(limit) {
		t.Fatalf("relayed expiry %v, token validTo %v: want the expiry at most %v and the token outliving it", time.UnixMilli(first.ExpiresAt), live[0].ValidTo, limit)
	}
	lf.setClock(lf.clock().Add(residentRejudgeEvery))
	if again := lf.relayedOK(t, "dev.azure.com", nil); again.JTI != first.JTI || lf.pats.createCount() != 1 {
		t.Fatalf("re-resolve after the interval: JTI %s creates %d, want the same token and no create", again.JTI, lf.pats.createCount())
	}
	lf.setClock(live[0].ValidTo.Add(-adoRunPATRenewWindow + time.Minute))
	renewed := lf.relayedOK(t, "dev.azure.com", nil)
	if renewed.JTI == first.JTI || lf.pats.createCount() != 2 {
		t.Fatalf("in the renew window: JTI %s creates %d, want a new token", renewed.JTI, lf.pats.createCount())
	}

	lf.srv.cfg.Runner = &pauseRunner{fakeRunner: &fakeRunner{}, freeze: map[types.ConfinementClass]bool{types.CC1: true}}
	run, _ := lf.st.GetRun(ctx, lf.run.ID)
	lf.srv.pauseRun(ctx, lf.st, run, types.PauseIdle, time.Hour)
	if live := lf.st.unrevoked(t); len(live) != 0 {
		t.Fatalf("live rows after pause = %+v, want none", live)
	}
	if w, _ := lf.relayed(t, lf.runner, "dev.azure.com", stale(renewed.JTI), true); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), reasonADOPATRunInactive) {
		t.Fatalf("paused relayed resolve: status %d body %q, want 409 %s", w.Code, w.Body.String(), reasonADOPATRunInactive)
	}
	run, _ = lf.st.GetRun(ctx, lf.run.ID)
	if err := lf.srv.resumeRun(ctx, lf.st, run, types.ActorSystem, "wardynd", "test"); err != nil {
		t.Fatal(err)
	}
	resumed := lf.relayedOK(t, "dev.azure.com", stale(renewed.JTI))
	if resumed.JTI == renewed.JTI {
		t.Fatalf("after resume: JTI %s, want a fresh token", resumed.JTI)
	}

	lf.audit.rows = nil
	lf.srv.failAndRevoke(ctx, lf.run.ID, types.RunRunning, "exec failed")
	if live := lf.st.unrevoked(t); len(live) != 0 {
		t.Fatalf("live rows after run end = %+v, want none", live)
	}
	if got := lf.auditReasons(adoPATAuditRevoke); len(got) == 0 || slices.ContainsFunc(got, func(r string) bool { return r != adoPATRevokeRunEnd }) {
		t.Fatalf("revoke reasons = %v, want run_end", got)
	}
	creates := lf.pats.createCount()
	if w, _ := lf.relayed(t, lf.runner, "dev.azure.com", stale(resumed.JTI), true); w.Code == http.StatusOK || lf.pats.createCount() != creates {
		t.Fatalf("relayed resolve after run end: status %d creates %d, want refused and no create", w.Code, lf.pats.createCount())
	}
}

// Only a minted_pat connection's opt-in counts at admission: a bearer row's opt-in leaves the lane refused.
func TestLocalADOOrigin_OnlyMintedPATCarriesTheOptIn(t *testing.T) {
	s := &Server{}
	for mode, want := range map[types.ADOTokenMode]bool{types.ADOTokenModeMintedPAT: true, types.ADOTokenModeBearer: false} {
		o := s.localADOOrigin(context.Background(), "p", adoEntraRun{rowID: "ado-row-1", owner: "p", tokenMode: mode, residentOptIn: true})
		if o.Class != placement.ClassBrokered || o.Delivery != placement.ClassADOMintedPAT || o.ResidentOptIn != want {
			t.Errorf("%s: origin %+v, want brokered ado_minted_pat with opt-in %v", mode, o, want)
		}
	}
}

// At create a plan holding an Azure DevOps minted token runner_resident is judged on the owner kind of the
// class's runners: a class whose runners are all an organization's refuses it, a class with a person's own
// admits it and marks the bind to keep only a person's own runners.
func TestLocalADOResident_AdmittedOnlyOnAPersonsOwnRunner(t *testing.T) {
	origin := placement.CredentialOrigin{Class: placement.ClassBrokered, Delivery: placement.ClassADOMintedPAT, ResidentOptIn: true}
	p := placement.LocalPlan{
		CredentialIntents: []placement.CredentialIntent{{Field: "ProxyConfig.ADOGrant[ado-row-1]", Origin: origin}},
		Delivery:          placement.DeliveryPolicy{Classes: map[string]placement.ClassPolicy{placement.ClassADOMintedPAT: {Mode: placement.ModeRunnerResident}}},
		Posture:           &types.RunnerPosture{OS: "linux"},
	}
	org, person := trustTarget(types.RunnerTrustPersonal), trustTarget(types.RunnerTrustPersonal)
	org.ownerKind, person.ownerKind = types.RunnerOwnerOrg, types.RunnerOwnerPerson
	personal := []types.RunnerTrustClass{types.RunnerTrustPersonal}
	if c, ref := judgeAtTrust(p, personal, trustOwners([]poolTarget{org})); c != "" || ref == nil || !strings.Contains(ref.Detail, "a person's own runner") {
		t.Fatalf("only an organization's runner: %q %+v, want refused", c, ref)
	}
	if c, ref := judgeAtTrust(p, personal, trustOwners([]poolTarget{org, person})); c != types.RunnerTrustPersonal || ref != nil {
		t.Fatalf("a person's own runner among them: %q %+v, want admitted", c, ref)
	}
	rp := runPlacement{eligible: []poolTarget{org, person}}
	rp.narrowToPersonOwned()
	if len(rp.eligible) != 1 || *rp.eligible[0].runnerID != *person.runnerID {
		t.Fatalf("narrowed to %+v, want only the person's own runner", rp.eligible)
	}
}
