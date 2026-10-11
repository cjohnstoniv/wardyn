// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package placement

import (
	"errors"
	"net/http"
	"reflect"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestDispatchPlanFieldsClassified is the closed switch (C6): a credential-bearing
// field added to SandboxSpec or ProxyConfig without a row in Table fails here.
func TestDispatchPlanFieldsClassified(t *testing.T) {
	if got := Unclassified(); len(got) != 0 {
		t.Fatalf("fields with no classification (add each to placement.Table, naming its class and local rule): %v", got)
	}
}

func TestUnclassifiedCatchesANewField(t *testing.T) {
	type spec struct {
		Env       map[string]string
		NewSecret string
		hidden    string
	}
	_ = spec{}.hidden
	if got := unclassifiedIn(StructSandboxSpec, reflect.TypeFor[spec]()); !slices.Equal(got, []string{"SandboxSpec.NewSecret"}) {
		t.Fatalf("unclassifiedIn = %v, want only the new exported field", got)
	}
}

// F8: a field added to RunPolicySpec (or one the table lists but the struct dropped) is not auto-exempt.
func TestUnclassifiedCatchesANewPolicyField(t *testing.T) {
	type policy struct {
		AllowedDomains []string
		NewSecretKnob  string
		hidden         string
	}
	_ = policy{}.hidden
	got := unclassifiedPaths(StructProxyConfig, "Policy", reflect.TypeFor[policy]())
	if !slices.Equal(got, []string{"ProxyConfig.Policy.NewSecretKnob"}) {
		t.Fatalf("unclassifiedPaths = %v, want only the new field", got)
	}
}

func TestTableRowsAreWellFormed(t *testing.T) {
	grantKinds := map[string]bool{"ssh_key": true, "git_pat": true, "cloud_sts": true, "api_key": true, "env_secret": true, "file_secret": true, "github_token": true}
	types := map[string]reflect.Type{
		StructSandboxSpec: reflect.TypeFor[runner.SandboxSpec](),
		StructProxyConfig: reflect.TypeFor[runner.ProxyConfig](),
	}
	for _, e := range Table {
		where := e.Struct + "." + e.Field + " (" + e.Variant + ")"
		if e.Struct == StructGrantKind {
			if !grantKinds[e.Field] {
				t.Errorf("%s: unknown grant kind", where)
			}
		} else {
			st, ok := types[e.Struct]
			if !ok {
				t.Errorf("%s: unknown struct", where)
				continue
			}
			f, ok := st.FieldByName(e.Field)
			if !ok {
				t.Errorf("%s: no such field (renamed or removed: update the table)", where)
				continue
			}
			if e.Path != "" && !pathExists(f.Type, e.Path) {
				t.Errorf("%s: path %q does not resolve", where, e.Path)
			}
		}
		if e.Class == ClassComposite && !(e.Struct == StructSandboxSpec && e.Field == "ProxyConfig") {
			t.Errorf("%s: only SandboxSpec.ProxyConfig is composite", where)
		}
		if e.Class == ClassExempt && e.Delivery != "" {
			t.Errorf("%s: an exempt field carries no delivery class", where)
		}
		switch e.Rule {
		case RuleDelivery, RuleViaOrgRefuse, RuleNotConfigured:
			if len(AllowedModes(e.Delivery)) == 0 {
				t.Errorf("%s: rule %s needs a known delivery class, has %q", where, e.Rule, e.Delivery)
			}
		}
		if e.Rule == RuleViaOrgRefuse && slices.Contains(AllowedModes(e.Delivery), ModeRunnerResident) {
			t.Errorf("%s: a via_org_refuse row's class %q allows runner_resident", where, e.Delivery)
		}
		if e.Rule == RuleNotConfigured && Writable(e.Delivery) {
			t.Errorf("%s: a not-configurable row's class %q is writable", where, e.Delivery)
		}
		if e.Rule == RuleDelivery && !slices.Contains(AllowedModes(e.Delivery), ModeRunnerResident) && !slices.Contains(AllowedModes(e.Delivery), ModeViaOrg) {
			t.Errorf("%s: class %q allows neither via_org nor runner_resident", where, e.Delivery)
		}
		if e.Reason != "" && !e.Reason.Valid() {
			t.Errorf("%s: reason %q is outside the closed set", where, e.Reason)
		}
		if (e.Rule == RuleRefuse || e.Rule == RuleDelivery || e.Rule == RuleViaOrgRefuse || e.Rule == RuleBound) && e.Reason == "" {
			t.Errorf("%s: a refusing rule has no reason", where)
		}
	}
}

func TestTableIsClosedAndUnambiguous(t *testing.T) {
	if problems := tableProblems(Table); len(problems) != 0 {
		t.Fatalf("ill-formed table: %v", problems)
	}
}

func TestEveryClassHasARowOrIsNotSent(t *testing.T) {
	used := map[string]bool{}
	for _, e := range Table {
		if e.Delivery != "" {
			used[e.Delivery] = true
		}
	}
	// Classes reached only through a field-less mint route or a class that is not a dispatched field of its own.
	for _, c := range DeliveryClasses() {
		// A Bedrock bearer provider's key classifies through the provider-key
		// paths (own or operator by namespace), so no field row names it.
		if !used[c] && c != ClassOAuthSubscription && c != ClassBedrockBearer {
			t.Errorf("delivery class %q is named by no classification row", c)
		}
	}
}

func TestReasonsMatchDesign(t *testing.T) {
	want := map[Reason]int{
		"placement_required": 422, "placement_unavailable": 422, "placement_denied": 403, "placement_credential": 422,
		"placement_trusted_output": 422, "placement_component_self_defined": 403, "placement_capability": 422,
		"placement_local_path": 422, "runner_offline": 422, "runner_ambiguous": 422, "runner_not_found": 404,
		"runner_unclaimed": 409, "runner_revoked": 401, "runner_token_invalid": 401, "runner_claim_mismatch": 403,
		"runner_posture_unmet": 422, "runner_keystore_unavailable": 422, "runner_action_pending": 409,
		"relay_runner_mismatch": 403, "relay_required": 403, "relay_route_refused": 403, "delivery_via_org": 403,
		"delivery_runner_resident": 403, "delivery_not_via_org": 403, "via_org_destination_refused": 403,
		"via_org_cap_exceeded": http.StatusTooManyRequests,
	}
	if got := Reasons(); len(got) != len(want) {
		t.Fatalf("%d reasons, design §12.3 lists %d", len(got), len(want))
	}
	for r, status := range want {
		if r.Status() != status || !r.Valid() {
			t.Errorf("%s: status %d valid=%v, want %d", r, r.Status(), r.Valid(), status)
		}
	}
	if Reason("nope").Valid() || Reason("nope").Status() != 0 {
		t.Error("an unknown reason is valid")
	}
}

func TestRequestValidate(t *testing.T) {
	id := uuid.NewString()
	for _, r := range []Request{{}, {Placement: Remote}, {Placement: Local}, {Placement: Local, RunnerID: id}} {
		if err := r.Validate(); err != nil {
			t.Errorf("%+v: %v", r, err)
		}
	}
	for _, r := range []Request{{Placement: "cloud"}, {Placement: Remote, RunnerID: id}, {RunnerID: id}, {Placement: Local, RunnerID: "r1"}} {
		if err := r.Validate(); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%+v validated (%v)", r, err)
		}
	}
}

