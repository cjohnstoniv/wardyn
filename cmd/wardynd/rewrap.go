// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	secretstorepg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// rewrapActor is the audit `actor` of the -rewrap maintenance mode.
const rewrapActor = "wardyn/rewrap"

// rewrapMode is wardynd's `-rewrap` MAINTENANCE MODE (design §2.13 c): rewrap
// every local row's data key onto the KEK its purpose uses under this
// configuration — the rows written before the purpose split, and, once
// WARDYN_PLATFORM_KEY_FILE is set, the boot keys still under the age key —
// in one transaction, and exit. No value is decrypted. It takes the rekey
// lock, so it never runs beside a -rotate-age-key.
//
// Safe beside a serving daemon with the same WARDYN_AGE_KEY: it reads both
// the old and the new KEK of a credential row, and reads the boot keys only
// at boot. Every replica then restarts with the platform key file set.
func rewrapMode(f *bootFlags) error {
	if strings.TrimSpace(*f.dsn) == "" {
		return fmt.Errorf("refusing to rewrap: -rewrap needs the secret store's database; set WARDYN_PG_DSN")
	}
	ageKey := strings.TrimSpace(*f.ageKey)
	if ageKey == "" {
		return fmt.Errorf("refusing to rewrap: WARDYN_AGE_KEY (-age-key) is empty, so there is no local key to rewrap from")
	}
	id, err := age.ParseX25519Identity(ageKey)
	if err != nil {
		return fmt.Errorf("parse the age identity (WARDYN_AGE_KEY): %w", err)
	}
	platform, err := readPlatformKey(*f.platformKeyFile, ageKey)
	if err != nil {
		return err
	}

	ctx := context.Background()
	connCtx, cancel := context.WithTimeout(ctx, rekeyConnectTimeout)
	defer cancel()
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

	n, err := secretstorepg.Rewrap(ctx, pool, id, optionalIdentity(platform))
	if err != nil {
		return err
	}
	emitRewrapAudit(ctx, rec, n, platform != nil)
	slog.Info("wardynd: stored secrets rewrapped onto this configuration's keys; restart every replica with the same WARDYN_AGE_KEY and WARDYN_PLATFORM_KEY_FILE",
		slog.Int("secrets", n), slog.Bool("platform_key_separate", platform != nil))
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

// emitRewrapAudit writes the secret.rewrap event: the row count and whether
// the boot keys now sit under a separate platform key. Like secret.rekey it
// names no secret.
func emitRewrapAudit(ctx context.Context, rec audit.Recorder, count int, separate bool) {
	data, _ := json.Marshal(map[string]any{"secrets": count, "platform_key_separate": separate})
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
