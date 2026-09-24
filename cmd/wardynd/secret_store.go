// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/azurekv"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	secretstorepg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/vaultkv"
)

// openSecretStore builds the configured external store client (if any) and
// the audited secret store over it (buildSecretStore), as a serving boot does.
func openSecretStore(ctx context.Context, pool *pgxpool.Pool, f *bootFlags, rec audit.Recorder) (secretstore.Store, error) {
	c, err := buildStoreClients(ctx, f)
	if err != nil {
		return nil, err
	}
	return buildSecretStore(ctx, pool, *f.ageKey, *f.secretStoreSel, c, rec)
}

// buildStoreClients builds the configured external store client and key
// service, as both a serving boot and a maintenance mode open them.
func buildStoreClients(ctx context.Context, f *bootFlags) (storeClients, error) {
	ext, err := buildExternalStore(ctx, f.vault, f.azure, *f.trustedCAFile)
	if err != nil {
		return storeClients{}, err
	}
	k, writes, err := buildKEK(ctx, f.vault, *f.trustedCAFile)
	if err != nil {
		return storeClients{}, err
	}
	return storeClients{ext: ext, kek: k, kekWrites: writes, timeout: *f.vault.timeout}, nil
}

// storeClients are the configured clients a secret store is built over.
type storeClients struct {
	// ext is the external store client, or nil.
	ext secretstore.External
	// kek is the key service (Vault Transit), or nil; kekWrites says whether
	// it wraps new rows (WARDYN_KEK=transit) or only reads its own.
	kek       kek.KEK
	kekWrites bool
	// timeout bounds each call to ext.
	timeout time.Duration
}

// buildSecretStore is newSecretStore wrapped in secretstore.Audited, in every
// store mode, so every read through it is recorded once on rec, whatever the
// backend holding the value.
func buildSecretStore(ctx context.Context, pool *pgxpool.Pool, ageKey, storeName string, c storeClients, rec audit.Recorder) (secretstore.Store, error) {
	s, err := newSecretStore(ctx, pool, ageKey, storeName, c, rec)
	if err != nil {
		return nil, err
	}
	return secretstore.Audited(s, rec), nil
}

