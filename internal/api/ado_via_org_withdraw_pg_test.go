// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// A local run's Azure DevOps token delivered via_org is judged again by every write that can withdraw it and by every
// resolve the run's engine makes. A withdraw revokes the run's tokens (reason drift) and releases the run's engine on
// this replica; the engine's next resolve is refused. Guarded by WARDYN_TEST_PG.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	viaOrgWithdrawPolicy  = `{"classes":{"ado_minted_pat":{"mode":"via_org"}}}`
	viaOrgWithdrawPosture = `{"classes":{"ado_minted_pat":{"mode":"via_org","require_posture":{"disk_encrypted":true}}}}`
)

// viaOrgWithdrawEnv is withdrawEnv's run with its token delivered via_org: the connection's runner_via_org switch on
// (and runner_resident off), the grant recorded via_org, and the run's engine built on this replica.
type viaOrgWithdrawEnv struct {
	*withdrawEnv
	vrun *viaOrgRun
	gen  *viaOrgGen
}

// viaOrgWithdrawSite is the minted_pat connection with only its via_org switch on, edited by edit.
func viaOrgWithdrawSite(edit func(*types.GitProvider)) types.SiteConfig {
	return withdrawSite(func(r *types.GitProvider) {
		r.Entra.RunnerResident, r.Entra.RunnerViaOrg = false, true
		edit(r)
	})
}

