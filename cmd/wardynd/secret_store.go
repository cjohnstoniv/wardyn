// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/cliutil"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/azurekv"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	secretstorepg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/vaultkv"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// openSecretStore builds the configured external store client (if any) and
// the audited secret store over it (buildSecretStore), as a serving boot does.
func openSecretStore(ctx context.Context, pool *pgxpool.Pool, f *bootFlags, rec audit.Recorder) (secretstore.Store, error) {
	platform, err := readPlatformKey(*f.platformKeyFile, *f.ageKey)
	if err != nil {
		return nil, err
	}
	c, err := buildStoreClients(ctx, f)
	if err != nil {
		return nil, err
	}
	st, err := buildSecretStore(ctx, pool, *f.ageKey, platform, *f.secretStoreSel, c, rec)
	if err != nil {
		return nil, err
	}
	if err := refuseKEKRequired(*f.vault.kekRequired, st); err != nil {
		return nil, err
	}
	if err := sweepRetiredModelCredentials(ctx, st, rec); err != nil {
		return nil, err
	}
	// The Azure DevOps sweep names its hosts from the provider rows, which
	// migration 0103 has already rewritten (their addresses are kept).
	sc, err := store.NewPG(pool).GetSiteConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("refusing to start: read the site config for the Azure DevOps credential sweep: %w", err)
	}
	return st, sweepRetiredADOSharedCredentials(ctx, pool, st, sc, rec)
}

// buildStoreClients builds the configured external store client and key
// service, as both a serving boot and a maintenance mode open them.
func buildStoreClients(ctx context.Context, f *bootFlags) (storeClients, error) {
	ext, err := buildExternalStore(ctx, f.vault, f.azure, *f.trustedCAFile)
	if err != nil {
		return storeClients{}, err
	}
	k, writes, err := buildKEK(ctx, f.vault, f.azure, *f.trustedCAFile)
	if err != nil {
		return storeClients{}, err
	}
	pk, err := buildPlatformKEK(ctx, f.vault, *f.trustedCAFile, false)
	if err != nil {
		return storeClients{}, err
	}
	return storeClients{ext: ext, kek: k, kekWrites: writes, platformKEK: pk, timeout: *f.vault.timeout}, nil
}

// storeClients are the configured clients a secret store is built over.
type storeClients struct {
	// ext is the external store client, or nil.
	ext secretstore.External
	// kek is the key service (Vault Transit, Key Vault), or nil; kekWrites
	// says whether it wraps new rows (WARDYN_KEK=transit or azurekv) or only
	// reads its own.
	kek       kek.KEK
	kekWrites bool
	// platformKEK is the second key service that wraps the boot keys alone
	// (WARDYN_VAULT_TRANSIT_KEY_PLATFORM), or nil.
	platformKEK kek.KEK
	// timeout bounds each call to ext.
	timeout time.Duration
}

// readPlatformKey parses WARDYN_PLATFORM_KEY_FILE (design §2.13 c), or returns
// nil when it is unset. It is a second age identity for the boot keys alone,
// so it must be a durable key of its own: it is refused without a
// WARDYN_AGE_KEY (store mode keeps the boot keys in the organisation's store;
// an ephemeral age key would strand the rows beside them), when it is the age
// key itself, or when it is a published one.
func readPlatformKey(path, ageKey string) (*age.X25519Identity, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, nil
	}
	if strings.TrimSpace(ageKey) == "" {
		return nil, fmt.Errorf("refusing to start: WARDYN_PLATFORM_KEY_FILE is set but WARDYN_AGE_KEY is not — the platform key separates the boot keys from a durable age key; in store mode they already live in the organisation's store, so unset it")
	}
	b, err := cliutil.ReadSecretFile("WARDYN_PLATFORM_KEY_FILE", path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("refusing to start: WARDYN_PLATFORM_KEY_FILE: %w", err)
	}
	var key string
	if err == nil {
		if key, err = parseAgeKeyFileContent(path, b); err != nil {
			return nil, fmt.Errorf("refusing to start: WARDYN_PLATFORM_KEY_FILE: %w", err)
		}
	}
	if key == "" {
		return nil, fmt.Errorf("refusing to start: WARDYN_PLATFORM_KEY_FILE %s does not exist or holds no age identity — mint one with `wardynd -gen-age-key`", path)
	}
	if isKnownPublicAgeKey(key) {
		return nil, fmt.Errorf("refusing to start: WARDYN_PLATFORM_KEY_FILE holds a publicly-known key (published in this repo's git history); mint your own with `wardynd -gen-age-key`")
	}
	id, err := age.ParseX25519Identity(key)
	if err != nil {
		return nil, fmt.Errorf("refusing to start: WARDYN_PLATFORM_KEY_FILE: %w", err)
	}
	if ageID, err := age.ParseX25519Identity(strings.TrimSpace(ageKey)); err == nil && ageID.String() == id.String() {
		return nil, fmt.Errorf("refusing to start: WARDYN_PLATFORM_KEY_FILE holds the same key as WARDYN_AGE_KEY, which separates nothing; mint a second one with `wardynd -gen-age-key`")
	}
	return id, nil
}

