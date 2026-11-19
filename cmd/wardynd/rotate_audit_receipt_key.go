// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auditsign"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// rotateAuditReceiptKeyMode is `wardynd -rotate-audit-receipt-key`: a maintenance mode that replaces the key
// audit checkpoints are signed with, keeps every old public key so old checkpoints still verify, writes a
// secret.rekey row and exits. It never opens a listener. Every replica must be stopped, for the reason
// rotateGatewayIdentityKeyMode gives.
func rotateAuditReceiptKeyMode(f *bootFlags) error {
	return withRotationStore(f, "-rotate-audit-receipt-key", *f.rotateGatewayIdentityKey, func(ctx context.Context, secrets bootKeyStore, rec audit.Recorder) error {
		return secrets.rotateAuditReceipt(ctx, rec, *f.retirePrevious, time.Now())
	})
}

// rotateAuditReceipt writes the key value that follows the stored one and its audit row. No row is a refusal:
// wardynd makes the key on its first start with signing on, and a rotation is not how a key gets made. A row
// that cannot be read or parsed is a refusal too, and nothing is written over it. With retirePrevious the
// replaced key is marked retired (treated as stolen): wardyn audit verify then fails every checkpoint it signed,
// and history is not re-signed, because that would launder whatever a thief rewrote.
func (secrets bootKeyStore) rotateAuditReceipt(ctx context.Context, rec audit.Recorder, retirePrevious bool, now time.Time) error {
	raw, ok, err := loadBootKey(ctx, secrets, secretAuditReceiptKey, func([]byte) bool { return true })
	if err != nil {
		return fmt.Errorf("refusing to rotate: %w", err)
	}
	if !ok {
		return errors.New("refusing to rotate: this install has no audit receipt key; start wardynd once with audit checkpoints on and it makes one")
	}
	old, err := auditsign.Load(raw)
	if err != nil {
		return fmt.Errorf("refusing to rotate: %w", err)
	}
	next, err := auditsign.Rotate(raw, retirePrevious, now)
	if err != nil {
		return fmt.Errorf("refusing to rotate: %w", err)
	}
	rotated, err := auditsign.Load(next)
	if err != nil {
		return fmt.Errorf("refusing to rotate: %w", err)
	}
	if err := secrets.Put(ctx, secretAuditReceiptKey, next); err != nil {
		return fmt.Errorf("store the rotated key: %w", err)
	}

	data, _ := json.Marshal(map[string]any{"kid": rotated.KeyID(), "replaced_kid": old.KeyID(), "retired_previous": retirePrevious})
	ev := types.AuditEvent{
		ID:        uuid.New(),
		Time:      now.UTC(),
		ActorType: types.ActorSystem,
		Actor:     "wardyn/rotate-audit-receipt-key",
		Action:    "secret.rekey",
		Target:    secretAuditReceiptKey,
		Outcome:   "success",
		Data:      json.RawMessage(data),
	}
	if err := rec.Record(ctx, ev); err != nil {
		audit.LogWriteFailure(ctx, ev, err)
	}
	slog.Info("wardynd: audit receipt key rotated. Start wardynd, then save the public keys again with wardyn audit keys and keep them where wardynd cannot change them.",
		slog.String("kid", rotated.KeyID()),
		slog.String("replaced_kid", old.KeyID()),
		slog.Bool("retired_previous", retirePrevious),
	)
	return nil
}
