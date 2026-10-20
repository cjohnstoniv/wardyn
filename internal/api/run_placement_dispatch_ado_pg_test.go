// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// An Azure DevOps lane on a run the pool places on the person's own runner, against a real Postgres and
// the real broker. Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	secretspg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// adoLocalEnv is a placement env whose site holds the own-token row and whose server reads the person's
// namespace from a real secret store through the real broker.
func adoLocalEnv(t *testing.T, row types.GitProvider) (*placementEnv, *secretspg.Store) {
	t.Helper()
	e := newPlacementEnv(t)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	sec, err := secretspg.New(e.pool, id)
	if err != nil {
		t.Fatal(err)
	}
	e.srv.cfg.Secrets = sec
	e.srv.cfg.Broker = broker.New(broker.NewPgxStore(e.pool), sec, e.h.audit, e.h.idp, nil)
	e.srv.router = e.srv.routes() // the injection resolve is mounted only with a secret store
	ctx := context.Background()
	sc, err := e.pg.GetSiteConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sc.WorkspaceProviders = &types.WorkspaceProviders{Git: []types.GitProvider{row}}
	if _, err := e.pg.PutSiteConfig(ctx, sc); err != nil {
		t.Fatal(err)
	}
	return e, sec
}

// adoLaneGrants is the run's Azure DevOps injection grants.
func adoLaneGrants(t *testing.T, e *placementEnv, runID uuid.UUID) map[string]types.CredentialGrant {
	t.Helper()
	rows, err := e.pg.ListGrantsByRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]types.CredentialGrant{}
	for _, g := range rows {
		var sc struct {
			Host       string `json:"host"`
			SecretName string `json:"secret_name"`
		}
		if g.Spec.Kind == types.GrantAPIKey && json.Unmarshal(g.Spec.Scope, &sc) == nil && sc.SecretName == types.ADOEntraAccessTokenSecret {
			out[sc.Host] = g
		}
	}
	return out
}

// A person's own PAT on their own runner: create admits it, dispatch writes the lane's grants as own,
// the runner's relayed resolve is served, and the sandbox holds only the inert placeholder.
func TestPG_LocalRun_ADOOwnPATIsDeliveredOwn(t *testing.T) {
	e, sec := adoLocalEnv(t, ownPATTestRow())
	ctx := context.Background()
	raw, _ := json.Marshal(adoOwnPATBlob{Token: ownPATToken, Org: "contoso", ExpiresOn: time.Now().UTC().AddDate(0, 0, 5), StoredAt: time.Now().UTC()})
	if err := sec.For(e.personP).Put(secretstore.WithPurpose(ctx, secretstore.PurposeStatus), adoOwnPATSecretName(ownPATRowID), raw); err != nil {
		t.Fatal(err)
	}
	laptops := e.createPool("Laptops", types.RunnerPoolUser)
	awake := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2, types.CC3)
	e.addToPool(e.person, laptops.ID, awake)
	e.stream(awake, types.CC1, types.CC2, types.CC3)
	e.personDefaults(e.person, fmt.Sprintf(`{"pool_id":%q}`, laptops.ID))

	run := e.localCreate(`{"agent":"claude-code","repo":"` + adoTestRepo + `","task":"t"}`)
	if run.Placement != types.PlacementLocal || run.RunnerID == nil || *run.RunnerID != awake {
		t.Fatalf("row = placement %q runner %v, want local on %s", run.Placement, run.RunnerID, awake)
	}
	spec := e.dispatchedSpec(run.ID)

	grants := adoLaneGrants(t, e, run.ID)
	if len(grants) != len(adoContosoHosts) {
		t.Fatalf("%d Azure DevOps grants, want one per host (%d)", len(grants), len(adoContosoHosts))
	}
	for host, g := range grants {
		if g.Delivery != types.GrantDeliveryOwn {
			t.Errorf("%s grant persisted delivery %q, want %q", host, g.Delivery, types.GrantDeliveryOwn)
		}
	}

	g, ok := grants["dev.azure.com"]
	if !ok {
		t.Fatalf("no dev.azure.com grant among %v", grants)
	}
	tok, err := e.srv.cfg.Identity.MintRunIdentity(ctx, run.ID, e.personP, "", internalAudience, false)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/internal/injection/"+g.ID.String(), nil)
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req = req.WithContext(relayConnContext(req.Context(), relayedConn{runner: awake}))
	w := httptest.NewRecorder()
	e.srv.relayRoutes(e.srv.InternalHandler()).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("relayed injection = %d %s, want 200", w.Code, w.Body)
	}

	if got := spec.Env[adoEntraPlaceholderEnv]; got != adoEntraPlaceholderValue {
		t.Errorf("sandbox %s = %q, want the inert placeholder", adoEntraPlaceholderEnv, got)
	}
	for k, v := range spec.Env {
		if strings.Contains(v, ownPATToken) {
			t.Errorf("sandbox env %s holds the PAT", k)
		}
	}
	for k, v := range spec.SecretEnv {
		if strings.Contains(v, ownPATToken) {
			t.Errorf("sandbox secret env %s holds the PAT", k)
		}
	}
}

