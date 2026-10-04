// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/azurekv"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/keydomain"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/vaultkv"
)

// buildKeyDomains builds the KEK of every domain WARDYN_KEY_DOMAINS_FILE
// declares, or returns nil when it declares none. Each is proven at boot the way
// the credential KEK is (vaultkv.NewDomainTransit, azurekv.NewKEK: a round trip,
// and a refusal to unwrap under another row's binding), so a domain whose key
// cannot be reached, or does not bind a wrap to its row, fails boot.
//
// Domains come from the file alone. A domain that names the same key as another
// domain, the platform key or the credential KEK separates nothing and is refused
// before any key service is called (checkKeyDomains).
func buildKeyDomains(ctx context.Context, v vaultFlags, az azureFlags, trustedCAFile string) (secretstore.KeyDomains, error) {
	file, err := keydomain.Load(*v.keyDomainsFile)
	if err != nil {
		return nil, fmt.Errorf("refusing to start: %w", err)
	}
	if len(file) == 0 {
		return nil, nil
	}
	if err := checkKeyDomains(file, v, az); err != nil {
		return nil, fmt.Errorf("refusing to start: %w", err)
	}
	out := secretstore.KeyDomains{}
	for _, name := range file.Names() {
		k, err := buildKeyDomainKEK(ctx, name, file[name], v, az, trustedCAFile)
		if err != nil {
			return nil, err
		}
		out[name] = k
	}
	return out, nil
}

func buildKeyDomainKEK(ctx context.Context, name string, e keydomain.Entry, v vaultFlags, az azureFlags, trustedCAFile string) (kek.KEK, error) {
	if e.Transit != nil {
		if strings.TrimSpace(*v.addr) == "" {
			return nil, fmt.Errorf("refusing to start: key domain %q names a Transit key but WARDYN_VAULT_ADDR is not set", name)
		}
		cfg := vaultConfig(v, trustedCAFile)
		// The domain's key is reached as its own role, or as the credential
		// role; never as the platform role.
		cfg.RolePlatform = ""
		if role := strings.TrimSpace(e.Transit.Role); role != "" {
			cfg.Role = role
		}
		t, err := vaultkv.NewDomainTransit(ctx, cfg, strings.TrimSpace(*v.transitMount), strings.TrimSpace(e.Transit.Key), name)
		if err != nil {
			return nil, fmt.Errorf("refusing to start: %w", err)
		}
		return t, nil
	}
	client := strings.TrimSpace(e.AzureKV.ClientID)
	if client == "" {
		client = strings.TrimSpace(*az.clientID)
	}
	where := fmt.Sprintf("key domain %q in WARDYN_KEY_DOMAINS_FILE", name)
	return newAzureKEK(ctx, v, az, trustedCAFile, azurekv.KEKConfig{
		Key: strings.TrimSpace(e.AzureKV.Key), SigningKey: strings.TrimSpace(e.AzureKV.SigningKey), ClientID: client,
		KeySetting: where + " key", SigningKeySetting: where + " signingKey",
	})
}

// checkKeyDomains refuses a domains file that separates nothing or cannot work:
//
//   - a domain naming the same key as another domain, the platform key or the
//     default credential KEK;
//   - a domain naming a Vault role while WARDYN_VAULT_AUTH is not "kubernetes"
//     (a token-file login ignores the role, as buildPlatformKEK refuses for the
//     platform key), or the same role as WARDYN_VAULT_ROLE or
//     WARDYN_VAULT_ROLE_PLATFORM.
//
// A domain that names no role is reached as the credential role. The process
// holds that role's token anyway; the separation of a role only defends a
// leaked token.
func checkKeyDomains(file keydomain.File, v vaultFlags, az azureFlags) error {
	mount := strings.TrimSpace(*v.transitMount)
	taken := map[string]string{} // key identity -> the setting that names it
	claim := func(identity, owner string) {
		if _, ok := taken[identity]; !ok {
			taken[identity] = owner
		}
	}
	for _, c := range []struct{ setting, key string }{
		{"WARDYN_VAULT_TRANSIT_KEY", *v.transitKey}, {"WARDYN_VAULT_TRANSIT_KEY_PLATFORM", *v.transitKeyPlatform},
	} {
		if key := strings.TrimSpace(c.key); key != "" {
			claim("transit:"+mount+"/"+key, c.setting)
		}
	}
	for _, c := range []struct{ setting, key string }{
		{"WARDYN_AZURE_KEK_KEY", *az.kekKey}, {"WARDYN_AZURE_KEK_SIGNING_KEY", *az.kekSigningKey},
		{"WARDYN_AZURE_KEK_KEY_PLATFORM", *az.kekKeyPlatform}, {"WARDYN_AZURE_KEK_SIGNING_KEY_PLATFORM", *az.kekSigningKeyPlatform},
	} {
		if key := strings.TrimSpace(c.key); key != "" {
			if id, err := azurekv.KeyIdentity(c.setting, key); err == nil {
				claim("azurekv:"+id, c.setting)
			}
		}
	}
	for _, name := range file.Names() {
		e := file[name]
		var identities []string
		switch {
		case e.Transit != nil:
			identities = append(identities, "transit:"+mount+"/"+strings.TrimSpace(e.Transit.Key))
			if err := checkDomainRole(name, strings.TrimSpace(e.Transit.Role), v); err != nil {
				return err
			}
		case e.AzureKV != nil:
			for _, k := range []struct{ field, key string }{{"key", e.AzureKV.Key}, {"signingKey", e.AzureKV.SigningKey}} {
				id, err := azurekv.KeyIdentity(fmt.Sprintf("key domain %q %s", name, k.field), strings.TrimSpace(k.key))
				if err != nil {
					return err
				}
				identities = append(identities, "azurekv:"+id)
			}
		}
		for _, id := range identities {
			if other, ok := taken[id]; ok {
				return fmt.Errorf("key domain %q names the same key as %s, which separates nothing; name a key of its own", name, other)
			}
		}
		for _, id := range identities {
			taken[id] = fmt.Sprintf("key domain %q", name)
		}
	}
	return nil
}

// checkDomainRole refuses a domain's Vault role that would be ignored or would
// separate nothing.
func checkDomainRole(domain, role string, v vaultFlags) error {
	if role == "" {
		return nil
	}
	if strings.TrimSpace(*v.auth) != vaultkv.AuthKubernetes {
		return fmt.Errorf("key domain %q names Vault role %q, but WARDYN_VAULT_AUTH is not %s: a token-file login ignores the role, so the domain's key would be reached with the credential token, which separates nothing", domain, role, vaultkv.AuthKubernetes)
	}
	switch role {
	case strings.TrimSpace(*v.role):
		return fmt.Errorf("key domain %q names the same Vault role as WARDYN_VAULT_ROLE, which separates nothing; name a role of its own, or none", domain)
	case strings.TrimSpace(*v.rolePlatform):
		return fmt.Errorf("key domain %q names the same Vault role as WARDYN_VAULT_ROLE_PLATFORM, which separates nothing; name a role of its own, or none", domain)
	}
	return nil
}
