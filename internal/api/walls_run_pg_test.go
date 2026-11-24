// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runWallsPGRun stores a running run of owner's in unit (nil: none), created at created and changed by mut.
func runWallsPGRun(t *testing.T, e *govEnv, owner string, unit *uuid.UUID, created time.Time, mut func(*types.AgentRun)) types.AgentRun {
	t.Helper()
	id := uuid.New()
	run := types.AgentRun{ID: id, CreatedAt: created, UpdatedAt: created, CreatedBy: owner, Agent: "shell", Task: "walls",
		ConfinementClass: types.CC2, State: types.RunRunning, SPIFFEID: "spiffe://wardyn.local/agent-run/" + id.String(),
		RunnerTarget: "docker", UnitID: unit}
	if mut != nil {
		mut(&run)
	}
	got, err := e.pg.CreateRun(context.Background(), run)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	return got
}

// runWallsPGRows stores rows as the run's audit rows.
func runWallsPGRows(t *testing.T, e *govEnv, runID uuid.UUID, rows ...pvRow) {
	t.Helper()
	for _, r := range rows {
		raw, err := json.Marshal(r.data)
		if err != nil {
			t.Fatalf("marshal %s: %v", r.action, err)
		}
		outcome := r.outcome
		if outcome == "" {
			outcome = "success"
		}
		id := runID
		ev := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), RunID: &id, ActorType: types.ActorSystem, Actor: "wardynd",
			Action: r.action, Target: runID.String(), Outcome: outcome, Data: raw}
		if err := store.InsertAuditEvent(context.Background(), e.pool, &ev); err != nil {
			t.Fatalf("seed %s: %v", r.action, err)
		}
	}
}

// runWallsPGResolve is a run.policy.resolve row: served, set as prov records.
func runWallsPGResolve(served types.RunPolicySpec, prov knobProvenance) pvRow {
	return pvRow{action: "run.policy.resolve", data: effectivePolicyDatum{RunPolicySpec: served, Provenance: prov}}
}

// runWallsPGGet is route B for runID as c's caller, which must answer 200.
func runWallsPGGet(t *testing.T, e *govEnv, c *http.Cookie, runID uuid.UUID) (string, RunWalls) {
	t.Helper()
	w := e.call(c, http.MethodGet, buildPath(runWallsPattern, runID.String()), "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET run walls: %d %s", w.Code, w.Body.String())
	}
	var body RunWalls
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode run walls: %v (%s)", err, w.Body.String())
	}
	return w.Body.String(), body
}

// TestRunWallsCarriesNoMountsOrHostPath: a run whose policy mounts a host path and names a secret serves neither,
// nor a mounts key, to the security tier or to the owner.
func TestRunWallsCarriesNoMountsOrHostPath(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	const owner = "sub-walls-mount-owner"
	scope := json.RawMessage(`{"name":"TOKEN","secret_name":"planted-secret-name"}`)
	spec := types.RunPolicySpec{AllowedDomains: []string{"pypi.org"},
		WorkspaceMounts: []types.WorkspaceMount{{Source: "/srv/planted-host-path", Target: "/workspace/data"}},
		EligibleGrants:  []types.GrantSpec{{Kind: types.GrantEnvSecret, Scope: scope}}}
	run := runWallsPGRun(t, e, owner, nil, time.Now().UTC().Add(-time.Minute), nil)
	runWallsPGRows(t, e, run.ID, pvCreate(nil, pvSource(policyKindInline, nil, "", false, spec)), runWallsPGResolve(spec, nil))
	if _, err := e.pg.CreateGrant(context.Background(), types.CredentialGrant{ID: uuid.New(), RunID: run.ID, CreatedAt: run.CreatedAt,
		Spec: types.GrantSpec{Kind: types.GrantEnvSecret, Scope: scope}}); err != nil {
		t.Fatalf("create grant: %v", err)
	}
	for _, c := range []*http.Cookie{e.alice, ssoSession(t, owner, "mount-owner@corp.example", oidc.RoleUser)} {
		raw, body := runWallsPGGet(t, e, c, run.ID)
		if len(body.Grants) != 1 {
			t.Errorf("%s: grants %+v, want the one", body.Projection, body.Grants)
		}
		for _, planted := range []string{`"mounts"`, "workspace_mounts", "/srv/planted-host-path", "planted-secret-name"} {
			if strings.Contains(raw, planted) {
				t.Errorf("%s: the answer carries %s: %s", body.Projection, planted, raw)
			}
		}
	}
}