// Every other token mode is minted by the organisation, so a local run's Azure DevOps grant write still
// refuses and the run fails: no grant is recorded.
func TestPG_LocalRun_ADOOrgTokenGrantWriteRefuses(t *testing.T) {
	for _, mode := range []types.ADOTokenMode{types.ADOTokenModeMintedPAT, types.ADOTokenModeBearer} {
		t.Run(string(mode), func(t *testing.T) {
			row := ownPATTestRow()
			row.Entra.TokenMode = mode
			e, _ := adoLocalEnv(t, row)
			ctx := context.Background()
			laptop := e.claimedRunner(e.personP)
			now := time.Now().UTC()
			run, err := e.pg.CreateRun(ctx, types.AgentRun{ID: uuid.New(), CreatedAt: now, UpdatedAt: now, CreatedBy: e.personP, Agent: "claude-code",
				ConfinementClass: types.CC1, State: types.RunStarting, SPIFFEID: "spiffe://wardyn.local/agent-run/" + uuid.NewString(),
				RunnerTarget: "docker", Placement: types.PlacementLocal, RunnerID: &laptop})
			if err != nil {
				t.Fatal(err)
			}
			snapshot := adoEntraScopeSnapshot{ProviderRowID: row.ID, Organisation: "contoso", OwnerSubject: e.personP,
				CredentialSource: string(types.CredentialSourcePerUser), TokenMode: string(mode), Capabilities: row.Entra.DefaultProfile}
			assertADOGrantWriteRefused(t, e, run, snapshot)
		})
	}
}

// An own PAT that is not the run owner's own (the snapshot names another person, or the PAT sits in
// another person's namespace) is refused the same way: no grant is recorded.
func TestPG_LocalRun_ADOOwnPATNotTheOwnersRefuses(t *testing.T) {
	for _, tc := range []struct {
		name, snapshotOwner, patHolder string
	}{
		{"snapshot owner is another person", "someone-else", ""},
		{"PAT outside the owner's namespace", "", "someone-else"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := ownPATTestRow()
			e, sec := adoLocalEnv(t, row)
			ctx := context.Background()
			owner, holder := e.personP, e.personP
			if tc.snapshotOwner != "" {
				owner = tc.snapshotOwner
			}
			if tc.patHolder != "" {
				holder = tc.patHolder
			}
			raw, _ := json.Marshal(adoOwnPATBlob{Token: ownPATToken, Org: "contoso", ExpiresOn: time.Now().UTC().AddDate(0, 0, 5), StoredAt: time.Now().UTC()})
			if err := sec.For(holder).Put(secretstore.WithPurpose(ctx, secretstore.PurposeStatus), adoOwnPATSecretName(ownPATRowID), raw); err != nil {
				t.Fatal(err)
			}
			laptop := e.claimedRunner(e.personP)
			now := time.Now().UTC()
			run, err := e.pg.CreateRun(ctx, types.AgentRun{ID: uuid.New(), CreatedAt: now, UpdatedAt: now, CreatedBy: e.personP, Agent: "claude-code",
				ConfinementClass: types.CC1, State: types.RunStarting, SPIFFEID: "spiffe://wardyn.local/agent-run/" + uuid.NewString(),
				RunnerTarget: "docker", Placement: types.PlacementLocal, RunnerID: &laptop})
			if err != nil {
				t.Fatal(err)
			}
			snapshot := adoEntraScopeSnapshot{ProviderRowID: row.ID, Organisation: "contoso", OwnerSubject: owner,
				CredentialSource: string(types.CredentialSourcePerUser), TokenMode: string(types.ADOTokenModeOwnPAT), Capabilities: row.Entra.DefaultProfile}
			assertADOGrantWriteRefused(t, e, run, snapshot)
		})
	}
}

