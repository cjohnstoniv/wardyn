// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	secretstorepg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// rewrapActor is the audit `actor` of the -rewrap maintenance mode.
const rewrapActor = "wardyn/rewrap"

// rewrapMode is wardynd's `-rewrap` MAINTENANCE MODE: rewrap every stored
// secret's data key onto the KEK a write uses under this configuration, in
// one transaction, and exit (secretstorepg.RewrapKeys). That is the local key
// of the row's purpose (design §2.13 c) — the rows written before the purpose
// split, and, once WARDYN_PLATFORM_KEY_FILE is set, the boot keys still under
// the age key — or, with WARDYN_KEK=transit or azurekv, the key service's key
// at its latest version (design §2.3); with WARDYN_KEK=local and the key
// service still named, the rows under it move back to the local key. No value is
// decrypted. It takes the rekey lock, so it never runs beside a
// -rotate-age-key.
//
// Safe beside a serving daemon configured the same way: it reads a row under
// both the old and the new KEK, and reads the boot keys only at boot. While
// it runs, a write to an existing secret waits for its commit.
func rewrapMode(f *bootFlags) error {
	if strings.TrimSpace(*f.dsn) == "" {
		return fmt.Errorf("refusing to rewrap: -rewrap needs the secret store's database; set WARDYN_PG_DSN")
	}
	ageKey := strings.TrimSpace(*f.ageKey)
	var id *age.X25519Identity
	switch {
	case ageKey != "":
		var err error
		if id, err = age.ParseX25519Identity(ageKey); err != nil {
			return fmt.Errorf("parse the age identity (WARDYN_AGE_KEY): %w", err)
		}
	case !slices.Contains([]string{kekTransit, kekAzure}, strings.TrimSpace(*f.vault.kek)):
		return fmt.Errorf("refusing to rewrap: WARDYN_AGE_KEY (-age-key) is empty, so there is no local key to rewrap from, and WARDYN_KEK is not %q or %q", kekTransit, kekAzure)
	}
	platform, err := readPlatformKey(*f.platformKeyFile, ageKey)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The key service (buildKEK) keeps its Vault token alive until ctx ends.
	svc, writes, err := buildKEK(ctx, f.vault, f.azure, *f.trustedCAFile)
	if err != nil {
		return err
	}
	retire := *f.rewrapRetirePlatformKey
	if retire && strings.TrimSpace(*f.vault.transitKeyPlatform) == "" {
		return fmt.Errorf("refusing to rewrap: -rewrap-retire-platform-key needs WARDYN_VAULT_TRANSIT_KEY_PLATFORM, the key to retire")
	}
	platformSvc, err := buildPlatformKEK(ctx, f.vault, *f.trustedCAFile, retire)
	if err != nil {
		return err
	}
	connCtx, cancelConn := context.WithTimeout(ctx, rekeyConnectTimeout)
	defer cancelConn()
	pool, err := db.Connect(connCtx, *f.dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	release, ok, err := db.TryAdvisoryLock(ctx, pool, db.SecretRekeyLockKey)
	if err != nil {
		return fmt.Errorf("take the rekey lock: %w", err)
	}
	if !ok {
		return fmt.Errorf("refusing to rewrap: a `wardynd -rotate-age-key` or `-rewrap` holds the rekey advisory lock on this database; wait for it to finish")
	}
	defer release()

	rec, fan, _, _, err := buildAuditChain(ctx, *f.auditSinks, *f.auditSpool, *f.auditSource, pool, secretmask.NewRegistry())
	if err != nil {
		return err
	}
	if fan != nil {
		defer func() { _ = fan.Close() }()
	}

	d := secretstore.Deps{
		Pool: pool, AgeIdentity: optionalIdentity(id), PlatformIdentity: optionalIdentity(platform), KEK: svc, KEKWrites: writes,
	}
	d = withPlatformKEK(d, platformSvc, retire)
	return rewrapKeys(ctx, rec, d)
}

// withPlatformKEK adds the platform key service to d. Retiring reads it and
// writes nowhere under it: the boot keys move to the key a write uses today.
func withPlatformKEK(d secretstore.Deps, k kek.KEK, retire bool) secretstore.Deps {
	if k != nil {
		d.PlatformKEK, d.PlatformKEKWrites = k, !retire
	}
	return d
}

// rewrapKeys is -rewrap's work once its inputs are checked and its lock held:
// the rewrap under d's keys, its secret.rewrap event, and what to do next.
func rewrapKeys(ctx context.Context, rec audit.Recorder, d secretstore.Deps) error {
	res, err := secretstorepg.RewrapKeys(ctx, d)
	separate := d.PlatformIdentity != nil || d.PlatformKEKWrites
	if err != nil {
		// An abort is audited like secret.migrate's, even when ctx is what
		// ended the run: under Transit it has already made decrypt calls that
		// Vault's audit device records. The error names a row, so it goes to
		// the operator only.
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rewrapAbortAuditTimeout)
		defer cancel()
		emitRewrapAudit(actx, rec, res, separate, true)
		return err
	}
	emitRewrapAudit(ctx, rec, res, separate, false)
	slog.Info("wardynd: stored secrets rewrapped onto this configuration's keys; restart every replica with the same WARDYN_AGE_KEY, WARDYN_PLATFORM_KEY_FILE and WARDYN_KEK",
		slog.Int("secrets", res.Rewrapped), slog.Bool("platform_key_separate", separate), slog.String("key_service", res.KeyService))
	if res.KeyVersion != "" {
		what := "secret"
		if d.PlatformKEKWrites {
			what = "credential" // the boot keys are under the platform key, reported below
		}
		fmt.Fprintf(os.Stdout, "every sealed %s is wrapped under %s\n", what, retireStep(res.KeyService, res.KeyVersion))
	}
	if d.PlatformKEK != nil && !d.PlatformKEKWrites {
		fmt.Fprintf(os.Stdout, "no boot key is wrapped under %s any more; unset WARDYN_VAULT_TRANSIT_KEY_PLATFORM and restart every replica\n", d.PlatformKEK.ID())
	}
	if res.PlatformKeyVersion != "" {
		fmt.Fprintf(os.Stdout, "every boot key is wrapped under %s version %s; raising that Transit key's min_decryption_version to %s now retires the older versions\n", res.PlatformKeyService, res.PlatformKeyVersion, res.PlatformKeyVersion)
	}
	return nil
}

