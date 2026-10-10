// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/google/uuid"
)

func TestAuthenticatedNoEndLeaseKeepsFiniteTokenLapse(t *testing.T) {
	m, _, elapsed, org := managerFixture(t)
	id := uuid.New()
	ctx := context.Background()
	lease := runnerwire.Lease{RunID: id, TokenRenewedAt: org}
	if err := m.Lease(ctx, lease); err != nil {
		t.Fatal(err)
	}
	if err := m.BeforeCreate(ctx, runner.SandboxSpec{RunID: id}); err != nil {
		t.Fatal(err)
	}
	*elapsed = identity.RunTokenLapseAfter - time.Nanosecond
	if m.expired(id) {
		t.Fatal("no-end lease expired early")
	}
	if err := m.Lease(ctx, lease); err != nil {
		t.Fatal(err)
	}
	*elapsed += time.Nanosecond
	if !m.expired(id) {
		t.Fatal("repeated no-end lease extended token lapse")
	}
}

func TestNoEndChangeRequiresCurrentAuthenticatedLease(t *testing.T) {
	m, _, elapsed, org := managerFixture(t)
	id := createLocal(t, m, org)
	ctx := context.Background()
	lease := runnerwire.Lease{RunID: id, TokenRenewedAt: org, EndsAt: org.Add(time.Minute)}
	if err := m.Lease(ctx, lease); err != nil {
		t.Fatal(err)
	}
	m.Offline(ctx)
	lease.EndsAt = time.Time{}
	if err := m.Lease(ctx, lease); err == nil {
		t.Fatal("offline input cleared finite end")
	}
	*elapsed = time.Minute
	if !m.expired(id) {
		t.Fatal("offline input extended finite end")
	}
	if err := m.BeforeReady(ctx, &runnerwire.Peer{}, true); err != nil {
		t.Fatal(err)
	}
	if err := m.Lease(ctx, lease); err != nil {
		t.Fatal(err)
	}
	if m.expired(id) {
		t.Fatal("authenticated policy change did not remove finite end")
	}
	*elapsed = identity.RunTokenLapseAfter
	if !m.expired(id) {
		t.Fatal("policy change extended token lapse")
	}
}