// A run row carries the placement, so the canonical declarations moved to
// internal/types (which cannot import this package). The alias and the constants
// are what every caller here already used, and they must keep the same values —
// a rename that quietly forked the vocabulary would leave two "local" strings
// that no test compares.
func TestPlacementVocabularyIsTypes(t *testing.T) {
	var p Placement = types.PlacementLocal
	var e EvidenceSource = types.RunEvidenceRunnerAsserted
	if p != Local || e != EvidenceRunnerAsserted {
		t.Fatalf("aliases read %q/%q, want %q/%q", p, e, Local, EvidenceRunnerAsserted)
	}
	if Remote != "remote" || Local != "local" || EvidenceSubstrate != "substrate" || EvidenceRunnerAsserted != "runner_asserted" {
		t.Fatal("the placement vocabulary's values changed")
	}
	var req types.AgentRun
	req.Placement = Local // a run row takes the same value, with no conversion
	req.EvidenceSource = EvidenceFor(Local)
	if req.Placement != types.PlacementLocal || req.EvidenceSource != types.RunEvidenceRunnerAsserted {
		t.Fatal("a run row's placement fields did not take the placement package's values")
	}
}

func TestCapacityFits(t *testing.T) {
	c := Capacity{CPUMillisMax: 4000, MemoryMiBMax: 8192}
	if !c.Fits(0, 0) || !c.Fits(4000, 8192) || c.Fits(4001, 0) || c.Fits(0, 8193) {
		t.Fatal("Capacity.Fits is wrong at its bounds")
	}
}

