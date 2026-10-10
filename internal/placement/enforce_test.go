// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package placement

import (
	"bytes"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// fillSentinel sets every exported field of v to a non-zero value.
func fillSentinel(v reflect.Value) {
	switch v.Kind() {
	case reflect.String:
		v.SetString("sentinel")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1)
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fillSentinel(v.Elem())
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		fillSentinel(v.Index(0))
	case reflect.Array:
		for i := range v.Len() {
			fillSentinel(v.Index(i))
		}
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
		k, e := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
		fillSentinel(k)
		fillSentinel(e)
		v.SetMapIndex(k, e)
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				fillSentinel(v.Field(i))
			}
		}
	case reflect.Interface:
		if v.Type().NumMethod() > 0 {
			v.Set(reflect.ValueOf(&bytes.Buffer{}))
		}
	case reflect.Func:
		v.Set(reflect.MakeFunc(v.Type(), func([]reflect.Value) []reflect.Value {
			out := make([]reflect.Value, v.Type().NumOut())
			for i := range out {
				out[i] = reflect.Zero(v.Type().Out(i))
			}
			return out
		}))
	}
}

// tableProblems is the table's well-formedness rule beyond the per-row checks:
// every Rule and Class is in its closed set, and where a (Struct, Field, Class)
// has a credential-refusing row, every row of that set names a distinct,
// non-empty delivery, so no delivery-less row can shadow a refusal.
func tableProblems(rows []Entry) []string {
	var out []string
	type set struct {
		s, f string
		c    Class
	}
	groups := map[set][]Entry{}
	for _, e := range rows {
		where := e.Struct + "." + e.Field + " (" + e.Variant + ")"
		if !e.Rule.known() {
			out = append(out, where+": unknown rule "+string(e.Rule))
		}
		if !e.Class.known() {
			out = append(out, where+": unknown class "+string(e.Class))
		}
		groups[set{e.Struct, e.Field, e.Class}] = append(groups[set{e.Struct, e.Field, e.Class}], e)
	}
	for k, g := range groups {
		if !slices.ContainsFunc(g, func(e Entry) bool {
			return slices.Contains([]Rule{RuleDelivery, RuleViaOrgRefuse, RuleNotConfigured}, e.Rule)
		}) {
			continue
		}
		seen := map[string]bool{}
		for _, e := range g {
			if e.Delivery == "" || seen[e.Delivery] {
				out = append(out, k.s+"."+k.f+" class "+string(k.c)+": a refusing set needs distinct non-empty deliveries")
				break
			}
			seen[e.Delivery] = true
		}
	}
	slices.Sort(out)
	return out
}

// expectedOutcome restates, independently of rulesOf, what a field's rows
// demand, so the guard cannot agree with a wrong rulesOf.
func expectedOutcome(rows []Entry) ruleSet {
	has := func(rules ...Rule) bool {
		return slices.ContainsFunc(rows, func(e Entry) bool { return slices.Contains(rules, e.Rule) && e.Class != ClassComposite })
	}
	return ruleSet{
		credential: has(RuleDelivery, RuleViaOrgRefuse, RuleNotConfigured, RuleOwnerOnlyOwn),
		gated:      has(RuleRefuse, RuleBound) || slices.ContainsFunc(rows, func(e Entry) bool { return !slices.Contains(allRules, e.Rule) }),
		notSent:    has(RuleNotSent),
		strip:      has(RuleStrip),
	}
}

