// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
)

// TestAuditChainStampsDelegation: the chain every writer shares is headed by
// audit.DelegationRecorder, so a row the identity provider or the broker
// records under a portal's delegated request names the portal too (#1142),
// not only the rows the API writes itself.
func TestAuditChainStampsDelegation(t *testing.T) {
	rec, _, _, _, err := buildAuditChain(context.Background(), "", "", "", nil, secretmask.NewRegistry(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rec.(audit.DelegationRecorder); !ok {
		t.Fatalf("audit chain head = %T, want audit.DelegationRecorder", rec)
	}
}
