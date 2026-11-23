// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/ghscope"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// An organisation's rules for each agent harness (agent_rules.go, run_agent_rules.go): the type admits no
// widening, every knob a write accepts is enforced, the run step binds every arm and caller, and a run without
// the agent is not bound.

// agentRulesTestStore is wallsStore plus an agent rules table that answers rules, or rulesErr, and run
// harness rows that answer harnessErr when it is set.
type agentRulesTestStore struct {
	*wallsStore
	rules      []types.AgentRules
	rulesErr   error
	harnessErr error
}

func (s *agentRulesTestStore) ListAgentRules(context.Context) ([]types.AgentRules, error) {
	if s.rulesErr != nil {
		return nil, s.rulesErr
	}
	return slices.Clone(s.rules), nil
}

func (s *agentRulesTestStore) PutAgentRules(context.Context, types.AgentRules, int) (types.AgentRules, error) {
	return types.AgentRules{}, errors.New("the fake keeps its rules read-only")
}

func (s *agentRulesTestStore) DeleteAgentRules(context.Context, string, int) (types.AgentRules, error) {
	return types.AgentRules{}, errors.New("the fake keeps its rules read-only")
}

func (s *agentRulesTestStore) ListRunHarnesses(ctx context.Context, id uuid.UUID) ([]types.RunHarness, error) {
	if s.harnessErr != nil {
		return nil, s.harnessErr
	}
	return s.wallsStore.ListRunHarnesses(ctx, id)
}

var (
	_ store.AgentRulesStore = (*agentRulesTestStore)(nil)
	_ store.RunHarnessStore = (*agentRulesTestStore)(nil)
)

// agentRulesServer is the walls fixture over a store that keeps rules.
func agentRulesServer(t *testing.T, rules ...types.AgentRules) (*Server, *agentRulesTestStore) {
	t.Helper()
	srv, ws := wallsRunFixture(t)
	st := &agentRulesTestStore{wallsStore: ws, rules: rules}
	srv.cfg.Store = st
	return srv, st
}

func claudeRules(spec types.PersonalWallsSpec) types.AgentRules {
	return types.AgentRules{Harness: "claude-code", Spec: spec, Version: 3, UpdatedBy: "sub-admin"}
}

// agentRulesStep runs the rules of req's harnesses over spec and answers the spec, its source and the refusal.
func agentRulesStep(t *testing.T, srv *Server, req createRunRequest, spec types.RunPolicySpec) (types.RunPolicySpec, policySourceRecord, []string, *runRefusal) {
	t.Helper()
	spec = spec.Clone()
	source := policySourceRecord{knobs: startProvenance(memberProvenance)}
	warns, refusal := srv.agentRulesRunStep(context.Background(), req, &spec, &source)
	return spec, source, warns, refusal
}

var claudeRun = createRunRequest{Agent: "claude-code", Task: "t"}

// agentRulesKnobRow is one knob an agent rule accepts and enforces: a rule that names it, a policy that
// leaves it unset, a policy that sets it wider than the rule, and a policy already as narrow; what the resolved
// spec shows (effect, kind "unset" or "wide"), the seal layer's effect at dispatch, and the recording floor.
type agentRulesKnobRow struct {
	rule                types.PersonalWallsSpec
	unset, wide, narrow types.RunPolicySpec
	unsetNoop           bool
	knob                string
	effect              func(kind string, s types.RunPolicySpec) bool
	seal                func(layers []sealLayer) bool
	floor               types.RecordingLevel
}

func sealHas(layers []sealLayer, f func(sealLayer) bool) bool {
	for _, l := range layers {
		if l.rec.Layer == policyref.LayerAgentRules && f(l) {
			return true
		}
	}
	return false
}

