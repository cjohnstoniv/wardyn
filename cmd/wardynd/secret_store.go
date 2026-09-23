// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	secretstorepg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/vaultkv"
)

// openSecretStore builds the configured external store client (if any) and
// the secret store over it, as a serving boot and every maintenance mode do.
func openSecretStore(ctx context.Context, pool *pgxpool.Pool, f *bootFlags) (secretstore.Store, error) {
	ext, err := buildExternalStore(ctx, f.vault, *f.trustedCAFile)
	if err != nil {
		return nil, err
	}
	return buildSecretStore(ctx, pool, *f.ageKey, *f.secretStoreSel, ext)
}

// buildSecretStore constructs the secret store and readies its rows
// (convertSecretStore). The age identity comes from -age-key. In local mode
// ("pg") an empty key is generated and logged (operators MUST persist it
// across restarts to keep prior ciphertext readable). In store mode (an
// external store selected, design §2.3a.7) no key is generated: every value
// lives in the organisation's store, and a missing key only matters while
// local rows remain, which convertSecretStore refuses by name. ext is the
// configured external client, or nil.
func buildSecretStore(ctx context.Context, pool *pgxpool.Pool, ageKey, storeName string, ext secretstore.External) (secretstore.Store, error) {
	storeMode := storeName != "" && storeName != "pg"
	var id *age.X25519Identity
	var err error
	switch {
	case ageKey == "" && storeMode:
		// No local key at all: nothing to generate, nothing to warn about.
	case ageKey == "":
		id, err = age.GenerateX25519Identity()
		if err != nil {
			return nil, fmt.Errorf("generate age identity: %w", err)
		}
		// F10: log the PUBLIC recipient as a fingerprint, never the secret identity.
		// The old message printed the full AGE-SECRET-KEY- to a log file created at
		// the default umask (~/.wardyn/host-wardynd.log), leaking the secret-store
		// master key. To persist, mint one with `wardynd -gen-age-key` (prints to
		// stdout by design) and set WARDYN_AGE_KEY — do not copy it out of this log.
		slog.Warn("wardynd: generated ephemeral age identity; secrets are LOST on restart. Persist one with `wardynd -gen-age-key` + set WARDYN_AGE_KEY",
			slog.String("public_recipient", id.Recipient().String()),
		)
	default:
		if isKnownPublicAgeKey(ageKey) {
			return nil, fmt.Errorf("refusing to start: WARDYN_AGE_KEY is a publicly-known key (published in this repo's git history) — secrets encrypted under it are not protected; unset WARDYN_AGE_KEY to generate an ephemeral key, or mint your own with `wardynd -gen-age-key`")
		}
		id, err = age.ParseX25519Identity(ageKey)
		if err != nil {
			return nil, fmt.Errorf("parse age identity: %w", err)
		}
	}
	deps := secretstore.Deps{Pool: pool, External: ext}
	if id != nil {
		// A typed nil in the interface would read as "a key is configured".
		deps.AgeIdentity = id
	}
	s, err := secretstore.New(storeName, deps)
	if err != nil {
		return nil, fmt.Errorf("secret store: %w", err)
	}
	if err := convertSecretStore(ctx, s, id, ageKey == "" && !storeMode); err != nil {
		return nil, err
	}
	return s, nil
}

// convertSecretStore readies the pg store's rows before anything reads them —
// before loadOrCreateSecret above all, whose boot keys share the table. It
// converts every legacy (v0) row to envelope v1, aborting boot on one that will
// not decrypt; and it refuses an ephemeral age key while rows sealed under an
// age key exist, rather than let a fresh key strand them. In store mode with
// no age key (id nil) it refuses while any local row remains. An alternate
// backend keeps its own format and is left alone.
func convertSecretStore(ctx context.Context, s secretstore.Store, id *age.X25519Identity, ephemeral bool) error {
	ps, ok := s.(*secretstorepg.Store)
	if !ok {
		return nil
	}
	if id == nil {
		n, err := ps.LocalRows(ctx)
		if err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("refusing to start: WARDYN_AGE_KEY is unset, but %d stored secrets are still sealed under it — set it until `wardynd -migrate-secrets -to=%s` reports none left", n, s.Name())
		}
		return nil
	}
	if ephemeral {
		n, err := ps.LocalRows(ctx)
		if err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("refusing to start: WARDYN_AGE_KEY is unset, but %d stored secrets are sealed under an age key — an ephemeral key would make every one unreadable; set WARDYN_AGE_KEY to the key they were written with", n)
		}
		return nil
	}
	n, err := ps.ConvertV0(ctx, id)
	if err != nil {
		return fmt.Errorf("refusing to start: %w", err)
	}
	if n > 0 {
		slog.Info("wardynd: converted stored secrets to envelope v1; an older wardynd can no longer read them", slog.Int("secrets", n))
	}
	return nil
}

