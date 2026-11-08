// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// fillSpec sets every field of v to a non-zero value, so a test sees the whole
// shape of RunPolicySpec and not only the fields a fixture happens to name. It
// refuses an interface field: a value of unknown type is the one thing that can
// make Marshal then Unmarshal drift, and the digest is only reproducible from a
// stored row while it cannot. A json.RawMessage field is allowed (Marshal compacts it, so
// the first Marshal is already canonical) and is filled with a value that needs escaping.
func fillSpec(t *testing.T, v reflect.Value, path string) {
	t.Helper()
	switch v.Kind() {
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				fillSpec(t, v.Field(i), path+"."+v.Type().Field(i).Name)
			}
		}
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fillSpec(t, v.Elem(), path)
	case reflect.Slice:
		if v.Type() == reflect.TypeFor[json.RawMessage]() {
			v.SetBytes([]byte(`{"a":[1,"<&>"]}`))
			return
		}
		if v.Type().Elem().Kind() == reflect.Uint8 {
			v.SetBytes([]byte("b"))
			return
		}
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		fillSpec(t, v.Index(0), path+"[]")
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
		k, e := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
		fillSpec(t, k, path+"{key}")
		fillSpec(t, e, path+"{}")
		v.SetMapIndex(k, e)
	case reflect.String:
		v.SetString("a")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(3)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(3)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1.5)
	case reflect.Interface:
		t.Fatalf("%s is an interface field of RunPolicySpec: Marshal then Unmarshal is no longer a fixed point there, so Digest cannot be recomputed from a stored row", path)
	default:
		t.Fatalf("%s: unhandled kind %s", path, v.Kind())
	}
}

// TestPolicyDigest_RoundTripStable pins that Marshal then Unmarshal is a fixed
// point over a spec with every field set: the digest recorded at dispatch must
// equal the one recomputed from the run.policy.resolve row's stored data.
func TestPolicyDigest_RoundTripStable(t *testing.T) {
	var spec RunPolicySpec
	fillSpec(t, reflect.ValueOf(&spec).Elem(), "RunPolicySpec")
	first, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	var back RunPolicySpec
	if err := json.Unmarshal(first, &back); err != nil {
		t.Fatalf("a filled spec does not unmarshal from its own marshal: %v", err)
	}
	second, err := json.Marshal(back)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("Marshal(Unmarshal(Marshal(spec))) differs from Marshal(spec):\n first  %s\n second %s", first, second)
	}
	if spec.Digest() != back.Digest() {
		t.Errorf("digest of the spec %s != digest of its round trip %s", spec.Digest(), back.Digest())
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(spec.Digest()) {
		t.Errorf("digest = %q, want 64 lowercase hex characters", spec.Digest())
	}
}

// TestPolicyDigest_IgnoresEnvelopeExtras: the run.policy.resolve row is the spec
// plus keys that are not policy (disk fill facts, provenance, host marks) and
// keys the audit chain adds to any row's data. Reading the row back into a
// RunPolicySpec drops all of them, so none can change the digest.
func TestPolicyDigest_IgnoresEnvelopeExtras(t *testing.T) {
	spec := RunPolicySpec{AllowedDomains: []string{"api.example.com"}, FirstUseApproval: FirstUseAlwaysDeny}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	row := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"disk_mib_filled": `true`, "disk_mib_basis": `"default"`, "provenance": `{"x":{"layer":"org"}}`,
		"host_marks": `{"open":["a"]}`, "policy_digest": `"abc"`, "via": `{"delegate":"d"}`, "dry_run": `true`,
		"relay": `{"a":1}`, "mask_scope": `"globals_only"`, "correlation_id": `"c1"`,
	} {
		row[k] = json.RawMessage(v)
	}
	stored, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	var back RunPolicySpec
	if err := json.Unmarshal(stored, &back); err != nil {
		t.Fatal(err)
	}
	if back.Digest() != spec.Digest() {
		t.Errorf("digest from the stored row %s != digest of the spec %s", back.Digest(), spec.Digest())
	}
}