// buildSecretStore is newSecretStore wrapped in secretstore.Audited, in every
// store mode, so every read through it is recorded once on rec, whatever the
// backend holding the value.
func buildSecretStore(ctx context.Context, pool *pgxpool.Pool, ageKey string, platform *age.X25519Identity, storeName string, c storeClients, rec audit.Recorder) (secretstore.Store, error) {
	s, err := newSecretStore(ctx, pool, ageKey, platform, storeName, c, rec)
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
// refuses by name. platform is the separate platform identity
// (readPlatformKey), or nil. The store is returned unwrapped: a serving boot
// reads through buildSecretStore, and only the maintenance modes use it bare,
// for the pg store's Migrate and Reconcile, which never Get
// (secretStoreMaintenance).
func newSecretStore(ctx context.Context, pool *pgxpool.Pool, ageKey string, platform *age.X25519Identity, storeName string, c storeClients, rec audit.Recorder) (secretstore.Store, error) {
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
	// Only a platform key that is actually set: a typed nil reads as a key.
	if c.platformKEK != nil {
		deps.PlatformKEK, deps.PlatformKEKWrites = c.platformKEK, true
	}
	// A typed nil in the interface would read as "a key is configured".
	if id != nil {
		deps.AgeIdentity = id
	}
	if platform != nil {
		deps.PlatformIdentity = platform
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

// ephemeralKeyRecoverySQL deletes every row sealed under a local key: the way
// out when the age key they were written under was ephemeral and is gone. Its
// local/platform: rows under a separate WARDYN_PLATFORM_KEY_FILE key are still
// recoverable with that file (docs/OPERATIONS.md says how to keep them). The
// refusal below names it, and docs/OPERATIONS.md carries it verbatim.
const ephemeralKeyRecoverySQL = "DELETE FROM secrets WHERE enc_version=0 OR kek_id LIKE 'local:%' OR kek_id LIKE 'local/%'"

// convertSecretStore readies the pg store's rows before anything reads them —
// before loadOrCreateSecret above all, whose boot keys share the table. It
// converts every legacy (v0) row to envelope v1, aborting boot on one that will
// not decrypt; and it refuses an ephemeral age key while rows sealed under an
// age key exist, rather than let a fresh key strand them. In store mode with
// no age key (id nil) it refuses while any local row remains; with a key
// service that wraps every write, it refuses an age key no row is sealed
// under any more (refuseIdleAgeKey). An alternate
// backend keeps its own format and is left alone. Each row the conversion
// opened was a read of its value, recorded as a secret.read with purpose boot
// (outcome failure for every row an aborted conversion opened, including the
// failing row when it decrypted before its seal or update failed).
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
				"delete them (%s) and boot with a persistent key from `wardynd -gen-age-key`", n, ephemeralKeyRecoverySQL)
		}
		return nil
	}
	converted, err := ps.ConvertV0(secretstore.WithPurpose(ctx, secretstore.PurposeBoot), id)
	// An aborted conversion still opened the rows it returns: each is a read
	// with outcome failure, since nothing it did was committed.
	for _, row := range converted {
		secretstore.RecordRead(ctx, rec, secretstore.PurposeBoot, row.Owner, row, err)
	}
	if err != nil {
		return fmt.Errorf("refusing to start: %w", err)
	}
	if len(converted) > 0 {
		slog.Info("wardynd: converted stored secrets to envelope v1; an older wardynd can no longer read them", slog.Int("secrets", len(converted)))
	}
	return refuseIdleAgeKey(ctx, ps)
}