// assertADOGrantWriteRefused writes a local run's Azure DevOps grants from snapshot and asserts the write
// refuses: no grant is recorded, a grant_write refusal is audited and the run is FAILED.
func assertADOGrantWriteRefused(t *testing.T, e *placementEnv, run types.AgentRun, snapshot adoEntraScopeSnapshot) {
	t.Helper()
	ctx := context.Background()
	if _, ok := e.srv.createADOEntraGrants(ctx, run, snapshot, adoEntraHosts("contoso")); ok {
		t.Fatal("a local run's Azure DevOps grant was written")
	}
	if got := adoLaneGrants(t, e, run.ID); len(got) != 0 {
		t.Errorf("%d grants recorded, want none", len(got))
	}
	refused := false
	for _, ev := range e.h.audit.snapshot() {
		if ev.RunID != nil && *ev.RunID == run.ID && ev.Action == "run.create" && ev.Outcome == "failure" &&
			strings.Contains(string(ev.Data), `"reason":"grant_write"`) &&
			strings.Contains(string(ev.Data), "could not record the Azure DevOps credential grant") {
			refused = true
		}
	}
	if !refused {
		t.Errorf("no grant_write refusal; audit: %s", auditDump(e.h.audit.snapshot(), run.ID))
	}
	if stored, err := e.pg.GetRun(ctx, run.ID); err != nil || stored.State != types.RunFailed {
		t.Errorf("run state = %q (%v), want FAILED", stored.State, err)
	}
}

// Option L at create: a minted_pat run the pool places on a personal runner is admitted with the
// connection's opt-in and a delivery policy that sends ado_minted_pat runner_resident. Off by default:
// with no opt-in or no policy row it is refused at create, as before. The admitted run's dispatch then
// fails closed here, because the person has no captured sign-in to mint from.
func TestPG_LocalRun_ADOMintedPATResidentAtCreate(t *testing.T) {
	const resident = `{"classes":{"ado_minted_pat":{"mode":"runner_resident"}}}`
	for name, tc := range map[string]struct {
		optIn  bool
		policy string
		want   int
	}{
		"opted in, resident policy": {true, resident, http.StatusCreated},
		"no opt-in":                 {false, resident, http.StatusUnprocessableEntity},
		"no policy row":             {true, "", http.StatusUnprocessableEntity},
	} {
		t.Run(name, func(t *testing.T) {
			row := ownPATTestRow()
			row.Entra.TokenMode, row.Entra.RunnerResident = types.ADOTokenModeMintedPAT, tc.optIn
			e, _ := adoLocalEnv(t, row)
			if tc.policy != "" {
				e.deliveryPolicy(tc.policy)
			}
			laptops := e.createPool("Laptops", types.RunnerPoolUser)
			awake := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2, types.CC3)
			e.addToPool(e.person, laptops.ID, awake)
			e.stream(awake, types.CC1, types.CC2, types.CC3)
			e.personDefaults(e.person, fmt.Sprintf(`{"pool_id":%q}`, laptops.ID))

			w := e.call(e.person, http.MethodPost, "/api/v1/runs", `{"agent":"claude-code","repo":"`+adoTestRepo+`","task":"t"}`)
			if w.Code != tc.want {
				t.Fatalf("create = %d %s, want %d", w.Code, w.Body, tc.want)
			}
			if tc.want != http.StatusCreated {
				if !strings.Contains(w.Body.String(), string(placement.ReasonPlacementCredential)) {
					t.Errorf("refusal %s, want %s", w.Body, placement.ReasonPlacementCredential)
				}
				return
			}
			var created types.AgentRun
			if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
				t.Fatal(err)
			}
			if created.Placement != types.PlacementLocal || created.RunnerID == nil || *created.RunnerID != awake {
				t.Fatalf("row = placement %q runner %v, want local on %s", created.Placement, created.RunnerID, awake)
			}
			for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
				run, err := e.pg.GetRun(context.Background(), created.ID)
				if err == nil && run.State == types.RunFailed {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("run state %q (%v), want FAILED without a sign-in; audit: %s", run.State, err, auditDump(e.h.audit.snapshot(), created.ID))
				}
			}
			if got := adoLaneGrants(t, e, created.ID); len(got) != 0 {
				t.Errorf("%d grants recorded with no token minted, want none", len(got))
			}
		})
	}
}