// TestPolicyDigest_NamesTheContent: a different policy has a different digest,
// and a Clone has the same one.
func TestPolicyDigest_NamesTheContent(t *testing.T) {
	a := RunPolicySpec{AllowedDomains: []string{"a.example.com"}}
	b := RunPolicySpec{AllowedDomains: []string{"b.example.com"}}
	if a.Digest() == b.Digest() {
		t.Error("two policies that allow different hosts share a digest")
	}
	if a.Digest() != a.Clone().Digest() {
		t.Error("a Clone has a different digest from its original")
	}
}

// TestPolicyDigest_IgnoresScopeSpelling: a grant scope is a json.RawMessage, which Marshal keeps in the
// writer's member order and escape spelling, while the audit table's JSONB reorders keys and rewrites
// strings. All spellings of one scope are one policy, so they are one digest, and so is the spec read
// back from a row whose scope the database has rewritten.
func TestPolicyDigest_IgnoresScopeSpelling(t *testing.T) {
	withScope := func(scope string) RunPolicySpec {
		return RunPolicySpec{AllowedDomains: []string{"api.example.com"},
			EligibleGrants: []GrantSpec{{Kind: GrantGitPAT, Scope: json.RawMessage(scope), TTLSeconds: 60}}}
	}
	canonical := withScope(`{"host":"api.example.com","secret_name":"s","n":100,"nested":{"a":1,"b":[true,null,"x"]}}`)
	want := canonical.Digest()
	for name, scope := range map[string]string{
		"reordered members":  `{"secret_name":"s","host":"api.example.com","nested":{"b":[true,null,"x"],"a":1},"n":100}`,
		"escaped characters": `{"host":"api\u002eexample.com","secret_name":"\u0073","n":100,"nested":{"a":1,"b":[true,null,"x"]}}`,
		"whitespace":         "{ \"host\" : \"api.example.com\",\n\"secret_name\":\"s\",\"n\":100,\"nested\":{\"a\":1,\"b\":[true,null,\"x\"]} }",
		"number spelling":    `{"host":"api.example.com","secret_name":"s","n":1e2,"nested":{"a":1.0,"b":[true,null,"x"]}}`,
	} {
		if got := withScope(scope).Digest(); got != want {
			t.Errorf("%s: digest %s != %s", name, got, want)
		}
	}
	if withScope(`{"host":"other.example.com","secret_name":"s","n":100,"nested":{"a":1,"b":[true,null,"x"]}}`).Digest() == want {
		t.Error("a scope naming another host has the same digest")
	}
}

// TestPolicyDigest_ScopeNumbersAndRepeatedKeys: a scope number's spelling does not change the digest,
// its value does, and a repeated scope key digests as its last value, as JSONB stores it.
func TestPolicyDigest_ScopeNumbersAndRepeatedKeys(t *testing.T) {
	withScope := func(scope string) RunPolicySpec {
		return RunPolicySpec{AllowedDomains: []string{"api.example.com"},
			EligibleGrants: []GrantSpec{{Kind: GrantGitPAT, Scope: json.RawMessage(scope), TTLSeconds: 60}}}
	}
	if a, b := withScope(`{"host":"h","n":1e1001}`).Digest(), withScope(`{"host":"h","n":1`+strings.Repeat("0", 1001)+`}`).Digest(); a != b {
		t.Errorf("1e1001 and its plain integer: digests %s != %s", a, b)
	}
	if withScope(`{"host":"h","n":1e-41}`).Digest() == withScope(`{"host":"h","n":0}`).Digest() {
		t.Error("1e-41 and 0 have the same digest")
	}
	if a, b := withScope(`{"host":"a","host":"b"}`).Digest(), withScope(`{"host":"b"}`).Digest(); a != b {
		t.Errorf("a repeated key: digest %s != %s of its last value", a, b)
	}
}
