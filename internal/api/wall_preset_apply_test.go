// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/ghscope"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// A run's wall preset (personal_walls.go): resolved at launch and refused, never run as Open, when it
// is not there; met by the walls' own step on every arm; frozen on the run; and re-asserted from that
// copy at the seal, a revive, the mint door, an approval and the queue's start.

// frozenPreset is the copy a launch takes of p.
func frozenPreset(p types.WallPreset) *types.WallPresetOnRun {
	return &types.WallPresetOnRun{ID: p.ID, Name: p.Name, Scope: p.Scope, GroupID: p.GroupID, Version: p.Version, Spec: p.Spec}
}

func builtinFrozen(t *testing.T, id string) *types.WallPresetOnRun {
	t.Helper()
	p, ok := types.BuiltinWallPreset(id)
	if !ok {
		t.Fatalf("no built-in wall preset %q", id)
	}
	return frozenPreset(p)
}

// tightPreset is a stored preset of scope naming what the seal, an approval and the dispatch floor
// re-assert: a deny, a desktop block, a withheld credential, every push refused, a recording floor
// and an idle cap.
func tightPreset(scope types.WallPresetScope, name string) types.WallPreset {
	on, rec, stop := true, types.RecordingTerminal, 600
	p := types.WallPreset{ID: uuid.NewString(), Scope: scope, Name: name, Version: 1, Spec: types.PersonalWallsSpec{
		DeniedDomains: wallsList("pastebin.com"), Desktop: &types.DesktopControls{PasteIn: types.TransferBlocked},
		WithheldGrants: []types.GrantSpec{{Kind: types.GrantSSHKey}}, PushRules: &types.PersonalWallsPushRules{DenyAll: &on},
		Recording: &rec, AutoStopAfterSec: &stop,
	}}
	switch scope {
	case types.WallPresetScopePerson:
		p.OwnerID = wallsSub
	case types.WallPresetScopeGroup:
		p.GroupID = "eng"
	}
	return p
}

// TestWallPresetsNeverWiden mirrors TestPersonalWallsNeverWiden for the built-ins and a stored preset:
// whatever spec the preset step is handed, what it leaves is at most as wide (composer.Leq), every push
// is refused where the preset says so, a spec already narrower keeps its own values, Open changes
// nothing, and a copy that names a widening is refused rather than applied.
func TestWallPresetsNeverWiden(t *testing.T) {
	srv, _, _ := uvServer(t)
	read, write := adoscope.CapCodeRead, adoscope.CapCodeWrite
	blocked := types.DesktopControls{PasteIn: types.TransferBlocked, CopyOut: types.TransferBlocked, Upload: types.TransferBlocked, Download: types.TransferBlocked}
	bases := map[string]types.RunPolicySpec{
		"open": {AllowAllEgress: true, AllowedDomains: []string{"api.anthropic.com"}, EligibleGrants: []types.GrantSpec{wallsSSH}, AutoStopAfterSec: 3600,
			AzureDevOpsCapabilities: []adoscope.Capability{read, write}, GitHubCapabilities: []ghscope.Capability{ghscope.CapCodeRead, ghscope.CapCodeWrite}},
		"already narrower": {AllowedDomains: []string{}, FirstUseApproval: types.FirstUseAlwaysDeny, AutoStopAfterSec: 60, Desktop: &blocked,
			AzureDevOpsCapabilities: []adoscope.Capability{read}, GitHubCapabilities: []ghscope.Capability{ghscope.CapCodeRead},
			PushRules: &types.PushRulesSpec{DenyAll: true}},
	}
	step := func(p *types.WallPresetOnRun, base types.RunPolicySpec) (types.RunPolicySpec, policySourceRecord, *runRefusal) {
		spec, source := base.Clone(), policySourceRecord{knobs: startProvenance(memberProvenance)}
		_, refusal := srv.wallPresetRunStep(&spec, &source, p)
		return spec, source, refusal
	}
	for _, p := range []*types.WallPresetOnRun{builtinFrozen(t, types.WallPresetReadOnly), builtinFrozen(t, types.WallPresetSealed),
		frozenPreset(tightPreset(types.WallPresetScopeOrg, "Org tight"))} {
		for name, base := range bases {
			got, source, refusal := step(p, base)
			if refusal != nil {
				t.Fatalf("%s over %s: refusal %+v", p.Name, name, refusal)
			}
			if !composer.Leq(got, base, types.GovernanceLimits{}, types.GovernanceLimits{}) {
				t.Errorf("%s over %s widened the run: %+v from %+v", p.Name, name, got, base)
			}
			if got.PushRules == nil || !got.PushRules.DenyAll {
				t.Errorf("%s over %s: push_rules %+v, want every push refused", p.Name, name, got.PushRules)
			}
			if source.WallPreset == nil || source.WallPreset.ID != p.ID || source.WallPreset.Version != p.Version || source.wallPreset != p {
				t.Errorf("%s over %s: policy_source.wall_preset %+v, want the preset recorded and its copy kept", p.Name, name, source.WallPreset)
			}
			if name == "already narrower" && (got.AutoStopAfterSec != 60 || !slices.Equal(got.AzureDevOpsCapabilities, []adoscope.Capability{read}) ||
				len(got.AllowedDomains) != 0 || got.FirstUseApproval != types.FirstUseAlwaysDeny) {
				t.Errorf("%s over a narrower spec changed it: %+v", p.Name, got)
			}
		}
	}
	for name, base := range bases {
		got, source, refusal := step(builtinFrozen(t, types.WallPresetOpen), base)
		if refusal != nil || !reflect.DeepEqual(specJSON(got), specJSON(base)) {
			t.Errorf("Open over %s: refusal %+v, spec %+v, want it byte for byte as it was", name, refusal, got)
		}
		if source.WallPreset == nil || source.WallPreset.ID != types.WallPresetOpen || source.WallPreset.WallsSourceRecord != nil {
			t.Errorf("Open over %s: policy_source.wall_preset %+v, want Open recorded with nothing narrowed", name, source.WallPreset)
		}
	}
	on := true
	wide := &types.WallPresetOnRun{ID: uuid.NewString(), Name: "Wide", Scope: types.WallPresetScopeOrg, Version: 1,
		Spec: types.PersonalWallsSpec{AllowAllEgress: &on}}
	got, _, refusal := step(wide, bases["already narrower"])
	if refusal == nil || refusal.status != http.StatusUnprocessableEntity || refusal.body.Reason != reasonWallPresetInvalid ||
		!reflect.DeepEqual(specJSON(got), specJSON(bases["already narrower"])) {
		t.Errorf("a copy naming allow_all_egress true: refusal %+v spec %+v, want 422 %s and nothing applied", refusal, got, reasonWallPresetInvalid)
	}
}