// agentRulesKnobRows is keyed by json name, one row per enforced knob (push_rules is deny_all).
var agentRulesKnobRows = func() map[string]agentRulesKnobRow {
	on, off := true, false
	always, review := types.FirstUseAlwaysDeny, types.FirstUseWaitForReview
	cc3, screen, terminal, sec := types.CC3, types.RecordingScreenActions, types.RecordingTerminal, 600
	blocked := types.DesktopControls{PasteIn: types.TransferBlocked}
	allowed := types.DesktopControls{PasteIn: types.TransferAllowed}
	hold := types.ToolRule{Tool: "Bash", Effect: types.ToolHold}
	deny := func(r []types.ToolRule) bool { return effectiveToolEffect(r, "Bash") != types.ToolAllow }
	return map[string]agentRulesKnobRow{
		"allowed_domains": {rule: types.PersonalWallsSpec{AllowedDomains: wallsList("api.anthropic.com")}, knob: "allowed_domains",
			unset: types.RunPolicySpec{AllowAllEgress: true}, wide: types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com", "pypi.org"}},
			narrow: types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}},
			effect: func(_ string, s types.RunPolicySpec) bool {
				return len(s.AllowedDomains) > 0 && !slices.ContainsFunc(s.AllowedDomains, func(d string) bool { return d != "api.anthropic.com" })
			}},
		"denied_domains": {rule: types.PersonalWallsSpec{DeniedDomains: wallsList("evil.example.com")}, knob: "denied_domains",
			unset: types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}}, wide: types.RunPolicySpec{DeniedDomains: []string{"other.example.com"}},
			narrow: types.RunPolicySpec{DeniedDomains: []string{"evil.example.com"}},
			effect: func(_ string, s types.RunPolicySpec) bool {
				return slices.Contains(s.DeniedDomains, "evil.example.com")
			},
			seal: func(ls []sealLayer) bool {
				return sealHas(ls, func(l sealLayer) bool {
					return l.egress.DeniedDomains != nil && slices.Contains(*l.egress.DeniedDomains, "evil.example.com")
				})
			}},
		"allow_all_egress": {rule: types.PersonalWallsSpec{AllowAllEgress: &off}, knob: "allow_all_egress", unsetNoop: true,
			unset: types.RunPolicySpec{}, wide: types.RunPolicySpec{AllowAllEgress: true}, narrow: types.RunPolicySpec{},
			effect: func(_ string, s types.RunPolicySpec) bool { return !s.AllowAllEgress }},
		"first_use_approval": {rule: types.PersonalWallsSpec{FirstUseApproval: &always}, knob: "first_use_approval",
			unset: types.RunPolicySpec{}, wide: types.RunPolicySpec{FirstUseApproval: review}, narrow: types.RunPolicySpec{FirstUseApproval: always},
			effect: func(_ string, s types.RunPolicySpec) bool { return s.FirstUseApproval == types.FirstUseAlwaysDeny }},
		"min_confinement_class": {rule: types.PersonalWallsSpec{MinConfinementClass: &cc3}, knob: "min_confinement_class",
			unset: types.RunPolicySpec{}, wide: types.RunPolicySpec{MinConfinementClass: types.CC1}, narrow: types.RunPolicySpec{MinConfinementClass: types.CC3},
			effect: func(_ string, s types.RunPolicySpec) bool { return s.MinConfinementClass == types.CC3 }},
		"desktop": {rule: types.PersonalWallsSpec{Desktop: &blocked}, knob: "desktop",
			unset: types.RunPolicySpec{}, wide: types.RunPolicySpec{Desktop: &allowed}, narrow: types.RunPolicySpec{Desktop: &blocked},
			effect: func(_ string, s types.RunPolicySpec) bool {
				return s.Desktop.Effective().PasteIn == types.TransferBlocked
			},
			seal: func(ls []sealLayer) bool {
				return sealHas(ls, func(l sealLayer) bool { return l.desktop != nil && l.desktop.PasteIn == types.TransferBlocked })
			}},
		"withheld_grants": {rule: types.PersonalWallsSpec{WithheldGrants: []types.GrantSpec{{Kind: types.GrantSSHKey}}}, knob: "eligible_grants", unsetNoop: true,
			unset: types.RunPolicySpec{}, wide: types.RunPolicySpec{EligibleGrants: []types.GrantSpec{wallsSSH}},
			narrow: types.RunPolicySpec{EligibleGrants: []types.GrantSpec{{Kind: types.GrantAPIKey, Scope: json.RawMessage(`{"host":"api.example"}`)}}},
			effect: func(_ string, s types.RunPolicySpec) bool {
				return !slices.ContainsFunc(s.EligibleGrants, func(g types.GrantSpec) bool { return g.Kind == types.GrantSSHKey })
			},
			seal: func(ls []sealLayer) bool {
				return sealHas(ls, func(l sealLayer) bool {
					return slices.ContainsFunc(l.withheldGrants, func(g types.GrantSpec) bool { return g.Kind == types.GrantSSHKey })
				})
			}},
		"tool_rules": {rule: types.PersonalWallsSpec{ToolRules: &[]types.ToolRule{hold}}, knob: "tool_rules",
			unset: types.RunPolicySpec{}, wide: types.RunPolicySpec{ToolRules: []types.ToolRule{{Tool: "*", Effect: types.ToolAllow}}},
			narrow: types.RunPolicySpec{ToolRules: []types.ToolRule{{Tool: "Bash", Effect: types.ToolDeny}}},
			effect: func(_ string, s types.RunPolicySpec) bool { return deny(s.ToolRules) }},
		"push_rules": {rule: types.PersonalWallsSpec{PushRules: &types.PersonalWallsPushRules{DenyAll: &on}}, knob: "push_rules",
			unset: types.RunPolicySpec{}, wide: types.RunPolicySpec{PushRules: &types.PushRulesSpec{DenyPaths: []string{"secrets/**"}}},
			narrow: types.RunPolicySpec{PushRules: &types.PushRulesSpec{DenyAll: true}},
			effect: func(_ string, s types.RunPolicySpec) bool { return s.PushRules != nil && s.PushRules.DenyAll },
			seal:   func(ls []sealLayer) bool { return sealHas(ls, func(l sealLayer) bool { return l.denyAllPushes }) }},
		"push_rules.deny_paths": {rule: types.PersonalWallsSpec{PushRules: &types.PersonalWallsPushRules{DenyPaths: wallsList(".github/workflows/**")}}, knob: "push_rules",
			unset: types.RunPolicySpec{}, wide: types.RunPolicySpec{PushRules: &types.PushRulesSpec{DenyPaths: []string{"secrets/**"}}},
			narrow: types.RunPolicySpec{PushRules: &types.PushRulesSpec{DenyPaths: []string{".github/workflows/**"}}},
			effect: func(_ string, s types.RunPolicySpec) bool {
				return s.PushRules != nil && slices.Contains(s.PushRules.DenyPaths, ".github/workflows/**")
			}},
		// A policy that names no level stays naming none at create (wallsBeyondMeet): the floor is read at dispatch.
		"recording": {rule: types.PersonalWallsSpec{Recording: &screen}, knob: "recording", floor: types.RecordingScreenActions,
			unset: types.RunPolicySpec{}, wide: types.RunPolicySpec{Recording: terminal}, narrow: types.RunPolicySpec{Recording: screen},
			effect: func(kind string, s types.RunPolicySpec) bool {
				if kind == "unset" {
					return s.Recording == ""
				}
				return s.Recording == types.RecordingScreenActions
			}},
		"auto_stop_after_sec": {rule: types.PersonalWallsSpec{AutoStopAfterSec: &sec}, knob: "auto_stop_after_sec",
			unset: types.RunPolicySpec{}, wide: types.RunPolicySpec{AutoStopAfterSec: 3600}, narrow: types.RunPolicySpec{AutoStopAfterSec: 60},
			effect: func(_ string, s types.RunPolicySpec) bool { return s.AutoStopAfterSec == 600 }},
	}
}()

