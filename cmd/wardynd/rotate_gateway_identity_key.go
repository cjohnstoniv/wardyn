// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/gatewayidentity"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// maintenance runs a key rotation mode (-rotate-gateway-identity-key or
// -rotate-audit-receipt-key) when asked, ahead of every other maintenance mode
// (each refuses the others beside it), and maintenanceMode otherwise.
func (f *bootFlags) maintenance() (bool, error) {
	switch {
	case *f.rotateGatewayIdentityKey:
		return true, withBootTransport(f, rotateGatewayIdentityKeyMode)
	case *f.rotateAuditReceiptKey:
		return true, withBootTransport(f, rotateAuditReceiptKeyMode)
	case *f.retirePrevious:
		return true, errRetireAlone
	}
	return maintenanceMode(f)
}

var errRetireAlone = errors.New("refusing to run: -retire-previous is a mode of -rotate-gateway-identity-key or -rotate-audit-receipt-key; " +
	"run `wardynd -rotate-gateway-identity-key -retire-previous` or `wardynd -rotate-audit-receipt-key -retire-previous`")

// rotateGatewayIdentityKeyMode is `wardynd -rotate-gateway-identity-key`: a
// maintenance mode that replaces the key an AI gateway's run identity token is
// signed with, writes a secret.rekey row and exits. It never opens a listener.
//
// Every replica must be stopped: a serving one keeps signing with the key it
// loaded. Without WARDYN_HA the single-instance lock shows a serving daemon and
// this refuses; with it no lock can, so stopping them is the operator's step.
func rotateGatewayIdentityKeyMode(f *bootFlags) error {
	if !*f.rotateGatewayIdentityKey {
		return errRetireAlone
	}
	return withRotationStore(f, "-rotate-gateway-identity-key", *f.rotateAuditReceiptKey, func(ctx context.Context, secrets bootKeyStore, rec audit.Recorder) error {
		return secrets.rotateGatewayIdentity(ctx, rec, *f.retirePrevious, time.Now())
	})
}

// withRotationStore is the scaffolding every boot-key rotation mode shares: it
// refuses any other maintenance mode beside this one (other is the other
// rotation mode's flag), connects, takes the single-instance and rekey locks
// on sessions of their own, builds the audit chain and the secret store, and
// runs rotate with them.
func withRotationStore(f *bootFlags, mode string, other bool, rotate func(ctx context.Context, secrets bootKeyStore, rec audit.Recorder) error) error {
	for _, o := range []bool{
		other, strings.TrimSpace(*f.rotateAgeKey) != "", *f.rewrap, *f.rewrapRetirePlatformKey, *f.rewrapAdoptBootKeys, *f.rewrapPrincipalKeys,
		*f.migrateOnly, *f.migrateSecrets, *f.reconcile, *f.auditSplitLegacy,
		*f.recoveryInventory, *f.recoveryManifest, *f.recoveryVerify, *f.recoveryRehearse,
	} {
		if o {
			return fmt.Errorf("refusing to run: %s is a mode of its own; run it with no other maintenance mode", mode)
		}
	}
	if strings.TrimSpace(*f.dsn) == "" {
		return fmt.Errorf("refusing to rotate: %s needs the secret store's database; set WARDYN_PG_DSN", mode)
	}

	// The key service (buildStoreClients) keeps its Vault token alive until ctx ends.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	connCtx, cancelConn := context.WithTimeout(ctx, rekeyConnectTimeout)
	defer cancelConn()
	pool, err := db.Connect(connCtx, *f.dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	// Each lock on a session of its own, as claimSingleInstance holds it: the mode's work below asks
	// this pool for connections while both are held, and a hold on a pooled connection is the nested
	// acquire the pool guard refuses.
	_, releaseInstance, ok, err := db.TryAdvisoryLockDedicated(ctx, pool, db.SingleInstanceLockKey)
	if err != nil {
		return fmt.Errorf("take the single-instance lock: %w", err)
	}
	if !ok {
		return errors.New("refusing to rotate: a wardynd is serving this database, and it would keep signing with the key it loaded; stop it first")
	}
	defer releaseInstance()
	_, releaseRekey, ok, err := db.TryAdvisoryLockDedicated(ctx, pool, db.SecretRekeyLockKey)
	if err != nil {
		return fmt.Errorf("take the rekey lock: %w", err)
	}
	if !ok {
		return errors.New("refusing to rotate: another maintenance mode holds the rekey advisory lock on this database; wait for it to finish")
	}
	defer releaseRekey()

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
	clients, err := buildStoreClients(ctx, f)
	if err != nil {
		return err
	}
	secrets, err := buildSecretStore(ctx, pool, *f.ageKey, platform, *f.secretStoreSel, clients, rec)
	if err != nil {
		return err
	}
	return rotate(ctx, newBootKeyStore(secrets, pool, true), rec)
}

// rotateGatewayIdentity writes the row that follows the stored one and its audit
// row. No row is a refusal: identity was never turned on, and a rotation is not
// how a key gets made. A row that cannot be read or parsed is a refusal too, and
// nothing is written over it. The audit row names the kids, which are public, so
// a person can tell which key was replaced and whether the old ones were retired.
func (secrets bootKeyStore) rotateGatewayIdentity(ctx context.Context, rec audit.Recorder, retirePrevious bool, now time.Time) error {
	raw, ok, err := loadBootKey(ctx, secrets, secretGatewayIdentityKey, func([]byte) bool { return true })
	if err != nil {
		return fmt.Errorf("refusing to rotate: %w", err)
	}
	if !ok {
		return errors.New("refusing to rotate: this install has no AI gateway identity key, so identity was never turned on and there is nothing to rotate")
	}
	old, err := gatewayidentity.New(raw)
	if err != nil {
		return fmt.Errorf("refusing to rotate: %w", err)
	}
	next, err := gatewayidentity.Rotate(raw, retirePrevious, now)
	if err != nil {
		return fmt.Errorf("refusing to rotate: %w", err)
	}
	rotated, err := gatewayidentity.New(next)
	if err != nil {
		return fmt.Errorf("refusing to rotate: %w", err)
	}
	if err := secrets.Put(ctx, secretGatewayIdentityKey, next); err != nil {
		return fmt.Errorf("store the rotated key: %w", err)
	}

	fields := map[string]any{"kid": rotated.KeyID(), "replaced_kid": old.KeyID(), "retired_previous": retirePrevious}
	if !retirePrevious {
		fields["previous_retire_at"] = rotated.RetireAt()
	}
	data, _ := json.Marshal(fields)
	ev := types.AuditEvent{
		ID:        uuid.New(),
		Time:      now.UTC(),
		ActorType: types.ActorSystem,
		Actor:     "wardyn/rotate-gateway-identity-key",
		Action:    "secret.rekey",
		Target:    secretGatewayIdentityKey,
		Outcome:   "success",
		Data:      json.RawMessage(data),
	}
	if err := rec.Record(ctx, ev); err != nil {
		audit.LogWriteFailure(ctx, ev, err)
	}
	slog.Info("wardynd: AI gateway identity key rotated. Start wardynd, then restart each gateway: a gateway reads the key set when it loads its config and does not start if that read fails.",
		slog.String("kid", rotated.KeyID()),
		slog.String("replaced_kid", old.KeyID()),
		slog.Bool("retired_previous", retirePrevious),
	)
	return nil
}