// refuseKEKRequired refuses a serving boot when WARDYN_KEK_REQUIRED is set and
// neither a key service wraps the credentials nor an external store holds them,
// so the local key (set or ephemeral) would. It sits in the serving path, not in
// newSecretStore, so `wardynd -rewrap`, the remedy, still runs.
func refuseKEKRequired(required bool, s secretstore.Store) error {
	if !required || keyService(s) != "" || storesExternally(s) != "" {
		return nil
	}
	return fmt.Errorf("refusing to start: WARDYN_KEK_REQUIRED is set but credentials are wrapped by the local key — set WARDYN_KEK=transit|azurekv and run `wardynd -rewrap` to move them onto it")
}

// refuseIdleAgeKey refuses boot when a key service wraps every write
// (WARDYN_KEK=transit or azurekv) and WARDYN_AGE_KEY is set with no stored row left under
// it: the key then only lets whoever also holds the database forge a row the
// store still reads under it — Wardyn's boot keys among them.
func refuseIdleAgeKey(ctx context.Context, ps *secretstorepg.Store) error {
	if ps.KeyService() == "" {
		return nil
	}
	n, err := ps.LocalRows(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("refusing to start: every write is wrapped by %s and no stored secret is sealed under WARDYN_AGE_KEY — unset it; while it is set, whoever holds it and the database can forge Wardyn's boot keys", ps.KeyService())
	}
	return nil
}

// vaultFlags configure the Vault KV v2 external store (docs/ENV.md). Addr
// empty means no Vault client at all. There is deliberately no token-in-env
// setting: the token comes from a projected service-account login or a file
// (design rule 20).
type vaultFlags struct {
	addr, namespace, auth, authMount, role, rolePlatform, k8sTokenFile, tokenFile, caCertFile *string
	kvMount, kvPrefix                                                                         *string
	maxVersions                                                                               *int
	timeout                                                                                   *time.Duration
	// kek is WARDYN_KEK; transitMount and transitKey name the Transit key it
	// selects (or, with kek=local, reads).
	kek, transitMount, transitKey *string
	// kekRequired is WARDYN_KEK_REQUIRED: refuse a serving boot on the local key.
	kekRequired *bool
	// transitKeyPlatform is the second Transit key the boot keys alone are
	// wrapped under, reached as rolePlatform.
	transitKeyPlatform *string
}