// agentRulesEnforced are the PersonalWallsSpec names (push_rules' two leaves as dotted names) an agent rule
// accepts, each a key of agentRulesKnobRows; agentRulesRefused are the two it refuses.
var agentRulesEnforced = []string{"allowed_domains", "denied_domains", "allow_all_egress", "first_use_approval", "min_confinement_class",
	"desktop", "withheld_grants", "tool_rules", "push_rules", "push_rules.deny_paths", "recording", "auto_stop_after_sec"}

var agentRulesRefused = []string{"github_capabilities", "azure_devops_capabilities"}

// jsonNamesOf is the json names of t's fields, with push_rules' leaves named under it.
func jsonNamesOf(t reflect.Type) []string {
	var out []string
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		if name == "push_rules" {
			out = append(out, "push_rules")
			for _, leaf := range jsonNamesOf(reflect.TypeFor[types.PersonalWallsPushRules]()) {
				out = append(out, "push_rules."+leaf)
			}
			continue
		}
		out = append(out, name)
	}
	return out
}

// TestAgentRulesType_NoWideningField: a rule's spec is the walls a person may set for themselves, which can
// only narrow, and a write carries nothing else but the inner walls and the version it replaces.
func TestAgentRulesType_NoWideningField(t *testing.T) {
	spec, ok := reflect.TypeFor[types.AgentRules]().FieldByName("Spec")
	if !ok || spec.Type != reflect.TypeFor[types.PersonalWallsSpec]() {
		t.Fatalf("AgentRules.Spec = %v, want PersonalWallsSpec", spec.Type)
	}
	var fields []string
	for i := range reflect.TypeFor[types.AgentRulesWrite]().NumField() {
		fields = append(fields, reflect.TypeFor[types.AgentRulesWrite]().Field(i).Name)
	}
	if !slices.Equal(fields, []string{"Spec", "InnerWalls", "Version"}) {
		t.Errorf("AgentRulesWrite fields = %v, want Spec, InnerWalls, Version", fields)
	}
}