// run owner's namespace, and refused once it sits in another person's.
func TestPG_LocalRun_ADOOwnPATReviveHoldsTheOwnerNamespace(t *testing.T) {
	e, sec := adoLocalEnv(t, ownPATTestRow())
	ctx := context.Background()
	put := secretstore.WithPurpose(ctx, secretstore.PurposeStatus)
	name := adoOwnPATSecretName(ownPATRowID)
	raw, _ := json.Marshal(adoOwnPATBlob{Token: ownPATToken, Org: "contoso", ExpiresOn: time.Now().UTC().AddDate(0, 0, 5), StoredAt: time.Now().UTC()})
	if err := sec.For(e.personP).Put(put, name, raw); err != nil {
		t.Fatal(err)
	}
	laptops := e.createPool("Laptops", types.RunnerPoolUser)
	awake := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2, types.CC3)
	e.addToPool(e.person, laptops.ID, awake)
	e.stream(awake, types.CC1, types.CC2, types.CC3)
	e.personDefaults(e.person, fmt.Sprintf(`{"pool_id":%q}`, laptops.ID))
	created := e.localCreate(`{"agent":"claude-code","repo":"` + adoTestRepo + `","task":"t"}`)
	spec := e.dispatchedSpec(created.ID)
	run, err := e.pg.GetRun(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := e.pg.GetSiteConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	revive := func() (*proxy.Config, error) {
		t.Helper()
		rendered, err := runner.BuildProxyConfig(run.ID, spec.ProxyConfig, runner.ProxyListenPort)
		if err != nil {
			t.Fatal(err)
		}
		var cfg proxy.Config
		if err := json.Unmarshal(rendered, &cfg); err != nil {
			t.Fatal(err)
		}
		return &cfg, e.srv.refreshLocalDeploymentConfig(ctx, run, sc, &cfg)
	}

	cfg, err := revive()
	if err != nil {
		t.Fatalf("revive with the PAT in the owner's namespace: %v", err)
	}
	if len(cfg.Injection) != len(adoContosoHosts) || len(cfg.Injection) != len(spec.ProxyConfig.Injection) {
		t.Fatalf("revive kept %d injections, want %d", len(cfg.Injection), len(adoContosoHosts))
	}
	for i, in := range cfg.Injection {
		if in.GrantID != spec.ProxyConfig.Injection[i].GrantID {
			t.Errorf("injection %d grant = %s, want %s", i, in.GrantID, spec.ProxyConfig.Injection[i].GrantID)
		}
	}

	if err := sec.For(e.personP).Delete(put, name); err != nil {
		t.Fatal(err)
	}
	if err := sec.For("someone-else").Put(put, name, raw); err != nil {
		t.Fatal(err)
	}
	var ref *placement.Refusal
	if _, err := revive(); !errors.As(err, &ref) || ref.Field != "ProxyConfig.Injection[0]" ||
		!strings.Contains(ref.Detail, "credential requires proof of its owner namespace") {
		t.Fatalf("revive with the PAT in another namespace = %v, want the owner-namespace refusal on ProxyConfig.Injection[0]", err)
	}
}

// An Azure DevOps Server row takes the person's own PAT on their own runner the same way: the one Server
// host's grant is delivered own, the runner's relayed resolve is served, and the sandbox holds no token.
func TestPG_LocalRun_ADOServerOwnPATIsDeliveredOwn(t *testing.T) {
	e, sec := adoLocalEnv(t, adoServerTestRow(adoServerOrgKey))
	ctx := context.Background()
	raw, _ := json.Marshal(adoOwnPATBlob{Token: adoServerToken, Org: "defaultcollection", ExpiresOn: time.Now().UTC().AddDate(0, 0, 5), StoredAt: time.Now().UTC()})
	if err := sec.For(e.personP).Put(secretstore.WithPurpose(ctx, secretstore.PurposeStatus), adoOwnPATSecretName(ownPATRowID), raw); err != nil {
		t.Fatal(err)
	}
	laptops := e.createPool("Laptops", types.RunnerPoolUser)
	awake := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2, types.CC3)
	e.addToPool(e.person, laptops.ID, awake)
	e.stream(awake, types.CC1, types.CC2, types.CC3)
	e.personDefaults(e.person, fmt.Sprintf(`{"pool_id":%q}`, laptops.ID))

	run := e.localCreate(`{"agent":"claude-code","repo":"` + adoServerRepo + `","task":"t"}`)
	if run.Placement != types.PlacementLocal || run.RunnerID == nil || *run.RunnerID != awake {
		t.Fatalf("row = placement %q runner %v, want local on %s", run.Placement, run.RunnerID, awake)
	}
	spec := e.dispatchedSpec(run.ID)

	grants := adoLaneGrants(t, e, run.ID)
	g, ok := grants[adoServerHost]
	if len(grants) != 1 || !ok {
		t.Fatalf("Azure DevOps grants = %v, want one for %s", grants, adoServerHost)
	}
	if g.Delivery != types.GrantDeliveryOwn {
		t.Errorf("%s grant persisted delivery %q, want %q", adoServerHost, g.Delivery, types.GrantDeliveryOwn)
	}
	tok, err := e.srv.cfg.Identity.MintRunIdentity(ctx, run.ID, e.personP, "", internalAudience, false)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/internal/injection/"+g.ID.String(), nil)
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req = req.WithContext(relayConnContext(req.Context(), relayedConn{runner: awake}))
	w := httptest.NewRecorder()
	e.srv.relayRoutes(e.srv.InternalHandler()).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("relayed injection = %d %s, want 200", w.Code, w.Body)
	}

	// Server is git only: the broker is its one door, and no placeholder is set for a REST tool.
	if got, set := spec.Env[adoEntraPlaceholderEnv]; set {
		t.Errorf("sandbox %s = %q, want unset on a Server run", adoEntraPlaceholderEnv, got)
	}
	if got := spec.Env["WARDYN_GIT_PAT_BROKER_HOSTS"]; got != adoServerHost {
		t.Errorf("git broker hosts = %q, want %q", got, adoServerHost)
	}
	for k, v := range spec.Env {
		if strings.Contains(v, adoServerToken) {
			t.Errorf("sandbox env %s holds the PAT", k)
		}
	}
	for k, v := range spec.SecretEnv {
		if strings.Contains(v, adoServerToken) {
			t.Errorf("sandbox secret env %s holds the PAT", k)
		}
	}
}