// TestEveryTableRuleIsEnforced fills one field at a time with a sentinel and
// requires the outcome its table rows demand, so a row cannot exist without
// something enforcing it: a credential-bearing row yields a path and refuses
// without provenance, a gated row refuses, a not_sent field comes back zeroed
// (a field never marshalled stays), a strip row narrows the copy, and a field
// with only allow/exempt rows passes unchanged.
func TestEveryTableRuleIsEnforced(t *testing.T) {
	for _, st := range []struct {
		name, prefix string
		typ          reflect.Type
	}{
		{StructSandboxSpec, "SandboxSpec", reflect.TypeFor[runner.SandboxSpec]()},
		{StructProxyConfig, "ProxyConfig", reflect.TypeFor[runner.ProxyConfig]()},
	} {
		checked := 0
		for i := range st.typ.NumField() {
			f := st.typ.Field(i)
			if !f.IsExported() {
				continue
			}
			checked++
			rows := Entries(st.name, f.Name)
			rs := expectedOutcome(rows)
			var spec runner.SandboxSpec
			target := reflect.ValueOf(&spec).Elem()
			if st.name == StructProxyConfig {
				target = target.FieldByName("ProxyConfig")
			}
			field := target.Field(i)
			if st.name == StructSandboxSpec && f.Name == "ProxyConfig" {
				continue // composite: its fields are walked as the ProxyConfig struct
			}
			fillSentinel(field)
			if f.Name == "Policy" {
				spec.ProxyConfig.Policy.EligibleGrants = nil
			}
			plan := LocalPlan{Spec: spec, OrgConfigKeys: []string{"sentinel"}}
			out, r := LocalEligibility(plan)
			where := st.prefix + "." + f.Name
			switch {
			case rs.credential || rs.gated:
				if r == nil || !strings.HasPrefix(r.Field, where) {
					t.Errorf("%s has %v rows but a filled value was not refused at its path: %v", where, rs, r)
				}
				continue
			case r != nil:
				t.Errorf("%s: unexpected refusal %v", where, r)
				continue
			}
			got := reflect.ValueOf(&out).Elem()
			if st.name == StructProxyConfig {
				got = got.FieldByName("ProxyConfig")
			}
			got = got.Field(i)
			switch {
			case rs.notSent && f.Tag.Get("json") != "-":
				if !got.IsZero() {
					t.Errorf("%s is not_sent but survives in the local copy", where)
				}
			case f.Name == "Env":
				if _, kept := out.Env["sentinel"]; kept {
					t.Errorf("%s keeps an org component key", where)
				}
			case f.Name == "MITMHosts":
				if len(out.ProxyConfig.MITMHosts) != 0 {
					t.Errorf("%s keeps a host with no own injection: %v", where, out.ProxyConfig.MITMHosts)
				}
			case f.Name == "Policy":
				li := out.ProxyConfig.Policy.LLMInspection
				if li == nil || li.WorkspaceSecretNames != nil || li.WorkspaceSecretValues != nil {
					t.Errorf("%s keeps resolved inspection secrets: %+v", where, li)
				}
			case rs.strip:
				t.Errorf("%s has a strip row this guard does not know how to observe", where)
			default:
				if !reflect.DeepEqual(got.Interface(), field.Interface()) && f.Tag.Get("json") != "-" {
					t.Errorf("%s has only allow/exempt rows but changed in the copy", where)
				}
			}
		}
		if checked == 0 {
			t.Fatalf("%s: no fields walked", st.name)
		}
	}
}

// Every strip row has a stripper and every stripper a row; every gated field
// has a gate. Nothing is silently skipped.
func TestEveryStripAndGateRowHasAnImplementation(t *testing.T) {
	want := map[string]bool{}
	for _, e := range Table {
		if e.Struct == StructGrantKind {
			continue
		}
		key := e.Struct + "." + e.Field
		if e.Rule == RuleStrip && e.Class != ClassComposite {
			if e.Path != "" {
				key += "." + e.Path
			}
			want[key] = true
			if _, ok := strippers[key]; !ok {
				t.Errorf("strip row %s has no stripper", key)
			}
		}
		if (e.Rule == RuleRefuse || e.Rule == RuleBound) && gates[key] == nil {
			t.Errorf("gated row %s has no gate", key)
		}
	}
	for k := range strippers {
		if !want[k] {
			t.Errorf("stripper %s has no strip row", k)
		}
	}
}

type probeSpec struct {
	NewToken string
	NewGate  string
	NewStrip string
	Plain    string
}

// The reviewers' probes: a row for a field that Go now has. The row is enforced
// with no one editing a field list: a delivery row yields a path that refuses
// without provenance and with a configured delivery; a gated or strip row
// with no implementation refuses.
func TestTableRowForAnUnlistedFieldFailsClosed(t *testing.T) {
	rows := []Entry{
		{Struct: StructSandboxSpec, Field: "NewToken", Class: ClassOperator, Rule: RuleDelivery, Delivery: ClassEnvSecret, Reason: ReasonPlacementCredential},
		{Struct: StructSandboxSpec, Field: "NewGate", Class: ClassOperator, Rule: RuleRefuse, Reason: ReasonPlacementCapability},
		{Struct: StructSandboxSpec, Field: "NewStrip", Class: ClassOperator, Rule: RuleStrip},
		{Struct: StructSandboxSpec, Field: "Plain", Class: ClassExempt, Rule: RuleExempt},
	}
	src := func(s, f string) []Entry {
		return slices.DeleteFunc(slices.Clone(rows), func(e Entry) bool { return e.Struct != s || e.Field != f })
	}
	v := reflect.ValueOf(&probeSpec{NewToken: "operator-bearer-value", NewGate: "x", NewStrip: "x", Plain: "x"}).Elem()
	paths := structPaths(src, StructSandboxSpec, "SandboxSpec", v)
	if !slices.Equal(paths, []string{"SandboxSpec.NewToken"}) {
		t.Fatalf("paths=%v", paths)
	}
	for _, origin := range []CredentialOrigin{{}, {Class: ClassOperator, Delivery: ClassEnvSecret, Stored: true}} {
		for _, mode := range []Mode{ModeRefuse, ModeRunnerResident} {
			policy := DeliveryPolicy{Classes: map[string]ClassPolicy{ClassEnvSecret: {Mode: mode}}}
			if r := classifyCredential(src, paths[0], origin, policy); r == nil || r.Reason != ReasonPlacementCredential {
				t.Fatalf("origin %+v mode %s: %v", origin, mode, r)
			}
		}
	}
	if r := enforceStruct(src, LocalPlan{}, &runner.SandboxSpec{}, StructSandboxSpec, "SandboxSpec", v); r == nil || r.Field != "SandboxSpec.NewGate" {
		t.Fatalf("a gated row with no gate was skipped: %v", r)
	}
	v.FieldByName("NewGate").SetString("")
	if r := enforceStruct(src, LocalPlan{}, &runner.SandboxSpec{}, StructSandboxSpec, "SandboxSpec", v); r == nil || r.Field != "SandboxSpec.NewStrip" {
		t.Fatalf("a strip row with no stripper was skipped: %v", r)
	}
}

