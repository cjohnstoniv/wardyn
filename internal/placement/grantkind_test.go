// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package placement

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// grantKindFixture is the expected local outcome per grant kind. A new kind
// has no entry here and no table row, so the guards below fail until it is
// classified on purpose.
var grantKindFixture = map[types.GrantKind]struct {
	delivery  string
	ownAllows bool
}{
	types.GrantAPIKey:      {ClassAPIKey, true},
	types.GrantEnvSecret:   {ClassEnvSecret, true},
	types.GrantFileSecret:  {ClassFileSecret, true},
	types.GrantSSHKey:      {ClassSSHKey, true},
	types.GrantGitPAT:      {ClassGitPATHelper, true},
	types.GrantGitHubToken: {ClassGitHubToken, false},
	types.GrantCloudSTS:    {ClassCloudSTS, false},
}

// declaredGrantKinds reads every GrantKind constant from the types source, so
// the fixture cannot silently lag a constant someone adds.
func declaredGrantKinds(t *testing.T) []types.GrantKind {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "../types/types.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []types.GrantKind
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, sp := range gd.Specs {
			vs := sp.(*ast.ValueSpec)
			if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "GrantKind" {
				continue
			}
			for _, v := range vs.Values {
				lit, ok := v.(*ast.BasicLit)
				if !ok {
					t.Fatalf("GrantKind constant %v is not a string literal", vs.Names)
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				kinds = append(kinds, types.GrantKind(s))
			}
		}
	}
	return kinds
}

func TestEveryGrantKindIsClassifiedAndFixtured(t *testing.T) {
	kinds := declaredGrantKinds(t)
	if len(kinds) < len(grantKindFixture) {
		t.Fatalf("parsed %d kinds, fixture has %d: the parser broke", len(kinds), len(grantKindFixture))
	}
	for _, k := range kinds {
		if len(Entries(StructGrantKind, string(k))) == 0 {
			t.Errorf("grant kind %q has no placement.Table row", k)
		}
		if _, ok := grantKindFixture[k]; !ok {
			t.Errorf("grant kind %q has no expected local outcome in grantKindFixture", k)
		}
	}
	for k := range grantKindFixture {
		if !slices.Contains(kinds, k) {
			t.Errorf("fixture names %q, which is not a declared grant kind", k)
		}
	}
}

func grantPlan(kind types.GrantKind, grantOwnerOnly bool, origin CredentialOrigin) LocalPlan {
	path := "ProxyConfig.Policy.EligibleGrants[0]"
	return LocalPlan{
		Spec:    runner.SandboxSpec{ProxyConfig: runner.ProxyConfig{Policy: types.RunPolicySpec{EligibleGrants: []types.GrantSpec{{Kind: kind, OwnerOnly: grantOwnerOnly}}}}},
		Origins: map[string]CredentialOrigin{path: origin},
	}
}

func TestLocalEligibilityClassifiesEveryGrantKind(t *testing.T) {
	for _, kind := range declaredGrantKinds(t) {
		want := grantKindFixture[kind]
		own := CredentialOrigin{Class: ClassOwn, Delivery: want.delivery, GrantKind: kind, Stored: true, OwnNamespace: true, OwnerOnly: true}
		refuses := func(name string, p LocalPlan) {
			t.Helper()
			if _, r := LocalEligibility(p); r == nil || r.Reason != ReasonPlacementCredential || r.Field != "ProxyConfig.Policy.EligibleGrants[0]" {
				t.Errorf("%s/%s: refusal=%v", kind, name, r)
			}
		}
		_, r := LocalEligibility(grantPlan(kind, true, own))
		if (r == nil) != want.ownAllows {
			t.Errorf("%s: own namespace outcome=%v, want allowed=%v", kind, r, want.ownAllows)
		}
		for _, class := range []Class{ClassOperator, ClassBrokered, ClassPlatform} {
			refuses("class "+string(class), grantPlan(kind, true, CredentialOrigin{Class: class, Delivery: want.delivery, GrantKind: kind, Stored: true, OwnNamespace: true, OwnerOnly: true}))
		}
		// An org-held class stays refused whatever delivery the org configures.
		for _, mode := range []Mode{ModeViaOrg, ModeRunnerResident} {
			p := grantPlan(kind, true, CredentialOrigin{Class: ClassOperator, Delivery: want.delivery, GrantKind: kind, Stored: true})
			p.Delivery = DeliveryPolicy{Classes: map[string]ClassPolicy{want.delivery: {Mode: mode}}}
			refuses("operator delivery "+string(mode), p)
		}
		refuses("unknown provenance", grantPlan(kind, true, CredentialOrigin{GrantKind: kind}))
		refuses("origin of another kind", grantPlan(kind, true, CredentialOrigin{Class: ClassOwn, Delivery: want.delivery, GrantKind: "other", Stored: true, OwnNamespace: true, OwnerOnly: true}))
		refuses("missing provenance", LocalPlan{Spec: grantPlan(kind, true, own).Spec})
		if !want.ownAllows {
			continue
		}
		noNS := own
		noNS.OwnNamespace = false
		refuses("own without namespace proof", grantPlan(kind, true, noNS))
		refuses("grant not OwnerOnly", grantPlan(kind, false, own))
		noOwnerOnly := own
		noOwnerOnly.OwnerOnly = false
		refuses("origin not OwnerOnly", grantPlan(kind, true, noOwnerOnly))
	}
}

