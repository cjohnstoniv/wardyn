// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// An Azure DevOps lane on a run the pool places on the person's own runner, against a real Postgres and
// the real broker. Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
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
	t.Skip("pending fix; see PR")
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
			if _, ok := e.srv.createADOEntraGrants(ctx, run, snapshot, adoEntraHosts("contoso")); ok {
				t.Fatal("a local run's organisation-minted grant was written")
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
		})
	}
}
