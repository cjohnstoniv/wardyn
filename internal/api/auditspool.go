// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"os"
	"sync"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// AuditSpool durably records audit events whose PRIMARY store write failed, so a
// security event is never silently lost (C1). The control-plane audit log is the
// system of record; silently dropping a credential.mint / run.kill / egress.deny
// event when the database blips would let a run proceed while reporting success —
// exactly the failure a governance tool must not have.
//
// It is a mutex-guarded append-only JSONL file: intentionally simple, fit for the
// local-first single-host deployment, and lets an operator (or a future drain job)
// replay spooled events into the audit log once the store recovers. A nil
// *AuditSpool disables spooling (a failed primary write is then only logged loudly).
type AuditSpool struct {
	mu sync.Mutex
	f  *os.File
}

// NewAuditSpool opens (creating if needed) an append-only JSONL spool at path.
func NewAuditSpool(path string) (*AuditSpool, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &AuditSpool{f: f}, nil
}

// Append writes one event as a single JSON line and fsyncs it, so a crash right
// after a primary-write failure still preserves the event. It returns an error
// only when even the fallback write fails (the last-resort signal the event is lost).
func (a *AuditSpool) Append(ev types.AuditEvent) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, err := a.f.Write(append(b, '\n')); err != nil {
		return err
	}
	return a.f.Sync()
}
