// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package placement

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
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

// declaredGrantKinds reads every GrantKind constant of every non-test file of
// the types package, so a constant added elsewhere, or declared as
// GrantKind("x"), cannot lag the fixture. A value that is not a literal fails.
func declaredGrantKinds(t *testing.T) []types.GrantKind {
	t.Helper()
	files, err := filepath.Glob("../types/*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no types sources: %v", err)
	}
	var kinds []types.GrantKind
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, sp := range gd.Specs {
				vs := sp.(*ast.ValueSpec)
				for i, v := range vs.Values {
					if k, ok := grantKindLiteral(t, vs, i, v); ok {
						kinds = append(kinds, k)
					}
				}
			}
		}
	}
	return kinds
}

// grantKindLiteral recognises "X GrantKind = \"lit\"" and "X = GrantKind(\"lit\")".
func grantKindLiteral(t *testing.T, vs *ast.ValueSpec, i int, v ast.Expr) (types.GrantKind, bool) {
	t.Helper()
	typed := false
	if id, ok := vs.Type.(*ast.Ident); ok && id.Name == "GrantKind" {
		typed = true
	}
	if call, ok := v.(*ast.CallExpr); ok {
		if fn, ok := call.Fun.(*ast.Ident); ok && fn.Name == "GrantKind" && len(call.Args) == 1 {
			typed, v = true, call.Args[0]
		}
	}
	if !typed {
		return "", false
	}
	lit, ok := v.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		t.Fatalf("GrantKind constant %v is not a string literal: classify it by hand", vs.Names[i])
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		t.Fatal(err)
	}
	return types.GrantKind(s), true
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
		return LocalPlan{Spec: runner.SandboxSpec{Mounts: []runner.Mount{{Source: "/work/src", Target: "/w", MemberAuthored: authored}}}}
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
	got := SchemaUnclassified(reflect.TypeFor[carrier](), "carrier", known, nil)
	if !slices.Equal(got, []string{"carrier.Fresh"}) {
		t.Fatalf("unlisted child: %v", got)
	}
	known[reflect.TypeFor[carrier]()] = []string{"Known", "Fresh"}
	if got := SchemaUnclassified(reflect.TypeFor[carrier](), "carrier", known, nil); len(got) != 0 {
		t.Fatalf("fully listed carrier flagged: %v", got)
	}
}

func TestGrantKindLiteralReaderSeesBothConstForms(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "x.go", "package x\nconst (\n\tA GrantKind = \"a\"\n\tB = GrantKind(\"b\")\n\tC = \"not a kind\"\n)\n", 0)
	if err != nil {
		t.Fatal(err)
	}
	var got []types.GrantKind
	for _, sp := range f.Decls[0].(*ast.GenDecl).Specs {
		vs := sp.(*ast.ValueSpec)
		if k, ok := grantKindLiteral(t, vs, 0, vs.Values[0]); ok {
			got = append(got, k)
		}
	}
	if !slices.Equal(got, []types.GrantKind{"a", "b"}) {
		t.Fatalf("got %v", got)
	}
}

// Resident Bedrock role credentials sign in-process: even the person's own
// bedrock_sso is runner_resident or refuse (design 6.3, 7.2), never allowed.
func TestLocalEligibilityOwnBedrockRoleCredentialsAreDeliveryOrRefuse(t *testing.T) {
	path := "SandboxSpec.SecretEnv[ROLE_SIGNING_KEY]"
	own := CredentialOrigin{Class: ClassOwn, Delivery: ClassBedrockRoleCreds, Stored: true, OwnNamespace: true, OwnerOnly: true}
	for _, mode := range []Mode{ModeRefuse, ModeRunnerResident} {
		p := LocalPlan{Spec: runner.SandboxSpec{SecretEnv: map[string]string{"ROLE_SIGNING_KEY": "v"}}, Origins: map[string]CredentialOrigin{path: own},
			Delivery: DeliveryPolicy{Classes: map[string]ClassPolicy{ClassBedrockRoleCreds: {Mode: mode}}}}
		if _, r := LocalEligibility(p); r == nil || r.Field != path || r.Reason != ReasonPlacementCredential || !strings.Contains(r.Detail, ClassBedrockRoleCreds) {
			t.Fatalf("mode %s: %v", mode, r)
		}
	}
}