func registerVaultFlags() vaultFlags {
	return vaultFlags{
		addr:               flagEnv("vault-addr", "WARDYN_VAULT_ADDR", "", "Vault (or OpenBao) address for the vaultkv secret store, https://; http:// only to a loopback host. Empty = no Vault client"),
		namespace:          flagEnv("vault-namespace", "WARDYN_VAULT_NAMESPACE", "", "Vault Enterprise/HCP namespace, sent as X-Vault-Namespace; empty = none"),
		auth:               flagEnv("vault-auth", "WARDYN_VAULT_AUTH", vaultkv.AuthKubernetes, `Vault auth method: "kubernetes" (a projected service-account token) or "token-file" (a Vault Agent sink or CSI file)`),
		authMount:          flagEnv("vault-auth-mount", "WARDYN_VAULT_AUTH_MOUNT", "kubernetes", "mount path of Vault's Kubernetes auth method"),
		role:               flagEnv("vault-role", "WARDYN_VAULT_ROLE", "", "Vault Kubernetes-auth role wardynd logs in as"),
		rolePlatform:       flagEnv("vault-role-platform", "WARDYN_VAULT_ROLE_PLATFORM", "", "optional second Kubernetes-auth role wardynd reads and writes its own signing, session and SSH host keys as (the <prefix>/platform/ paths); WARDYN_VAULT_ROLE then serves only the credentials. Empty = one role for both. Recommended; see docs/operations/secrets-and-keys.md"),
		k8sTokenFile:       flagEnv("vault-k8s-token-file", "WARDYN_VAULT_K8S_TOKEN_FILE", "", "path of the projected service-account token (audience vault) for Kubernetes auth; re-read at every login"),
		tokenFile:          flagEnv("vault-token-file", "WARDYN_VAULT_TOKEN_FILE", "", "path of a file holding a Vault token (WARDYN_VAULT_AUTH=token-file); re-read on every 403"),
		caCertFile:         flagEnv("vault-cacert-file", "WARDYN_VAULT_CACERT_FILE", "", "PEM bundle added to the system roots for the Vault client only; empty = WARDYN_TRUSTED_CA_FILE, else system roots"),
		kvMount:            flagEnv("vault-kv-mount", "WARDYN_VAULT_KV_MOUNT", "wardyn", "Vault KV v2 mount the vaultkv store writes under"),
		kvPrefix:           flagEnv("vault-kv-prefix", "WARDYN_VAULT_KV_PREFIX", "wardyn", "path prefix under the mount for this install (the chart sets the release namespace)"),
		maxVersions:        flagIntEnv("vault-kv-max-versions", "WARDYN_VAULT_KV_MAX_VERSIONS", 1, "max_versions set on each secret the vaultkv store creates (1: a replaced value does not linger)"),
		timeout:            flagDuration("secret-store-timeout", "WARDYN_SECRET_STORE_TIMEOUT", 5*time.Second, "timeout of each call to an external secret store"),
		kek:                flagEnv("kek", "WARDYN_KEK", kekLocal, `key that wraps each stored secret's data key: "local" (derived from WARDYN_AGE_KEY), "transit" (the Vault Transit key WARDYN_VAULT_TRANSIT_KEY names, over the WARDYN_VAULT_* client) or "azurekv" (the Key Vault keys WARDYN_AZURE_KEK_KEY and WARDYN_AZURE_KEK_SIGNING_KEY name, as the WARDYN_AZURE_* identity)`),
		kekRequired:        flagBool("kek-required", "WARDYN_KEK_REQUIRED", false, "refuse to start while credentials are wrapped by the local key: a key service (WARDYN_KEK=transit|azurekv) or an external store must hold them. `wardynd -rewrap` still runs"),
		transitMount:       flagEnv("vault-transit-mount", "WARDYN_VAULT_TRANSIT_MOUNT", "transit", "mount path of Vault's Transit engine"),
		transitKey:         flagEnv("vault-transit-key", "WARDYN_VAULT_TRANSIT_KEY", "", "Transit key (type aes256-gcm96) that wraps data keys with WARDYN_KEK=transit; set with WARDYN_KEK=local it only reads the rows sealed under it, for `wardynd -rewrap` back to the local key"),
		transitKeyPlatform: flagEnv("vault-transit-key-platform", "WARDYN_VAULT_TRANSIT_KEY_PLATFORM", "", "second Transit key (type aes256-gcm96, same mount) that wraps wardynd's own signing, session and SSH host keys, reached as WARDYN_VAULT_ROLE_PLATFORM; WARDYN_VAULT_TRANSIT_KEY then wraps only the credentials. Needs WARDYN_KEK=transit and WARDYN_VAULT_ROLE_PLATFORM; `wardynd -rewrap -rewrap-adopt-boot-keys` moves the boot keys onto it, once; `-rewrap -rewrap-retire-platform-key` moves them back off. Empty = one key for both. See docs/operations/secrets-and-keys.md"),
	}
}