// TestAgentRulesKnobs_EveryWallsFieldClassified: every json name of PersonalWallsSpec (and of its push rules) is
// enforced, with a row in agentRulesKnobRows, or refused at the write. A new walls field reds here until it is
// classified.
func TestAgentRulesKnobs_EveryWallsFieldClassified(t *testing.T) {
	rowKey := func(name string) string {
		if name == "push_rules.deny_all" {
			return "push_rules"
		}
		return name
	}
	for _, name := range jsonNamesOf(reflect.TypeFor[types.PersonalWallsSpec]()) {
		if name == "push_rules" {
			continue // a container: its leaves are classified below
		}
		enforced, refused := slices.Contains(agentRulesEnforced, name) || name == "push_rules.deny_all", slices.Contains(agentRulesRefused, name)
		switch {
		case enforced == refused:
			t.Errorf("%s: enforced %v, refused %v: a walls field is exactly one of them", name, enforced, refused)
		case enforced:
			if _, ok := agentRulesKnobRows[rowKey(name)]; !ok {
				t.Errorf("%s is enforced but agentRulesKnobRows has no %q row", name, rowKey(name))
			}
		}
	}
	for _, name := range agentRulesEnforced {
		if _, ok := agentRulesKnobRows[name]; !ok {
			t.Errorf("agentRulesEnforced names %s, which has no row", name)
		}
	}
	srv, _ := agentRulesServer(t)
	admin := ssoSession(t, "sub-sec-admin", "sec@corp.example", oidc.RoleSecurityAdmin)
	for _, w := range []types.AgentRulesWrite{
		{Spec: types.PersonalWallsSpec{GitHubCapabilities: &[]ghscope.Capability{ghscope.CapCodeRead}}},
		{Spec: types.PersonalWallsSpec{AzureDevOpsCapabilities: &[]adoscope.Capability{adoscope.CapCodeRead}}},
	} {
		body, _ := json.Marshal(w)
		resp := doSSO(t, srv, http.MethodPut, "/api/v1/agent-rules/claude-code", admin, string(body))
		if resp.Code != http.StatusUnprocessableEntity || wireReason(t, resp) != reasonAgentRulesInvalid {
			t.Errorf("PUT %s = %d %s, want 422 %s", body, resp.Code, resp.Body, reasonAgentRulesInvalid)
		}
	}
}

// TestAgentRulesRunStep_NarrowsEveryKnob: for each enforced knob, a policy that leaves it unset and one that
// sets it wider than the rule are narrowed by it (the resolved spec, the seal at dispatch, the recording floor),
// the knob's provenance names the rule, and a policy already as narrow changes nothing and records nothing.
func TestAgentRulesRunStep_NarrowsEveryKnob(t *testing.T) {
	ctx := context.Background()
	for name, row := range agentRulesKnobRows {
		t.Run(name, func(t *testing.T) {
			srv, _ := agentRulesServer(t, claudeRules(row.rule))
			prov := agentRulesProvenance("claude-code")
			for _, kind := range []string{"unset", "wide"} {
				base := row.unset
				if kind == "wide" {
					base = row.wide
				}
				got, source, _, refusal := agentRulesStep(t, srv, claudeRun, base)
				if refusal != nil {
					t.Fatalf("%s: refusal %+v", kind, refusal)
				}
				if kind == "unset" && row.unsetNoop {
					if !reflect.DeepEqual(canonicalKnobs(got), canonicalKnobs(base)) {
						t.Errorf("unset: %+v from %+v, want it as it was", got, base)
					}
					continue
				}
				if !composer.Leq(got, base, types.GovernanceLimits{}, types.GovernanceLimits{}) {
					t.Errorf("%s: %+v is wider than %+v", kind, got, base)
				}
				if !row.effect(kind, got) {
					t.Errorf("%s: resolved spec %+v does not show the rule", kind, got)
				}
				if changed := !reflect.DeepEqual(canonicalKnobs(got), canonicalKnobs(base)); changed {
					if source.knobs[row.knob] != prov {
						t.Errorf("%s: provenance of %s = %+v, want %+v", kind, row.knob, source.knobs[row.knob], prov)
					}
					if len(source.AgentRules) != 1 || source.AgentRules[0].Harness != "claude-code" || source.AgentRules[0].Version != 3 {
						t.Errorf("%s: policy_source.agent_rules = %+v, want one record for claude-code at version 3", kind, source.AgentRules)
					}
				}
			}
			got, source, warns, refusal := agentRulesStep(t, srv, claudeRun, row.narrow)
			if refusal != nil || !reflect.DeepEqual(canonicalKnobs(got), canonicalKnobs(row.narrow)) || len(source.AgentRules) != 0 || len(warns) != 0 {
				t.Errorf("narrow: spec %+v source %+v warnings %v refusal %+v, want nothing changed and nothing recorded", got, source.AgentRules, warns, refusal)
			}
			run := types.AgentRun{ID: uuid.New(), Agent: "claude-code", CreatedBy: "sub-agent-rules"}
			if row.seal != nil && !row.seal(srv.agentRulesSealLayers(ctx, run)) {
				t.Errorf("the seal at dispatch does not carry the rule: %+v", srv.agentRulesSealLayers(ctx, run))
			}
			if row.floor != "" {
				if floor, err := srv.wallsRecordingFloor(ctx, run, nil); err != nil || floor != row.floor {
					t.Errorf("recording floor = %q, %v, want %q", floor, err, row.floor)
				}
				choice, rf := srv.createRecordingChoice(ctx, httpRequestAs("sub-agent-rules"), "", runPlacement{}, governanceCeiling{Operator: true}, false, []string{"claude-code"})
				if rf != nil || choice.WallsFloor != row.floor {
					t.Errorf("createRecordingChoice floor = %q, %+v, want %q", choice.WallsFloor, rf, row.floor)
				}
			}
		})
	}
}

// httpRequestAs is a request from principal, for the reads that name the caller.
func httpRequestAs(principal string) *http.Request {
	return governReq(memberCtxFor(principal))
}

