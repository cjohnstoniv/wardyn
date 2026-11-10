// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/audit/sinks"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// strictAuditRecorder is WARDYN_AUDIT_STRICT's recorder (api.Config.AuditStrict), nil when off. It is
// buildAuditChain's chain with no spool and no coalescer: the delegation and dry-run stamps, the policy
// digest, masking and sealing every row gets, then the store, and the sinks once the store took the row
// (storedFanoutRecorder). A failed store write comes back to the caller, which refuses the change,
// instead of being spooled or sent on. A row whose subject key cannot be had fails too, since
// sealingRecorder holds such a row only in a spool.
func strictAuditRecorder(on bool, pool *pgxpool.Pool, fan *sinks.Fanout, maskReg *secretmask.Registry, seal *auditSealSource, scope *maskScope) audit.Recorder {
	if !on {
		return nil
	}
	var rec audit.Recorder = store.Recorder{Pool: pool}
	if fan != nil {
		rec = storedFanoutRecorder{pool: pool, fanout: fan}
	}
	masked := maskingRecorder{inner: sealingRecorder{inner: rec, src: seal}, reg: maskReg, scope: scope}
	return audit.DelegationRecorder{Inner: audit.DryRunRecorder{Inner: withPolicyStamp(pool, masked)}}
}

// storedFanoutRecorder is fanoutRecorder for strict mode: the sinks get a row only after the store took
// it, so a change refused because its intent row was not stored never reaches a sink as an intent. The
// row they get carries the hash chain the insert filled in, as fanoutRecorder's does.
type storedFanoutRecorder struct {
	pool   *pgxpool.Pool
	fanout *sinks.Fanout
}

func (f storedFanoutRecorder) Record(ctx context.Context, ev types.AuditEvent) error {
	if err := store.InsertAuditEvent(ctx, f.pool, &ev); err != nil {
		return err
	}
	// Detached and best-effort, as fanoutRecorder's: the row is stored, so the change goes ahead.
	if err := f.fanout.Emit(context.WithoutCancel(ctx), ev); err != nil {
		slog.ErrorContext(ctx, "wardynd: audit fanout emit failed", slog.String("action", ev.Action), slog.Any("err", err))
	}
	return nil
}