func TestLocalEligibilityFailsClosedWhenTableLosesAClassification(t *testing.T) {
	saved := slices.Clone(Table)
	t.Cleanup(func() { Table = saved })
	Table = slices.DeleteFunc(slices.Clone(Table), func(e Entry) bool { return e.Struct == StructSandboxSpec && e.Field == "Labels" })
	if len(Unclassified()) == 0 {
		t.Fatal("dropping a row left every field classified")
	}
	_, r := LocalEligibility(LocalPlan{})
	if r == nil || r.Reason != ReasonPlacementCredential || !strings.Contains(r.Field, "Labels") {
		t.Fatalf("an unclassified dispatch field did not refuse: %v", r)
	}
}

func TestLocalEligibilityOwnWithoutNamespaceProofRefusesEvenWhenNotStored(t *testing.T) {
	path := "ProxyConfig.PATGrants[git.example]"
	p := LocalPlan{Spec: runner.SandboxSpec{ProxyConfig: runner.ProxyConfig{PATGrants: map[string]proxy.PATGrant{"git.example": {}}}}}
	for _, o := range []CredentialOrigin{{Class: ClassOwn}, {Class: ClassOwn, OwnerOnly: true}} {
		p.Origins = map[string]CredentialOrigin{path: o}
		if _, r := LocalEligibility(p); r == nil || r.Field != path || r.Reason != ReasonPlacementCredential {
			t.Fatalf("%+v admitted without its owner namespace: %v", o, r)
		}
	}
	p.Origins = map[string]CredentialOrigin{path: {Class: ClassOwn, OwnNamespace: true}}
	if _, r := LocalEligibility(p); r != nil {
		t.Fatalf("a proven own credential was refused: %v", r)
	}
}

func TestLocalEligibilityMountsAndDriveStayBoundToTheRunner(t *testing.T) {
	mount := func(authored bool) LocalPlan {
		return LocalPlan{Spec: runner.SandboxSpec{Mounts: []runner.Mount{{Source: "/home/p/src", Target: "/w", MemberAuthored: authored}}}}
	}
	if _, r := LocalEligibility(mount(false)); r == nil || r.Reason != ReasonPlacementCapability {
		t.Fatalf("operator host mount: %v", r)
	}
	if _, r := LocalEligibility(mount(true)); r == nil || r.Reason != ReasonPlacementLocalPath {
		t.Fatalf("unbound member mount: %v", r)
	}
	bound := mount(true)
	bound.VerifiedLocalPaths = map[string]bool{"SandboxSpec.Mounts[0]": true}
	if _, r := LocalEligibility(bound); r != nil {
		t.Fatal(r)
	}
	drive := LocalPlan{Spec: runner.SandboxSpec{Drive: &types.DriveMount{Backend: types.DriveBackendK8sPVC}}}
	if _, r := LocalEligibility(drive); r == nil || r.Reason != ReasonPlacementCapability {
		t.Fatalf("org drive: %v", r)
	}
	drive.Spec.Drive.Backend = types.DriveBackendHostPath
	if _, r := LocalEligibility(drive); r == nil || r.Reason != ReasonPlacementLocalPath {
		t.Fatalf("unbound host drive: %v", r)
	}
}

func TestSchemaFlagsAnUnlistedChildOfAKnownCarrier(t *testing.T) {
	type carrier struct{ Known, Fresh string }
	known := map[reflect.Type][]string{reflect.TypeFor[carrier](): {"Known"}}
	got := SchemaUnclassified(reflect.TypeFor[carrier](), "carrier", known)
	if !slices.Equal(got, []string{"carrier.Fresh"}) {
		t.Fatalf("unlisted child: %v", got)
	}
	known[reflect.TypeFor[carrier]()] = []string{"Known", "Fresh"}
	if got := SchemaUnclassified(reflect.TypeFor[carrier](), "carrier", known); len(got) != 0 {
		t.Fatalf("fully listed carrier flagged: %v", got)
	}
}
