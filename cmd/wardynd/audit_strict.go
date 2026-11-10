// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/audit/sinks"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// strictAuditRecorder is WARDYN_AUDIT_STRICT's recorder (api.Config.AuditStrict), nil when off. It is
// buildAuditChain's chain with no spool and no coalescer: the delegation and dry-run stamps, the policy
// digest, masking and sealing every row gets, then the store, and the sinks when configured. A failed
// store write comes back to the caller, which refuses the change, instead of being spooled. A row whose
// subject key cannot be had fails too, since sealingRecorder holds such a row only in a spool.
func strictAuditRecorder(on bool, pool *pgxpool.Pool, fan *sinks.Fanout, maskReg *secretmask.Registry, seal *auditSealSource, scope *maskScope) audit.Recorder {
	if !on {
		return nil
	}
	storeRec := store.Recorder{Pool: pool}
	var rec audit.Recorder = storeRec
	if fan != nil {
		rec = fanoutRecorder{primary: storeRec, fanout: fan}
	}
	masked := maskingRecorder{inner: sealingRecorder{inner: rec, src: seal}, reg: maskReg, scope: scope}
	return audit.DelegationRecorder{Inner: audit.DryRunRecorder{Inner: withPolicyStamp(pool, masked)}}
}
