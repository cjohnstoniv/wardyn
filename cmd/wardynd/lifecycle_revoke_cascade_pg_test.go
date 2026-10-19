// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// failingRevokeBroker is a broker whose revocation fails: the injected failure.
type failingRevokeBroker struct{}

func (failingRevokeBroker) MintForGrant(context.Context, *identity.Claims, uuid.UUID) (broker.Minted, error) {
	return broker.Minted{}, errors.New("not minted in this test")
}

func (failingRevokeBroker) RevokeRun(context.Context, uuid.UUID) error {
	return errors.New("injected: the broker could not revoke")
}

// An idle stop runs the kill's revocation cascade, not a subset of it: the run's
// stored proxy config is dropped, and a step that fails reaches the reaper under
// the key the kill's run.revoke row uses. Guarded by WARDYN_TEST_PG.
func TestPG_IdleStopRunsTheKillsRevocationCascade(t *testing.T) {
	pool := revocationPool(t)
	ctx := context.Background()
	pg := store.PG{Pool: pool}
	srv := api.New(api.Config{Store: pg, Broker: failingRevokeBroker{}})
	run := skewedRun(t, pg, 60)
	if err := pg.PutRunProxyConfig(ctx, run.ID, []byte("sealed")); err != nil {
		t.Fatalf("store the run's proxy config: %v", err)
	}

	out, err := lifecycleStopper{pool: pool, revoke: srv.RevokeStoppedRun}.StopRun(ctx, run.ID, time.Now().Add(time.Hour))
	if err != nil || !out.Applied {
		t.Fatalf("StopRun = %+v, %v; want applied", out, err)
	}
	if _, err := pg.GetRunProxyConfig(ctx, run.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the idle-stopped run's proxy config is still stored (read err %v); want it dropped", err)
	}
	if !strings.Contains(out.Errors["broker_error"], "injected") {
		t.Errorf("StopRun errors = %v; want the broker's failure under broker_error", out.Errors)
	}
}
