// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// A resident delivery is admitted only for a run with a live initial lease
// and no creation intent, on an online runner that is not revoked.
func TestManager_AdmitResident(t *testing.T) {
	m, _, elapsed, org := managerFixture(t)
	ctx := context.Background()
	if err := m.AdmitResident(uuid.New()); err == nil {
		t.Fatal("a run with no lease was admitted")
	}
	id := uuid.New()
	if err := m.Lease(ctx, runnerwire.Lease{RunID: id, TokenRenewedAt: org, EndsAt: org.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := m.AdmitResident(id); err != nil {
		t.Fatalf("a leased run = %v, want admitted", err)
	}
	*elapsed = 2 * time.Hour
	if err := m.AdmitResident(id); err == nil {
		t.Fatal("a run whose lease lapsed was admitted")
	}
	*elapsed = 0
	created := createLocal(t, m, org)
	if err := m.AdmitResident(created); err == nil {
		t.Fatal("a run with a creation intent was admitted")
	}
	if err := m.Revoked(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.AdmitResident(id); !errors.Is(err, runner.ErrRunnerOffline) || !m.IsRevoked() {
		t.Fatalf("after revoke = %v (revoked %v), want offline", err, m.IsRevoked())
	}
}
