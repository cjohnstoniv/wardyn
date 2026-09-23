// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestDecisionRoundTripsThroughJSON is the serializability seam: a Decision
// with every field set survives a JSON round trip unchanged, so 0.9 can answer
// one over the wire without a second shape.
func TestDecisionRoundTripsThroughJSON(t *testing.T) {
	run := uuid.New()
	d := Drop(ReasonCapabilityEgressHost, "runs.inline_policy", []string{"a.example", "b.example"}).
		OnRun(run).With("host", "a.example")
	d.Sentence = "a sentence"
	d.Trace = []Step{{Rule: "deny_overlaps", Tier: "group", RowID: uuid.NewString(), Outcome: "deny"}}

	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var back Decision
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, d) {
		t.Fatalf("round trip changed the decision:\n got %#v\nwant %#v", back, d)
	}
	if !strings.Contains(string(b), `"schema":"authz/v1"`) {
		t.Errorf("wire form %s does not name its schema", b)
	}
}

// TestDenyTakesItsEffectFromTheRegistry: a door names a reason, never a status.
func TestDenyTakesItsEffectFromTheRegistry(t *testing.T) {
	for _, tc := range []struct {
		reason Reason
		effect Effect
		status int
	}{
		{ReasonAdminSurface, EffectDeny, 403},
		{ReasonNotOwner, EffectHidden, 404},
		{ReasonHarnessLoginMechanismPrincipal, EffectUnprocessable, 422},
		{Reason("never_registered"), EffectDeny, 403},
	} {
		d := Deny(tc.reason, "t", "")
		if d.Effect != tc.effect || d.Status != tc.status {
			t.Errorf("Deny(%s) = %s/%d, want %s/%d", tc.reason, d.Effect, d.Status, tc.effect, tc.status)
		}
	}
}

// TestDatumMarksWhereAndHowARefusalWasMet pins the audit row's reserved keys.
func TestDatumMarksWhereAndHowARefusalWasMet(t *testing.T) {
	plain := Datum(Deny(ReasonNotOwner, "t", ""), Principal{Subject: "bob"}, "GET")
	if want := map[string]any{"reason": "not_owner", "method": "GET"}; !reflect.DeepEqual(plain, want) {
		t.Errorf("plain datum = %#v, want %#v — member_mode and device_channel are markers, absent when unset", plain, want)
	}

	dev := uuid.New()
	marked := Datum(Deny(ReasonAdminSurface, "t", ""), Principal{MemberView: true, Origin: Origin{DeviceID: &dev}}, "")
	if marked["member_mode"] != true {
		t.Errorf("member view not marked: %#v", marked)
	}
	if ch, _ := marked["device_channel"].(map[string]any); ch["device_id"] != dev.String() {
		t.Errorf("device_channel = %#v, want the device id the org authenticated", marked["device_channel"])
	}
	if _, ok := marked["device_origin"]; ok {
		t.Error("device_origin is the ingest marker of a forwarded laptop row; a decision stamps device_channel")
	}
	if _, ok := marked["method"]; ok {
		t.Error("no request, no method")
	}

	// A detail cannot forge a reserved key.
	forged := Deny(ReasonNotOwner, "t", "").With("reason", "admin_surface").With("member_mode", "true").With("host", "h")
	got := Datum(forged, Principal{}, "")
	if got["reason"] != "not_owner" || got["member_mode"] != nil || got["host"] != "h" {
		t.Errorf("datum = %#v, want the decision's reason, no marker, and the host detail", got)
	}

	drop := Datum(Drop(ReasonCapabilitySecret, "runs.inline_policy", []string{"x", "y"}), Principal{}, "POST")
	if !reflect.DeepEqual(drop["dropped"], []string{"x", "y"}) {
		t.Errorf("dropped = %#v, want the dropped entries in order", drop["dropped"])
	}
}

// TestWithNeverChangesTheDecisionItWasBuiltFrom: decisions are values.
func TestWithNeverChangesTheDecisionItWasBuiltFrom(t *testing.T) {
	base := Deny(ReasonNotOwner, "t", "").With("a", "1")
	_ = base.With("b", "2")
	if len(base.Detail) != 1 {
		t.Errorf("base.Detail = %v, want only its own detail", base.Detail)
	}
}

// TestWireContractGolden is the append-only wire contract: every schema,
// effect, obligation kind and reason code this package declares, against
// testdata/wire.golden. Before the 0.8.0 tag a rename edits both; from 0.8.0 a
// line may only be appended — removing or renaming one is a major version,
// because a SIEM rule or a 0.9 laptop is reading it.
func TestWireContractGolden(t *testing.T) {
	var got []string
	got = append(got, "schema "+Schema)
	for typ, vals := range declaredConsts(t) {
		for _, v := range vals {
			got = append(got, strings.ToLower(typ)+" "+v)
		}
	}
	slices.Sort(got)

	b, err := os.ReadFile("testdata/wire.golden")
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			want = append(want, line)
		}
	}
	slices.Sort(want)
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("%q is in the wire contract but no longer declared — removing or renaming a wire value is a major version", w)
		}
	}
	for _, g := range got {
		if !slices.Contains(want, g) {
			t.Errorf("%q is declared but not in testdata/wire.golden — append it", g)
		}
	}
}

// TestEveryDeclaredReasonIsRegistered: a Reason constant with no registry row
// would refuse with a code nothing documents; a row with no constant is dead.
func TestEveryDeclaredReasonIsRegistered(t *testing.T) {
	declared := slices.Sorted(slices.Values(declaredConsts(t)["Reason"]))
	var registered []string
	for _, r := range Reasons() {
		registered = append(registered, string(r))
	}
	if !slices.Equal(declared, registered) {
		t.Errorf("declared Reason constants %v\n != registered reasons %v", declared, registered)
	}
}

// declaredConsts reads this package's typed string constants, by type name.
func declaredConsts(t *testing.T) map[string][]string {
	t.Helper()
	pkgs, err := parser.ParseDir(token.NewFileSet(), ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, f := range pkgs["authz"].Files {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				typ, ok := vs.Type.(*ast.Ident)
				if !ok || !slices.Contains([]string{"Effect", "ObligationKind", "Reason"}, typ.Name) {
					continue
				}
				for _, v := range vs.Values {
					lit, ok := v.(*ast.BasicLit)
					if !ok {
						t.Fatalf("a %s constant is not a literal", typ.Name)
					}
					s, _ := strconv.Unquote(lit.Value)
					out[typ.Name] = append(out[typ.Name], s)
				}
			}
		}
	}
	return out
}