// azureFlags configure the Azure Key Vault external store and the Key Vault
// KEK (docs/ENV.md), which share the identity settings. VaultURL empty means
// no Key Vault store client; kekKey empty, no Key Vault KEK.
type azureFlags struct {
	vaultURL, auth, tenantID, clientID, federatedTokenFile, authorityHost, prefix, purge *string
	maxVersions                                                                          *int
	kekKey, kekSigningKey                                                                *string
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
		kekKey:             flagEnv("azure-kek-key", "WARDYN_AZURE_KEK_KEY", "", "versionless id (https://<vault>.vault.azure.net/keys/<name>) of the Key Vault RSA 3072/4096 key, key_ops wrapKey and unwrapKey only, not exportable, that wraps data keys with WARDYN_KEK=azurekv; set with WARDYN_KEK=local it only reads the rows sealed under it, for `wardynd -rewrap` back to the local key"),
		kekSigningKey:      flagEnv("azure-kek-signing-key", "WARDYN_AZURE_KEK_SIGNING_KEY", "", "versionless id of the EC P-256 key, sign and verify only, in the same vault, that signs every wrap WARDYN_AZURE_KEK_KEY makes; set together with it"),
	}
}

// KEK providers (WARDYN_KEK).
const (
	kekLocal   = "local"
	kekTransit = "transit"
	kekAzure   = "azurekv"
)