// TestWallPresetToolRulesComposeInAnyOrder: a preset's tool rules read the same in either order, and the
// preset step's result is never wider than the preset or the policy it starts from for any tool.
func TestWallPresetToolRulesComposeInAnyOrder(t *testing.T) {
	srv, _, _ := uvServer(t)
	star, bashHold := types.ToolRule{Tool: "*", Effect: types.ToolDeny}, types.ToolRule{Tool: "Bash", Effect: types.ToolHold}
	inherited := types.RunPolicySpec{ToolRules: []types.ToolRule{{Tool: "Bash", Effect: types.ToolDeny}}}
	for _, doc := range [][]types.ToolRule{{star, bashHold}, {bashHold, star}} {
		p := &types.WallPresetOnRun{ID: uuid.NewString(), Name: "Tools", Scope: types.WallPresetScopeOrg, Version: 1,
			Spec: types.PersonalWallsSpec{ToolRules: &doc}}
		for name, base := range map[string]types.RunPolicySpec{"no rules": {}, "inherited deny": inherited} {
			spec, source := base.Clone(), policySourceRecord{knobs: startProvenance(memberProvenance)}
			if _, refusal := srv.wallPresetRunStep(&spec, &source, p); refusal != nil {
				t.Fatalf("%v over %s: refusal %+v", doc, name, refusal)
			}
			want := types.ToolHold
			if name == "inherited deny" {
				want = types.ToolDeny
			}
			if effectiveToolEffect(spec.ToolRules, "Bash") != want || effectiveToolEffect(spec.ToolRules, "Glob") != types.ToolDeny {
				t.Errorf("%v over %s: %+v, want Bash %s and an unnamed tool denied", doc, name, spec.ToolRules, want)
			}
			if layerRank(spec.ToolRules, "Bash") < layerRank(doc, "Bash") || layerRank(spec.ToolRules, "Glob") < layerRank(doc, "Glob") ||
				layerRank(spec.ToolRules, "Bash") < layerRank(base.ToolRules, "Bash") {
				t.Errorf("%v over %s: %+v is wider than the preset or the policy", doc, name, spec.ToolRules)
			}
		}
	}
}