// TestAgentRulesRunStep_EveryArmAndCaller: the rules bind the default arm and the inline arm, for a member, a
// security admin and a super admin alike; both arms end in the one tail that runs the step.
func TestAgentRulesRunStep_EveryArmAndCaller(t *testing.T) {
	srv, _ := agentRulesServer(t, claudeRules(types.PersonalWallsSpec{DeniedDomains: wallsList("evil.example.com")}))
	for caller, role := range map[string]string{"member": oidc.RoleUser, "security admin": oidc.RoleSecurityAdmin, "super admin": oidc.RoleAdmin} {
		cookie := ssoSession(t, "sub-ar-"+strings.ReplaceAll(caller, " ", "-"), "ar@corp.example", role)
		for arm, body := range map[string]string{
			"default": `{"agent":"claude-code","task":"t"}`,
			"inline":  `{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC2","allowed_domains":["api.anthropic.com"]}}`,
		} {
			w := doSSO(t, srv, http.MethodPost, previewDoor, cookie, body)
			var pv policyPreviewResponse
			if err := json.Unmarshal(w.Body.Bytes(), &pv); err != nil || w.Code != http.StatusOK {
				t.Fatalf("%s, %s arm: preview %d %v: %s", caller, arm, w.Code, err, w.Body)
			}
			if !slices.Contains(pv.Spec.DeniedDomains, "evil.example.com") {
				t.Errorf("%s, %s arm: denied %v, want the agent rule's evil.example.com", caller, arm, pv.Spec.DeniedDomains)
			}
		}
	}
	src := string(mustReadFile(t, "run_policy_resolution.go"))
	if n := strings.Count(src, "s.boundaryThenNoRepositories(ctx, r, ceiling, "); n != 2 {
		t.Errorf("run_policy_resolution.go calls the shared tail %d times, want once per arm (2)", n)
	}
	body := src[strings.Index(src, "func (s *Server) boundaryThenNoRepositories"):]
	if !strings.Contains(body, "s.agentRulesRunStep(ctx, req, spec, source)") {
		t.Error("boundaryThenNoRepositories does not run the agent rules step")
	}
}

// TestAgentRulesRunStep_RunWithoutTheAgentIsNotBound: the same request without the harness, with another
// harness only, or with "Your own tools" is not narrowed and carries no agent_rules provenance.
func TestAgentRulesRunStep_RunWithoutTheAgentIsNotBound(t *testing.T) {
	srv, _ := agentRulesServer(t, claudeRules(types.PersonalWallsSpec{DeniedDomains: wallsList("evil.example.com")}))
	base := types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}}
	for name, req := range map[string]createRunRequest{
		"another harness only": {Agent: "codex-cli", Task: "t"},
		"your own tools":       {Agent: "none", Task: "t"},
		"no agent":             {Task: "t"},
	} {
		got, source, warns, refusal := agentRulesStep(t, srv, req, base)
		if refusal != nil || !reflect.DeepEqual(got, base) || len(source.AgentRules) != 0 || len(warns) != 0 {
			t.Errorf("%s: spec %+v source %+v warnings %v refusal %+v, want it untouched", name, got, source.AgentRules, warns, refusal)
		}
		for knob, p := range source.knobs {
			if p.Layer == policyref.LayerAgentRules {
				t.Errorf("%s: knob %s names the agent rules", name, knob)
			}
		}
	}
	if got, _, _, _ := agentRulesStep(t, srv, claudeRun, base); !slices.Contains(got.DeniedDomains, "evil.example.com") {
		t.Errorf("with the agent: denied %v, want evil.example.com", got.DeniedDomains)
	}
}

// TestAgentRulesRunStep_ProvenanceNamesTheLayer: each knob the step changed names the harness, the record
// reads back through the run.create row's decode, and an unnarrowed run has no record.
func TestAgentRulesRunStep_ProvenanceNamesTheLayer(t *testing.T) {
	srv, _ := agentRulesServer(t, claudeRules(types.PersonalWallsSpec{DeniedDomains: wallsList("evil.example.com")}))
	got, source, warns, refusal := agentRulesStep(t, srv, claudeRun, types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}})
	if refusal != nil {
		t.Fatalf("refusal %+v", refusal)
	}
	if want := (policyref.Provenance{Layer: "agent_rules", Name: "claude-code"}); source.knobs["denied_domains"] != want {
		t.Errorf("denied_domains provenance = %+v, want %+v", source.knobs["denied_domains"], want)
	}
	if source.knobs["allowed_domains"].Layer == policyref.LayerAgentRules {
		t.Errorf("allowed_domains names the agent rules though they left it as it was: %+v", got.AllowedDomains)
	}
	if !slices.Contains(warns, "your organisation's rules for Claude Code narrowed denied_domains") {
		t.Errorf("warnings = %v, want the rules named for denied_domains", warns)
	}
	raw, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	var back policySourceRecord
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("policy_source with agent_rules does not read back: %v", err)
	}
	if len(back.AgentRules) != 1 || back.AgentRules[0].Harness != "claude-code" || back.AgentRules[0].Version != 3 ||
		back.AgentRules[0].WallsSourceRecord == nil || !slices.Contains(back.AgentRules[0].Narrowed, "denied_domains") ||
		!slices.Contains(back.AgentRules[0].Added["denied_domains"], "evil.example.com") {
		t.Errorf("read back %+v, want claude-code v3 narrowing denied_domains with evil.example.com added", back.AgentRules)
	}
	if strings.Contains(string(raw), "policySetInnerWalls") {
		t.Errorf("an in-memory field is on the wire: %s", raw)
	}
}