func TestEvidenceFor(t *testing.T) {
	if EvidenceFor(Local) != EvidenceRunnerAsserted || EvidenceFor(Remote) != EvidenceSubstrate {
		t.Fatal("a local run is runner_asserted, a remote run substrate")
	}
	if EvidenceRunnerAsserted != "runner_asserted" || EvidenceSubstrate != "substrate" {
		t.Fatal("evidence_source values are substrate and runner_asserted")
	}
}

func TestRunnerNames(t *testing.T) {
	if RunnerSubstrateName("r1") != "runner:r1" || RunnerRefPrefix("r1") != "runner:r1/" {
		t.Fatal("runner naming changed")
	}
}

func TestResidentPlaceholder(t *testing.T) {
	id := uuid.New()
	got, ok := ResidentGrantID(ResidentPlaceholder(id))
	if !ok || got != id {
		t.Fatalf("placeholder round trip = %v, %v", got, ok)
	}
	for _, v := range []string{"", "s3cret", "wardyn-resident:", "wardyn-resident:nope"} {
		if _, ok := ResidentGrantID(v); ok {
			t.Errorf("%q read as a placeholder", v)
		}
	}
}

func TestDeliveryPolicyValidate(t *testing.T) {
	posture := &PostureRequirement{MDMManaged: true, DiskEncrypted: true}
	ok := DeliveryPolicy{Classes: map[string]ClassPolicy{
		"api_key":      {Mode: ModeViaOrg},
		"github_token": {Mode: ModeViaOrg},
		"env_secret":   {Mode: ModeRunnerResident, RequirePosture: posture},
		"ssh_key":      {Mode: ModeRefuse},
	}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("the design's example policy: %v", err)
	}
	if err := (DeliveryPolicy{}).Validate(); err != nil {
		t.Fatalf("an empty policy (everything refuses): %v", err)
	}
	bad := map[string]DeliveryPolicy{
		"unknown class":            {Classes: map[string]ClassPolicy{"made_up": {Mode: ModeViaOrg}}},
		"oauth_subscription fixed": {Classes: map[string]ClassPolicy{"oauth_subscription": {Mode: ModeOwn}}},
		"cloud_sts fixed":          {Classes: map[string]ClassPolicy{"cloud_sts": {Mode: ModeRefuse}}},
		"github_token resident":    {Classes: map[string]ClassPolicy{"github_token": {Mode: ModeRunnerResident}}},
		"ado_minted_pat resident":  {Classes: map[string]ClassPolicy{"ado_minted_pat": {Mode: ModeRunnerResident}}},
		"env_secret via_org":       {Classes: map[string]ClassPolicy{"env_secret": {Mode: ModeViaOrg}}},
		"bedrock role via_org":     {Classes: map[string]ClassPolicy{"bedrock_role_credentials": {Mode: ModeViaOrg}}},
		"posture on via_org":       {Classes: map[string]ClassPolicy{"api_key": {Mode: ModeViaOrg, RequirePosture: posture}}},
		"posture on refuse":        {Classes: map[string]ClassPolicy{"api_key": {Mode: ModeRefuse, RequirePosture: posture}}},
		"own is not a policy mode": {Classes: map[string]ClassPolicy{"api_key": {Mode: ModeOwn}}},
		"empty mode":               {Classes: map[string]ClassPolicy{"api_key": {}}},
	}
	for name, p := range bad {
		if err := p.Validate(); !errors.Is(err, ErrInvalidPolicy) {
			t.Errorf("%s: validated (%v)", name, err)
		}
	}
	if got := ok.For("api_key").Mode; got != ModeViaOrg {
		t.Errorf("api_key = %q", got)
	}
	if got := ok.For("bedrock_bearer").Mode; got != ModeRefuse {
		t.Errorf("an absent class = %q, want refuse", got)
	}
}

func TestDeliveryClassesMatchDesign(t *testing.T) {
	want := []string{"ado_minted_pat", "api_key", "aws_sso_bearer", "azure_foundry_token", "bedrock_bearer", "bedrock_role_credentials",
		"cloud_sts", "env_secret", "file_secret", "git_pat_broker", "git_pat_helper", "github_token", "oauth_subscription", "ssh_key"}
	if got := DeliveryClasses(); !slices.Equal(got, want) {
		t.Fatalf("classes = %v", got)
	}
	// OD-12 bars runner_resident for github_token, ado_minted_pat and cloud_sts; oauth_subscription is own only.
	barred := map[string]bool{"github_token": true, "ado_minted_pat": true, "cloud_sts": true, "oauth_subscription": true}
	for _, c := range DeliveryClasses() {
		if resident := slices.Contains(AllowedModes(c), ModeRunnerResident); resident == barred[c] {
			t.Errorf("%s: runner_resident allowed=%v", c, resident)
		}
	}
}