// TestWallPresetOnEveryArm: a launch's wall preset binds the default arm (the preview shows it) and the
// inline arm, where run.create records it and the run row keeps the frozen copy.
func TestWallPresetOnEveryArm(t *testing.T) {
	srv, _ := wallsRunFixture(t)
	member := ssoSession(t, wallsSub, "walls@corp.example", oidc.RoleUser)

	w := doSSO(t, srv, http.MethodPost, previewDoor, member, `{"agent":"claude-code","task":"t","wall_preset":"sealed"}`)
	var pv policyPreviewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &pv); err != nil || w.Code != http.StatusOK {
		t.Fatalf("preview %d %v: %s", w.Code, err, w.Body)
	}
	if pv.Spec.AllowAllEgress || pv.Spec.FirstUseApproval != types.FirstUseAlwaysDeny || pv.Spec.PushRules == nil || !pv.Spec.PushRules.DenyAll {
		t.Errorf("default arm under Sealed: open %v first use %q push_rules %+v, want closed, always_deny and every push refused",
			pv.Spec.AllowAllEgress, pv.Spec.FirstUseApproval, pv.Spec.PushRules)
	}

	body := `{"agent":"claude-code","task":"t","wall_preset":"read-only","inline_policy":{"min_confinement_class":"CC2","allowed_domains":["api.anthropic.com"]}}`
	w = doSSO(t, srv, http.MethodPost, "/api/v1/runs", member, body)
	var created struct {
		ID uuid.UUID `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || w.Code != http.StatusCreated {
		t.Fatalf("create = %d %v: %s", w.Code, err, w.Body)
	}
	ev := waitForRecAudit(t, srv.cfg.Audit.(*recRecorder), created.ID, "run.create", "success")
	var data struct {
		PolicySource struct {
			WallPreset *wallPresetSourceRecord `json:"wall_preset"`
		} `json:"policy_source"`
	}
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatal(err)
	}
	if p := data.PolicySource.WallPreset; p == nil || p.ID != types.WallPresetReadOnly || p.Name != "Read-only" || p.Version != 1 || p.Scope != types.WallPresetScopeBuiltin {
		t.Errorf("inline arm: policy_source.wall_preset = %+v, want Read-only, built in, version 1", p)
	}
	run, err := srv.cfg.Store.GetRun(context.Background(), created.ID)
	if err != nil || run.WallPreset == nil || run.WallPreset.ID != types.WallPresetReadOnly || !run.WallPreset.Spec.DeniesAllPushes() {
		t.Errorf("the run row's wall preset = %+v (%v), want the frozen Read-only copy", run.WallPreset, err)
	}
}

// TestWallPresetUnknownNeverRunsOpen: a launch naming a preset that does not exist, or that this store
// cannot hold, is refused 422 wall_preset_unknown at Review and at create; it is never run as Open.
func TestWallPresetUnknownNeverRunsOpen(t *testing.T) {
	srv, _ := wallsRunFixture(t)
	member := ssoSession(t, wallsSub, "walls@corp.example", oidc.RoleUser)
	for _, id := range []string{"Sealed", "no-such-preset", uuid.NewString()} {
		for _, door := range []string{previewDoor, "/api/v1/runs"} {
			w := doSSO(t, srv, http.MethodPost, door, member, `{"agent":"claude-code","task":"t","wall_preset":"`+id+`"}`)
			if w.Code != http.StatusUnprocessableEntity || wireReason(t, w) != reasonWallPresetUnknown {
				t.Errorf("%s with wall preset %q = %d %s, want 422 %s", door, id, w.Code, w.Body, reasonWallPresetUnknown)
			}
		}
	}
}

// TestWallPresetServiceIdentity: a service identity applies the built-ins and an organisation preset,
// and a group preset answers as a missing one (422 wall_preset_unknown), as does someone else's own
// preset; a member of the group applies it, so the refusal is the authority's and not the lookup's.
func TestWallPresetServiceIdentity(t *testing.T) {
	e := newWallPresetEnv(t)
	org, grp, mine := tightPreset(types.WallPresetScopeOrg, "Org tight"), tightPreset(types.WallPresetScopeGroup, "Eng tight"),
		tightPreset(types.WallPresetScopePerson, "Mine")
	for _, p := range []types.WallPreset{org, grp, mine} {
		e.wp.presets[p.ID] = p
	}
	launch := func(ctx context.Context, id string) (*types.WallPresetOnRun, *runRefusal) {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(ctx)
		return e.srv.launchWallPreset(ctx, r, id)
	}
	service := withServiceIdentity(context.Background(), "ci-bot", "sub-sponsor")
	for _, id := range []string{types.WallPresetSealed, org.ID} {
		if p, refusal := launch(service, id); refusal != nil || p == nil || p.ID != id || p.Spec.Empty() {
			t.Errorf("service identity, %s: copy %+v refusal %+v, want the preset applied", id, p, refusal)
		}
	}
	for _, id := range []string{grp.ID, mine.ID} {
		if p, refusal := launch(service, id); p != nil || refusal == nil || refusal.status != http.StatusUnprocessableEntity || refusal.body.Reason != reasonWallPresetUnknown {
			t.Errorf("service identity, %s: copy %+v refusal %+v, want 422 %s", id, p, refusal, reasonWallPresetUnknown)
		}
	}
	member := withHumanIdentity(context.Background(), "gina", "gina@corp.example", oidc.RoleUser, types.UserTypeStandard, []string{"eng"}, false)
	if p, refusal := launch(member, grp.ID); refusal != nil || p == nil || p.GroupID != "eng" {
		t.Errorf("a member of the group: copy %+v refusal %+v, want the group preset applied", p, refusal)
	}
}

// TestWallPresetEditAfterLaunch: a stored preset edited, then deleted, after a run launched with it
// changes nothing on that run: its seal layer, recording floor and approval bound read the run's frozen
// copy, never the store. The queue's start sees the edit and the delete (queuePolicyRef), so a waiting
// run ends rather than starting under a preset nobody reviewed.
func TestWallPresetEditAfterLaunch(t *testing.T) {
	e := newWallPresetEnv(t)
	stored := tightPreset(types.WallPresetScopeOrg, "Org tight")
	e.wp.presets[stored.ID] = stored
	ctx := memberCtxFor(wallsSub)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(ctx)
	frozen, refusal := e.srv.launchWallPreset(ctx, r, stored.ID)
	if refusal != nil {
		t.Fatalf("launch: %+v", refusal)
	}
	before, err := e.srv.queuePolicyRef(ctx, nil, createRunRequest{WallPreset: stored.ID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	run := types.AgentRun{ID: uuid.New(), CreatedBy: wallsSub, WallPreset: frozen}

	edited := stored
	edited.Spec, edited.Version = types.PersonalWallsSpec{DeniedDomains: wallsList("other.example")}, 2
	e.wp.presets[stored.ID] = edited
	after, err := e.srv.queuePolicyRef(ctx, nil, createRunRequest{WallPreset: stored.ID}, nil)
	if err != nil || jsonEqual(before, after) {
		t.Errorf("queue ref before %s after the edit %s (%v), want them to differ", before, after, err)
	}
	if pinned, err := e.srv.queuePolicyRef(ctx, nil, createRunRequest{WallPreset: stored.ID}, frozen); err != nil || !jsonEqual(before, pinned) {
		t.Errorf("queue ref from the frozen copy after the edit = %s (%v), want the launch's %s: the copy is recorded, never a second read", pinned, err, before)
	}
	delete(e.wp.presets, stored.ID)
	gone, err := e.srv.queuePolicyRef(ctx, nil, createRunRequest{WallPreset: stored.ID}, nil)
	if err != nil || !strings.Contains(string(gone), `"wall_preset_gone":true`) {
		t.Errorf("queue ref after the delete = %s (%v), want wall_preset_gone", gone, err)
	}
	builtin, _ := e.srv.queuePolicyRef(ctx, nil, createRunRequest{WallPreset: types.WallPresetSealed}, nil)
	if again, _ := e.srv.queuePolicyRef(ctx, nil, createRunRequest{WallPreset: types.WallPresetSealed}, nil); !jsonEqual(builtin, again) {
		t.Errorf("a built-in's queue ref moved: %s then %s", builtin, again)
	}

	layers := wallPresetSealLayer(run)
	if len(layers) != 1 || !layers[0].denyAllPushes || layers[0].rec != wallPresetProvenance(frozen) {
		t.Fatalf("seal layers %+v, want the frozen copy's, refusing every push", layers)
	}
	spec := types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com", "pastebin.com"}}
	sealPolicy(&spec, layers)
	if !slices.Contains(spec.DeniedDomains, "pastebin.com") || slices.Contains(spec.DeniedDomains, "other.example") ||
		spec.Desktop.Effective().PasteIn != types.TransferBlocked {
		t.Errorf("sealed denied %v desktop %+v, want the launch's pastebin.com deny and paste_in block, not the edit's", spec.DeniedDomains, spec.Desktop.Effective())
	}
	if w := (withholding{layers: layers}); !w.credentialWithheld(wallsSSH) {
		t.Error("the frozen copy's withheld ssh_key was not withheld")
	}
	if floor, err := e.srv.wallsRecordingFloor(context.Background(), run); err != nil || floor != types.RecordingTerminal {
		t.Errorf("recording floor = %q (%v), want the frozen copy's terminal", floor, err)
	}

	approve := func(decider, host string) (*httptest.ResponseRecorder, bool) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/approvals/x/approve", nil).WithContext(memberCtxFor(decider))
		ap := types.ApprovalRequest{RunID: run.ID, Kind: types.ApprovalEgressDomain, RequestedScope: json.RawMessage(`{"host":"` + host + `"}`)}
		_, _, ok := e.srv.wallsAdmitApproval(w, req, ap, run, true)
		return w, ok
	}
	w, ok := approve("sub-approver", "pastebin.com")
	var body errorBody
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if ok || w.Code != http.StatusForbidden || body.Reason != reasonWallPresetHost || !strings.Contains(body.Error, "the Org tight wall preset") ||
		body.Policy == nil || body.Policy.Source != policyref.LayerWallPreset || body.Policy.Name != "Org tight" {
		t.Errorf("approving a host the frozen copy denies: ok %v %d %s, want 403 %s naming the preset", ok, w.Code, w.Body, reasonWallPresetHost)
	}
	if _, ok := approve("sub-approver", "other.example"); !ok {
		t.Error("a host only the edit denies was refused; the run keeps the copy it launched with")
	}
}

// TestWallPresetPersonalNameStaysWithItsOwner: a personal preset on a run is named to the run's owner
// only. Anyone else reading the run, or refused by it at an approval, is told a personal wall preset
// applied, with no name; an organisation preset is named to everyone.
func TestWallPresetPersonalNameStaysWithItsOwner(t *testing.T) {
	e := newWallPresetEnv(t)
	mine := frozenPreset(tightPreset(types.WallPresetScopePerson, "Mine"))
	run := types.AgentRun{ID: uuid.New(), CreatedBy: wallsSub, WallPreset: mine}
	reader := func(sub string) *http.Request {
		return httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+run.ID.String(), nil).WithContext(memberCtxFor(sub))
	}
	if v := runWallPresetView(reader(wallsSub), run); v == nil || v.Name != "Mine" || v.ID != mine.ID || v.Version != 1 {
		t.Errorf("the owner's read = %+v, want the name, id and version", v)
	}
	if v := runWallPresetView(reader("sub-admin"), run); v == nil || *v != (client.RunWallPreset{Scope: string(types.WallPresetScopePerson)}) {
		t.Errorf("someone else's read = %+v, want the scope alone", v)
	}
	org := run
	org.WallPreset = frozenPreset(tightPreset(types.WallPresetScopeOrg, "Org tight"))
	if v := runWallPresetView(reader("sub-admin"), org); v == nil || v.Name != "Org tight" {
		t.Errorf("an org preset read by someone else = %+v, want its name", v)
	}
	if runWallPresetView(reader(wallsSub), types.AgentRun{ID: uuid.New(), CreatedBy: wallsSub}) != nil {
		t.Error("a run with no wall preset reads one")
	}

	for decider, want := range map[string]string{wallsSub: "the Mine wall preset", "sub-approver": "a personal wall preset"} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/approvals/x/approve", nil).WithContext(memberCtxFor(decider))
		ap := types.ApprovalRequest{RunID: run.ID, Kind: types.ApprovalEgressDomain, RequestedScope: json.RawMessage(`{"host":"pastebin.com"}`)}
		_, _, ok := e.srv.wallsAdmitApproval(w, req, ap, run, true)
		var body errorBody
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if ok || !strings.Contains(body.Error, want) || body.Policy == nil || body.Policy.Name != "" || strings.Contains(w.Body.String(), "Mine") && decider != wallsSub {
			t.Errorf("%s approving: %s, want 403 naming %q and no preset name in the policy", decider, w.Body, want)
		}
	}
	if p := wallPresetProvenance(mine); p.Name != "" || p.Layer != policyref.LayerWallPreset {
		t.Errorf("a personal preset's provenance = %+v, want the layer and no name", p)
	}
}

// TestWallsAndPresetReassertOnRestart: the owner's walls and the run's wall preset each give the seal a
// layer that refuses every push, and a revive or a restart puts deny_all back on the proxy config it
// replays, on whichever regime the config carries its rules.
func TestWallsAndPresetReassertOnRestart(t *testing.T) {
	srv, st, _ := uvServer(t)
	on := true
	saveWalls(t, st, wallsSub, types.PersonalWallsSpec{PushRules: &types.PersonalWallsPushRules{DenyAll: &on}})
	if l := srv.personalWallsSealLayer(context.Background(), types.AgentRun{ID: uuid.New(), CreatedBy: wallsSub}); len(l) != 1 || !l[0].denyAllPushes {
		t.Errorf("walls refusing every push: layers %+v, want one that refuses every push", l)
	}
	if l := wallPresetSealLayer(types.AgentRun{ID: uuid.New(), WallPreset: builtinFrozen(t, types.WallPresetReadOnly)}); len(l) != 1 || !l[0].denyAllPushes {
		t.Errorf("Read-only: layers %+v, want one that refuses every push", l)
	}
	if l := wallPresetSealLayer(types.AgentRun{ID: uuid.New(), WallPreset: builtinFrozen(t, types.WallPresetOpen)}); l != nil {
		t.Errorf("Open: layers %+v, want none", l)
	}

	refuses := []sealLayer{{rec: wallPresetProvenance(builtinFrozen(t, types.WallPresetSealed)), denyAllPushes: true}}
	runWide := &proxy.Config{Policy: types.RunPolicySpec{PushRules: &types.PushRulesSpec{DenyPaths: []string{"secrets/**"}}}}
	reassertDenyAllPushes(runWide, refuses)
	if r := runWide.Policy.PushRules; r == nil || !r.DenyAll || !slices.Equal(r.DenyPaths, []string{"secrets/**"}) || runWide.PushRuleBase != nil {
		t.Errorf("run-wide rules = %+v base %+v, want deny_all added and the deny paths kept", r, runWide.PushRuleBase)
	}
	none := &proxy.Config{}
	reassertDenyAllPushes(none, refuses)
	if none.Policy.PushRules == nil || !none.Policy.PushRules.DenyAll {
		t.Errorf("a config with no rules = %+v, want deny_all", none.Policy.PushRules)
	}
	sets := []types.PushRuleSet{{Provider: "github", Host: "github.com", Org: "acme", Rules: types.PushRulesSpec{DenyPaths: []string{"ci/**"}}}}
	perEntry := &proxy.Config{PushRuleSets: sets}
	reassertDenyAllPushes(perEntry, refuses)
	if !perEntry.PushRuleSets[0].Rules.DenyAll || perEntry.PushRuleBase == nil || !perEntry.PushRuleBase.DenyAll || perEntry.Policy.PushRules != nil {
		t.Errorf("per-entry rules = %+v base %+v policy %+v, want deny_all on every set and the base, none on the policy",
			perEntry.PushRuleSets, perEntry.PushRuleBase, perEntry.Policy.PushRules)
	}
	if sets[0].Rules.DenyAll {
		t.Error("the re-assertion wrote through to the caller's slice")
	}
	untouched := &proxy.Config{}
	reassertDenyAllPushes(untouched, []sealLayer{{rec: personalProvenance}})
	if untouched.Policy.PushRules != nil {
		t.Errorf("no layer refuses every push, yet rules = %+v", untouched.Policy.PushRules)
	}

	// The revive itself: a run launched with Sealed whose stored config predates any push rule.
	f := newReviveFixture(t)
	f.st.run.WallPreset = builtinFrozen(t, types.WallPresetSealed)
	f.run = f.st.run
	if code := f.revive(t); code != http.StatusOK {
		t.Fatalf("revive: code %d, want 200", code)
	}
	cfg := f.newConfig(t)
	if cfg.Policy.PushRules == nil || !cfg.Policy.PushRules.DenyAll {
		t.Errorf("revived push_rules = %+v, want the preset's deny_all re-asserted", cfg.Policy.PushRules)
	}
	if d := cfg.Policy.Desktop.Effective(); d.PasteIn != types.TransferBlocked || d.Upload != types.TransferBlocked {
		t.Errorf("revived desktop = %+v, want the preset's blocks re-asserted", d)
	}
}

// TestWallPresetListGroupLabels: the list labels the groups the caller may save to and those of the
// group presets they can read, so a card and `--group <label>` name a group by its label; a group
// outside both is never labelled, and a caller in no group gets no labels.
func TestWallPresetListGroupLabels(t *testing.T) {
	e := newWallPresetEnv(t)
	for _, g := range []types.TemplateGroupAdmin{{Group: "eng", Person: "gary", Label: "Engineering"},
		{Group: "ops", Person: "olga", Label: "Operations"}, {Group: "hr", Person: "hana", Label: "People"}} {
		g.GrantedBy = "admin"
		if _, err := e.ts.PutTemplateGroupAdmin(context.Background(), g); err != nil {
			t.Fatal(err)
		}
	}
	olga := templateCookie(t, "olga", oidc.RoleUser, false, []string{"ops"}, false)
	e.save(olga, `{"scope":"group","group_id":"ops","name":"Ops tight",`+tightSpec+`}`)

	list := func(c *http.Cookie) client.WallPresetList {
		var out client.WallPresetList
		if err := json.Unmarshal([]byte(e.expect(c, http.MethodGet, "/api/v1/wall-presets", "", http.StatusOK, "")), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	gary := templateCookie(t, "gary", oidc.RoleUser, false, []string{"eng", "ops"}, false)
	got := list(gary)
	if want := map[string]string{"eng": "Engineering", "ops": "Operations"}; !maps.Equal(got.Publish.GroupLabels, want) {
		t.Errorf("gary's group_labels = %v, want %v (eng to save to, ops from its preset, never hr)", got.Publish.GroupLabels, want)
	}
	if !slices.Equal(got.Publish.Groups, []string{"eng"}) {
		t.Errorf("gary's publish groups = %v, want [eng]", got.Publish.Groups)
	}
	bob := templateCookie(t, "bob", oidc.RoleUser, false, []string{}, false)
	if got := list(bob); len(got.Publish.GroupLabels) != 0 {
		t.Errorf("bob's group_labels = %v, want none", got.Publish.GroupLabels)
	}
}

// TestWallPresetPersonalNameNeverKept: what a personal preset leaves on the run, which an admin reads
// too, says only that a personal wall preset applied: the warnings (kept as clamp_warnings and shown by
// the policy view), the refusals that become a queued run's end or a dispatch refusal, and the policy
// view's wall_preset. Its name and its values never appear there; an org preset keeps both.
func TestWallPresetPersonalNameNeverKept(t *testing.T) {
	srv, _, _ := uvServer(t)
	mine := frozenPreset(tightPreset(types.WallPresetScopePerson, "Mine"))
	never := func(where, text string, also ...string) {
		t.Helper()
		for _, s := range append([]string{"Mine"}, also...) {
			if strings.Contains(text, s) {
				t.Errorf("%s = %q, want no %q", where, text, s)
			}
		}
	}

	spec, source := types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com", "pastebin.com"}, AutoStopAfterSec: 3600}, policySourceRecord{knobs: startProvenance(memberProvenance)}
	warns, refusal := srv.wallPresetRunStep(&spec, &source, mine)
	if refusal != nil || len(warns) == 0 {
		t.Fatalf("warnings %v refusal %+v, want the knobs it narrowed", warns, refusal)
	}
	for _, w := range warns {
		never("warning", w)
		if !strings.HasPrefix(w, "a personal wall preset narrowed ") {
			t.Errorf("warning %q, want it to name a personal wall preset", w)
		}
	}

	on := true
	bad := *mine
	bad.Spec = types.PersonalWallsSpec{AllowAllEgress: &on}
	spec = types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}}
	if _, refusal := srv.wallPresetRunStep(&spec, &policySourceRecord{knobs: startProvenance(memberProvenance)}, &bad); refusal == nil {
		t.Error("a copy naming allow_all_egress true was applied")
	} else {
		never("invalid refusal", refusal.body.Error, "allow_all_egress")
	}

	unmet := *mine
	unmet.Spec = types.PersonalWallsSpec{AzureDevOpsCapabilities: &[]adoscope.Capability{adoscope.CapCodeWrite}}
	spec = types.RunPolicySpec{AzureDevOpsCapabilities: []adoscope.Capability{adoscope.CapCodeRead}}
	if _, refusal := srv.wallPresetRunStep(&spec, &policySourceRecord{knobs: startProvenance(memberProvenance)}, &unmet); refusal == nil ||
		refusal.body.Reason != reasonGovernanceOverlayUnsatisfiable {
		t.Errorf("a capability list sharing nothing: refusal %+v, want 403 %s", refusal, reasonGovernanceOverlayUnsatisfiable)
	} else {
		never("meet refusal", refusal.body.Error, "code_write")
		if !strings.HasPrefix(refusal.body.Error, "A personal wall preset and this run's policy leave ") {
			t.Errorf("meet refusal = %q, want it to name a personal wall preset", refusal.body.Error)
		}
	}

	ghWall := func(w types.PersonalWallsSpec) *[]ghscope.Capability { return w.GitHubCapabilities }
	caps := []ghscope.Capability{ghscope.CapCodeRead}
	unmet.Spec = types.PersonalWallsSpec{GitHubCapabilities: &[]ghscope.Capability{ghscope.CapCodeWrite}}
	run := types.AgentRun{ID: uuid.New(), CreatedBy: wallsSub, WallPreset: &unmet}
	if _, refused := wallsCapabilityMeet(context.Background(), srv, run, "github_capabilities", caps, ghWall); refused == nil {
		t.Error("a personal preset sharing no capability with the default was met")
	} else {
		never("capability refusal", refused.detail, "code_write")
	}
	org := frozenPreset(tightPreset(types.WallPresetScopeOrg, "Org tight"))
	org.Spec = unmet.Spec
	run.WallPreset = org
	if _, refused := wallsCapabilityMeet(context.Background(), srv, run, "github_capabilities", caps, ghWall); refused == nil ||
		!strings.Contains(refused.detail, "The Org tight wall preset") || !strings.Contains(refused.detail, "(code_write)") {
		t.Errorf("an org preset's capability refusal = %+v, want its name and its list", refused)
	}

	runOf := types.AgentRun{ID: uuid.New(), CreatedBy: wallsSub}
	rec := &wallPresetSourceRecord{ID: mine.ID, Name: "Mine", Scope: types.WallPresetScopePerson, Version: 1,
		WallsSourceRecord: &wallsSourceRecord{Narrowed: []string{"first_use_approval"}}}
	reader := func(sub string) *http.Request {
		return httptest.NewRequest(http.MethodGet, "/api/v1/runs/x/policy", nil).WithContext(memberCtxFor(sub))
	}
	if v := runPolicyWallPresetOf(reader("sub-admin"), runOf, rec); v == nil || !reflect.DeepEqual(*v, runPolicyWallPreset{Scope: "person"}) {
		t.Errorf("an admin's policy view wall_preset = %+v, want the scope alone", v)
	}
	if v := runPolicyWallPresetOf(reader(wallsSub), runOf, rec); v == nil || v.Name != "Mine" || !slices.Equal(v.Narrowed, []string{"first_use_approval"}) {
		t.Errorf("the owner's policy view wall_preset = %+v, want the name and what it narrowed", v)
	}
}

// TestWallPresetCapabilityMeetSecondList: the preset's list meets what the owner's walls kept, never
// the provider default afresh, so a capability either leaves out is not given.
func TestWallPresetCapabilityMeetSecondList(t *testing.T) {
	srv, st, _ := uvServer(t)
	saveWalls(t, st, wallsSub, types.PersonalWallsSpec{GitHubCapabilities: &[]ghscope.Capability{ghscope.CapCodeRead, ghscope.CapIssuesWrite}})
	org := frozenPreset(tightPreset(types.WallPresetScopeOrg, "Org tight"))
	org.Spec = types.PersonalWallsSpec{GitHubCapabilities: &[]ghscope.Capability{ghscope.CapCodeRead, ghscope.CapCodeWrite}}
	run := types.AgentRun{ID: uuid.New(), CreatedBy: wallsSub, WallPreset: org}
	ghWall := func(w types.PersonalWallsSpec) *[]ghscope.Capability { return w.GitHubCapabilities }
	caps := []ghscope.Capability{ghscope.CapCodeRead, ghscope.CapCodeWrite, ghscope.CapIssuesWrite}
	kept, refused := wallsCapabilityMeet(context.Background(), srv, run, "github_capabilities", caps, ghWall)
	if refused != nil || !slices.Equal(kept, []ghscope.Capability{ghscope.CapCodeRead}) {
		t.Errorf("kept %v refused %+v, want [code_read]: the walls drop code_write, the preset issues_write", kept, refused)
	}
	run.WallPreset = nil
	if kept, _ := wallsCapabilityMeet(context.Background(), srv, run, "github_capabilities", caps, ghWall); !slices.Equal(kept, []ghscope.Capability{ghscope.CapCodeRead, ghscope.CapIssuesWrite}) {
		t.Errorf("without a preset kept %v, want the walls' own [code_read issues_write]", kept)
	}
}

// TestGitPushWithheldByThePreset: a preset whose capability list leaves out code_write withholds the
// push on that forge even when neither the policy nor the walls name a list.
func TestGitPushWithheldByThePreset(t *testing.T) {
	read := frozenPreset(tightPreset(types.WallPresetScopeOrg, "Read"))
	read.Spec = types.PersonalWallsSpec{GitHubCapabilities: &[]ghscope.Capability{ghscope.CapCodeRead}}
	if w := gitPushWithheldBy(types.RunPolicySpec{}, types.PersonalWallsSpec{}, read); !w.github || w.ado {
		t.Errorf("a preset listing github code_read only: %+v, want github withheld, ado not", w)
	}
	read.Spec = types.PersonalWallsSpec{AzureDevOpsCapabilities: &[]adoscope.Capability{adoscope.CapCodeRead}}
	if w := gitPushWithheldBy(types.RunPolicySpec{}, types.PersonalWallsSpec{}, read); w.github || !w.ado {
		t.Errorf("a preset listing ado code_read only: %+v, want ado withheld, github not", w)
	}
	if w := gitPushWithheldBy(types.RunPolicySpec{}, types.PersonalWallsSpec{}, nil); w.github || w.ado {
		t.Errorf("no preset, no lists: %+v, want nothing withheld", w)
	}
}

// TestPolicyViewAttributesThePreset: in the policy view, what the person's own walls changed is cause
// walls and what the run's wall preset changed is cause wall_preset, each entry under its own; a knob
// both narrowed is the walls', whose step runs first; what neither changed is neither's.
func TestPolicyViewAttributesThePreset(t *testing.T) {
	ev := evidenceOf(func(e *auditEvidence) {
		e.walls = &wallsSourceRecord{Narrowed: []string{"auto_stop_after_sec"}, Removed: map[string][]string{fieldAllowed: {"pypi.org"}}}
		e.preset = &wallsSourceRecord{Narrowed: []string{"first_use_approval", "auto_stop_after_sec"},
			Removed: map[string][]string{fieldAllowed: {"npm.example"}}, Added: map[string][]string{fieldDenied: {"pastebin.com"}}}
	})
	base := pvSpec([]string{"api.anthropic.com", "pypi.org", "npm.example"}, nil)
	got := explainRunPolicy(&base, pvSpec([]string{"api.anthropic.com"}, func(sp *types.RunPolicySpec) { sp.DeniedDomains = []string{"pastebin.com"} }),
		ev, types.SiteConfig{}, types.AgentRun{})
	if c := pvChange(got, causeWalls, fieldAllowed); c == nil || !slices.Equal(c.Removed, []string{"pypi.org"}) {
		t.Errorf("walls allowed_domains change = %+v, want pypi.org alone (changes %+v)", c, got)
	}
	if c := pvChange(got, causeWallPreset, fieldAllowed); c == nil || !slices.Equal(c.Removed, []string{"npm.example"}) {
		t.Errorf("wall_preset allowed_domains change = %+v, want npm.example alone (changes %+v)", c, got)
	}
	if c := pvChange(got, causeWallPreset, fieldDenied); c == nil || !slices.Equal(c.Added, []string{"pastebin.com"}) {
		t.Errorf("wall_preset denied_domains change = %+v, want pastebin.com (changes %+v)", c, got)
	}
	if pvChange(got, causeWalls, fieldDenied) != nil {
		t.Errorf("the preset's deny was given to the person's walls: %+v", got)
	}

	x := &explainer{ev: ev}
	for _, tc := range []struct {
		field, entry string
		added        bool
		want         string
	}{
		{"first_use_approval", "", false, causeWallPreset},
		{"auto_stop_after_sec", "", false, causeWalls},
		{fieldAllowed, "github.com", false, ""},
	} {
		if cause, _, _, _ := x.evidenceCause(tc.field, tc.entry, tc.added); cause != tc.want {
			t.Errorf("evidenceCause(%s, %q) = %q, want %q", tc.field, tc.entry, cause, tc.want)
		}
	}
}

// TestQueueWallPresetChangeDetail: a queued run ended because its wall preset changed carries what the
// preset became, the version it was queued under, and a new version or gone; the run read names a
// personal preset to its owner only, and gives anyone the versions.
func TestQueueWallPresetChangeDetail(t *testing.T) {
	ref := func(v map[string]any) json.RawMessage { return mustJSON(v) }
	id := uuid.NewString()
	queued := ref(map[string]any{"wall_preset": id, "wall_preset_version": 1})
	for _, tc := range []struct {
		name string
		now  json.RawMessage
		want *types.WallPresetChange
	}{
		{"unchanged", ref(map[string]any{"wall_preset": id, "wall_preset_version": 1}), nil},
		{"edited", ref(map[string]any{"wall_preset": id, "wall_preset_version": 3}), &types.WallPresetChange{Version: 3}},
		{"gone", ref(map[string]any{"wall_preset": id, "wall_preset_gone": true}), &types.WallPresetChange{Gone: true}},
		{"only the stored policy moved", ref(map[string]any{"wall_preset": id, "wall_preset_version": 1, "spec_sha256": "x"}), nil},
	} {
		v, got := queueWallPresetChange(queued, tc.now)
		if !reflect.DeepEqual(got, tc.want) || (tc.want != nil && v != 1) {
			t.Errorf("%s: queued %d change %+v, want 1 and %+v", tc.name, v, got, tc.want)
		}
	}
	if _, got := queueWallPresetChange(ref(map[string]any{}), ref(map[string]any{"policy_id": "p"})); got != nil {
		t.Errorf("a run with no wall preset: change %+v, want none", got)
	}

	mine := frozenPreset(tightPreset(types.WallPresetScopePerson, "Mine"))
	mine.Changed = &types.WallPresetChange{Version: 2}
	run := types.AgentRun{ID: uuid.New(), CreatedBy: wallsSub, WallPreset: mine}
	reader := func(sub string) *http.Request {
		return httptest.NewRequest(http.MethodGet, "/api/v1/runs/x", nil).WithContext(memberCtxFor(sub))
	}
	if v := runWallPresetView(reader("sub-admin"), run); v == nil || v.Name != "" || v.ID != "" || v.Version != 1 || v.Changed == nil || v.Changed.Version != 2 {
		t.Errorf("an admin's read of a personal preset that changed = %+v, want scope, versions 1 then 2, and no name", v)
	}
	if v := runWallPresetView(reader(wallsSub), run); v == nil || v.Name != "Mine" || v.Changed == nil || v.Changed.Version != 2 {
		t.Errorf("the owner's read = %+v, want the name and the change", v)
	}
	org := frozenPreset(tightPreset(types.WallPresetScopeOrg, "Org tight"))
	org.Changed = &types.WallPresetChange{Gone: true}
	run.WallPreset = org
	if v := runWallPresetView(reader("sub-admin"), run); v == nil || v.Name != "Org tight" || v.Version != 1 || v.Changed == nil || !v.Changed.Gone {
		t.Errorf("an org preset that went = %+v, want its name, version 1 and gone", v)
	}
}
