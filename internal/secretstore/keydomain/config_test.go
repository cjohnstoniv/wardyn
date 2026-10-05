// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package keydomain

import (
	"strings"
	"testing"
)

func TestParse_AcceptsBothKinds(t *testing.T) {
	f, err := Parse([]byte(`{
		"acme": {"transit": {"key": "acme-keys", "role": "wardyn-acme"}},
		"beta-2": {"azurekv": {"key": "https://v.vault.azure.net/keys/a", "signingKey": "https://v.vault.azure.net/keys/b", "clientId": "c"}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Names(); len(got) != 2 || got[0] != "acme" || got[1] != "beta-2" {
		t.Fatalf("Names = %v", got)
	}
	if f["acme"].Transit.Role != "wardyn-acme" || f["beta-2"].AzureKV.ClientID != "c" {
		t.Fatalf("parsed = %+v", f)
	}
}

// The chart renders kek.domains with toJson (keys sorted, no whitespace); this is
// that output for the two-domain example in deploy/helm/wardyn/README.md.
func TestParse_ReadsWhatTheChartRenders(t *testing.T) {
	f, err := Parse([]byte(`{"acme":{"transit":{"key":"acme-keys","role":"wardyn-acme"}},"beta":{"azurekv":{"clientId":"c","key":"https://beta.vault.azure.net/keys/wrap","signingKey":"https://beta.vault.azure.net/keys/sign"}}}`))
	if err != nil || len(f) != 2 || f["beta"].AzureKV.SigningKey != "https://beta.vault.azure.net/keys/sign" || f["acme"].Transit.Role != "wardyn-acme" {
		t.Fatalf("Parse = (%+v, %v)", f, err)
	}
}

func TestParse_EmptyDeclaresNone(t *testing.T) {
	for _, in := range []string{"", "  \n", "{}"} {
		f, err := Parse([]byte(in))
		if err != nil || len(f) != 0 {
			t.Errorf("Parse(%q) = (%v, %v); want no domain", in, f, err)
		}
	}
}

func TestParse_Refusals(t *testing.T) {
	const key = `{"transit": {"key": "k"}}`
	for name, tc := range map[string]struct{ in, want string }{
		"a domain named default": {`{"default": ` + key + `}`, `named "default"`},
		"an empty name":          {`{"": ` + key + `}`, "empty"},
		"upper case":             {`{"Acme": ` + key + `}`, "a-z, 0-9"},
		"an underscore":          {`{"a_b": ` + key + `}`, "a-z, 0-9"},
		"a slash":                {`{"a/b": ` + key + `}`, "a-z, 0-9"},
		"too long":               {`{"` + strings.Repeat("a", MaxNameLen+1) + `": ` + key + `}`, "characters"},
		"no key":                 {`{"a": {}}`, "names no key"},
		"both kinds":             {`{"a": {"transit": {"key": "k"}, "azurekv": {"key": "x", "signingKey": "y"}}}`, "both"},
		"a transit with no key":  {`{"a": {"transit": {"role": "r"}}}`, "needs a key"},
		"half a key vault pair":  {`{"a": {"azurekv": {"key": "x"}}}`, "key and signingKey"},
		"an unknown field":       {`{"a": {"transit": {"key": "k", "rol": "r"}}}`, "unknown field"},
		"not an object":          {`["a"]`, "JSON object"},
		"trailing content":       {`{} {}`, "trailing"},
	} {
		if _, err := Parse([]byte(tc.in)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Parse = %v; want a refusal containing %q", name, err, tc.want)
		}
	}
}

func TestLoad(t *testing.T) {
	if f, err := Load(""); err != nil || len(f) != 0 {
		t.Fatalf("Load(\"\") = (%v, %v); want no domain", f, err)
	}
	if _, err := Load("/nonexistent/key-domains.json"); err == nil || !strings.Contains(err.Error(), "WARDYN_KEY_DOMAINS_FILE") {
		t.Fatalf("Load of a missing file = %v; want a refusal naming the setting", err)
	}
}