// TestAgentRulesRunStep_ReadErrorRefuses: rules that cannot be read refuse the run (500), at the step and
// through a front door; the run never resolves without them.
func TestAgentRulesRunStep_ReadErrorRefuses(t *testing.T) {
	srv, st := agentRulesServer(t, claudeRules(types.PersonalWallsSpec{DeniedDomains: wallsList("evil.example.com")}))
	st.rulesErr = errors.New("the store is down")
	if _, _, _, refusal := agentRulesStep(t, srv, claudeRun, types.RunPolicySpec{}); refusal == nil || refusal.err == nil || refusal.operation != "read the agent rules" {
		t.Errorf("refusal = %+v, want a server error reading the agent rules", refusal)
	}
	member := ssoSession(t, wallsSub, "walls@corp.example", oidc.RoleUser)
	if w := doSSO(t, srv, http.MethodPost, previewDoor, member, `{"agent":"claude-code","task":"t"}`); w.Code != http.StatusInternalServerError {
		t.Errorf("preview with unreadable rules = %d %s, want 500", w.Code, w.Body)
	}
	// A run without the agent reads no rule it could not read: nothing binds it.
	if _, _, _, refusal := agentRulesStep(t, srv, createRunRequest{Agent: "none", Task: "t"}, types.RunPolicySpec{}); refusal != nil {
		t.Errorf("a run without the agent was refused: %+v", refusal)
	}
}

// TestAgentRulesRunStep_InvalidStoredRuleRefuses: a stored rule the walls rules now refuse refuses the run
// 422 agent_rules_invalid, quoting no value; it never runs without the rule.
func TestAgentRulesRunStep_InvalidStoredRuleRefuses(t *testing.T) {
	on := true
	srv, _ := agentRulesServer(t, claudeRules(types.PersonalWallsSpec{AllowAllEgress: &on}))
	_, _, _, refusal := agentRulesStep(t, srv, claudeRun, types.RunPolicySpec{})
	if refusal == nil || refusal.status != http.StatusUnprocessableEntity || refusal.body.Reason != reasonAgentRulesInvalid ||
		!strings.Contains(refusal.body.Error, "Your organisation's rules for Claude Code no longer pass") || strings.Contains(refusal.body.Error, "allow_all_egress") {
		t.Errorf("refusal = %+v, want 422 %s naming the rules and quoting no value", refusal, reasonAgentRulesInvalid)
	}
	member := ssoSession(t, wallsSub, "walls@corp.example", oidc.RoleUser)
	if w := doSSO(t, srv, http.MethodPost, previewDoor, member, `{"agent":"claude-code","task":"t"}`); w.Code != http.StatusUnprocessableEntity || wireReason(t, w) != reasonAgentRulesInvalid {
		t.Errorf("preview = %d %s, want 422 %s", w.Code, w.Body, reasonAgentRulesInvalid)
	}
}

// TestAgentRulesSeal_UnreadSealsShut: unreadable rules, or unreadable harness rows, seal the run to no egress
// and no credential; a run with no rule has no layer.
func TestAgentRulesSeal_UnreadSealsShut(t *testing.T) {
	run := types.AgentRun{ID: uuid.New(), Agent: "claude-code"}
	for name, mutate := range map[string]func(*agentRulesTestStore){
		"rules":   func(st *agentRulesTestStore) { st.rulesErr = errors.New("the store is down") },
		"harness": func(st *agentRulesTestStore) { st.harnessErr = errors.New("the store is down") },
	} {
		srv, st := agentRulesServer(t, claudeRules(types.PersonalWallsSpec{DeniedDomains: wallsList("evil.example.com")}))
		mutate(st)
		layers := srv.agentRulesSealLayers(context.Background(), run)
		if len(layers) != 1 || layers[0].egress.AllowedDomains == nil || len(*layers[0].egress.AllowedDomains) != 0 ||
			layers[0].egress.AllowAllEgress == nil || *layers[0].egress.AllowAllEgress || layers[0].grants == nil || len(*layers[0].grants) != 0 {
			t.Errorf("%s unread: layers %+v, want one layer with no egress and no credential", name, layers)
		}
	}
	srv, _ := agentRulesServer(t)
	if layers := srv.agentRulesSealLayers(context.Background(), run); len(layers) != 0 {
		t.Errorf("no rules: layers %+v, want none", layers)
	}
}

