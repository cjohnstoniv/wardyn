// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/secretstore/keydomain"
)

// domainTestFlags is a Transit install: the credential key and role, a platform
// key and role, and a Key Vault credential pair beside them to compare against.
func domainTestFlags() (vaultFlags, azureFlags) {
	v := vaultFlags{
		addr: strp(""), auth: strp("kubernetes"), role: strp("wardyn-cred"), rolePlatform: strp("wardyn-platform"),
		transitMount: strp("transit"), transitKey: strp("cred-key"), transitKeyPlatform: strp("platform-key"), keyDomainsFile: strp(""),
	}
	az := azureFlags{
		kekKey: strp("https://v.vault.azure.net/keys/cred"), kekSigningKey: strp("https://v.vault.azure.net/keys/cred-sig"),
		kekKeyPlatform: strp("https://v.vault.azure.net/keys/plat"), kekSigningKeyPlatform: strp("https://v.vault.azure.net/keys/plat-sig"),
		clientID: strp(""), clientIDPlatform: strp(""),
	}
	return v, az
}

func parseDomains(t *testing.T, doc string) keydomain.File {
	t.Helper()
	f, err := keydomain.Parse([]byte(doc))
	if err != nil {
		t.Fatalf("parse %s: %v", doc, err)
	}
	return f
}

// Each boot refusal over the file alone fires, and names what it refused.
func TestCheckKeyDomains_Refusals(t *testing.T) {
	for name, tc := range map[string]struct {
		doc  string
		edit func(*vaultFlags, *azureFlags)
		want string
	}{
		"the same key as another domain": {
			doc:  `{"a": {"transit": {"key": "k1"}}, "b": {"transit": {"key": "k1"}}}`,
			want: `key domain "b" names the same key as key domain "a"`,
		},
		"the platform key": {
			doc:  `{"a": {"transit": {"key": "platform-key"}}}`,
			want: "WARDYN_VAULT_TRANSIT_KEY_PLATFORM",
		},
		"the default credential key": {
			doc:  `{"a": {"transit": {"key": "cred-key"}}}`,
			want: "WARDYN_VAULT_TRANSIT_KEY,",
		},
		"a Key Vault pair equal to the credential pair, in another case": {
			doc:  `{"a": {"azurekv": {"key": "https://V.vault.azure.net/keys/CRED", "signingKey": "https://v.vault.azure.net/keys/own-sig"}}}`,
			want: "WARDYN_AZURE_KEK_KEY",
		},
		"a Key Vault signing key equal to the platform signing key": {
			doc:  `{"a": {"azurekv": {"key": "https://v.vault.azure.net/keys/own", "signingKey": "https://v.vault.azure.net/keys/plat-sig"}}}`,
			want: "WARDYN_AZURE_KEK_SIGNING_KEY_PLATFORM",
		},
		"a Key Vault pair equal to another domain's": {
			doc: `{"a": {"azurekv": {"key": "https://v.vault.azure.net/keys/x", "signingKey": "https://v.vault.azure.net/keys/xs"}},
			        "b": {"azurekv": {"key": "https://v.vault.azure.net/keys/y", "signingKey": "https://v.vault.azure.net/keys/xs"}}}`,
			want: `key domain "b" names the same key as key domain "a"`,
		},
		"a Vault role under token-file auth": {
			doc:  `{"a": {"transit": {"key": "k1", "role": "own-role"}}}`,
			edit: func(v *vaultFlags, _ *azureFlags) { v.auth = strp("token-file") },
			want: "token-file login ignores the role",
		},
		"the credential Vault role": {
			doc:  `{"a": {"transit": {"key": "k1", "role": "wardyn-cred"}}}`,
			want: "same Vault role as WARDYN_VAULT_ROLE,",
		},
		"the platform Vault role": {
			doc:  `{"a": {"transit": {"key": "k1", "role": "wardyn-platform"}}}`,
			want: "same Vault role as WARDYN_VAULT_ROLE_PLATFORM",
		},
		"a malformed Key Vault key id": {
			doc:  `{"a": {"azurekv": {"key": "not-a-url", "signingKey": "https://v.vault.azure.net/keys/s"}}}`,
			want: `key domain "a" key`,
		},
	} {
		v, az := domainTestFlags()
		if tc.edit != nil {
			tc.edit(&v, &az)
		}
		err := checkKeyDomains(parseDomains(t, tc.doc), v, az)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: checkKeyDomains = %v; want a refusal containing %q", name, err, tc.want)
		}
	}
}

// Domains of their own key, a role of their own under Kubernetes auth, or no
// role at all, pass.
func TestCheckKeyDomains_Accepts(t *testing.T) {
	v, az := domainTestFlags()
	doc := `{
		"a": {"transit": {"key": "a-key", "role": "wardyn-a"}},
		"b": {"transit": {"key": "b-key"}},
		"c": {"azurekv": {"key": "https://v.vault.azure.net/keys/c", "signingKey": "https://v.vault.azure.net/keys/c-sig", "clientId": "x"}}
	}`
	if err := checkKeyDomains(parseDomains(t, doc), v, az); err != nil {
		t.Fatalf("checkKeyDomains = %v; want accepted", err)
	}
	// A domain role under a deployment that names no credential role is still a role of its own.
	v.role = strp("")
	if err := checkKeyDomains(parseDomains(t, `{"a": {"transit": {"key": "a-key", "role": "wardyn-a"}}}`), v, az); err != nil {
		t.Fatalf("a domain role beside no credential role = %v; want accepted", err)
	}
}

// A domains file that is malformed, or a domain with no key service to reach it,
// refuses boot before any key service is called.
func TestBuildKeyDomains_BootRefusals(t *testing.T) {
	dir := t.TempDir()
	write := func(name, doc string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for name, tc := range map[string]struct{ file, want string }{
		"a domain named default":      {write("d.json", `{"default": {"transit": {"key": "k"}}}`), `named "default"`},
		"an invalid name":             {write("n.json", `{"Bad_Name": {"transit": {"key": "k"}}}`), "a-z, 0-9"},
		"an empty name":               {write("e.json", `{"": {"transit": {"key": "k"}}}`), "empty"},
		"a missing file":              {filepath.Join(dir, "none.json"), "WARDYN_KEY_DOMAINS_FILE"},
		"a Transit key with no Vault": {write("t.json", `{"a": {"transit": {"key": "k"}}}`), "WARDYN_VAULT_ADDR is not set"},
		"a shared key":                {write("s.json", `{"a": {"transit": {"key": "cred-key"}}}`), "separates nothing"},
	} {
		v, az := domainTestFlags()
		v.keyDomainsFile = strp(tc.file)
		_, err := buildKeyDomains(t.Context(), v, az, "")
		if err == nil || !strings.Contains(err.Error(), "refusing to start") || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: buildKeyDomains = %v; want a boot refusal containing %q", name, err, tc.want)
		}
	}
	v, az := domainTestFlags()
	if got, err := buildKeyDomains(t.Context(), v, az, ""); err != nil || got != nil {
		t.Fatalf("no file = (%v, %v); want no domains", got, err)
	}
}
