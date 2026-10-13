// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	secretstorepg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
)

// Production passes the audit-wrapped store; the counter must reach the Postgres store under it.
func TestLocalRowsCounterSeesThroughTheAuditWrapper(t *testing.T) {
	if localRowsCounter(secretstore.Audited(&secretstorepg.Store{}, &captureRecorder{})) == nil {
		t.Error("localRowsCounter(Audited(pg store)) = nil: the kek_local_rows setup row never appears in production")
	}
	if localRowsCounter(secretstore.Audited(custodyStore{}, &captureRecorder{})) != nil {
		t.Error("localRowsCounter counted a store with no local rows")
	}
}