// TestAgentRulesLiveMint_UnreadIs503: at a live credential mint an unreadable rule set is an error the door
// retries (503), never the shut layer whose 403 would say the credential is withheld.
func TestAgentRulesLiveMint_UnreadIs503(t *testing.T) {
	srv, st := agentRulesServer(t, claudeRules(types.PersonalWallsSpec{DeniedDomains: wallsList("evil.example.com")}))
	run := types.AgentRun{ID: uuid.New(), Agent: "claude-code", CreatedBy: "sub-agent-rules"}
	if _, err := srv.liveWithholding(context.Background(), run); err != nil {
		t.Fatalf("readable rules: %v", err)
	}
	st.rulesErr = errors.New("the store is down")
	if _, err := srv.liveWithholding(context.Background(), run); err == nil {
		t.Error("unreadable rules answered the mint door as no rules")
	}
	st.rulesErr, st.harnessErr = nil, errors.New("the store is down")
	if _, err := srv.liveWithholding(context.Background(), run); err == nil {
		t.Error("unreadable harness rows answered the mint door as no rules")
	}
}

// failingRulesStore is the dispatch fixture's store with agent rules that cannot be read.
type failingRulesStore struct{ *dispatchTestStore }

func (failingRulesStore) ListAgentRules(context.Context) ([]types.AgentRules, error) {
	return nil, errors.New("the store is down")
}

func (failingRulesStore) PutAgentRules(context.Context, types.AgentRules, int) (types.AgentRules, error) {
	return types.AgentRules{}, errors.New("read-only")
}

func (failingRulesStore) DeleteAgentRules(context.Context, string, int) (types.AgentRules, error) {
	return types.AgentRules{}, errors.New("read-only")
}

// TestAgentRulesRecordingFloor_UnreadRefusesDispatch: a failing read of the rules for the recording floor
// fails the run at dispatch; the run is never recorded at a lower level.
func TestAgentRulesRecordingFloor_UnreadRefusesDispatch(t *testing.T) {
	srv, st, audit, run := dispatchTeardownFixture(t, &fakeRunner{}, types.RunPending)
	srv.cfg.Store = failingRulesStore{st}
	if floor, err := srv.wallsRecordingFloor(context.Background(), run, nil); err == nil || floor != "" {
		t.Fatalf("floor = %q, %v, want an error and no level", floor, err)
	}
	if srv.applyDispatchRecording(context.Background(), &run, &runner.SandboxSpec{}, types.RunPolicySpec{}, dispatchCeiling{}, types.SiteConfig{}, nil) {
		t.Error("the dispatch went on though the rules could not be read")
	}
	if ev := findAudit(audit.snapshot(), run.ID, "run.create", "failure"); ev == nil {
		t.Error("no run.create failure row for the refused dispatch")
	}
}

// innerRule is a rule for claude-code that sets only the harness's own walls.
func innerRule(w *types.HarnessInnerWalls) types.AgentRules {
	return types.AgentRules{Harness: "claude-code", InnerWalls: w, Version: 4, UpdatedBy: "sub-admin"}
}

// TestAgentRulesInnerWalls_RefusalNamesTheRule: an interactive Claude Code run that cannot meet the rule's
// approvals is refused 422 agent_rules_conflict in the rule's words, never the policy's.
func TestAgentRulesInnerWalls_RefusalNamesTheRule(t *testing.T) {
	srv, _ := agentRulesServer(t, innerRule(innerWalls("", types.InnerApprovalsWardyn)))
	spec, source, _, refusal := agentRulesStep(t, srv, innerClaudeAttach, types.RunPolicySpec{})
	if refusal != nil || spec.HarnessInnerWalls == nil || spec.HarnessInnerWalls.Approvals != types.InnerApprovalsWardyn {
		t.Fatalf("step: %+v, %+v, want the rule's approvals met into the policy", spec.HarnessInnerWalls, refusal)
	}
	if source.knobs["harness_inner_walls"] != agentRulesProvenance("claude-code") {
		t.Errorf("harness_inner_walls provenance = %+v, want the rule's", source.knobs["harness_inner_walls"])
	}
	_, refusal = agentRulesInnerWallsFor(innerClaudeAttach, spec, nil, types.CC1, source)
	if refusal == nil || refusal.status != http.StatusUnprocessableEntity || refusal.body.Reason != reasonAgentRulesConflict ||
		!strings.Contains(refusal.body.Error, "Your organisation's rules for Claude Code") || !strings.Contains(refusal.body.Error, "approvals") ||
		strings.Contains(refusal.body.Error, "use a policy without") {
		t.Errorf("refusal = %+v, want 422 %s naming the rule and the approvals axis", refusal, reasonAgentRulesConflict)
	}
	// The same refusal over a policy of the person's own keeps the policy's words.
	own := types.RunPolicySpec{HarnessInnerWalls: innerWalls("", types.InnerApprovalsWardyn)}
	if _, refusal := agentRulesInnerWallsFor(innerClaudeAttach, own, nil, types.CC1, policySourceRecord{knobs: startProvenance(memberProvenance)}); refusal == nil ||
		refusal.body.Reason == reasonAgentRulesConflict {
		t.Errorf("a policy's own refusal = %+v, want its own reason, not %s", refusal, reasonAgentRulesConflict)
	}
}

