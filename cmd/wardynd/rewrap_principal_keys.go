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

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	secretstorepg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// rewrapPrincipalKeysActor is the audit `actor` of -rewrap-principal-keys.
const rewrapPrincipalKeysActor = "wardyn/rewrap-principal-keys"

// rewrapPrincipalKeysMode is wardynd's `-rewrap-principal-keys` MAINTENANCE
// MODE: move every person's v1 credential row into an enc_version 3 envelope
// under that person's principal key, one row per transaction, and exit
// (secretstorepg.Store.SealToPrincipalKeys). It is not a root rotation, which
// is `-rewrap`; it takes the same rekey lock, so it never runs beside one or
// beside a `-rotate-age-key`. Values are never decrypted.
//
// Safe beside a serving daemon configured the same way: each row is locked
// while it moves, and a row that stopped being v1 meanwhile is skipped.
// Idempotent and resumable: an abort names the row, with every earlier row
// committed.
func rewrapPrincipalKeysMode(f *bootFlags) error {
	if strings.TrimSpace(*f.dsn) == "" {
		return fmt.Errorf("refusing to run: -rewrap-principal-keys needs the secret store's database; set WARDYN_PG_DSN")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
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
		return fmt.Errorf("refusing to run: a `wardynd -rotate-age-key`, `-rewrap` or `-rewrap-principal-keys` holds the rekey advisory lock on this database; wait for it to finish")
	}
	defer release()

	rec, fan, _, _, err := buildAuditChain(ctx, *f.auditSinks, *f.auditSpool, *f.auditSource, pool, secretmask.NewRegistry(), newAuditSealSource(audit.SealOff))
	if err != nil {
		return err
	}
	if fan != nil {
		defer func() { _ = fan.Close() }()
	}
	platform, err := readPlatformKey(*f.platformKeyFile, *f.ageKey)
	if err != nil {
		return err
	}
	c, err := buildStoreClients(ctx, f)
	if err != nil {
		return err
	}
	// The store bare, as the other maintenance modes open it: the move reads no value.
	s, err := newSecretStore(ctx, pool, *f.ageKey, platform, *f.secretStoreSel, c, rec)
	if err != nil {
		return err
	}
	ps, ok := s.(*secretstorepg.Store)
	if !ok {
		return fmt.Errorf("refusing to run: secret store %q keeps no credential rows to move", s.Name())
	}
	return sealToPrincipalKeys(ctx, ps, rec)
}

// sealToPrincipalKeys is the mode's work once its store is open: the move, its
// secret.rewrap event, and what to do next.
func sealToPrincipalKeys(ctx context.Context, ps *secretstorepg.Store, rec audit.Recorder) error {
	res, err := ps.SealToPrincipalKeys(ctx)
	if err != nil {
		// An abort is audited whatever ended the run. The error names a row,
		// so it goes to the operator only.
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rewrapAbortAuditTimeout)
		defer cancel()
		emitRewrapPrincipalKeysAudit(actx, rec, res, err)
		return err
	}
	emitRewrapPrincipalKeysAudit(ctx, rec, res, nil)
	slog.Info("wardynd: person-owned credentials sealed under their principal keys",
		slog.Int("moved", res.Moved), slog.Int("remaining", res.Remaining))
	if res.Remaining > 0 {
		fmt.Fprintf(os.Stdout, "%d person-owned credentials were written under the credential key while this run moved the rest; run `wardynd -rewrap-principal-keys` again until it leaves none\n", res.Remaining)
	}
	return nil
}

// emitRewrapPrincipalKeysAudit writes the secret.rewrap event of this mode:
// `mode` principal_keys, `secrets` the rows moved (what was committed, on an
// abort), and on success `remaining`, the person-owned v1 rows left. It carries
// no secret names, like the -rewrap event.
func emitRewrapPrincipalKeysAudit(ctx context.Context, rec audit.Recorder, res secretstorepg.SealToPrincipalKeysResult, failure error) {
	fields := map[string]any{"mode": "principal_keys", "secrets": res.Moved}
	outcome := "success"
	if failure != nil {
		outcome, fields["reason"] = "failure", "aborted"
	} else {
		fields["remaining"] = res.Remaining
	}
	data, _ := json.Marshal(fields)
	ev := types.AuditEvent{
		ID:        uuid.New(),
		Time:      time.Now().UTC(),
		ActorType: types.ActorSystem,
		Actor:     rewrapPrincipalKeysActor,
		Action:    "secret.rewrap",
		Target:    "pg",
		Outcome:   outcome,
		Data:      json.RawMessage(data),
	}
	if err := rec.Record(ctx, ev); err != nil {
		audit.LogWriteFailure(ctx, ev, err)
	}
}