// TestRunWallsVerifiedRemoverFixtures: removed_by is set only when every condition of the verified re-fold holds;
// each fixture breaks one. A deny entry no step of the fold added is never verified.
func TestRunWallsVerifiedRemoverFixtures(t *testing.T) {
	leaf := policyref.Provenance{Layer: policyref.LayerUnit, Name: "refold-leaf"}
	byLeaf := &RunWallsRemovedBy{Layer: policyref.LayerUnit, Name: "refold-leaf", Verified: true}
	byMid := &RunWallsRemovedBy{Layer: refoldByMid.Layer, Name: refoldByMid.Name, Verified: true}
	for _, tc := range []struct {
		name      string
		prov      knobProvenance // nil: the leaf set allowed_domains
		inDefault bool           // the run is in the Default unit, whose fold reads the deployment default
		captured  bool           // the run captured a restriction that does not exist now
		edit      func(t *testing.T, e *govEnv, tree refoldTree)
		verified  bool
	}{
		{name: "every condition holds", verified: true},
		{name: "the deployment default", inDefault: true},
		{name: "a chain unit written within the margin", edit: func(t *testing.T, e *govEnv, tree refoldTree) { refoldPut(t, e, tree.root) }},
		{name: "a captured restriction gone", captured: true},
		{name: "a boundary written since", edit: func(t *testing.T, e *govEnv, _ refoldTree) {
			if _, err := e.pg.PutBoundary(context.Background(), refoldOlderBoundary(), "seed"); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "a boundary recorded at create and absent now",
			prov: knobProvenance{"allowed_domains": leaf, "min_confinement_class": {Layer: policyref.LayerBoundary}}},
		{name: "a binding restriction written since", edit: func(t *testing.T, e *govEnv, _ refoldTree) {
			seedRestriction(t, e, "walls-later", types.RestrictionSpec{DeniedDomains: &[]string{"later.example"}},
				types.RestrictionLimits{}, userMember(refoldOwner))
		}},
		{name: "the remover later than the recorded setter",
			prov: knobProvenance{"allowed_domains": {Layer: policyref.LayerUnit, Name: "refold-root"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, tree := refoldSeedTree(t, refoldNoBoundary)
			created := refoldAge(t, e)
			unit := tree.leaf.ID
			if tc.inDefault {
				unit = store.DefaultUnitID
			}
			run := runWallsPGRun(t, e, refoldOwner, &unit, created, func(r *types.AgentRun) {
				if tc.captured {
					r.RestrictionIDs = []uuid.UUID{uuid.New()}
				}
			})
			prov := tc.prov
			if prov == nil {
				prov = knobProvenance{"allowed_domains": leaf}
			}
			base := types.RunPolicySpec{AllowedDomains: []string{"keep.example", "unit-drops.example", "restriction-drops.example", "boundary-drops.example"}}
			served := types.RunPolicySpec{AllowedDomains: []string{"keep.example", "restriction-drops.example", "boundary-drops.example"},
				DeniedDomains: []string{"leaf-denies.example", "nowhere-denied.example"}}
			runWallsPGRows(t, e, run.ID, pvCreate(nil, pvSource(policyKindInline, nil, "", true, base)), runWallsPGResolve(served, prov))
			if tc.edit != nil {
				tc.edit(t, e, tree)
			}
			want := map[string]*RunWallsRemovedBy{"unit-drops.example": nil, "leaf-denies.example": nil, "nowhere-denied.example": nil}
			if tc.verified {
				want["unit-drops.example"], want["leaf-denies.example"] = byMid, byLeaf
			}
			_, body := runWallsPGGet(t, e, e.alice, run.ID)
			if len(body.Struck) != len(want) {
				t.Fatalf("struck = %+v, want %d entries", body.Struck, len(want))
			}
			for _, s := range body.Struck {
				w, ok := want[s.Entry]
				if !ok || s.Cause != causeLimits {
					t.Errorf("unexpected strike %+v", s)
					continue
				}
				if (s.RemovedBy == nil) != (w == nil) || s.RemovedBy != nil && *s.RemovedBy != *w {
					t.Errorf("%s: removed_by %+v, want %+v", s.Entry, s.RemovedBy, w)
				}
			}
		})
	}
}

// TestRunWallsOwnerGetsTheDoor: route B's run.egress, for the owner and for a security admin, is the topology's
// for the same run, an allowlist run's and a sealed No network run's.
func TestRunWallsOwnerGetsTheDoor(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	const owner = "sub-walls-door-owner"
	created := time.Now().UTC().Add(-time.Minute)
	allowlist := runWallsPGRun(t, e, owner, nil, created, nil)
	runWallsPGRows(t, e, allowlist.ID, runWallsPGResolve(types.RunPolicySpec{AllowedDomains: []string{"pypi.org"}}, nil))
	sealed := runWallsPGRun(t, e, owner, nil, created, func(r *types.AgentRun) {
		r.WallPreset = &types.WallPresetOnRun{ID: types.WallPresetNoNetwork, Name: "No network", Scope: types.WallPresetScopeBuiltin, Version: 1}
	})
	runWallsPGRows(t, e, sealed.ID, runWallsPGResolve(types.RunPolicySpec{}, nil))

	w := e.call(e.alice, http.MethodGet, "/api/v1/walls/topology", "")
	if w.Code != http.StatusOK {
		t.Fatalf("topology: %d %s", w.Code, w.Body.String())
	}
	var topo WallsTopology
	if err := json.Unmarshal(w.Body.Bytes(), &topo); err != nil {
		t.Fatal(err)
	}
	door := map[uuid.UUID]string{}
	for _, it := range topo.Runs.Items {
		door[it.ID] = it.Egress
	}
	ownerCookie := ssoSession(t, owner, "door-owner@corp.example", oidc.RoleUser)
	for _, tc := range []struct {
		run  types.AgentRun
		want string
	}{{allowlist, wallsEgressAllowlist}, {sealed, wallsEgressSealed}} {
		if door[tc.run.ID] != tc.want {
			t.Fatalf("topology egress %q, want %q", door[tc.run.ID], tc.want)
		}
		for _, c := range []*http.Cookie{ownerCookie, e.alice} {
			if _, body := runWallsPGGet(t, e, c, tc.run.ID); body.Run.Egress != door[tc.run.ID] {
				t.Errorf("%s: run.egress %q, topology's %q", body.Projection, body.Run.Egress, door[tc.run.ID])
			}
		}
	}
}

// TestRunWallsExitsEndToEnd: a site-hosted run behind a configured upstream, with one bypassed host and one
// internal host, gets its exits, gates and inner nodes through the mounted route, with no address below super
// admin.
func TestRunWallsExitsEndToEnd(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	ctx := context.Background()
	if _, err := e.pg.PutSiteConfig(ctx, types.SiteConfig{UpstreamProxyURL: "http://proxy.corp.example:3128",
		UpstreamProxyNoProxy: []string{"bypass.example"}, InternalHosts: []types.InternalHost{{HostSuffix: "corp.internal"}}}); err != nil {
		t.Fatalf("put site config: %v", err)
	}
	served := types.RunPolicySpec{AllowedDomains: []string{"pypi.org", "bypass.example", "git.corp.internal"}}
	run := runWallsPGRun(t, e, "sub-walls-exits-owner", nil, time.Now().UTC().Add(-time.Minute), nil)
	runWallsPGRows(t, e, run.ID, pvCreate(nil, pvSource(policyKindInline, nil, "", false, served)), runWallsPGResolve(served, nil),
		pvRow{action: "egress.allow", data: map[string]any{"host": "proxy.corp.example", "port": 3128, "rule_source": egress.RuleSourceUpstreamProxy}})

	raw, body := runWallsPGGet(t, e, e.alice, run.ID)
	gates := map[string]WallsGate{}
	for _, g := range body.Gates {
		gates[g.ID] = g
	}
	if len(gates) != 2 || gates["direct"].Kind != "direct" || gates["upstream-site"].Kind != "upstream" || gates["upstream-site"].Source != "site" {
		t.Errorf("gates = %+v, want direct and upstream-site", body.Gates)
	}
	var node RunWallsInner
	for _, n := range body.Inner {
		if n.Kind == "internal_host" {
			node = n
		}
	}
	if node.ID == "" || node.Label != "git.corp.internal" || strings.Contains(node.ID, "corp") {
		t.Fatalf("inner = %+v, want one internal host node labelled git.corp.internal", body.Inner)
	}
	if !slices.ContainsFunc(body.Inner, func(n RunWallsInner) bool { return n.ID == "control-plane" && n.Kind == "control_plane" }) ||
		slices.ContainsFunc(body.Inner, func(n RunWallsInner) bool { return n.Kind == "secret_store" }) || len(body.Inner) != 2 {
		t.Errorf("inner = %+v, want the control plane and the internal host only", body.Inner)
	}
	want := []RunWallsExit{
		{Path: "pypi.org", Route: "upstream", Gate: "upstream-site"},
		{Path: "bypass.example", Route: "direct", Gate: "direct"},
		{Path: "git.corp.internal", Route: "upstream", Gate: "upstream-site", Inner: node.ID, Hairpin: true},
	}
	if !slices.Equal(body.Exits, want) {
		t.Errorf("exits = %+v, want %+v", body.Exits, want)
	}
	for _, key := range []string{`"addr"`, `"suffix"`} {
		if strings.Contains(raw, key) {
			t.Errorf("a security admin's answer carries %s: %s", key, raw)
		}
	}
}