// TestAgentRulesInnerWalls_PolicyAndRuleBothNamed: a policy that set harness_inner_walls and a rule that
// then changed it are both named, never the rule alone.
func TestAgentRulesInnerWalls_PolicyAndRuleBothNamed(t *testing.T) {
	srv, _ := agentRulesServer(t, innerRule(innerWalls(types.InnerSandboxReadOnly, "")))
	policy := types.RunPolicySpec{HarnessInnerWalls: innerWalls("", types.InnerApprovalsWardyn)}
	spec, source, _, refusal := agentRulesStep(t, srv, innerClaudeAttach, policy)
	if refusal != nil || spec.HarnessInnerWalls == nil || spec.HarnessInnerWalls.SandboxMode != types.InnerSandboxReadOnly ||
		spec.HarnessInnerWalls.Approvals != types.InnerApprovalsWardyn {
		t.Fatalf("step: %+v, %+v, want the policy's approvals and the rule's sandbox mode", spec.HarnessInnerWalls, refusal)
	}
	_, refusal = agentRulesInnerWallsFor(innerClaudeAttach, spec, nil, types.CC1, source)
	if refusal == nil || refusal.body.Reason != reasonAgentRulesConflict ||
		!strings.Contains(refusal.body.Error, "This run's policy and your organisation's rules for Claude Code together") {
		t.Errorf("refusal = %+v, want the policy and the rule both named", refusal)
	}
}

// TestAgentRulesInnerWalls_LooserRuleRefuses: a rule looser than the agent's lane refuses the run; it never
// launches looser.
func TestAgentRulesInnerWalls_LooserRuleRefuses(t *testing.T) {
	srv, _ := agentRulesServer(t, innerRule(innerWalls("", types.InnerApprovalsHarness)))
	spec, source, _, refusal := agentRulesStep(t, srv, innerClaudeHold, types.RunPolicySpec{})
	if refusal != nil {
		t.Fatalf("step refusal %+v", refusal)
	}
	_, refusal = agentRulesInnerWallsFor(innerClaudeHold, spec, nil, types.CC1, source)
	if refusal == nil || refusal.status != http.StatusUnprocessableEntity || refusal.body.Reason != reasonAgentRulesConflict {
		t.Errorf("a rule asking approvals harness on a hold-lane run: %+v, want 422 %s", refusal, reasonAgentRulesConflict)
	}
}

// TestAgentRulesInnerWalls_RuleAgainstPolicyApprovals: the rule's approvals must be the policy's when both set
// them.
func TestAgentRulesInnerWalls_RuleAgainstPolicyApprovals(t *testing.T) {
	srv, _ := agentRulesServer(t, innerRule(innerWalls("", types.InnerApprovalsWardyn)))
	_, _, _, refusal := agentRulesStep(t, srv, innerClaudeAttach, types.RunPolicySpec{HarnessInnerWalls: innerWalls("", types.InnerApprovalsHarness)})
	if refusal == nil || refusal.status != http.StatusUnprocessableEntity || refusal.body.Reason != reasonAgentRulesConflict ||
		!strings.Contains(refusal.body.Error, "harness_inner_walls.approvals") {
		t.Errorf("refusal = %+v, want 422 %s naming harness_inner_walls.approvals", refusal, reasonAgentRulesConflict)
	}
}

// TestExplainRunPolicy_AgentRuleChangeIsNotWalls pins what the explain does today: a host an agent rule denies
// is listed under launch, or limits for a bounded run, never walls (no cause names the rules yet).
func TestExplainRunPolicy_AgentRuleChangeIsNotWalls(t *testing.T) {
	base := pvSpec([]string{"a.example"}, nil)
	resolved := pvSpec([]string{"a.example"}, func(s *types.RunPolicySpec) { s.DeniedDomains = []string{"evil.example"} })
	got := explainRunPolicy(ptr(base), resolved, evidenceOf(nil), types.SiteConfig{}, types.AgentRun{})
	if pvChange(got, causeWalls, fieldDenied) != nil || pvChange(got, causeLaunch, fieldDenied) == nil {
		t.Errorf("changes = %+v, want the deny under launch and never walls", got)
	}
	got = explainRunPolicy(ptr(base), resolved, evidenceOf(func(e *auditEvidence) { e.bounded = true }), types.SiteConfig{}, types.AgentRun{})
	if pvChange(got, causeWalls, fieldDenied) != nil || pvChange(got, causeLimits, fieldDenied) == nil {
		t.Errorf("a bounded run's changes = %+v, want the deny under limits and never walls", got)
	}
}
