// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
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
// the age key — or, with WARDYN_KEK=transit, the Vault Transit key at its
// latest version (design §2.3); with WARDYN_KEK=local and the Transit key
// still named, the rows under it move back to the local key. No value is
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
	case strings.TrimSpace(*f.vault.kek) != kekTransit:
		return fmt.Errorf("refusing to rewrap: WARDYN_AGE_KEY (-age-key) is empty, so there is no local key to rewrap from, and WARDYN_KEK is not %q", kekTransit)
	}
	platform, err := readPlatformKey(*f.platformKeyFile, ageKey)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The key service (buildKEK) keeps its Vault token alive until ctx ends.
	svc, writes, err := buildKEK(ctx, f.vault, *f.trustedCAFile)
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

	return rewrapKeys(ctx, rec, secretstore.Deps{
		Pool: pool, AgeIdentity: optionalIdentity(id), PlatformIdentity: optionalIdentity(platform), KEK: svc, KEKWrites: writes,
	})
}

// rewrapKeys is -rewrap's work once its inputs are checked and its lock held:
// the rewrap under d's keys, its secret.rewrap event, and what to do next.
func rewrapKeys(ctx context.Context, rec audit.Recorder, d secretstore.Deps) error {
	res, err := secretstorepg.RewrapKeys(ctx, d)
	if err != nil {
		return err
	}
	separate := d.PlatformIdentity != nil
	emitRewrapAudit(ctx, rec, res, separate)
	slog.Info("wardynd: stored secrets rewrapped onto this configuration's keys; restart every replica with the same WARDYN_AGE_KEY, WARDYN_PLATFORM_KEY_FILE and WARDYN_KEK",
		slog.Int("secrets", res.Rewrapped), slog.Bool("platform_key_separate", separate), slog.String("key_service", res.KeyService))
	if res.KeyVersion > 0 {
		fmt.Fprintf(os.Stdout, "every sealed secret is wrapped under %s version %d; raising the Transit key's min_decryption_version to %d now retires the older versions\n", res.KeyService, res.KeyVersion, res.KeyVersion)
	}
	return nil
}

// optionalIdentity is the platform identity as the interface the store takes:
// nil stays an untyped nil, never a typed nil that reads as "a key is set".
func optionalIdentity(id *age.X25519Identity) age.Identity {
	if id == nil {
		return nil
	}
	return id
}

// emitRewrapAudit writes the secret.rewrap event: the row count, whether the
// boot keys now sit under a separate platform key, and, when a key service
// wraps every write, its kek_id and the key version every row is now under.
// Like secret.rekey it names no secret.
func emitRewrapAudit(ctx context.Context, rec audit.Recorder, res secretstorepg.RewrapResult, separate bool) {
	fields := map[string]any{"secrets": res.Rewrapped, "platform_key_separate": separate}
	if res.KeyService != "" {
		fields["key_service"] = res.KeyService
	}
	if res.KeyVersion > 0 {
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
		Outcome:   "success",
		Data:      json.RawMessage(data),
	}
	if err := rec.Record(ctx, ev); err != nil {
		audit.LogWriteFailure(ctx, ev, err)
	}
}