// ProxyConfig.DeployKey probe: the same rule on the proxy side.
func TestProxyRowForAnUnlistedFieldYieldsAPathAndRefuses(t *testing.T) {
	type probeProxy struct{ DeployKey map[string]string }
	src := func(s, f string) []Entry {
		if s == StructProxyConfig && f == "DeployKey" {
			return []Entry{{Struct: s, Field: f, Class: ClassOperator, Rule: RuleDelivery, Delivery: ClassAPIKey, Reason: ReasonPlacementCredential}}
		}
		return nil
	}
	paths := structPaths(src, StructProxyConfig, "ProxyConfig", reflect.ValueOf(probeProxy{DeployKey: map[string]string{"corp": "org-secret"}}))
	if !slices.Equal(paths, []string{"ProxyConfig.DeployKey[corp]"}) {
		t.Fatalf("paths=%v", paths)
	}
	if r := classifyCredential(src, paths[0], CredentialOrigin{Class: ClassOperator, Delivery: ClassAPIKey}, DeliveryPolicy{}); r == nil {
		t.Fatal("an operator delivery row admitted")
	}
	if got := structPaths(src, StructProxyConfig, "ProxyConfig", reflect.ValueOf(probeProxy{})); len(got) != 0 {
		t.Fatalf("an empty field produced paths: %v", got)
	}
}

func TestPolicyStripRowsNarrowTheCopyOnly(t *testing.T) {
	li := &types.LLMInspectionSpec{WorkspaceSecretNames: []string{"n"}, WorkspaceSecretValues: []string{"v"}, DetectSecrets: true}
	p := LocalPlan{Spec: runner.SandboxSpec{ProxyConfig: runner.ProxyConfig{Policy: types.RunPolicySpec{LLMInspection: li}}}}
	out, r := LocalEligibility(p)
	if r != nil || !out.ProxyConfig.Policy.LLMInspection.DetectSecrets || out.ProxyConfig.Policy.LLMInspection.WorkspaceSecretValues != nil {
		t.Fatalf("%v %+v", r, out.ProxyConfig.Policy.LLMInspection)
	}
	if li.WorkspaceSecretValues == nil {
		t.Fatal("the admitted snapshot was mutated")
	}
}

type embeddedProbe struct{ Token string }
type keyProbe struct{ Secret string }
type blindSpots struct {
	Known string
	embeddedProbe
	Any    any
	ByKey  map[keyProbe]string
	Reader func()
}

// The closed schema walk does not stop at an unexported embedded struct (its
// exported fields promote into JSON), an interface or func value, or a map key.
func TestSchemaWalkFlagsEmbeddedInterfaceAndMapKeyBlindSpots(t *testing.T) {
	typ := reflect.TypeFor[blindSpots]()
	manifest := func(listed ...string) map[reflect.Type][]string {
		return map[reflect.Type][]string{
			typ:                         listed,
			reflect.TypeFor[keyProbe](): {"Secret"}, reflect.TypeFor[embeddedProbe](): {"Token"},
		}
	}
	got := SchemaUnclassified(typ, "blind", manifest("Known", "Any", "ByKey", "Reader"), nil)
	for _, want := range []string{"blind.embeddedProbe", "blind.Any", "blind.Reader"} {
		if !slices.Contains(got, want) {
			t.Errorf("%s escaped the walk: %v", want, got)
		}
	}
	keys := SchemaUnclassified(typ, "blind", map[reflect.Type][]string{typ: {"ByKey"}, reflect.TypeFor[keyProbe](): {}}, nil)
	if !slices.Contains(keys, "blind.ByKey[key].Secret") {
		t.Errorf("an unlisted map-key field escaped: %v", keys)
	}
	if got := SchemaUnclassified(reflect.TypeFor[runner.SandboxSpec](), "SandboxSpec", nil, nil); len(got) != 0 {
		t.Errorf("the real spec has unclassified paths: %v", got)
	}
}

