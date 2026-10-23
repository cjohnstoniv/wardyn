// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// A write that withdraws a local run's runner_resident Azure DevOps delivery revokes the run's token
// itself, before the run's proxy resolves again; a write that withdraws nothing revokes nothing.
// Guarded by WARDYN_TEST_PG.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const withdrawOwner = "sub-person"

// withdrawEnv is a live local run on its owner's personal runner, holding one live Azure DevOps token
// delivered runner_resident.
type withdrawEnv struct {
	*poolEnv
	pats   *fakeADOPATs
	runner uuid.UUID
	run    types.AgentRun
	grant  types.CredentialGrant
	authID uuid.UUID
}

func newWithdrawEnv(t *testing.T, policy string) *withdrawEnv {
	t.Helper()
	ctx := context.Background()
	cfg := ADOEntraConfig{RowID: "ado-row-1", TenantID: adoTestTenant, ClientID: adoTestClient,
		LoginClientID: adoTestClient, LoginTenantID: adoTestTenant, RedirectURL: adoCallbackURL,
		Scopes: adoscope.MintScopes(), ClientSecret: mintTestSecret, TokenMode: types.ADOTokenModeMintedPAT}
	e := &withdrawEnv{poolEnv: newPoolEnvWith(t, func(c *Config) {
		c.Runner, c.RunnerOrgURL = &fakeRunner{}, poolTestOrgURL
		c.ADOLoginFacts = func() (string, string, bool) { return adoTestClient, adoTestTenant, true }
		c.ADOEntra = func(context.Context) (ADOEntraConfig, bool, error) { return cfg, true, nil }
		c.ADOEntraByRow = func(context.Context, string) (ADOEntraConfig, bool, error) { return cfg, true, nil }
	}), pats: &fakeADOPATs{live: map[string]adoPAT{}}}
	e.srv.adoPATs = e.pats
	// The sign-in's access token, as a redemption would have left it.
	key := strings.Join([]string{withdrawOwner, cfg.RowID, cfg.TenantID, cfg.ClientID, strings.Join(adoscope.MintScopes(), " ")}, "\x00")
	e.srv.adoEntraTokens.put(key, adoEntraCached{ADOEntraAccess: ADOEntraAccess{AccessToken: "access", Scopes: adoscope.MintScopes(), ExpiresAt: time.Now().Add(time.Hour)}})

	e.runner = e.claimedRunner(withdrawOwner)
	if _, err := e.pg.RecordRunnerPosture(ctx, e.runner, types.RunnerPosture{OS: "linux", DiskEncrypted: true, Evidence: types.PostureEvidenceOSProbe}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pg.PutCredentialDeliveryPolicy(ctx, json.RawMessage(policy), "seed", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pg.PutSiteConfig(ctx, withdrawSite(func(*types.GitProvider) {})); err != nil {
		t.Fatal(err)
	}

	e.run, e.grant, e.authID = e.localRun(e.runner)
	// The fixture is only a pin if its delivery is admitted before the write.
	if admitted, _ := e.srv.judgeResidentADO(ctx, e.run, withdrawOwner, e.grant); !admitted {
		t.Fatal("the run's runner_resident delivery is not admitted before the write")
	}
	return e
}

// localRun is a live local run of withdrawOwner bound to runnerID, holding one live token delivered
// runner_resident: the run, its grant and the token's authorization id.
func (e *withdrawEnv) localRun(runnerID uuid.UUID) (types.AgentRun, types.CredentialGrant, uuid.UUID) {
	e.t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	id := uuid.New()
	if _, err := e.pg.CreateRun(ctx, types.AgentRun{ID: id, CreatedAt: now, UpdatedAt: now, CreatedBy: withdrawOwner, Agent: "claude-code", Task: "t",
		ConfinementClass: types.CC2, State: types.RunStopped, SPIFFEID: "spiffe://wardyn.test/agent-run/" + id.String(), RunnerTarget: "docker"}); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE agent_runs SET state = 'RUNNING', placement = 'local', runner_id = $2 WHERE id = $1`, id, runnerID); err != nil {
		e.t.Fatal(err)
	}
	run, err := e.pg.GetRun(ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	row := withdrawRow()
	snapshot := adoEntraScopeSnapshot{ProviderRowID: row.ID, Organisation: "contoso", OwnerSubject: withdrawOwner,
		CredentialSource: string(row.CredentialSource), TenantID: adoTestTenant, ClientID: adoTestClient,
		TokenMode: string(types.ADOTokenModeMintedPAT), Capabilities: row.Entra.DefaultProfile}
	scope := mustJSON(map[string]any{"host": "dev.azure.com", "header": adoEntraInjectHeader, "format": adoInjectFormat(types.ADOTokenModeMintedPAT),
		"secret_name": types.ADOEntraAccessTokenSecret, "require_tls": true, "snapshot": snapshot})
	grant, err := e.pg.CreateGrant(ctx, types.CredentialGrant{ID: uuid.New(), RunID: id, CreatedAt: now, Delivery: types.GrantDeliveryResident,
		Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: scope, TTLSeconds: adoEntraGrantTTLSeconds}})
	if err != nil {
		e.t.Fatal(err)
	}
	authID := uuid.New()
	if err := e.pg.InsertRunPAT(ctx, store.RunPAT{RunID: id, AuthorizationID: authID, Owner: withdrawOwner, ProviderRowID: row.ID,
		Org: "contoso", Scope: "vso.code", ValidTo: now.Add(time.Hour)}); err != nil {
		e.t.Fatal(err)
	}
	e.pats.live[authID.String()] = adoPAT{AuthorizationID: authID.String()}
	return run, grant, authID
}

// withdrawRow is the minted_pat connection opted in to runner_resident.
func withdrawRow() types.GitProvider {
	row := adoEntraTestRow()
	row.Entra.TokenMode, row.Entra.RunnerResident = types.ADOTokenModeMintedPAT, true
	return row
}

func withdrawSite(edit func(*types.GitProvider)) types.SiteConfig {
	row := withdrawRow()
	edit(&row)
	return types.SiteConfig{WorkspaceProviders: &types.WorkspaceProviders{Git: []types.GitProvider{row}}}
}

// revoked reports the row of the token authID closed, with its reason.
func (e *withdrawEnv) revoked(authID uuid.UUID) (bool, string) {
	e.t.Helper()
	var closed bool
	var reason string
	if err := e.pool.QueryRow(context.Background(), `SELECT revoked_at IS NOT NULL, revoke_reason FROM ado_run_pats WHERE authorization_id = $1`,
		authID).Scan(&closed, &reason); err != nil {
		e.t.Fatal(err)
	}
	return closed, reason
}

func (e *withdrawEnv) wantOK(w *httptest.ResponseRecorder, what string) {
	e.t.Helper()
	if w.Code != http.StatusOK {
		e.t.Fatalf("%s = %d %s, want 200", what, w.Code, w.Body)
	}
}

const (
	withdrawResident     = `{"classes":{"ado_minted_pat":{"mode":"runner_resident"}}}`
	withdrawPersonalOnly = `{"classes":{"ado_minted_pat":{"mode":"refuse"}},"by_trust":{"personal":{"classes":{"ado_minted_pat":{"mode":"runner_resident"}}}}}`
	withdrawEncrypted    = `{"classes":{"ado_minted_pat":{"mode":"runner_resident","require_posture":{"disk_encrypted":true}}}}`
)

func TestPG_WithdrawnLocalDeliveryRevokesAtTheWrite(t *testing.T) {
	for name, tc := range map[string]struct {
		policy  string
		write   func(e *withdrawEnv)
		revokes bool
	}{
		"site config: opt-in turned off": {withdrawResident, func(e *withdrawEnv) {
			e.wantOK(e.call(e.super, http.MethodPut, "/api/v1/site-config",
				string(mustJSON(withdrawSite(func(r *types.GitProvider) { r.Entra.RunnerResident = false })))), "PUT /site-config")
		}, true},
		"connection disabled": {withdrawResident, func(e *withdrawEnv) {
			e.wantOK(e.call(e.super, http.MethodPut, "/api/v1/workspace-providers",
				string(mustJSON(withdrawSite(func(r *types.GitProvider) { r.Disabled = true }).WorkspaceProviders))), "PUT /workspace-providers")
		}, true},
		"connection token mode no longer minted": {withdrawResident, func(e *withdrawEnv) {
			e.wantOK(e.call(e.super, http.MethodPut, "/api/v1/workspace-providers",
				string(mustJSON(withdrawSite(func(r *types.GitProvider) { r.Entra.TokenMode = types.ADOTokenModeBearer }).WorkspaceProviders))), "PUT /workspace-providers")
		}, true},
		"delivery policy refuses": {withdrawResident, func(e *withdrawEnv) {
			e.wantOK(e.call(e.alice, http.MethodPut, "/api/v1/governance/credential-delivery", `{"classes":{"ado_minted_pat":{"mode":"refuse"}}}`), "PUT /governance/credential-delivery")
		}, true},
		"delivery policy refuses, held then approved": {withdrawResident, func(e *withdrawEnv) {
			e.t.Setenv(envGovernanceSecondHuman, "true")
			ch := e.pending(e.call(e.alice, http.MethodPut, "/api/v1/governance/credential-delivery", `{"classes":{"ado_minted_pat":{"mode":"refuse"}}}`))
			if closed, _ := e.revoked(e.authID); closed {
				e.t.Fatal("a held change revoked the token before it was approved")
			}
			e.approve(ch.ID)
		}, true},
		"runner trust class the policy does not deliver to": {withdrawPersonalOnly, func(e *withdrawEnv) {
			e.wantOK(e.call(e.alice, http.MethodPut, "/api/v1/runners/"+e.runner.String()+"/trust", `{"trust_class":"managed_device","reason":"moved to a managed laptop"}`), "PUT /runners/{id}/trust")
		}, true},
		"runner posture no longer meets the policy": {withdrawEncrypted, func(e *withdrawEnv) {
			ev := runnerwire.Event{Data: mustJSON(types.RunnerPosture{OS: "linux", DiskEncrypted: false, Evidence: types.PostureEvidenceOSProbe})}
			if err := e.srv.PostureFromRunner(context.Background(), e.runner, ev); err != nil {
				e.t.Fatal(err)
			}
		}, true},
		"runner posture: a token on another runner is not judged": {withdrawEncrypted, func(e *withdrawEnv) {
			// Withdrawn already (its runner reports no encrypted disk), so only the runner filter keeps it.
			other := e.claimedRunner(withdrawOwner)
			if _, err := e.pg.RecordRunnerPosture(context.Background(), other, types.RunnerPosture{OS: "linux", Evidence: types.PostureEvidenceOSProbe}, time.Now()); err != nil {
				e.t.Fatal(err)
			}
			_, _, otherAuth := e.localRun(other)
			ev := runnerwire.Event{Data: mustJSON(types.RunnerPosture{OS: "linux", DiskEncrypted: false, Evidence: types.PostureEvidenceOSProbe})}
			if err := e.srv.PostureFromRunner(context.Background(), e.runner, ev); err != nil {
				e.t.Fatal(err)
			}
			if closed, _ := e.revoked(otherAuth); closed || slices.Contains(e.pats.revokedIDs(), otherAuth.String()) {
				e.t.Fatal("a posture report revoked a token held by a run on another runner")
			}
		}, true},
		"control: a site config write that withdraws nothing": {withdrawResident, func(e *withdrawEnv) {
			sc := withdrawSite(func(*types.GitProvider) {})
			sc.SignInHelpText = "Ask the service desk."
			e.wantOK(e.call(e.super, http.MethodPut, "/api/v1/site-config", string(mustJSON(sc))), "PUT /site-config")
		}, false},
	} {
		t.Run(name, func(t *testing.T) {
			e := newWithdrawEnv(t, tc.policy)
			tc.write(e)
			closed, reason := e.revoked(e.authID)
			atADO := slices.Contains(e.pats.revokedIDs(), e.authID.String())
			if tc.revokes && (!closed || reason != adoPATRevokeDrift || !atADO) {
				t.Fatalf("after the write: closed=%v reason=%q revoked at Azure DevOps=%v, want the token revoked with reason drift", closed, reason, atADO)
			}
			if !tc.revokes && (closed || atADO) {
				t.Fatalf("after the write: closed=%v reason=%q revoked at Azure DevOps=%v, want the token left live", closed, reason, atADO)
			}
		})
	}
}