// retireStep names the key service id at version v, and what retires every
// other version at that service.
func retireStep(id, v string) string {
	if strings.HasPrefix(id, kek.AzureKeyIDPrefix) {
		return fmt.Sprintf("%s at versions %s (wrapping/signing); disabling every other version of both keys in Key Vault now retires them", id, v)
	}
	return fmt.Sprintf("%s version %s; raising the Transit key's min_decryption_version to %s now retires the older versions", id, v, v)
}

// optionalIdentity is the platform identity as the interface the store takes:
// nil stays an untyped nil, never a typed nil that reads as "a key is set".
func optionalIdentity(id *age.X25519Identity) age.Identity {
	if id == nil {
		return nil
	}
	return id
}

// rewrapAbortAuditTimeout bounds the aborted run's audit write, which no
// longer inherits the run's (possibly canceled) context.
const rewrapAbortAuditTimeout = 5 * time.Second

// emitRewrapAudit writes the secret.rewrap event: the row count, whether the
// boot keys now sit under a separate platform key, and, when a key service
// wraps every write, its kek_id and the key version every row is now under.
// An aborted run's event is outcome failure, reason aborted: its count is
// what was committed (0 — the rewrap is one transaction), and it carries no
// key_version, since no row moved to it. Like secret.rekey it names no secret.
func emitRewrapAudit(ctx context.Context, rec audit.Recorder, res secretstorepg.RewrapResult, separate, aborted bool) {
	fields := map[string]any{"secrets": res.Rewrapped, "platform_key_separate": separate}
	if res.KeyService != "" {
		fields["key_service"] = res.KeyService
	}
	outcome := "success"
	switch {
	case aborted:
		outcome, fields["reason"] = "failure", "aborted"
	case res.KeyVersion != "":
		fields["key_version"] = res.KeyVersion
	}
	data, _ := json.Marshal(fields)
	ev := types.AuditEvent{
		ID:        uuid.New(),
		Time:      time.Now().UTC(),
		ActorType: types.ActorSystem,
		Actor:     rewrapActor,
		Action:    "secret.rewrap",
		Target:    "pg",
		Outcome:   outcome,
		Data:      json.RawMessage(data),
	}
	if err := rec.Record(ctx, ev); err != nil {
		audit.LogWriteFailure(ctx, ev, err)
	}
}
