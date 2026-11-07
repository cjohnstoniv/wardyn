// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auditreceipt"
	"github.com/cjohnstoniv/wardyn/internal/auditsign"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// exitOnBadCheckpointInterval refuses to boot on a WARDYN_AUDIT_CHECKPOINT_INTERVAL that is negative or under
// 10s (0 is off), exit 2 like every other malformed setting.
func exitOnBadCheckpointInterval(f *bootFlags) {
	if err := auditsign.CheckInterval(*f.auditCheckpointInterval); err != nil {
		fmt.Fprintf(flag.CommandLine.Output(), "invalid WARDYN_AUDIT_CHECKPOINT_INTERVAL=%q: %v\n", f.auditCheckpointInterval.String(), err)
		os.Exit(2)
	}
}

// loadOrCreateAuditReceiptKey returns the audit checkpoint signing key, made on first boot like every boot key.
// valid is true for any stored row: a false one is what makes loadOrCreateSecret write a new key, and a key
// minted over the stored one would orphan every checkpoint already sent off-box. A row that fails to decrypt
// or to parse is a refusal, never replaced.
func loadOrCreateAuditReceiptKey(ctx context.Context, secrets bootKeyStore) (*auditsign.Key, error) {
	raw, err := loadOrCreateSecret(ctx, secrets, secretAuditReceiptKey,
		func([]byte) bool { return true },
		func() ([]byte, error) {
			slog.Info("wardynd: generated and persisted the audit receipt key")
			return auditsign.NewKey(time.Now())
		},
	)
	if err != nil {
		return nil, err
	}
	return auditsign.Load(raw)
}

// startAuditCheckpoints starts the checkpoint signer in the serving process and returns what the API reads:
// the public keys (nil while signing is off) and the signer's status for the metrics (nil when the interval
// is 0). It never fails the boot. Default on: it refuses nothing an install did before, and a chain signed
// only after an incident proves nothing, so it has to be running already. A key that cannot be loaded or
// made (a decrypt error, or an external store that refuses the new name) logs one ERROR, counts
// key_unavailable, and leaves signing off.
func startAuditCheckpoints(ctx context.Context, interval time.Duration, secrets bootKeyStore, cps store.AuditCheckpointer, rec audit.Recorder) (func() []auditreceipt.PublicKey, *auditsign.Status) {
	if interval == 0 {
		return nil, nil
	}
	status := &auditsign.Status{}
	key, err := loadOrCreateAuditReceiptKey(ctx, secrets)
	if err != nil {
		status.KeyUnavailable()
		slog.ErrorContext(ctx, "wardynd: audit checkpoint signing is off: the audit receipt key could not be loaded or made",
			slog.String("secret", secretAuditReceiptKey), slog.Any("err", err))
		return nil, status
	}
	if cps == nil {
		return nil, status
	}
	var locker db.Locker
	if l, ok := cps.(interface{ Locker() db.Locker }); ok {
		locker = l.Locker()
	}
	if locker == nil {
		locker = db.NewLocalLocker()
	}
	signer := &auditsign.Signer{
		Store:   checkpointStore{cps},
		Key:     key,
		Locker:  locker,
		Record:  rec,
		MaxRows: store.AuditCheckpointMaxRows,
		Now:     time.Now,
		Status:  status,
	}
	go signer.Run(ctx, interval)
	keys := key.PublicKeys()
	return func() []auditreceipt.PublicKey { return keys }, status
}

// checkpointStore adapts the Postgres store to the signer's Store.
type checkpointStore struct{ s store.AuditCheckpointer }

func (c checkpointStore) ChainHead(ctx context.Context) (int64, string, error) {
	return c.s.AuditChainHead(ctx)
}

func (c checkpointStore) Range(ctx context.Context, afterSeq int64, afterHash string, max int) ([]auditsign.Row, int64, error) {
	rows, hw, err := c.s.AuditCheckpointRange(ctx, afterSeq, afterHash, max)
	var br *store.AuditCheckpointBreak
	if errors.As(err, &br) {
		return nil, 0, &auditsign.ChainBreak{Seq: br.Seq, Reason: br.Reason}
	}
	if err != nil {
		return nil, 0, err
	}
	out := make([]auditsign.Row, len(rows))
	for i, r := range rows {
		out[i] = auditsign.Row{Seq: r.Seq, RowHash: r.RowHash}
	}
	return out, hw, nil
}

func (c checkpointStore) Insert(ctx context.Context, cp auditsign.Checkpoint) (auditsign.Checkpoint, error) {
	existing, err := c.s.InsertAuditCheckpoint(ctx, store.AuditCheckpoint{
		SeqLo: cp.SeqLo, SeqHi: cp.SeqHi, Kid: cp.Kid, Statement: cp.Statement, Signature: cp.Signature,
	})
	if errors.Is(err, store.ErrAuditCheckpointExists) {
		return fromStored(existing), auditsign.ErrExists
	}
	return auditsign.Checkpoint{}, err
}

func (c checkpointStore) Latest(ctx context.Context) (auditsign.Checkpoint, bool, error) {
	cp, found, err := c.s.LatestAuditCheckpoint(ctx)
	return fromStored(cp), found, err
}

func (c checkpointStore) EventSeq(ctx context.Context, id uuid.UUID, afterSeq int64) (int64, bool, error) {
	rows, err := c.s.AuditReceiptRows(ctx, []uuid.UUID{id}, nil, afterSeq, 1)
	if err != nil || len(rows) == 0 {
		return 0, false, err
	}
	return rows[0].Seq, true, nil
}

func fromStored(c store.AuditCheckpoint) auditsign.Checkpoint {
	return auditsign.Checkpoint{SeqLo: c.SeqLo, SeqHi: c.SeqHi, Kid: c.Kid, Statement: c.Statement, Signature: c.Signature}
}
