// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package azurekv

import (
	"strings"
	"testing"
)

// A boot error names the settings the keys came from, so the platform pair
// (which has its own settings) is told about its own, never the credential
// pair's.
func TestNewKEK_RefusalsNameTheSettingsTheKeysCameFrom(t *testing.T) {
	const wrap = "https://myvault.vault.azure.net/keys/wrap"
	cases := []struct {
		name string
		cfg  KEKConfig
		want []string
	}{
		{
			"default settings, other vault",
			KEKConfig{Key: wrap, SigningKey: "https://other.vault.azure.net/keys/sign"},
			[]string{"WARDYN_AZURE_KEK_SIGNING_KEY is in other.vault.azure.net", "WARDYN_AZURE_KEK_KEY in myvault.vault.azure.net"},
		},
		{
			"named settings, other vault",
			KEKConfig{Key: wrap, SigningKey: "https://other.vault.azure.net/keys/sign", KeySetting: "PLAT_KEY", SigningKeySetting: "PLAT_SIGN"},
			[]string{"PLAT_SIGN is in other.vault.azure.net", "PLAT_KEY in myvault.vault.azure.net"},
		},
		{
			"named settings, same key spelled in two cases",
			KEKConfig{Key: wrap, SigningKey: "https://MYVAULT.vault.azure.net/keys/WRAP", KeySetting: "PLAT_KEY", SigningKeySetting: "PLAT_SIGN"},
			[]string{"PLAT_KEY and PLAT_SIGN name the same key"},
		},
		{
			"named setting on a malformed wrapping key",
			KEKConfig{Key: "https://myvault.vault.azure.net/secrets/wrap", SigningKey: wrap, KeySetting: "PLAT_KEY"},
			[]string{"PLAT_KEY", "is not a key identifier"},
		},
		{
			"named setting on a malformed signing key",
			KEKConfig{Key: wrap, SigningKey: "https://myvault.vault.azure.net/keys/sign/abc123", SigningKeySetting: "PLAT_SIGN"},
			[]string{"PLAT_SIGN", "names a key version"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newKEK(tc.cfg)
			if err == nil {
				t.Fatal("newKEK accepted the config")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not contain %q", err, w)
				}
			}
		})
	}
}

func TestKeyIdentity(t *testing.T) {
	a, err := KeyIdentity("S", "https://MyVault.vault.azure.net/keys/Wardyn-KEK")
	if err != nil {
		t.Fatal(err)
	}
	b, err := KeyIdentity("S", "https://myvault.VAULT.azure.net/keys/wardyn-kek")
	if err != nil {
		t.Fatal(err)
	}
	if a != "myvault.vault.azure.net/wardyn-kek" || a != b {
		t.Fatalf("identities %q and %q; want one lowercase vault/key identity for both spellings", a, b)
	}
	if other, _ := KeyIdentity("S", "https://myvault.vault.azure.net/keys/other"); other == a {
		t.Fatal("two different keys share an identity")
	}
	if _, err := KeyIdentity("MY_SETTING", "https://myvault.vault.azure.net/keys/a/version"); err == nil || !strings.Contains(err.Error(), "MY_SETTING") {
		t.Fatalf("a versioned id = %v, want a refusal naming the setting", err)
	}
}
