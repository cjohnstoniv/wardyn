// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Runner replay remains in its own durable spool until the org transaction
// commits. Pending-key sealing must therefore refuse ACK, never divert to the
// org fallback spool and acknowledge a cursor whose audit row is absent.
func prepareRunnerAudit(ctx context.Context, ev types.AuditEvent, mask maskingRecorder, src *auditSealSource) (types.AuditEvent, error) {
	ev = mask.prepare(ev)
	if src == nil || !src.seals(ev) {
		return ev, nil
	}
	seal := src.sealer.Load()
	if seal == nil {
		return ev, errors.New("runner audit sealing keys unavailable")
	}
	sealed, pending, err := seal.Seal(ctx, ev)
	if err != nil {
		return ev, err
	}
	if pending {
		return ev, errors.New("runner audit awaits its subject sealing key")
	}
	return sealed, nil
}