// newSecretStore constructs the secret store over the clients c and readies
// its rows (convertSecretStore), whose reads it records on rec. The age
// identity comes from -age-key. In local mode ("pg") an empty key is generated
// and logged (operators MUST persist it across restarts to keep prior
// ciphertext readable). In store mode (an external store selected, design
// §2.3a.7), or with a key service that wraps new rows, no key is generated: a
// missing key only matters while local rows remain, which convertSecretStore
// refuses by name. The store is returned unwrapped: a serving boot reads
// through buildSecretStore, and only the maintenance modes use it bare, for
// the pg store's Migrate, Reconcile and Rewrap, which never Get
// (secretStoreMaintenance).
func newSecretStore(ctx context.Context, pool *pgxpool.Pool, ageKey, storeName string, c storeClients, rec audit.Recorder) (secretstore.Store, error) {
	storeMode := storeName != "" && storeName != "pg"
	keyService := c.kek != nil && c.kekWrites
	var id *age.X25519Identity
	var err error
	switch {
	case ageKey == "" && (storeMode || keyService):
		// No local key at all: nothing to generate, nothing to warn about.
		// Every write goes to the store or is wrapped by the key service.
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
	deps := secretstore.Deps{Pool: pool, External: c.ext, ExternalTimeout: c.timeout, KEK: c.kek, KEKWrites: c.kekWrites}
	if id != nil {
		// A typed nil in the interface would read as "a key is configured".
		deps.AgeIdentity = id
	}
	s, err := secretstore.New(storeName, deps)
	if err != nil {
		return nil, fmt.Errorf("secret store: %w", err)
	}
	if err := convertSecretStore(ctx, s, id, ageKey == "" && !storeMode && !keyService, rec); err != nil {
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
// backend keeps its own format and is left alone. Each converted row was a
// read of its value, recorded as a secret.read with purpose migrate.
func convertSecretStore(ctx context.Context, s secretstore.Store, id *age.X25519Identity, ephemeral bool, rec audit.Recorder) error {
	ps, ok := s.(*secretstorepg.Store)
	if !ok {
		return nil
	}
	if id == nil {
		n, err := ps.LocalRows(ctx)
		if err != nil {
			return err
		}
		way := "`wardynd -rewrap`"
		if s.Name() != "pg" {
			way = "`wardynd -migrate-secrets -to=" + s.Name() + "`"
		}
		if n > 0 {
			return fmt.Errorf("refusing to start: WARDYN_AGE_KEY is unset, but %d stored secrets are still sealed under it — set it until %s reports none left", n, way)
		}
		return nil
	}
	if ephemeral {
		n, err := ps.LocalRows(ctx)
		if err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("refusing to start: WARDYN_AGE_KEY is unset, but %d stored secrets are sealed under an age key — "+
				"an ephemeral key would make every one unreadable; set WARDYN_AGE_KEY to the key they were written with. "+
				"If they were written under an earlier ephemeral key, no such key exists and they are unrecoverable: "+
				"delete them (DELETE FROM secrets WHERE enc_version=0 OR kek_id LIKE 'local:%%') and boot with a persistent key from `wardynd -gen-age-key`", n)
		}
		return nil
	}
	converted, err := ps.ConvertV0(secretstore.WithPurpose(ctx, secretstore.PurposeMigrate), id)
	if err != nil {
		return fmt.Errorf("refusing to start: %w", err)
	}
	for _, row := range converted {
		secretstore.RecordRead(ctx, rec, secretstore.PurposeMigrate, row.Owner, row, nil)
	}
	if len(converted) > 0 {
		slog.Info("wardynd: converted stored secrets to envelope v1; an older wardynd can no longer read them", slog.Int("secrets", len(converted)))
	}
	return nil
}

// vaultFlags configure the Vault KV v2 external store (docs/ENV.md). Addr
// empty means no Vault client at all. There is deliberately no token-in-env
// setting: the token comes from a projected service-account login or a file
// (design rule 20).
type vaultFlags struct {
	addr, namespace, auth, authMount, role, k8sTokenFile, tokenFile, caCertFile *string
	kvMount, kvPrefix                                                           *string
	maxVersions                                                                 *int
	timeout                                                                     *time.Duration
	// kek is WARDYN_KEK; transitMount and transitKey name the Transit key it
	// selects (or, with kek=local, reads).
	kek, transitMount, transitKey *string
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
		kek:          flagEnv("kek", "WARDYN_KEK", kekLocal, `key that wraps each stored secret's data key: "local" (derived from WARDYN_AGE_KEY) or "transit" (the Vault Transit key WARDYN_VAULT_TRANSIT_KEY names, over the WARDYN_VAULT_* client)`),
		transitMount: flagEnv("vault-transit-mount", "WARDYN_VAULT_TRANSIT_MOUNT", "transit", "mount path of Vault's Transit engine"),
		transitKey:   flagEnv("vault-transit-key", "WARDYN_VAULT_TRANSIT_KEY", "", "Transit key (type aes256-gcm96) that wraps data keys with WARDYN_KEK=transit; set with WARDYN_KEK=local it only reads the rows sealed under it, for `wardynd -rewrap` back to the local key"),
	}
}

// azureFlags configure the Azure Key Vault external store (docs/ENV.md).
// VaultURL empty means no Key Vault client at all.
type azureFlags struct {
	vaultURL, auth, tenantID, clientID, federatedTokenFile, authorityHost, prefix, purge *string
	maxVersions                                                                          *int
}

func registerAzureFlags() azureFlags {
	return azureFlags{
		vaultURL:           flagEnv("azure-kv-url", "WARDYN_AZURE_KV_URL", "", "Azure Key Vault base URL for the azurekv secret store (https://<vault>.vault.azure.net). Empty = no Key Vault client"),
		auth:               flagEnv("azure-auth", "WARDYN_AZURE_AUTH", azurekv.AuthWorkloadIdentity, `how wardynd gets its Entra token: "workload-identity" (a federated projected token) or "managed-identity" (a VM's instance metadata service)`),
		tenantID:           flagEnv("azure-tenant-id", "WARDYN_AZURE_TENANT_ID", "", "Entra tenant id (workload identity)"),
		clientID:           flagEnv("azure-client-id", "WARDYN_AZURE_CLIENT_ID", "", "client id of the app registration or user-assigned identity wardynd runs as"),
		federatedTokenFile: flagEnv("azure-federated-token-file", "WARDYN_AZURE_FEDERATED_TOKEN_FILE", os.Getenv("AZURE_FEDERATED_TOKEN_FILE"), "path of the projected token exchanged for an Entra token; default: AZURE_FEDERATED_TOKEN_FILE, which the workload identity webhook sets. Re-read at every exchange"),
		authorityHost:      flagEnv("azure-authority-host", "WARDYN_AZURE_AUTHORITY_HOST", "https://login.microsoftonline.com", "Entra authority host (a sovereign cloud's, if not the public one)"),
		prefix:             flagEnv("azure-kv-prefix", "WARDYN_AZURE_KV_PREFIX", "wardyn", "prefix of every secret name this install writes (the chart sets the release namespace)"),
		maxVersions:        flagIntEnv("azure-kv-max-versions", "WARDYN_AZURE_KV_MAX_VERSIONS", 100, "versions a secret holds before the next write starts a new name and deletes the old one"),
		purge:              flagEnv("azure-kv-purge", "WARDYN_AZURE_KV_PURGE", azurekv.PurgeAuto, `"auto": purge a deleted secret when the vault allows it; "never": leave it soft-deleted for the vault's retention`),
	}
}

// KEK providers (WARDYN_KEK).
const (
	kekLocal   = "local"
	kekTransit = "transit"
)

// buildKEK returns the configured key service, or nil, and whether it wraps
// new rows. WARDYN_KEK=transit writes under the Vault Transit key; with
// WARDYN_KEK=local a named Transit key is built read-only, so the rows sealed
// under it stay readable while `wardynd -rewrap` moves them back. The key is
// proven at boot (vaultkv.NewTransit): a key service that cannot be reached,
// or that does not bind a wrap to its row, fails boot (design K6).
func buildKEK(ctx context.Context, v vaultFlags, trustedCAFile string) (kek.KEK, bool, error) {
	sel := strings.TrimSpace(*v.kek)
	key := strings.TrimSpace(*v.transitKey)
	switch sel {
	case "", kekLocal:
		if key == "" {
			return nil, false, nil
		}
	case kekTransit:
		if key == "" {
			return nil, false, fmt.Errorf("refusing to start: WARDYN_KEK=transit needs WARDYN_VAULT_TRANSIT_KEY")
		}
	default:
		return nil, false, fmt.Errorf("refusing to start: WARDYN_KEK is %q; want %q or %q", sel, kekLocal, kekTransit)
	}
	if strings.TrimSpace(*v.addr) == "" {
		return nil, false, fmt.Errorf("refusing to start: WARDYN_VAULT_TRANSIT_KEY is set but WARDYN_VAULT_ADDR is not")
	}
	t, err := vaultkv.NewTransit(ctx, vaultConfig(v, trustedCAFile), strings.TrimSpace(*v.transitMount), key)
	if err != nil {
		return nil, false, fmt.Errorf("refusing to start: %w", err)
	}
	return t, sel == kekTransit, nil
}

// vaultConfig is the Vault client configuration both the KV store and the
// Transit KEK use.
func vaultConfig(v vaultFlags, trustedCAFile string) vaultkv.Config {
	ca := strings.TrimSpace(*v.caCertFile)
	if ca == "" {
		ca = strings.TrimSpace(trustedCAFile)
	}
	return vaultkv.Config{
		Addr: strings.TrimSpace(*v.addr), Namespace: strings.TrimSpace(*v.namespace),
		Auth: strings.TrimSpace(*v.auth), AuthMount: strings.TrimSpace(*v.authMount), Role: strings.TrimSpace(*v.role),
		K8sTokenFile: strings.TrimSpace(*v.k8sTokenFile), TokenFile: strings.TrimSpace(*v.tokenFile), CACertFile: ca,
		Mount: strings.TrimSpace(*v.kvMount), Prefix: strings.TrimSpace(*v.kvPrefix),
		MaxVersions: *v.maxVersions, Timeout: *v.timeout,
	}
}

// buildExternalStore returns the configured external store client, or nil
// when none is configured. Its token is kept alive for the life of ctx. A
// client that cannot log in fails boot (design K11). At most one external
// store is configured: a pointer row names one, and a migration between two
// goes through local.
func buildExternalStore(ctx context.Context, v vaultFlags, az azureFlags, trustedCAFile string) (secretstore.External, error) {
	addr := strings.TrimSpace(*v.addr)
	kvURL := strings.TrimSpace(*az.vaultURL)
	switch {
	case addr != "" && kvURL != "":
		return nil, fmt.Errorf("refusing to start: both WARDYN_VAULT_ADDR and WARDYN_AZURE_KV_URL are set; configure one external secret store")
	case kvURL != "":
		s, err := azurekv.New(ctx, azurekv.Config{
			VaultURL: kvURL, Auth: strings.TrimSpace(*az.auth), TenantID: strings.TrimSpace(*az.tenantID),
			ClientID: strings.TrimSpace(*az.clientID), FederatedTokenFile: strings.TrimSpace(*az.federatedTokenFile),
			AuthorityHost: strings.TrimSpace(*az.authorityHost), CACertFile: strings.TrimSpace(trustedCAFile),
			Prefix: strings.TrimSpace(*az.prefix), MaxVersions: *az.maxVersions, Purge: strings.TrimSpace(*az.purge),
			Timeout: *v.timeout,
		})
		if err != nil {
			return nil, fmt.Errorf("refusing to start: %w", err)
		}
		return s, nil
	case addr == "":
		return nil, nil
	}
	s, err := vaultkv.New(ctx, vaultConfig(v, trustedCAFile))
	if err != nil {
		return nil, fmt.Errorf("refusing to start: %w", err)
	}
	return s, nil
}

// storesExternally describes the external store every write goes to, or ""
// in local mode (the STORE_EXTERNAL setup row). The audited store forwards it.
func storesExternally(s secretstore.Store) string {
	if d, ok := s.(interface{ StoresExternally() string }); ok {
		return d.StoresExternally()
	}
	return ""
}

// keyService describes the key service that wraps every write ("Vault
// Transit at vault.example:8200"), or "" when the local key does (the
// kek_service setup row). The audited store forwards it.
func keyService(s secretstore.Store) string {
	if d, ok := s.(interface{ KeyService() string }); ok {
		return d.KeyService()
	}
	return ""
}

// secretsDurable reports whether stored secrets survive a restart: a supplied
// age key, store mode or a key service, where no local key holds anything.
func secretsDurable(ageKey string, s secretstore.Store) bool {
	return strings.TrimSpace(ageKey) != "" || storesExternally(s) != "" || keyService(s) != ""
}