type rootProbe struct {
	Known string
	embeddedProbe
}

// A struct the table classifies field by field cannot hide an unexported
// embedded struct: the table has no row that could name it.
func TestSchemaWalkFlagsAnEmbeddedStructOnATableRoot(t *testing.T) {
	typ := reflect.TypeFor[rootProbe]()
	tableRoots[typ] = true
	t.Cleanup(func() { delete(tableRoots, typ) })
	got := SchemaUnclassified(typ, "root", map[reflect.Type][]string{typ: {"Known"}, reflect.TypeFor[embeddedProbe](): {"Token"}}, nil)
	if !slices.Contains(got, "root.embeddedProbe") {
		t.Fatalf("an embedded unexported struct escaped a table root: %v", got)
	}
}

// Fable's probe: a typo in a rule name must not be an allow.
func TestUnknownRuleRefusesAndIsRejectedByTheTable(t *testing.T) {
	typo := Entry{Struct: StructProxyConfig, Field: "GitGrants", Class: ClassBrokered, Rule: Rule("via_org_refus"), Delivery: ClassGitHubToken, Reason: ReasonPlacementCredential}
	if problems := tableProblems([]Entry{typo}); len(problems) != 1 || !strings.Contains(problems[0], "unknown rule") {
		t.Fatalf("problems=%v", problems)
	}
	src := func(s, f string) []Entry {
		if s == typo.Struct && f == typo.Field {
			return []Entry{typo}
		}
		return nil
	}
	spec := runner.SandboxSpec{ProxyConfig: runner.ProxyConfig{GitGrants: map[string]uuid.UUID{"org/repo": uuid.New()}}}
	if r := enforceStruct(src, LocalPlan{Spec: spec}, &spec, StructProxyConfig, "ProxyConfig", reflect.ValueOf(&spec.ProxyConfig).Elem()); r == nil || r.Field != "ProxyConfig.GitGrants" {
		t.Fatalf("an unknown rule was not treated as gated-without-gate: %v", r)
	}
	for _, origin := range []CredentialOrigin{{Class: ClassBrokered, Delivery: ClassGitHubToken}} {
		if r := classifyCredential(src, "ProxyConfig.GitGrants[org/repo]", origin, DeliveryPolicy{}); r == nil {
			t.Fatal("an unknown rule admitted a credential")
		}
	}
	if !rulesOf([]Entry{typo}).gated {
		t.Fatal("rulesOf does not gate an unknown rule")
	}
}

// Fable's shadow probe: a future own refusing row after delivery-less own rows.
func TestDeliveryLessRowCannotShadowALaterRefusingRow(t *testing.T) {
	rows := []Entry{
		{Struct: StructProxyConfig, Field: "Injection", Class: ClassOwn, Rule: RuleAllow, Variant: "a"},
		{Struct: StructProxyConfig, Field: "Injection", Class: ClassOwn, Rule: RuleAllow, Variant: "b"},
		{Struct: StructProxyConfig, Field: "Injection", Class: ClassOwn, Rule: RuleDelivery, Delivery: ClassAPIKey, Reason: ReasonPlacementCredential, Variant: "future"},
	}
	if problems := tableProblems(rows); len(problems) != 1 || !strings.Contains(problems[0], "distinct non-empty deliveries") {
		t.Fatalf("the ambiguous set was accepted: %v", problems)
	}
	src := func(s, f string) []Entry { return rows }
	own := CredentialOrigin{Class: ClassOwn, Delivery: ClassAPIKey, Stored: true, OwnNamespace: true, OwnerOnly: true}
	if r := classifyCredential(src, "ProxyConfig.Injection[0]", own, DeliveryPolicy{}); r == nil {
		t.Fatal("a delivery-less row shadowed the refusing row")
	}
	if r := classifyCredential(src, "ProxyConfig.Injection[0]", CredentialOrigin{Class: ClassOwn, OwnNamespace: true}, DeliveryPolicy{}); r != nil {
		t.Fatalf("an origin with no delivery should still reach the general rows: %v", r)
	}
}