// vaultFlags configure the Vault KV v2 external store (docs/ENV.md). Addr
// empty means no Vault client at all. There is deliberately no token-in-env
// setting: the token comes from a projected service-account login or a file
// (design rule 20).
type vaultFlags struct {
	addr, namespace, auth, authMount, role, k8sTokenFile, tokenFile, caCertFile *string
	kvMount, kvPrefix                                                          *string
	maxVersions                                                                *int
	timeout                                                                    *time.Duration
}

func registerVaultFlags() vaultFlags {
	return vaultFlags{
		addr:         flagEnv("vault-addr", "WARDYN_VAULT_ADDR", "", "Vault (or OpenBao) address for the vaultkv secret store, https://; http:// only to a loopback host. Empty = no Vault client"),
		namespace:    flagEnv("vault-namespace", "WARDYN_VAULT_NAMESPACE", "", "Vault Enterprise/HCP namespace, sent as X-Vault-Namespace; empty = none"),
		auth:         flagEnv("vault-auth", "WARDYN_VAULT_AUTH", vaultkv.AuthKubernetes, `Vault auth method: "kubernetes" (a projected service-account token) or "token-file" (a Vault Agent sink or CSI file)`),
		authMount:    flagEnv("vault-auth-mount", "WARDYN_VAULT_AUTH_MOUNT", "kubernetes", "mount path of Vault's Kubernetes auth method"),
		role:         flagEnv("vault-role", "WARDYN_VAULT_ROLE", "", "Vault Kubernetes-auth role wardynd logs in as"),
		k8sTokenFile: flagEnv("vault-k8s-token-file", "WARDYN_VAULT_K8S_TOKEN_FILE", "", "path of the projected service-account token (audience vault) for Kubernetes auth; re-read at every login"),
		tokenFile:    flagEnv("vault-token-file", "WARDYN_VAULT_TOKEN_FILE", "", "path of a file holding a Vault token (WARDYN_VAULT_AUTH=token-file); re-read on every 403"),
		caCertFile:   flagEnv("vault-cacert-file", "WARDYN_VAULT_CACERT_FILE", "", "PEM bundle added to the system roots for the Vault client only; empty = WARDYN_TRUSTED_CA_FILE, else system roots"),
		kvMount:      flagEnv("vault-kv-mount", "WARDYN_VAULT_KV_MOUNT", "wardyn", "Vault KV v2 mount the vaultkv store writes under"),
		kvPrefix:     flagEnv("vault-kv-prefix", "WARDYN_VAULT_KV_PREFIX", "wardyn", "path prefix under the mount for this install (the chart sets the release namespace)"),
		maxVersions:  flagIntEnv("vault-kv-max-versions", "WARDYN_VAULT_KV_MAX_VERSIONS", 1, "max_versions set on each secret the vaultkv store creates (1: a replaced value does not linger)"),
		timeout:      flagDuration("secret-store-timeout", "WARDYN_SECRET_STORE_TIMEOUT", 5*time.Second, "timeout of each call to an external secret store"),
	}
}

// buildExternalStore returns the configured external store client, or nil
// when none is configured. Its token is kept alive for the life of ctx. A
// client that cannot log in fails boot (design K11).
func buildExternalStore(ctx context.Context, v vaultFlags, trustedCAFile string) (secretstore.External, error) {
	addr := strings.TrimSpace(*v.addr)
	if addr == "" {
		return nil, nil
	}
	ca := strings.TrimSpace(*v.caCertFile)
	if ca == "" {
		ca = strings.TrimSpace(trustedCAFile)
	}
	s, err := vaultkv.New(ctx, vaultkv.Config{
		Addr: addr, Namespace: strings.TrimSpace(*v.namespace),
		Auth: strings.TrimSpace(*v.auth), AuthMount: strings.TrimSpace(*v.authMount), Role: strings.TrimSpace(*v.role),
		K8sTokenFile: strings.TrimSpace(*v.k8sTokenFile), TokenFile: strings.TrimSpace(*v.tokenFile), CACertFile: ca,
		Mount: strings.TrimSpace(*v.kvMount), Prefix: strings.TrimSpace(*v.kvPrefix),
		MaxVersions: *v.maxVersions, Timeout: *v.timeout,
	})
	if err != nil {
		return nil, fmt.Errorf("refusing to start: %w", err)
	}
	return s, nil
}

// storesExternally describes the external store every write goes to, or ""
// in local mode (the STORE_EXTERNAL setup row).
func storesExternally(s secretstore.Store) string {
	if ps, ok := s.(*secretstorepg.Store); ok {
		return ps.StoresExternally()
	}
	return ""
}

// secretsDurable reports whether stored secrets survive a restart: a supplied
// age key, or store mode, where no local key holds anything.
func secretsDurable(ageKey string, s secretstore.Store) bool {
	return strings.TrimSpace(ageKey) != "" || storesExternally(s) != ""
}