func newViaOrgWithdrawEnv(t *testing.T, policy string) *viaOrgWithdrawEnv {
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
	e.srv.adoEntraTokens.put(key, adoEntraCached{ADOEntraAccess: ADOEntraAccess{AccessToken: "access", Scopes: adoscope.MintScopes(), ExpiresAt: time.Now().Add(time.Hour)}, filled: time.Now()})
	e.runner = e.claimedRunner(withdrawOwner)
	if _, err := e.pg.RecordRunnerPosture(ctx, e.runner, types.RunnerPosture{OS: "linux", DiskEncrypted: true, Evidence: types.PostureEvidenceOSProbe}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pg.PutCredentialDeliveryPolicy(ctx, json.RawMessage(policy), "seed", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pg.PutSiteConfig(ctx, viaOrgWithdrawSite(func(*types.GitProvider) {})); err != nil {
		t.Fatal(err)
	}
	e.run, e.grant, e.authID = e.localRun(e.runner)
	if _, err := e.pool.Exec(ctx, `UPDATE credential_grants SET delivery = 'via_org' WHERE id = $1`, e.grant.ID); err != nil {
		t.Fatal(err)
	}
	grants, err := e.pg.ListGrantsByRun(ctx, e.run.ID)
	if err != nil || len(grants) != 1 || !viaOrgADOLaneGrant(grants[0]) {
		t.Fatalf("the run's grant is not an Azure DevOps grant recorded via_org: %+v %v", grants, err)
	}
	e.grant = grants[0]
	// The fixture is only a pin if its delivery is admitted before the write.
	if admitted, _ := e.srv.judgeViaOrgADO(ctx, e.run, withdrawOwner, e.grant); !admitted {
		t.Fatal("the run's via_org delivery is not admitted before the write")
	}

	v := &viaOrgWithdrawEnv{withdrawEnv: e, gen: newIdleTestGen(t, e.run.ID)}
	v.vrun = &viaOrgRun{id: e.run.ID, runner: e.runner, bytes: new(atomic.Int64), done: make(chan struct{}),
		streams: map[*viaOrgStream]struct{}{}, idleSince: time.Now(), engine: v.gen,
		byteCap: viaOrgMaxBytes, idle: time.Hour, period: time.Hour}
	e.srv.viaOrg.mu.Lock()
	e.srv.viaOrg.runs = map[uuid.UUID]*viaOrgRun{e.run.ID: v.vrun}
	e.srv.viaOrg.mu.Unlock()
	t.Cleanup(func() {
		if !v.gen.engine.Closed() {
			closeViaOrgGen(e.run.ID, v.gen)
		}
	})
	return v
}

// resolve is the run's engine resolving the grant's injection in process (the marks viaOrgTransport sets), through
// refuseRelayedDelivery: whether it refused, and what it wrote.
func (v *viaOrgWithdrawEnv) resolve() (bool, *httptest.ResponseRecorder) {
	v.t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/internal/injection/"+v.grant.ID.String(), nil)
	ctx := context.WithValue(audit.WithRelay(context.Background(), v.runner), relayRunnerKey{}, v.runner)
	r = r.WithContext(context.WithValue(ctx, viaOrgCallKey{}, true))
	w := httptest.NewRecorder()
	return v.srv.refuseRelayedDelivery(w, r, &identity.Claims{RunID: v.run.ID}, v.grant.ID, true), w
}

// engineReleased reports whether the run's engine was released in the away form: the generation retired and closed,
// and the run's state kept for its next request. It waits for the close, which runs in the background.
func (v *viaOrgWithdrawEnv) engineReleased() bool {
	v.t.Helper()
	v.vrun.mu.Lock()
	retired := v.vrun.engine == nil
	v.vrun.mu.Unlock()
	if !retired {
		return false
	}
	deadline := time.After(30 * time.Second)
	poll := time.NewTicker(2 * time.Millisecond)
	defer poll.Stop()
	for !v.gen.engine.Closed() {
		select {
		case <-poll.C:
		case <-deadline:
			v.t.Fatal("the retired engine generation was not closed")
		}
	}
	v.srv.viaOrg.mu.Lock()
	kept := v.srv.viaOrg.runs[v.run.ID] == v.vrun
	v.srv.viaOrg.mu.Unlock()
	return kept && !v.vrun.isClosed()
}

// engineKept reports whether the run's engine is the one it was built with, still open.
func (v *viaOrgWithdrawEnv) engineKept() bool {
	v.vrun.mu.Lock()
	defer v.vrun.mu.Unlock()
	return v.vrun.engine == v.gen && !v.gen.engine.Closed()
}

// wantRevoked fails unless the run's token is closed with reason drift and revoked at Azure DevOps.
func (v *viaOrgWithdrawEnv) wantRevoked(when string) {
	v.t.Helper()
	closed, reason := v.revoked(v.authID)
	if atADO := slices.Contains(v.pats.revokedIDs(), v.authID.String()); !closed || reason != adoPATRevokeDrift || !atADO {
		v.t.Fatalf("%s: closed=%v reason=%q revoked at Azure DevOps=%v, want the token revoked with reason drift", when, closed, reason, atADO)
	}
}

func TestPG_WithdrawnViaOrgADODeliveryRevokesAndReleasesTheEngine(t *testing.T) {
	for name, tc := range map[string]struct {
		policy    string
		write     func(v *viaOrgWithdrawEnv)
		withdraws bool
	}{
		"the connection's via_org switch turned off": {viaOrgWithdrawPolicy, func(v *viaOrgWithdrawEnv) {
			v.wantOK(v.call(v.super, http.MethodPut, "/api/v1/site-config",
				string(mustJSON(viaOrgWithdrawSite(func(r *types.GitProvider) { r.Entra.RunnerViaOrg = false })))), "PUT /site-config")
		}, true},
		"the delivery policy flipped to runner_resident": {viaOrgWithdrawPolicy, func(v *viaOrgWithdrawEnv) {
			v.wantOK(v.call(v.alice, http.MethodPut, "/api/v1/governance/credential-delivery", withdrawResident), "PUT /governance/credential-delivery")
		}, true},
		"the connection disabled": {viaOrgWithdrawPolicy, func(v *viaOrgWithdrawEnv) {
			v.wantOK(v.call(v.super, http.MethodPut, "/api/v1/workspace-providers",
				string(mustJSON(viaOrgWithdrawSite(func(r *types.GitProvider) { r.Disabled = true }).WorkspaceProviders))), "PUT /workspace-providers")
		}, true},
		"the runner's posture no longer meets the policy": {viaOrgWithdrawPosture, func(v *viaOrgWithdrawEnv) {
			v.posture(types.RunnerPosture{OS: "linux", DiskEncrypted: false, Evidence: types.PostureEvidenceOSProbe})
		}, true},
		// via_org is never set per trust class and a by_trust entry cannot override it, so a trust write is judged
		// and withdraws nothing.
		"the runner's trust class changed": {viaOrgWithdrawPolicy, func(v *viaOrgWithdrawEnv) {
			v.wantOK(v.call(v.alice, http.MethodPut, "/api/v1/runners/"+v.runner.String()+"/trust", `{"trust_class":"managed_device","reason":"moved to a managed laptop"}`), "PUT /runners/{id}/trust")
		}, false},
		"control: a site config write that withdraws nothing": {viaOrgWithdrawPolicy, func(v *viaOrgWithdrawEnv) {
			sc := viaOrgWithdrawSite(func(*types.GitProvider) {})
			sc.SignInHelpText = "Ask the service desk."
			v.wantOK(v.call(v.super, http.MethodPut, "/api/v1/site-config", string(mustJSON(sc))), "PUT /site-config")
		}, false},
	} {
		t.Run(name, func(t *testing.T) {
			v := newViaOrgWithdrawEnv(t, tc.policy)
			tc.write(v)
			if !tc.withdraws {
				closed, reason := v.revoked(v.authID)
				if closed || slices.Contains(v.pats.revokedIDs(), v.authID.String()) || !v.engineKept() {
					t.Fatalf("after the write: closed=%v reason=%q engine kept=%v, want the token live and the engine kept", closed, reason, v.engineKept())
				}
				if refused, w := v.resolve(); refused {
					t.Fatalf("the engine's resolve after the write = %d %s, want it served", w.Code, w.Body)
				}
				return
			}
			v.wantRevoked("after the write")
			if !v.engineReleased() {
				t.Fatal("after the write the run's engine was not released (away form, the run's state kept)")
			}
			if refused, w := v.resolve(); !refused || errorReason(w) != string(placement.ReasonPlacementCredential) {
				t.Fatalf("the engine's next resolve = refused %v, %d %s, want %s", refused, w.Code, w.Body, placement.ReasonPlacementCredential)
			}
		})
	}
}

// A withdraw no write announced (the site config flipped under the store, so no door judged it) is caught by the
// engine's next resolve: refused, the run's tokens revoked with reason drift, and the engine released.
func TestPG_ViaOrgADOResolveRejudgesAWithdrawNoWriteAnnounced(t *testing.T) {
	v := newViaOrgWithdrawEnv(t, viaOrgWithdrawPolicy)
	if refused, w := v.resolve(); refused {
		t.Fatalf("the engine's resolve of an admitted delivery = %d %s, want it served", w.Code, w.Body)
	}
	if _, err := v.pg.PutSiteConfig(context.Background(), viaOrgWithdrawSite(func(r *types.GitProvider) { r.Entra.RunnerViaOrg = false })); err != nil {
		t.Fatal(err)
	}
	if closed, _ := v.revoked(v.authID); closed || !v.engineKept() {
		t.Fatal("the fixture's flip under the store was judged by a door; it must reach the resolve unannounced")
	}
	refused, w := v.resolve()
	if !refused || errorReason(w) != string(placement.ReasonPlacementCredential) {
		t.Fatalf("the engine's resolve after the flip = refused %v, %d %s, want %s", refused, w.Code, w.Body, placement.ReasonPlacementCredential)
	}
	v.wantRevoked("after the resolve")
	if !v.engineReleased() {
		t.Fatal("the resolve's withdraw did not release the run's engine (away form, the run's state kept)")
	}
}