// A Server own PAT in another person's namespace is refused: no grant is recorded.
func TestPG_LocalRun_ADOServerOwnPATOutsideTheOwnersNamespaceRefuses(t *testing.T) {
	row := adoServerTestRow(adoServerOrgKey)
	e, sec := adoLocalEnv(t, row)
	ctx := context.Background()
	raw, _ := json.Marshal(adoOwnPATBlob{Token: adoServerToken, Org: "defaultcollection", ExpiresOn: time.Now().UTC().AddDate(0, 0, 5), StoredAt: time.Now().UTC()})
	if err := sec.For("someone-else").Put(secretstore.WithPurpose(ctx, secretstore.PurposeStatus), adoOwnPATSecretName(ownPATRowID), raw); err != nil {
		t.Fatal(err)
	}
	laptop := e.claimedRunner(e.personP)
	now := time.Now().UTC()
	run, err := e.pg.CreateRun(ctx, types.AgentRun{ID: uuid.New(), CreatedAt: now, UpdatedAt: now, CreatedBy: e.personP, Agent: "claude-code",
		ConfinementClass: types.CC1, State: types.RunStarting, SPIFFEID: "spiffe://wardyn.local/agent-run/" + uuid.NewString(),
		RunnerTarget: "docker", Placement: types.PlacementLocal, RunnerID: &laptop})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := adoEntraScopeSnapshot{ProviderRowID: row.ID, Organisation: "defaultcollection", OwnerSubject: e.personP,
		CredentialSource: string(types.CredentialSourcePerUser), TokenMode: string(types.ADOTokenModeOwnPAT), Capabilities: adoServerCapabilities}
	assertADOGrantWriteRefusedOn(t, e, run, snapshot, []string{adoServerHost})
}

// assertADOGrantWriteRefused writes a local run's Azure DevOps grants from snapshot on hosts and asserts the write
// refuses: no grant is recorded, a grant_write refusal is audited and the run is FAILED.
func assertADOGrantWriteRefusedOn(t *testing.T, e *placementEnv, run types.AgentRun, snapshot adoEntraScopeSnapshot, hosts []string) {
	t.Helper()
	ctx := context.Background()
	if _, ok := e.srv.createADOEntraGrants(ctx, run, snapshot, hosts); ok {
		t.Fatal("a local run's Azure DevOps grant was written")
	}
	if got := adoLaneGrants(t, e, run.ID); len(got) != 0 {
		t.Errorf("%d grants recorded, want none", len(got))
	}
	refused := false
	for _, ev := range e.h.audit.snapshot() {
		if ev.RunID != nil && *ev.RunID == run.ID && ev.Action == "run.create" && ev.Outcome == "failure" &&
			strings.Contains(string(ev.Data), `"reason":"grant_write"`) &&
			strings.Contains(string(ev.Data), "could not record the Azure DevOps credential grant") {
			refused = true
		}
	}
	if !refused {
		t.Errorf("no grant_write refusal; audit: %s", auditDump(e.h.audit.snapshot(), run.ID))
	}
	if stored, err := e.pg.GetRun(ctx, run.ID); err != nil || stored.State != types.RunFailed {
		t.Errorf("run state = %q (%v), want FAILED", stored.State, err)
	}
}