// buildKEK returns the configured key service, or nil, and whether it wraps
// new rows. WARDYN_KEK=transit writes under the Vault Transit key and
// WARDYN_KEK=azurekv under the Key Vault keys; with WARDYN_KEK=local a named
// key service is built read-only, so the rows sealed under it stay readable
// while `wardynd -rewrap` moves them back. One key service at a time: a move
// between the two goes through the local key. The key is proven at boot
// (vaultkv.NewTransit, azurekv.NewKEK): a key service that cannot be reached,
// or that does not bind a wrap to its row, fails boot (design K6).
func buildKEK(ctx context.Context, v vaultFlags, az azureFlags, trustedCAFile string) (kek.KEK, bool, error) {
	sel := strings.TrimSpace(*v.kek)
	key := strings.TrimSpace(*v.transitKey)
	azKey, azSig := strings.TrimSpace(*az.kekKey), strings.TrimSpace(*az.kekSigningKey)
	azureNamed := azKey != "" || azSig != ""
	switch {
	case azureNamed && (key != "" || strings.TrimSpace(*v.transitKeyPlatform) != ""):
		return nil, false, fmt.Errorf("refusing to start: WARDYN_AZURE_KEK_KEY and WARDYN_VAULT_TRANSIT_KEY (or WARDYN_VAULT_TRANSIT_KEY_PLATFORM) both name a key service; configure one — a move between Transit and Key Vault goes through the local key (docs/operations/secrets-and-keys.md)")
	case sel == kekAzure && (azKey == "" || azSig == ""):
		return nil, false, fmt.Errorf("refusing to start: WARDYN_KEK=azurekv needs WARDYN_AZURE_KEK_KEY and WARDYN_AZURE_KEK_SIGNING_KEY")
	case azureNamed && (azKey == "" || azSig == ""):
		return nil, false, fmt.Errorf("refusing to start: WARDYN_AZURE_KEK_KEY and WARDYN_AZURE_KEK_SIGNING_KEY are set together or not at all")
	}
	switch sel {
	case "", kekLocal:
		if azureNamed {
			return buildAzureKEK(ctx, v, az, trustedCAFile, false)
		}
		if key == "" {
			return nil, false, nil
		}
	case kekTransit:
		if key == "" {
			return nil, false, fmt.Errorf("refusing to start: WARDYN_KEK=transit needs WARDYN_VAULT_TRANSIT_KEY")
		}
	case kekAzure:
		return buildAzureKEK(ctx, v, az, trustedCAFile, true)
	default:
		return nil, false, fmt.Errorf("refusing to start: WARDYN_KEK is %q; want %q, %q or %q", sel, kekLocal, kekTransit, kekAzure)
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

// buildAzureKEK builds the Key Vault KEK over the store's identity settings.
// It needs no WARDYN_AZURE_KV_URL: the vault is the one the keys name.
func buildAzureKEK(ctx context.Context, v vaultFlags, az azureFlags, trustedCAFile string, writes bool) (kek.KEK, bool, error) {
	k, err := azurekv.NewKEK(ctx, azurekv.KEKConfig{
		Key: strings.TrimSpace(*az.kekKey), SigningKey: strings.TrimSpace(*az.kekSigningKey),
		Auth: strings.TrimSpace(*az.auth), TenantID: strings.TrimSpace(*az.tenantID), ClientID: strings.TrimSpace(*az.clientID),
		FederatedTokenFile: strings.TrimSpace(*az.federatedTokenFile), AuthorityHost: strings.TrimSpace(*az.authorityHost),
		CACertFile: strings.TrimSpace(trustedCAFile), Timeout: *v.timeout,
	})
	if err != nil {
		return nil, false, fmt.Errorf("refusing to start: %w", err)
	}
	return k, writes, nil
}

// buildPlatformKEK returns the Transit key the boot keys alone are wrapped
// under (WARDYN_VAULT_TRANSIT_KEY_PLATFORM), or nil when none is named. It is
// reached as WARDYN_VAULT_ROLE_PLATFORM, so a token that reaches the
// credential key never reaches it. Like buildKEK it is proven at boot.
//
// retire is `wardynd -rewrap-retire-platform-key` alone: the key is built to be
// read, for the boot keys still under it, so it does not need WARDYN_KEK=transit
// (the boot keys may be moving back to the local key). A normal start and a
// plain -rewrap never pass it.
func buildPlatformKEK(ctx context.Context, v vaultFlags, trustedCAFile string, retire bool) (kek.KEK, error) {
	key := strings.TrimSpace(*v.transitKeyPlatform)
	if key == "" {
		return nil, nil
	}
	if sel := strings.TrimSpace(*v.kek); sel != kekTransit && !retire {
		return nil, fmt.Errorf("refusing to start: WARDYN_VAULT_TRANSIT_KEY_PLATFORM needs WARDYN_KEK=transit")
	}
	if strings.TrimSpace(*v.addr) == "" {
		return nil, fmt.Errorf("refusing to start: WARDYN_VAULT_TRANSIT_KEY_PLATFORM is set but WARDYN_VAULT_ADDR is not")
	}
	if strings.TrimSpace(*v.rolePlatform) == "" {
		return nil, fmt.Errorf("refusing to start: WARDYN_VAULT_TRANSIT_KEY_PLATFORM needs WARDYN_VAULT_ROLE_PLATFORM")
	}
	// A token-file login ignores the role: the platform key would be reached
	// with the credential token, which separates nothing.
	if strings.TrimSpace(*v.auth) != vaultkv.AuthKubernetes {
		return nil, fmt.Errorf("refusing to start: WARDYN_VAULT_TRANSIT_KEY_PLATFORM needs WARDYN_VAULT_AUTH=%s", vaultkv.AuthKubernetes)
	}
	if strings.TrimSpace(*v.rolePlatform) == strings.TrimSpace(*v.role) {
		return nil, fmt.Errorf("refusing to start: WARDYN_VAULT_ROLE_PLATFORM is the same role as WARDYN_VAULT_ROLE, which separates nothing; name the second role")
	}
	if key == strings.TrimSpace(*v.transitKey) {
		return nil, fmt.Errorf("refusing to start: WARDYN_VAULT_TRANSIT_KEY_PLATFORM is the same key as WARDYN_VAULT_TRANSIT_KEY, which separates nothing; name a second key")
	}
	cfg := vaultConfig(v, trustedCAFile)
	cfg.Role, cfg.RolePlatform = cfg.RolePlatform, ""
	t, err := vaultkv.NewPlatformTransit(ctx, cfg, strings.TrimSpace(*v.transitMount), key)
	if err != nil {
		return nil, fmt.Errorf("refusing to start: %w", err)
	}
	return t, nil
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
		RolePlatform: strings.TrimSpace(*v.rolePlatform),
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
// Transit at vault.example:8200", "Key Vault myvault"), or "" when the local key does (the
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
